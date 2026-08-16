package subagent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Cancelling one subagent must leave its siblings running. Before this, the
// per-task context existed but its cancel func was discarded by a defer, so
// the only way to stop a stuck subagent was Ctrl+C — which killed the whole
// fan-out, including the ones that had already been working for minutes.

// blockingExecutor runs until its context is cancelled, recording which tasks
// finished and how.
type blockingExecutor struct {
	mu      sync.Mutex
	started map[string]bool
	ended   map[string]error
	release chan struct{}
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{
		started: map[string]bool{},
		ended:   map[string]error{},
		release: make(chan struct{}),
	}
}

func (e *blockingExecutor) Execute(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
	e.mu.Lock()
	e.started[task.ID] = true
	e.mu.Unlock()

	select {
	case <-ctx.Done():
		e.mu.Lock()
		e.ended[task.ID] = ctx.Err()
		e.mu.Unlock()
		return ExecutionResult{}, ctx.Err()
	case <-e.release:
		e.mu.Lock()
		e.ended[task.ID] = nil
		e.mu.Unlock()
		return ExecutionResult{Result: "ok:" + task.ID}, nil
	}
}

func (e *blockingExecutor) waitStarted(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		e.mu.Lock()
		got := len(e.started)
		e.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d/%d tasks started", got, n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestPool_CancelTaskStopsOnlyThatTask(t *testing.T) {
	exec := newBlockingExecutor()
	pool := NewPool(exec, PoolConfig{})

	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		task, err := pool.StartTask(ctx, "task", "prompt", SubagentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	exec.waitStarted(t, 3)

	if !pool.CancelTask(ids[1]) {
		t.Fatal("CancelTask should report that it cancelled a running task")
	}

	// The cancelled one unwinds...
	got, err := pool.Wait(ctx, ids[1])
	if err == nil && got.Status != TaskStatusCancelled {
		t.Fatalf("task status = %v, want cancelled", got.Status)
	}

	// ...while the others are still running and only finish when released.
	exec.mu.Lock()
	_, aEnded := exec.ended[ids[0]]
	_, cEnded := exec.ended[ids[2]]
	exec.mu.Unlock()
	if aEnded || cEnded {
		t.Fatal("cancelling one task must not stop its siblings")
	}

	close(exec.release)
	for _, id := range []string{ids[0], ids[2]} {
		task, err := pool.Wait(ctx, id)
		if err != nil {
			t.Fatalf("sibling %s failed: %v", id, err)
		}
		if task.Status != TaskStatusCompleted {
			t.Fatalf("sibling %s status = %v, want completed", id, task.Status)
		}
	}
}

func TestPool_CancelTaskUnknownID(t *testing.T) {
	pool := NewPool(newBlockingExecutor(), PoolConfig{})
	if pool.CancelTask("nope") {
		t.Fatal("cancelling an unknown task must report false, not panic")
	}
}

func TestPool_CancelTaskAfterCompletion(t *testing.T) {
	exec := newBlockingExecutor()
	close(exec.release) // finish immediately
	pool := NewPool(exec, PoolConfig{})

	task, err := pool.StartTask(context.Background(), "task", "prompt", SubagentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	done, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != TaskStatusCompleted {
		t.Fatalf("status = %v, want completed", done.Status)
	}
	// Wait reaps the entry by design (pool.go: the transcript would otherwise
	// be retained for the process lifetime), so a late Ctrl+X on a task that
	// just resolved must simply report false rather than panic.
	if pool.CancelTask(task.ID) {
		t.Fatal("cancelling an already-reaped task must report false")
	}
}
