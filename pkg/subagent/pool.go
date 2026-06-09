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
	sem      chan struct{}
}

func NewPool(executor Executor, cfg PoolConfig) *Pool {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Defaults == nil {
		cfg.Defaults = map[string]SubagentConfig{
			"general-purpose": {
				AgentType: "general-purpose",
				MaxTurns:  6,
				Timeout:   cfg.Timeout,
				Tools:     []string{"file_ops"},
			},
			"bash": {
				AgentType: "bash",
				MaxTurns:  4,
				Timeout:   cfg.Timeout,
				Tools:     []string{"bash"},
			},
		}
	}
	return &Pool{
		executor: executor,
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
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

func (p *Pool) Wait(ctx context.Context, taskID string) (*Task, error) {
	task, ok := p.getTask(taskID)
	if !ok {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	select {
	case <-task.done:
		return task.snapshot(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

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

	select {
	case p.sem <- struct{}{}:
	case <-parentCtx.Done():
		p.finishTask(parentCtx, task, TaskStatusFailed, "", parentCtx.Err(), nil)
		return
	}
	defer func() { <-p.sem }()

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
	runCtx, cancel := context.WithTimeout(parentCtx, timeout)
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
	default:
		status = TaskStatusFailed
	}

	p.finishTask(parentCtx, task, status, result.Result, err, result.Messages)
}

func (p *Pool) finishTask(ctx context.Context, task *Task, status TaskStatus, result string, err error, messages []models.Message) {
	task.mu.Lock()
	task.Status = status
	task.Result = result
	task.Messages = append([]models.Message(nil), messages...)
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

func (p *Pool) resolveConfig(cfg SubagentConfig) SubagentConfig {
	agentType := cfg.EffectiveAgentType()
	if agentType == "" {
		agentType = "general-purpose"
	}

	base, ok := p.cfg.Defaults[agentType]
	if !ok {
		base, ok = p.cfg.Defaults["general-purpose"]
	}
	if !ok {
		base = SubagentConfig{AgentType: "general-purpose"}
	}

	if cfg.AgentType != "" {
		base.AgentType = cfg.AgentType
	}
	if base.AgentType == "" {
		base.AgentType = agentType
	}
	if cfg.MaxTurns > 0 {
		base.MaxTurns = cfg.MaxTurns
	}
	if cfg.Timeout > 0 {
		base.Timeout = cfg.Timeout
	}
	if strings.TrimSpace(cfg.SystemPrompt) != "" {
		base.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	}
	if len(cfg.Tools) > 0 {
		base.Tools = append([]string(nil), cfg.Tools...)
	}
	if strings.TrimSpace(cfg.Model) != "" {
		base.Model = strings.TrimSpace(cfg.Model)
	}
	if base.Timeout <= 0 {
		base.Timeout = p.cfg.Timeout
	}
	return base
}

func newTaskID() string {
	seq := atomic.AddUint64(&taskSeq, 1)
	return fmt.Sprintf("task_%d_%d", time.Now().UTC().UnixNano(), seq)
}

func newTaskRequestID() string {
	seq := atomic.AddUint64(&taskRequestSeq, 1)
	return fmt.Sprintf("subreq_%d_%d", time.Now().UTC().UnixNano(), seq)
}
