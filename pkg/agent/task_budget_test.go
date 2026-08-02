package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// budgetCapturingTaskPool is a fake subagent pool (same structural shape as
// the other task_*_test.go fakes) that records the SubagentConfig it
// receives, so the end-to-end test can assert on what react.go's Run loop
// actually threaded through to the task tool's handler.
type budgetCapturingTaskPool struct {
	mu       sync.Mutex
	started  int
	gotCfg   subagent.SubagentConfig
	gotCfgOK bool
}

func (p *budgetCapturingTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	p.mu.Lock()
	p.started++
	p.gotCfg = cfg
	p.gotCfgOK = true
	id := fmt.Sprintf("budget-task-%d", p.started)
	p.mu.Unlock()
	return &subagent.Task{ID: id}, nil
}

func (p *budgetCapturingTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok:" + taskID}, nil
}

func (p *budgetCapturingTaskPool) config() (subagent.SubagentConfig, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotCfg, p.gotCfgOK
}

// budgetTaskCallProvider emits, on its first turn, ONE stream chunk carrying
// both a Usage report (simulating tokens already spent by the parent's own
// LLM call THIS turn — accumulated into a.usage before tool dispatch, see
// react.go) and a single "task" tool call with the given explicit
// token_budget arg (0 = omit the arg entirely). It ends the run with no tool
// calls on the next turn.
type budgetTaskCallProvider struct {
	turn                int
	usageTotalTokens    int
	explicitTokenBudget float64 // 0 = omit the arg
	hasExplicitBudget   bool
}

func (p *budgetTaskCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *budgetTaskCallProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		args := map[string]any{"description": "sub A", "prompt": "do A"}
		if p.hasExplicitBudget {
			args["token_budget"] = p.explicitTokenBudget
		}
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-a", Name: "task", Arguments: args},
				},
				Usage: &llm.Usage{TotalTokens: p.usageTotalTokens},
				Stop:  "tool_calls",
				Done:  true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// runBudgetCase drives one end-to-end Run() with the given parent budget and
// per-turn usage, returning the SubagentConfig.TokenBudget the fake pool's
// StartTask actually received.
func runBudgetCase(t *testing.T, parentBudget, usageTotalTokens int, explicitBudget float64, hasExplicit bool) int {
	t.Helper()
	pool := &budgetCapturingTaskPool{}

	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	a := New(AgentConfig{
		LLMProvider: &budgetTaskCallProvider{
			usageTotalTokens:    usageTotalTokens,
			explicitTokenBudget: explicitBudget,
			hasExplicitBudget:   hasExplicit,
		},
		Tools:           reg,
		MaxTurns:        5,
		MaxTokensBudget: parentBudget,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "delegate to a sub-agent"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult")
	}
	cfg, ok := pool.config()
	if !ok {
		t.Fatal("StartTask was never called")
	}
	return cfg.TokenBudget
}

// TestParentBudgetPassthrough_DispatchInjectsRemainingIntoTaskConfig is the
// RED test for the M2.2 carry-forward: react.go's Run loop must inject the
// parent's REMAINING token budget (max(0, MaxTokensBudget-usage.TotalTokens))
// into the tool ctx at batch-dispatch time, and the task tool must fold it
// into SubagentConfig.TokenBudget as min(nonzero values) — the parent
// remaining caps an explicit token_budget arg and becomes the default when
// no explicit arg is given.
//
// RED signature (today): react.go's Run never calls
// tools.WithRemainingTokenBudget, so the task tool never sees a parent
// remaining budget and cfg.TokenBudget is always just the explicit arg (or
// 0) regardless of a.maxTokensBudget/usage.
func TestParentBudgetPassthrough_DispatchInjectsRemainingIntoTaskConfig(t *testing.T) {
	// Parent budget=1000, usage=400 at dispatch time, no explicit arg →
	// remaining=600 becomes the default.
	if got := runBudgetCase(t, 1000, 400, 0, false); got != 600 {
		t.Fatalf("no explicit arg: cfg.TokenBudget = %d, want 600", got)
	}

	// Parent budget=1000, usage=400, explicit=900 → capped to
	// min(600,900)=600.
	if got := runBudgetCase(t, 1000, 400, 900, true); got != 600 {
		t.Fatalf("explicit=900: cfg.TokenBudget = %d, want 600", got)
	}

	// Parent budget=1000, usage=400, explicit=300 → stays 300
	// (min(600,300)=300).
	if got := runBudgetCase(t, 1000, 400, 300, true); got != 300 {
		t.Fatalf("explicit=300: cfg.TokenBudget = %d, want 300", got)
	}

	// No parent budget (MaxTokensBudget=0) → explicit arg passes through
	// unchanged.
	if got := runBudgetCase(t, 0, 400, 900, true); got != 900 {
		t.Fatalf("no parent budget, explicit=900: cfg.TokenBudget = %d, want 900 (unchanged)", got)
	}

	// No parent budget, no explicit arg → stays 0 (unlimited), unchanged.
	if got := runBudgetCase(t, 0, 400, 0, false); got != 0 {
		t.Fatalf("no parent budget, no explicit arg: cfg.TokenBudget = %d, want 0 (unchanged)", got)
	}
}

// multiBudgetCapturingTaskPool is a fake subagent pool that records EVERY
// SubagentConfig it receives (as opposed to budgetCapturingTaskPool, which
// only needs the last one) — needed for the parallel-batch case below, where
// two task calls dispatch concurrently and both configs must be inspected.
type multiBudgetCapturingTaskPool struct {
	mu      sync.Mutex
	configs []subagent.SubagentConfig
}

func (p *multiBudgetCapturingTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	p.mu.Lock()
	p.configs = append(p.configs, cfg)
	id := fmt.Sprintf("multi-budget-task-%d", len(p.configs))
	p.mu.Unlock()
	return &subagent.Task{ID: id}, nil
}

func (p *multiBudgetCapturingTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok:" + taskID}, nil
}

func (p *multiBudgetCapturingTaskPool) snapshot() []subagent.SubagentConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]subagent.SubagentConfig(nil), p.configs...)
}

