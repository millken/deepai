package subagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

type fakeExecutor struct {
	execute func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error)
}

func (f fakeExecutor) Execute(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
	return f.execute(ctx, task, emit)
}

func TestPoolStartTaskCompletes(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			emit(TaskEvent{Type: "task_running", Message: "working"})
			return ExecutionResult{
				Result: "done",
				Messages: []models.Message{
					{ID: "m1", SessionID: task.ID, Role: models.RoleAI, Content: "done"},
				},
			}, nil
		},
	}, PoolConfig{Timeout: time.Second})

	var events []TaskEvent
	ctx := WithEventSink(context.Background(), func(evt TaskEvent) {
		events = append(events, evt)
	})

	task, err := pool.StartTask(ctx, "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Status != TaskStatusCompleted {
		t.Fatalf("status = %s, want %s", completed.Status, TaskStatusCompleted)
	}
	if completed.Result != "done" {
		t.Fatalf("result = %q, want %q", completed.Result, "done")
	}
	if completed.RequestID == "" {
		t.Fatal("RequestID = empty, want generated request id")
	}
	if len(completed.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(completed.Messages))
	}
	if len(events) < 3 {
		t.Fatalf("events = %d, want at least 3", len(events))
	}
	if events[0].Type != "task_started" {
		t.Fatalf("first event = %s, want task_started", events[0].Type)
	}
	if events[0].RequestID == "" {
		t.Fatal("first event missing request id")
	}
	if events[len(events)-1].Type != "task_completed" {
		t.Fatalf("last event = %s, want task_completed", events[len(events)-1].Type)
	}
}

func TestPoolStartTaskTimesOut(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-ctx.Done()
			return ExecutionResult{}, ctx.Err()
		},
	}, PoolConfig{Timeout: 20 * time.Millisecond})

	task, err := pool.StartTask(context.Background(), "timeout task", "sleep", SubagentConfig{AgentType: "bash"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Status != TaskStatusTimedOut {
		t.Fatalf("status = %s, want %s", completed.Status, TaskStatusTimedOut)
	}
	if completed.Error == "" {
		t.Fatalf("expected timeout error, got %q", completed.Error)
	}
}

func TestPoolWaitDeletesCompletedTaskFromMap(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{Timeout: time.Second})

	task, err := pool.StartTask(context.Background(), "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	if _, err := pool.Wait(context.Background(), task.ID); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	if got, ok := pool.GetTask(task.ID); ok {
		t.Fatalf("GetTask() after Wait completed = (%v, true), want (nil, false); task map leaked the transcript", got)
	}
}

func TestPoolStartTaskParentCancelledReportsCancelledNotFailed(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-ctx.Done()
			return ExecutionResult{}, ctx.Err()
		},
	}, PoolConfig{Timeout: time.Second})

	var events []TaskEvent
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	sinkCtx := WithEventSink(ctx, func(evt TaskEvent) {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
	})

	task, err := pool.StartTask(sinkCtx, "cancel task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	// Give runTask a moment to enter the executor before cancelling.
	time.Sleep(10 * time.Millisecond)
	cancel()

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Status != TaskStatusCancelled {
		t.Fatalf("status = %s, want %s", completed.Status, TaskStatusCancelled)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, evt := range events {
		if evt.Type == "task_cancelled" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a task_cancelled event, got %+v", events)
	}
}

func TestPoolPreSemaphoreBailDistinguishesDeadlineFromCancel(t *testing.T) {
	// MaxConcurrent 1, first task holds the only slot indefinitely, so the
	// second task must wait at the pre-semaphore select in runTask. Its
	// parent ctx has a short deadline (not an explicit cancel), so the bail
	// must classify as TimedOut, not Cancelled, mirroring the post-execution
	// ordering (DeadlineExceeded checked before Canceled).
	release := make(chan struct{})
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-release
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{MaxConcurrent: 1, Timeout: time.Minute})
	defer close(release)

	holder, err := pool.StartTask(context.Background(), "holder", "hold the slot", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() (holder) error = %v", err)
	}
	// Give the holder task time to acquire the only semaphore slot.
	time.Sleep(20 * time.Millisecond)

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	waiter, err := pool.StartTask(shortCtx, "waiter", "wait for the slot", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() (waiter) error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), waiter.ID)
	if err != nil {
		t.Fatalf("Wait() (waiter) error = %v", err)
	}
	if completed.Status != TaskStatusTimedOut {
		t.Fatalf("waiter status = %s, want %s (parent ctx deadline, not an explicit cancel)", completed.Status, TaskStatusTimedOut)
	}

	release <- struct{}{}
	if _, err := pool.Wait(context.Background(), holder.ID); err != nil {
		t.Fatalf("Wait() (holder) error = %v", err)
	}
}

