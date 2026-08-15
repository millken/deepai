package subagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/models"
)

var taskSeq uint64
var taskRequestSeq uint64

type Pool struct {
	executor Executor
	tasks    sync.Map
	cfg      PoolConfig
}

// NewPool creates a pool with NO local concurrency limiter, deliberately:
// there is no semaphore capping how many tasks run at once. Concurrency is
// governed by the issuers (react.go caps task calls at maxTaskCallsPerRun
// per run) and by the provider's own rate limiting — a burst that exceeds
// the API key's limits gets 429s, which the provider layer retries with
// backoff and surfaces as a visible error when persistent. A hardcoded local
// cap (the old MaxConcurrent=4) only added invisible queueing: the N+1th
// task silently waited on a slot with no feedback to the model or user, and
// no number fits every provider's real capacity anyway.
func NewPool(executor Executor, cfg PoolConfig) *Pool {
	// cfg.Timeout <= 0 means NO pool-wide deadline: a subagent's lifetime is
	// governed by its parent run's context (user Ctrl+C / the parent's own
	// request timeout) plus the in-run safety nets (repeat-call breaker,
	// stream idle watchdog, context-window compaction). A hardcoded wall-clock
	// governor cannot fit both quick lookups and whole-project delegations,
	// which can legitimately run for hours.
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Pool{
		executor: executor,
		cfg:      cfg,
	}
}

func (p *Pool) StartTask(ctx context.Context, description, prompt string, cfg SubagentConfig) (*Task, error) {
	if p == nil {
		return nil, errors.New("subagent pool is nil")
	}
	if p.executor == nil {
		return nil, errors.New("subagent executor is required")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}

	resolved := p.resolveConfig(cfg)
	now := time.Now().UTC()
	task := &Task{
		ID:          newTaskID(),
		RequestID:   newTaskRequestID(),
		Type:        SubagentType(resolved.EffectiveAgentType()),
		Config:      resolved,
		Status:      TaskStatusPending,
		Description: strings.TrimSpace(description),
		Prompt:      prompt,
		createdAt:   now,
		done:        make(chan struct{}),
	}
	if task.Description == "" {
		task.Description = task.Prompt
	}

	p.tasks.Store(task.ID, task)

	p.emit(ctx, TaskEvent{
		Type:        "task_started",
		TaskID:      task.ID,
		RequestID:   task.RequestID,
		Description: task.Description,
		Message:     "task queued",
	})

	go p.runTask(ctx, task)
	return task.snapshot(), nil
}

// Wait blocks until taskID finishes and returns its final snapshot. The task
// entry is consumed (deleted from the pool) by the first successful Wait, so
// a later Wait or GetTask for the same taskID returns not-found.
func (p *Pool) Wait(ctx context.Context, taskID string) (*Task, error) {
	task, ok := p.getTask(taskID)
	if !ok {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	select {
	case <-task.done:
		snap := task.snapshot()
		// The task has finished (completed/failed/timed out/cancelled) and its
		// snapshot has been taken, so it's safe to drop the map entry —
		// otherwise the Prompt, Result, and full message transcript are
		// retained for the rest of the process lifetime.
		p.tasks.Delete(taskID)
		return snap, nil
	case <-ctx.Done():
		// Do NOT delete here: the task itself is still running (only this
		// waiter is bailing on its own ctx). Known remaining window: if every
		// waiter for a task bails via ctx cancellation, the entry stays in
		// the map until process exit. Acceptable for now — out of scope.
		return nil, ctx.Err()
	}
}

// GetTask returns the current snapshot for taskID. Once the first successful
// Wait for taskID has consumed (deleted) its entry, GetTask returns (nil, false)
// even though the task did in fact run to completion.
func (p *Pool) GetTask(taskID string) (*Task, bool) {
	task, ok := p.getTask(taskID)
	if !ok {
		return nil, false
	}
	return task.snapshot(), true
}

func (p *Pool) getTask(taskID string) (*Task, bool) {
	task, ok := p.tasks.Load(taskID)
	if !ok {
		return nil, false
	}
	typed, ok := task.(*Task)
	return typed, ok
}

func (p *Pool) runTask(parentCtx context.Context, task *Task) {
	defer close(task.done)

	task.mu.Lock()
	task.Status = TaskStatusRunning
	task.mu.Unlock()

	p.emit(parentCtx, TaskEvent{
		Type:        "task_running",
		TaskID:      task.ID,
		RequestID:   task.RequestID,
		Description: task.Description,
		Message:     "task started",
	})

	timeout := task.Config.Timeout
	if timeout <= 0 {
		timeout = p.cfg.Timeout
	}
	// timeout <= 0 (no task- or pool-level deadline configured): run under
	// the parent ctx directly — lifetime is the parent run's lifetime.
	var runCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(parentCtx, timeout)
	} else {
		runCtx, cancel = context.WithCancel(parentCtx)
	}
	defer cancel()

	result, err := p.executor.Execute(runCtx, task, func(evt TaskEvent) {
		if evt.TaskID == "" {
			evt.TaskID = task.ID
		}
		if evt.RequestID == "" {
			evt.RequestID = task.RequestID
		}
		if evt.Description == "" {
			evt.Description = task.Description
		}
		p.emit(parentCtx, evt)
	})

	status := TaskStatusCompleted
	switch {
	case err == nil:
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		status = TaskStatusTimedOut
	case errors.Is(err, context.DeadlineExceeded):
		status = TaskStatusTimedOut
	case errors.Is(parentCtx.Err(), context.Canceled):
		status = TaskStatusCancelled
	case errors.Is(err, context.Canceled):
		status = TaskStatusCancelled
	default:
		status = TaskStatusFailed
	}

	p.finishTask(parentCtx, task, status, result.Result, err, result.Messages, result.Usage)
}

