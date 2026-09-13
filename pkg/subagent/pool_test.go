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

func TestPoolExpiredDeadlineClassifiesAsTimedOutNotCancelled(t *testing.T) {
	// A parent ctx deadline firing mid-execution (the only expiry path now
	// that the pool no longer queues tasks behind a semaphore) must classify
	// as TimedOut, not Cancelled: DeadlineExceeded is checked before Canceled
	// in runTask's status switch. The executor honors its ctx the way the
	// real SubagentExecutor does.
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-ctx.Done()
			return ExecutionResult{}, ctx.Err()
		},
	}, PoolConfig{Timeout: time.Minute})

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	task, err := pool.StartTask(shortCtx, "work", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Status != TaskStatusTimedOut {
		t.Fatalf("status = %s, want %s (parent ctx deadline, not an explicit cancel)", completed.Status, TaskStatusTimedOut)
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

// TestPoolFinishTask_NilUsageOnExpiredDeadline verifies a task dying to its
// parent deadline mid-execution — which never produces an executor Usage —
// still finishes cleanly with a nil Usage rather than panicking or leaving a
// stale value.
func TestPoolFinishTask_NilUsageOnExpiredDeadline(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			<-ctx.Done()
			return ExecutionResult{}, ctx.Err()
		},
	}, PoolConfig{Timeout: time.Minute})

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	task, err := pool.StartTask(shortCtx, "work", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Usage != nil {
		t.Fatalf("completed.Usage = %+v, want nil for a task that died before reporting usage", completed.Usage)
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
// defaults for "general-purpose" (MaxToolCalls 6, Tools [file_ops]) and "bash"
// (MaxToolCalls 4, Tools [bash]); because SubagentExecutor.Execute prefers
// task.Config over the resolved agent-type profile, those defaults silently
// shadowed a project .deepai/agents/<type>.yaml|md for exactly those two types.
// The agent-type profile (builtin > YAML > MD) is now the single source of
// truth.
func TestResolveConfig_NoPerTypeDefaults(t *testing.T) {
	p := NewPool(nil, PoolConfig{})

	for _, agentType := range []string{"general-purpose", "bash", "coder"} {
		got := p.resolveConfig(SubagentConfig{AgentType: agentType})
		if got.AgentType != agentType {
			t.Fatalf("AgentType = %q, want %q", got.AgentType, agentType)
		}
		if got.MaxToolCalls != 0 {
			t.Fatalf("%s MaxToolCalls = %d, want 0 so the agent-type profile decides", agentType, got.MaxToolCalls)
		}
		if len(got.Tools) != 0 {
			t.Fatalf("%s Tools = %v, want empty so the agent-type profile decides", agentType, got.Tools)
		}
		if got.SystemPrompt != "" {
			t.Fatalf("%s SystemPrompt = %q, want empty so the agent-type profile decides", agentType, got.SystemPrompt)
		}
	}

	// An empty agent type still normalizes to general-purpose. Timeout stays
	// UNSET here on purpose: stamping the pool-wide default into the task's
	// own config would make every task look explicitly bounded, and runTask
	// has to tell "this caller named a deadline" apart from "nobody did" —
	// only the second may be overridden by a deadline the parent ctx already
	// carries. See TestPoolRunTask_ParentDeadlineWinsOverPoolDefault.
	empty := p.resolveConfig(SubagentConfig{})
	if empty.AgentType != "general-purpose" {
		t.Fatalf("empty AgentType resolved to %q, want general-purpose", empty.AgentType)
	}
	if empty.Timeout != 0 {
		t.Fatalf("Timeout = %v, want 0 (unset) so runTask can resolve the effective deadline", empty.Timeout)
	}

	// Caller-supplied values still win.
	explicit := p.resolveConfig(SubagentConfig{AgentType: "coder", MaxToolCalls: 12, Tools: []string{"bash"}})
	if explicit.MaxToolCalls != 12 || len(explicit.Tools) != 1 || explicit.Tools[0] != "bash" {
		t.Fatalf("caller values dropped: %+v", explicit)
	}
}

// TestPoolStartTaskCompletes_CarriesRunStats is the stats sibling of
// TestPoolStartTaskCompletes_CarriesUsage: the executor's ExecutionResult
// now carries a RunStats workload profile, and finishTask/snapshot must
// propagate it onto the Task so the task tool can expose it via
// Data["subagent_stats"].
func TestPoolStartTaskCompletes_CarriesRunStats(t *testing.T) {
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			return ExecutionResult{
				Result: "done",
				Usage:  &TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
				Stats: &RunStats{
					AgentType:     "general-purpose",
					ToolCalls:     7,
					LLMTurns:      9,
					SchemaRetries: 1,
					MaxToolCalls:  45,
					DurationMS:    1234,
				},
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
	if completed.Stats == nil {
		t.Fatal("completed.Stats = nil, want the executor's RunStats propagated through finishTask/snapshot")
	}
	want := RunStats{AgentType: "general-purpose", ToolCalls: 7, LLMTurns: 9, SchemaRetries: 1, MaxToolCalls: 45, DurationMS: 1234}
	if *completed.Stats != want {
		t.Fatalf("completed.Stats = %+v, want %+v", *completed.Stats, want)
	}
}

// TestPoolRunTask_DeadlineReachesExecutor pins the mechanism the chat wiring
// depends on: pkg/agent's graceful wall-clock wind-down (react.go's
// shouldTriggerWallClockWrapUp) reads ctx.Deadline() and nothing else, so a
// pool configured with a Timeout MUST hand its executor a ctx that carries
// one. A pool left at Timeout 0 hands over a deadline-free ctx, and the
// wind-down it would otherwise trigger can never fire — the subagent then runs
// until the parent run ends, which is what made an interactive review
// unbounded.
func TestPoolRunTask_DeadlineReachesExecutor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		wantDeadlne bool
	}{
		{"configured timeout carries a deadline", 30 * time.Second, true},
		{"zero timeout carries none", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				hasDeadline bool
				remaining   time.Duration
			)
			pool := NewPool(fakeExecutor{
				execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
					dl, ok := ctx.Deadline()
					mu.Lock()
					hasDeadline = ok
					if ok {
						remaining = time.Until(dl)
					}
					mu.Unlock()
					return ExecutionResult{Result: "done"}, nil
				},
			}, PoolConfig{Timeout: tc.timeout})

			ctx := context.Background()
			task, err := pool.StartTask(ctx, "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
			if err != nil {
				t.Fatalf("StartTask() error = %v", err)
			}
			if _, err := pool.Wait(ctx, task.ID); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if hasDeadline != tc.wantDeadlne {
				t.Fatalf("executor ctx deadline present = %v, want %v", hasDeadline, tc.wantDeadlne)
			}
			if tc.wantDeadlne && (remaining <= 0 || remaining > tc.timeout) {
				t.Fatalf("remaining until deadline = %v, want within (0, %v]", remaining, tc.timeout)
			}
		})
	}
}