// twoTaskCallBudgetProvider emits ONE assistant turn with 2 distinct task
// tool calls (same shape as task_parallel_test.go's twoTaskCallProvider, so
// the ParallelSafe batch path engages), also reporting a Usage so the
// parent-budget dispatch computation has something to subtract from. Ends
// the run with no tool calls on the next turn.
type twoTaskCallBudgetProvider struct {
	turn             int
	usageTotalTokens int
}

func (p *twoTaskCallBudgetProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *twoTaskCallBudgetProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-a", Name: "task", Arguments: map[string]any{"description": "sub A", "prompt": "do A"}},
					{ID: "call-b", Name: "task", Arguments: map[string]any{"description": "sub B", "prompt": "do B"}},
				},
				Usage: &llm.Usage{TotalTokens: p.usageTotalTokens},
				Stop:  "tool_calls",
				Done:  true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestParentBudgetPassthrough_ParallelBatchBothConfigsCarryBudget is the
// regression test for review finding #3: the parallel tool-execution path
// (task is ParallelSafe, so a batch of 2+ task calls in one turn runs
// concurrently — see TestTaskTool_ParallelBatch_OverlapsSubagents) was never
// exercised for the parent-budget passthrough. dispatchCtx is computed ONCE
// per batch, before any goroutine is spawned (react.go), so both concurrent
// task calls in the same batch must see the SAME folded remaining budget.
func TestParentBudgetPassthrough_ParallelBatchBothConfigsCarryBudget(t *testing.T) {
	pool := &multiBudgetCapturingTaskPool{}

	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	a := New(AgentConfig{
		LLMProvider:     &twoTaskCallBudgetProvider{usageTotalTokens: 400},
		Tools:           reg,
		MaxTurns:        5,
		MaxTokensBudget: 1000,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "fan out to two sub-agents"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult")
	}

	configs := pool.snapshot()
	if len(configs) != 2 {
		t.Fatalf("StartTask was called %d times, want 2", len(configs))
	}
	for i, cfg := range configs {
		if cfg.TokenBudget != 600 {
			t.Fatalf("configs[%d].TokenBudget = %d, want 600 (parent budget=1000, usage=400 at dispatch → remaining=600, same for both concurrent calls in the batch)", i, cfg.TokenBudget)
		}
	}
}
