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

// usageTaskPool is a fake subagent pool (same structural shape as
// task_parallel_test.go's fakes) whose completed Task carries a Usage. It
// lets an end-to-end Agent.Run exercise the real tools.TaskTool handler
// (which must copy Task.Usage into ToolResult.Data["subagent_usage"], 12a)
// feeding react.go's roll-up into the run's Usage accumulator (12b).
type usageTaskPool struct {
	mu      sync.Mutex
	started int
	usage   *subagent.TokenUsage
	// status lets tests exercise the task tool's ERROR-returning branches
	// (TimedOut/Cancelled/default) while still carrying Usage — the fake pool
	// defaults to TaskStatusCompleted when unset.
	status subagent.TaskStatus
}

func (p *usageTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	p.mu.Lock()
	p.started++
	id := fmt.Sprintf("usage-task-%d", p.started)
	p.mu.Unlock()
	return &subagent.Task{ID: id}, nil
}

func (p *usageTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	status := p.status
	if status == "" {
		status = subagent.TaskStatusCompleted
	}
	return &subagent.Task{
		ID:     taskID,
		Status: status,
		Result: "done:" + taskID,
		Error:  "boom",
		Usage:  p.usage,
	}, nil
}

// oneTaskCallProvider emits exactly one "task" tool call on its first turn
// (never reporting its own LLM Usage, so any tokens observed in the final
// RunResult.Usage must have come from the subagent roll-up), then ends the
// run on the next turn.
type oneTaskCallProvider struct {
	turn int
}

func (p *oneTaskCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *oneTaskCallProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-a", Name: "task", Arguments: map[string]any{"description": "sub A", "prompt": "do A"}},
				},
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

// TestSubagentUsageRollsUpIntoRunResultUsage is the RED test for M2-2 (12a +
// 12b): a single task call completes with Usage{100,50,150}; the parent
// run's RunResult.Usage must include those tokens even though the fake
// provider itself never reports any Usage of its own.
//
// RED signature (today): SubagentExecutor.Execute drops result.Usage, the
// pool never stores it on the Task, the task tool never surfaces it in
// ToolResult.Data, and react.go never rolls it up — so RunResult.Usage stays
// at the zero value (nil InputTokens/OutputTokens/TotalTokens) instead of
// {100,50,150}.
func TestSubagentUsageRollsUpIntoRunResultUsage(t *testing.T) {
	pool := &usageTaskPool{usage: &subagent.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}

	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	a := New(AgentConfig{
		LLMProvider: &oneTaskCallProvider{},
		Tools:       reg,
		MaxTurns:    5,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "delegate to a sub-agent"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Usage == nil {
		t.Fatal("expected a non-nil RunResult.Usage")
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 50 || result.Usage.TotalTokens != 150 {
		t.Fatalf("RunResult.Usage = %+v, want {InputTokens:100 OutputTokens:50 TotalTokens:150} rolled up from the subagent", result.Usage)
	}

	// Sanity: the subagent's result content must still show up as the tool
	// result, undisturbed by the usage roll-up plumbing.
	found := false
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil && msg.ToolResult.CallID == "call-a" {
			found = true
			if !strings.Contains(msg.ToolResult.Content, "done:") {
				t.Fatalf("tool result content = %q, want it to contain the subagent's result", msg.ToolResult.Content)
			}
		}
	}
	if !found {
		t.Fatal("missing tool_result for call-a")
	}
}

// TestSubagentUsageRollsUpIntoRunResultUsage_EvenWhenSubagentFails is the RED
// test for review finding H1: a subagent that ends in error (timed out here;
// the same code path covers cancelled/token-budget-exceeded) still reported
// real token consumption before it failed, and that consumption must still
// reach the parent's RunResult.Usage. The task tool's TaskStatusTimedOut
// branch already sets Data["subagent_usage"] before returning an error
// (verified: pkg/tools/subagent.go sets Data ahead of the status switch), so
// this isolates the OTHER half of the chain: react.go's runOneTool must
// preserve that Data when it wraps the tool's error into a synthesized
// Failed ToolResult, instead of discarding it.
//
// RED signature (today): runOneTool's `if err != nil` branch replaces the
// tool's own result (which carries Data["subagent_usage"]) with a brand-new
// zero-value ToolResult that has no Data, so addSubagentUsage never sees the
// usage and RunResult.Usage stays {0,0,0}.
func TestSubagentUsageRollsUpIntoRunResultUsage_EvenWhenSubagentFails(t *testing.T) {
	pool := &usageTaskPool{
		usage:  &subagent.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		status: subagent.TaskStatusTimedOut,
	}

	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	a := New(AgentConfig{
		LLMProvider: &oneTaskCallProvider{},
		Tools:       reg,
		MaxTurns:    5,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "delegate to a sub-agent"},
	})
	// A single failed task call is not fatal (the repeat/validation breakers
	// need many more occurrences to trip) — the run continues to completion.
	if err != nil {
		t.Fatalf("Run() error = %v, want the run to continue past a single subagent failure", err)
	}
	if result == nil || result.Usage == nil {
		t.Fatal("expected a non-nil RunResult.Usage")
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 50 || result.Usage.TotalTokens != 150 {
		t.Fatalf("RunResult.Usage = %+v, want {InputTokens:100 OutputTokens:50 TotalTokens:150} rolled up even though the subagent timed out", result.Usage)
	}

	found := false
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil && msg.ToolResult.CallID == "call-a" {
			found = true
			if msg.ToolResult.Status != models.CallStatusFailed {
				t.Fatalf("tool result status = %s, want %s (subagent timed out)", msg.ToolResult.Status, models.CallStatusFailed)
			}
		}
	}
	if !found {
		t.Fatal("missing tool_result for call-a")
	}
}