// TestPoolDefaultTimeout_ReportsConfigured exists so the composition root's
// wiring (pkg/commands' registerChatTools) can be asserted on: the pool it
// builds must carry a real default timeout, not 0.
func TestPoolDefaultTimeout_ReportsConfigured(t *testing.T) {
	pool := NewPool(fakeExecutor{}, PoolConfig{Timeout: 90 * time.Second})
	if got := pool.DefaultTimeout(); got != 90*time.Second {
		t.Fatalf("DefaultTimeout() = %v, want 90s", got)
	}
	if got := NewPool(fakeExecutor{}, PoolConfig{}).DefaultTimeout(); got != 0 {
		t.Fatalf("DefaultTimeout() on an unconfigured pool = %v, want 0", got)
	}
}

// TestPoolRunTask_ParentDeadlineWinsOverPoolDefault pins the precedence the
// review gate depends on. The gate bounds its reviewer by wrapping the ctx it
// hands to the task tool (pkg/chat/review.go, config.yaml's review_timeout),
// not by setting SubagentConfig.Timeout. Once the pool acquired a default
// timeout of its own, a pool default SHORTER than the gate's bound would
// silently override it — raising review_timeout past the pool default would
// then do nothing, while the error text tells the user to do exactly that.
//
// So the pool default is for callers that bounded nothing: a ctx that already
// carries a deadline keeps it verbatim.
func TestPoolRunTask_ParentDeadlineWinsOverPoolDefault(t *testing.T) {
	var (
		mu        sync.Mutex
		remaining time.Duration
	)
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("executor ctx has no deadline, want the parent's")
				return ExecutionResult{}, nil
			}
			mu.Lock()
			remaining = time.Until(dl)
			mu.Unlock()
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{Timeout: 50 * time.Millisecond})

	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	task, err := pool.StartTask(parent, "test task", "do work", SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if _, err := pool.Wait(parent, task.ID); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if remaining < 30*time.Minute {
		t.Fatalf("remaining until deadline = %v, want the parent's ~1h — the pool default overrode a bound the caller had already set", remaining)
	}
}