func (p *Pool) finishTask(ctx context.Context, task *Task, status TaskStatus, result string, err error, messages []models.Message, usage *TokenUsage) {
	task.mu.Lock()
	task.Status = status
	task.Result = result
	task.Messages = append([]models.Message(nil), messages...)
	task.Usage = usage
	task.completedAt = time.Now().UTC()
	if err != nil {
		task.Error = err.Error()
	}
	task.mu.Unlock()

	event := TaskEvent{
		TaskID:      task.ID,
		RequestID:   task.RequestID,
		Description: task.Description,
		Result:      result,
	}

	switch status {
	case TaskStatusCompleted:
		event.Type = "task_completed"
		event.Message = "task completed"
	case TaskStatusTimedOut:
		event.Type = "task_timed_out"
		event.Message = "task timed out"
		event.Error = task.Error
	case TaskStatusFailed:
		event.Type = "task_failed"
		event.Message = "task failed"
		event.Error = task.Error
	case TaskStatusCancelled:
		event.Type = "task_cancelled"
		event.Message = "task cancelled"
		event.Error = task.Error
	default:
		event.Type = "task_failed"
		event.Message = "task failed"
		event.Error = task.Error
	}

	p.cfg.Logger.Info("subagent task", "id", task.ID, "request_id", task.RequestID, "type", task.Type, "status", task.Status)
	p.emit(ctx, event)
}

func (p *Pool) emit(ctx context.Context, evt TaskEvent) {
	EmitEvent(ctx, evt)
}

// resolveConfig normalizes a caller-supplied SubagentConfig: it fills in the
// agent type and the pool-wide Timeout fallback, sanitizes the values, and
// copies the caller's slices so a task can never alias them. It deliberately
// injects NO per-agent-type configuration of its own — MaxToolCalls, Tools,
// SystemPrompt and Model come from the agent-type profile (builtin > project
// YAML > project MD > plugin MD), resolved by the executor.
//
// The pool used to seed hardcoded per-type defaults here (general-purpose:
// MaxTurns 6 + Tools [file_ops]; bash: MaxTurns 4 + Tools [bash]). Because
// SubagentExecutor.Execute prefers task.Config over the resolved profile, those
// defaults silently shadowed a project .deepai/agents/<type>.yaml|md for
// exactly those two types, and pinned general-purpose to file_ops even though
// its builtin profile means "unrestricted". The profile is now the single
// source of truth.
func (p *Pool) resolveConfig(cfg SubagentConfig) SubagentConfig {
	resolved := cfg
	resolved.AgentType = cfg.EffectiveAgentType()
	if resolved.AgentType == "" {
		resolved.AgentType = "general-purpose"
	}
	resolved.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	resolved.Model = strings.TrimSpace(cfg.Model)
	if len(cfg.Tools) > 0 {
		resolved.Tools = append([]string(nil), cfg.Tools...)
	}
	if len(cfg.ContextFiles) > 0 {
		resolved.ContextFiles = append([]string(nil), cfg.ContextFiles...)
	}
	// Negative values are meaningless and would read as "explicitly set"
	// downstream (a negative MaxTokensBudget, for instance, is not the same as
	// unlimited); normalize them to the unset zero so the profile applies. A
	// model can produce one via the task tool's max_tool_calls / token_budget
	// arguments.
	if resolved.MaxToolCalls < 0 {
		resolved.MaxToolCalls = 0
	}
	if resolved.TokenBudget < 0 {
		resolved.TokenBudget = 0
	}
	if resolved.Timeout <= 0 {
		resolved.Timeout = p.cfg.Timeout
	}
	return resolved
}

func newTaskID() string {
	seq := atomic.AddUint64(&taskSeq, 1)
	return fmt.Sprintf("task_%d_%d", time.Now().UTC().UnixNano(), seq)
}

func newTaskRequestID() string {
	seq := atomic.AddUint64(&taskRequestSeq, 1)
	return fmt.Sprintf("subreq_%d_%d", time.Now().UTC().UnixNano(), seq)
}