func TestPoolWaitUnknownTask(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			return ExecutionResult{}, nil
		},
	}, PoolConfig{})

	if _, err := pool.Wait(context.Background(), "missing"); err == nil {
		t.Fatal("Wait() expected error for missing task")
	}
}

// TestPoolStartTaskCompletes_CarriesUsage is the RED test for M2-2 (12a):
// the executor's ExecutionResult now carries a Usage, and finishTask/snapshot
// must propagate it onto the Task so callers (pkg/tools' task tool) can read
// completed.Usage. Before ExecutionResult.Usage and Task.Usage exist, this
// fails to compile; that is the RED signature for this sub-item.
func TestPoolStartTaskCompletes_CarriesUsage(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			return ExecutionResult{
				Result: "done",
				Usage:  &TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			}, nil
		},
	}, PoolConfig{Timeout: time.Second})

	task, err := pool.StartTask(context.Background(), "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Usage == nil {
		t.Fatal("completed.Usage = nil, want the executor's TokenUsage propagated through finishTask/snapshot")
	}
	if completed.Usage.PromptTokens != 100 || completed.Usage.CompletionTokens != 50 || completed.Usage.TotalTokens != 150 {
		t.Fatalf("completed.Usage = %+v, want {100,50,150}", completed.Usage)
	}
}

// TestPoolSnapshot_CopiesTokenUsageNotSharesPointer covers review nit #3:
// snapshot() must copy the TokenUsage value, matching the isolation contract
// it already applies to Messages (a fresh slice per snapshot, not the live
// one), instead of handing out the same *TokenUsage the executor returned.
func TestPoolSnapshot_CopiesTokenUsageNotSharesPointer(t *testing.T) {
	srcUsage := &TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			return ExecutionResult{Result: "done", Usage: srcUsage}, nil
		},
	}, PoolConfig{Timeout: time.Second})

	task, err := pool.StartTask(context.Background(), "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Usage == srcUsage {
		t.Fatal("snapshot's Usage shares the executor's TokenUsage pointer; want an isolated copy")
	}
	if completed.Usage == nil || *completed.Usage != *srcUsage {
		t.Fatalf("completed.Usage = %+v, want a value-equal copy of %+v", completed.Usage, srcUsage)
	}
	completed.Usage.TotalTokens = 999
	if srcUsage.TotalTokens == 999 {
		t.Fatal("mutating the snapshot's Usage mutated the executor's original — pointer was shared, not copied")
	}
}

// TestPoolFinishTask_NilUsageOnPreSemaphoreBail verifies the pre-semaphore and
// cancel bail paths (which never reach the executor) still finish cleanly
// with a nil Usage rather than panicking or leaving a stale value.
func TestPoolFinishTask_NilUsageOnPreSemaphoreBail(t *testing.T) {
	release := make(chan struct{})
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-release
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{MaxConcurrent: 1, Timeout: time.Minute})
	defer close(release)

	holder, err := pool.StartTask(context.Background(), "holder", "hold the slot", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() (holder) error = %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	waiter, err := pool.StartTask(shortCtx, "waiter", "wait for the slot", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() (waiter) error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), waiter.ID)
	if err != nil {
		t.Fatalf("Wait() (waiter) error = %v", err)
	}
	if completed.Usage != nil {
		t.Fatalf("completed.Usage = %+v, want nil for a task that never reached the executor", completed.Usage)
	}

	release <- struct{}{}
	if _, err := pool.Wait(context.Background(), holder.ID); err != nil {
		t.Fatalf("Wait() (holder) error = %v", err)
	}
}

// TestResolveConfig_PreservesTokenBudget is the RED test for M2-2 (12d)'s
// pool-level plumbing: without resolveConfig forwarding TokenBudget, the
// task tool's token_budget arg would be silently dropped before it ever
// reaches the executor.
func TestResolveConfig_PreservesTokenBudget(t *testing.T) {
	p := NewPool(nil, PoolConfig{})
	got := p.resolveConfig(SubagentConfig{AgentType: "coder", TokenBudget: 500})
	if got.TokenBudget != 500 {
		t.Fatalf("resolveConfig dropped TokenBudget: got %d, want 500", got.TokenBudget)
	}
	if got2 := p.resolveConfig(SubagentConfig{AgentType: "coder"}); got2.TokenBudget != 0 {
		t.Fatalf("unexpected TokenBudget %d for config without TokenBudget", got2.TokenBudget)
	}
}