// TestPoolRunTask_TaskTimeoutWinsOverParentDeadline keeps the explicit
// per-task bound at the top of the precedence chain: a caller that names a
// Timeout for THIS task means it, even under a looser parent.
func TestPoolRunTask_TaskTimeoutWinsOverParentDeadline(t *testing.T) {
	var (
		mu        sync.Mutex
		remaining time.Duration
	)
	pool := NewPool(fakeExecutor{
		execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("executor ctx has no deadline, want the task's own")
				return ExecutionResult{}, nil
			}
			mu.Lock()
			remaining = time.Until(dl)
			mu.Unlock()
			return ExecutionResult{Result: "done"}, nil
		},
	}, PoolConfig{Timeout: time.Hour})

	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	task, err := pool.StartTask(parent, "test task", "do work", SubagentConfig{
		AgentType: "general-purpose",
		Timeout:   2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if _, err := pool.Wait(parent, task.ID); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if remaining > 2*time.Minute || remaining < time.Minute {
		t.Fatalf("remaining until deadline = %v, want ~2m from SubagentConfig.Timeout", remaining)
	}
}

// TestPoolFinishTask_CompletionCarriesWoundDownReason pins the plumbing that
// makes a forced wrap-up visible. A subagent that runs out of wall clock is
// wound down gracefully by pkg/agent and still returns an answer, so the pool
// reports task_completed for it — identical, without this field, to a run that
// finished on its own terms.
func TestPoolFinishTask_CompletionCarriesWoundDownReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats *RunStats
		want  string
	}{
		{"wound down on the clock", &RunStats{BudgetExhausted: true, WoundDownReason: "deadline"}, "deadline"},
		{"finished cleanly", &RunStats{BudgetExhausted: false, WoundDownReason: ""}, ""},
		{"no stats at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := NewPool(fakeExecutor{
				execute: func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
					return ExecutionResult{Result: "verdict", Stats: tc.stats}, nil
				},
			}, PoolConfig{Timeout: time.Second})

			var (
				mu       sync.Mutex
				finished TaskEvent
			)
			ctx := WithEventSink(context.Background(), func(evt TaskEvent) {
				if evt.Type == "task_completed" {
					mu.Lock()
					finished = evt
					mu.Unlock()
				}
			})

			task, err := pool.StartTask(ctx, "review", "review it", SubagentConfig{AgentType: "correctness-reviewer"})
			if err != nil {
				t.Fatalf("StartTask() error = %v", err)
			}
			if _, err := pool.Wait(ctx, task.ID); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if finished.Type != "task_completed" {
				t.Fatalf("no task_completed event observed, got %q", finished.Type)
			}
			if finished.WoundDownReason != tc.want {
				t.Fatalf("WoundDownReason = %q, want %q", finished.WoundDownReason, tc.want)
			}
		})
	}
}
