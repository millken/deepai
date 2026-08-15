package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// countingTaskPool is a fake subagent pool that completes every task
// immediately and counts how many tasks actually reached StartTask — the
// signal that proves a capped call was refused BEFORE execution, not just
// reported as failed after running.
type countingTaskPool struct {
	mu      sync.Mutex
	started int
}

func (p *countingTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	p.mu.Lock()
	p.started++
	id := fmt.Sprintf("cap-task-%d", p.started)
	p.mu.Unlock()
	return &subagent.Task{ID: id}, nil
}

func (p *countingTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok:" + taskID}, nil
}

func (p *countingTaskPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// manyTaskCallProvider emits one distinct "task" tool call per turn (unique
// args per turn so the repeat-call breaker never engages) for totalCalls
// turns, then ends the run with no tool calls.
type manyTaskCallProvider struct {
	turn       int
	totalCalls int
}

func (p *manyTaskCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *manyTaskCallProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn <= p.totalCalls {
		callID := fmt.Sprintf("call-%d", p.turn)
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{
					ID:   callID,
					Name: "task",
					Arguments: map[string]any{
						"description": fmt.Sprintf("sub %d", p.turn),
						"prompt":      fmt.Sprintf("do %d", p.turn),
					},
				}},
				Stop: "tool_calls",
				Done: true,
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

// TestTaskCallFanOutCap_RefusesBeyondLimit is the RED test for M2-2 (12c):
// a run that issues more than maxTaskCallsPerRun serial "task" calls must
// have the (cap+1)-th call refused (synthesized Failed ToolResult, never
// reaching the pool's StartTask) while the run itself continues to
// completion rather than aborting.
//
// RED signature (today): maxTaskCallsPerRun doesn't exist and there is no
// counter in Run(), so every task call executes — pool.count() would be
// maxTaskCallsPerRun+1, not maxTaskCallsPerRun, and no synthesized-failure
// tool result would exist for the capped call.
func TestTaskCallFanOutCap_RefusesBeyondLimit(t *testing.T) {
	pool := &countingTaskPool{}
	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	provider := &manyTaskCallProvider{totalCalls: maxTaskCallsPerRun + 1}
	a := New(AgentConfig{LLMProvider: provider, Tools: reg, MaxToolCalls: maxTaskCallsPerRun + 5})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "fan out a lot"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want the run to continue past the cap refusal (per-call refusal, not fatal)", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult")
	}

	if got := pool.count(); got != maxTaskCallsPerRun {
		t.Fatalf("pool.count() = %d, want %d — the %d-th call must be refused BEFORE StartTask", got, maxTaskCallsPerRun, maxTaskCallsPerRun+1)
	}

	cappedCallID := fmt.Sprintf("call-%d", maxTaskCallsPerRun+1)
	found := false
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil && msg.ToolResult.CallID == cappedCallID {
			found = true
			if msg.ToolResult.Status != models.CallStatusFailed {
				t.Fatalf("capped call status = %s, want %s", msg.ToolResult.Status, models.CallStatusFailed)
			}
			if !strings.Contains(msg.ToolResult.Error, "task call limit reached") {
				t.Fatalf("capped call error = %q, want it to mention the fan-out cap", msg.ToolResult.Error)
			}
		}
	}
	if !found {
		t.Fatalf("missing tool_result for the capped call %s", cappedCallID)
	}

	// Every call before the cap must have actually run (distinguishable
	// per-task content, not a synthesized failure).
	okCallID := fmt.Sprintf("call-%d", maxTaskCallsPerRun)
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil && msg.ToolResult.CallID == okCallID {
			if msg.ToolResult.Status != models.CallStatusCompleted {
				t.Fatalf("last-under-cap call status = %s, want %s", msg.ToolResult.Status, models.CallStatusCompleted)
			}
		}
	}
}

// straddleBatchProvider emits soloCalls distinct solo "task" calls (one per
// turn, unique args), then a single turn with a 2-call parallel batch, then
// ends the run.
type straddleBatchProvider struct {
	turn      int
	soloCalls int
}

func (p *straddleBatchProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *straddleBatchProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	switch {
	case p.turn <= p.soloCalls:
		callID := fmt.Sprintf("solo-%d", p.turn)
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{
					ID:   callID,
					Name: "task",
					Arguments: map[string]any{
						"description": fmt.Sprintf("solo %d", p.turn),
						"prompt":      fmt.Sprintf("do solo %d", p.turn),
					},
				}},
				Stop: "tool_calls",
				Done: true,
			}
		}()
		return ch, nil
	case p.turn == p.soloCalls+1:
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "batch-a", Name: "task", Arguments: map[string]any{"description": "batch A", "prompt": "do batch A"}},
					{ID: "batch-b", Name: "task", Arguments: map[string]any{"description": "batch B", "prompt": "do batch B"}},
				},
				Stop: "tool_calls",
				Done: true,
			}
		}()
		return ch, nil
	default:
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
}

// TestTaskCallFanOutCap_ParallelBatchStraddlesLimit is the RED test for M2-2
// (12c)'s parallel-path note: when a single parallel-safe batch straddles
// the cap, the calls under the cap must run and the calls over the cap must
// be refused — decided at dispatch time for the whole batch.
//
// RED signature (today): no cap exists, so both batch-a and batch-b run,
// pool.count() ends at soloCalls+2 instead of maxTaskCallsPerRun, and
// batch-b never gets a synthesized-failure result.
func TestTaskCallFanOutCap_ParallelBatchStraddlesLimit(t *testing.T) {
	pool := &countingTaskPool{}
	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	soloCalls := maxTaskCallsPerRun - 1
	provider := &straddleBatchProvider{soloCalls: soloCalls}
	a := New(AgentConfig{LLMProvider: provider, Tools: reg, MaxToolCalls: soloCalls + 5})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "fan out then batch"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want the run to continue past the cap refusal", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult")
	}

	if got := pool.count(); got != maxTaskCallsPerRun {
		t.Fatalf("pool.count() = %d, want %d — batch-b (the %d-th call) must be refused before StartTask", got, maxTaskCallsPerRun, maxTaskCallsPerRun+1)
	}

	var resA, resB *models.ToolResult
	for i := range result.Messages {
		msg := result.Messages[i]
		if msg.Role != models.RoleTool || msg.ToolResult == nil {
			continue
		}
		switch msg.ToolResult.CallID {
		case "batch-a":
			resA = msg.ToolResult
		case "batch-b":
			resB = msg.ToolResult
		}
	}
	if resA == nil {
		t.Fatal("missing tool_result for batch-a")
	}
	if resB == nil {
		t.Fatal("missing tool_result for batch-b")
	}
	if resA.Status != models.CallStatusCompleted {
		t.Fatalf("batch-a (under cap) status = %s, want %s", resA.Status, models.CallStatusCompleted)
	}
	if resB.Status != models.CallStatusFailed {
		t.Fatalf("batch-b (over cap) status = %s, want %s", resB.Status, models.CallStatusFailed)
	}
	if !strings.Contains(resB.Error, "task call limit reached") {
		t.Fatalf("batch-b error = %q, want it to mention the fan-out cap", resB.Error)
	}
}