func TestResolveConfig_PreservesModel(t *testing.T) {
	p := NewPool(nil, PoolConfig{})
	got := p.resolveConfig(SubagentConfig{AgentType: "coder", Model: "m-1"})
	if got.Model != "m-1" {
		t.Fatalf("resolveConfig dropped Model: got %q, want m-1", got.Model)
	}
	// empty Model must not be forced onto the resolved config
	if got2 := p.resolveConfig(SubagentConfig{AgentType: "coder"}); got2.Model != "" {
		t.Fatalf("unexpected Model %q for config without Model", got2.Model)
	}
}

// TestPoolStartTask_ContextFilesReachExecutor is the RED test for the M2-4
// no-op bug: resolveConfig (pool.go) copies Tools, Model, TokenBudget, etc.
// from the caller's SubagentConfig onto the resolved config, but never
// ContextFiles — so the task tool's context_files argument (pkg/tools/subagent.go)
// is silently dropped before it ever reaches the executor, even though
// SubagentExecutor.Execute (pkg/agent/subagent.go) fully supports it. This
// test crosses the real StartTask seam (not resolveConfig directly) with a
// capturing fake executor, asserting the executor actually observes
// task.Config.ContextFiles.
func TestPoolStartTask_ContextFilesReachExecutor(t *testing.T) {
	var gotContextFiles []string
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			gotContextFiles = task.Config.ContextFiles
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{Timeout: time.Second})

	wantFiles := []string{"a.go", "b.md"}
	task, err := pool.StartTask(context.Background(), "test task", "do work", SubagentConfig{
		AgentType:    "general-purpose",
		ContextFiles: wantFiles,
	})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	if _, err := pool.Wait(context.Background(), task.ID); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	if len(gotContextFiles) != len(wantFiles) {
		t.Fatalf("executor saw ContextFiles = %v, want %v", gotContextFiles, wantFiles)
	}
	for i, f := range wantFiles {
		if gotContextFiles[i] != f {
			t.Fatalf("executor saw ContextFiles = %v, want %v", gotContextFiles, wantFiles)
		}
	}
}

// TestResolveConfig_NoPerTypeDefaults pins the pool's role: it resolves only
// what the CALLER passed (plus the pool-wide Timeout fallback) and injects no
// per-agent-type configuration of its own. The pool used to seed hardcoded
// defaults for "general-purpose" (MaxTurns 6, Tools [file_ops]) and "bash"
// (MaxTurns 4, Tools [bash]); because SubagentExecutor.Execute prefers
// task.Config over the resolved agent-type profile, those defaults silently
// shadowed a project .deepai/agents/<type>.yaml|md for exactly those two types.
// The agent-type profile (builtin > YAML > MD) is now the single source of
// truth, with the executor's own safety floor as the last resort.
func TestResolveConfig_NoPerTypeDefaults(t *testing.T) {
	p := NewPool(nil, PoolConfig{})

	for _, agentType := range []string{"general-purpose", "bash", "coder"} {
		got := p.resolveConfig(SubagentConfig{AgentType: agentType})
		if got.AgentType != agentType {
			t.Fatalf("AgentType = %q, want %q", got.AgentType, agentType)
		}
		if got.MaxTurns != 0 {
			t.Fatalf("%s MaxTurns = %d, want 0 so the agent-type profile decides", agentType, got.MaxTurns)
		}
		if len(got.Tools) != 0 {
			t.Fatalf("%s Tools = %v, want empty so the agent-type profile decides", agentType, got.Tools)
		}
		if got.SystemPrompt != "" {
			t.Fatalf("%s SystemPrompt = %q, want empty so the agent-type profile decides", agentType, got.SystemPrompt)
		}
	}

	// An empty agent type still normalizes to general-purpose, and the
	// pool-wide Timeout still applies as the per-task deadline.
	empty := p.resolveConfig(SubagentConfig{})
	if empty.AgentType != "general-purpose" {
		t.Fatalf("empty AgentType resolved to %q, want general-purpose", empty.AgentType)
	}
	if empty.Timeout != p.cfg.Timeout {
		t.Fatalf("Timeout = %v, want the pool-wide %v", empty.Timeout, p.cfg.Timeout)
	}

	// Caller-supplied values still win.
	explicit := p.resolveConfig(SubagentConfig{AgentType: "coder", MaxTurns: 12, Tools: []string{"bash"}})
	if explicit.MaxTurns != 12 || len(explicit.Tools) != 1 || explicit.Tools[0] != "bash" {
		t.Fatalf("caller values dropped: %+v", explicit)
	}
}
