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

func TestResolveConfig_TypedAgentDoesNotInheritGeneralDefaults(t *testing.T) {
	p := NewPool(nil, PoolConfig{})

	// "coder" has no pool default → must NOT borrow general-purpose's MaxTurns(6)
	// or Tools([file_ops]); leave them unset so the executor's profile applies.
	coder := p.resolveConfig(SubagentConfig{AgentType: "coder"})
	if coder.AgentType != "coder" {
		t.Fatalf("AgentType = %q, want coder", coder.AgentType)
	}
	if coder.MaxTurns != 0 {
		t.Fatalf("coder MaxTurns = %d, want 0 (profile decides), not the general-purpose cap", coder.MaxTurns)
	}
	if len(coder.Tools) != 0 {
		t.Fatalf("coder Tools = %v, want empty (profile decides), not general-purpose's file_ops", coder.Tools)
	}

	// general-purpose still gets its pool default.
	gp := p.resolveConfig(SubagentConfig{AgentType: "general-purpose"})
	if gp.MaxTurns != 6 {
		t.Fatalf("general-purpose MaxTurns = %d, want 6 (pool default preserved)", gp.MaxTurns)
	}
}
