package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// wrapUpProvider cooperates with the budget wrap-up: while the request offers
// tools it emits one tool call; once the wrap-up request arrives with NO tools
// it emits the final text answer. It also records each request's tool count
// and message list so tests can assert the wrap-up request was tool-less and
// carried the wrap-up notice in its VIEW.
type wrapUpProvider struct {
	requestTools    []int
	lastReqMessages []models.Message
}

func (p *wrapUpProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wrapUpProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.requestTools = append(p.requestTools, len(req.Tools))
	p.lastReqMessages = req.Messages
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if len(req.Tools) > 0 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("wecho-%d", len(p.requestTools)), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "wrapped up: partial work summarized"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestRun_ToolCallBudget_GracefulWrapUp pins the budget-exhaustion contract:
// once MaxToolCalls calls have executed, the run must NOT hard-fail (the old
// "agent exceeded max turns" behavior discarded everything — the last turn
// before a cap is always a tool-call turn, so FinalOutput was empty). Instead
// the wrap-up request carries no tools, a stripped system prompt, and the
// wrap-up notice in its prompt VIEW, and the run ends normally with the
// model's final answer. The notice must NOT be appended to the canonical
// runMessages: those are persisted by the REPL and replayed for the rest of
// the session, while the budget is per-Run — a persisted "never use tools"
// instruction would poison every later turn.
func TestRun_ToolCallBudget_GracefulWrapUp(t *testing.T) {
	provider := &wrapUpProvider{}
	a := New(AgentConfig{
		LLMProvider:  provider,
		Tools:        newRegistryWithNoOpTool("wecho"),
		MaxToolCalls: 2,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want graceful wrap-up (nil)", err)
	}
	if result.FinalOutput != "wrapped up: partial work summarized" {
		t.Fatalf("FinalOutput = %q, want the wrap-up answer", result.FinalOutput)
	}

	// Exactly two tool calls executed, then a third tool-less request.
	toolResults := 0
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool {
			toolResults++
		}
		if msg.Role == models.RoleHuman && strings.Contains(msg.Content, "tool call limit") {
			t.Fatal("wrap-up notice leaked into the canonical runMessages — it would be persisted and poison later turns")
		}
	}
	if toolResults != 2 {
		t.Fatalf("tool result messages = %d, want 2 (the budget)", toolResults)
	}
	if len(provider.requestTools) != 3 {
		t.Fatalf("requests = %d, want 3 (two tool turns + one wrap-up)", len(provider.requestTools))
	}
	if last := provider.requestTools[len(provider.requestTools)-1]; last != 0 {
		t.Fatalf("wrap-up request offered %d tools, want 0", last)
	}

	// The notice rides the wrap-up request's VIEW instead: appended after the
	// batch's last tool result, never between an assistant tool_calls message
	// and its results (same position rule as the breaker hints, M1-7).
	noticeIdx, lastToolIdx := -1, -1
	for i, msg := range provider.lastReqMessages {
		if msg.Role == models.RoleTool {
			lastToolIdx = i
		}
		if msg.Role == models.RoleHuman && strings.Contains(msg.Content, "tool call limit") {
			noticeIdx = i
		}
	}
	if noticeIdx == -1 {
		t.Fatal("wrap-up notice missing from the wrap-up request's message view")
	}
	if noticeIdx < lastToolIdx {
		t.Fatalf("notice at %d precedes last tool result at %d", noticeIdx, lastToolIdx)
	}
}

// emptyWrapUpProvider emits tool calls while tools are offered, and on the
// tool-less wrap-up request returns NOTHING — no text, no tool calls.
type emptyWrapUpProvider struct{ calls int }

func (p *emptyWrapUpProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *emptyWrapUpProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if len(req.Tools) > 0 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("eecho-%d", p.calls), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}
		// Tool-less request: empty turn, just Done.
		ch <- llm.StreamChunk{Done: true, Stop: "end_turn"}
	}()
	return ch, nil
}

// TestRun_ToolCallBudget_EmptyWrapUpErrors: a wrap-up turn that produces no
// output must fail loudly — every executed call's work would otherwise be
// silently discarded behind a "successful" run with an empty FinalOutput
// that the parent model has no signal to retry on.
func TestRun_ToolCallBudget_EmptyWrapUpErrors(t *testing.T) {
	a := New(AgentConfig{
		LLMProvider:  &emptyWrapUpProvider{},
		Tools:        newRegistryWithNoOpTool("wecho"),
		MaxToolCalls: 1,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a loud error for an empty wrap-up turn")
	}
	if !strings.Contains(err.Error(), "tool call budget") {
		t.Fatalf("error = %v, want tool call budget error", err)
	}
	if result == nil || result.FinalOutput != "" {
		t.Fatalf("FinalOutput = %q, want empty (nothing was produced)", result.FinalOutput)
	}
}

// pathologicalProvider ignores the tool list entirely and always returns tool
// calls — the misbehaving-backend shape the wrap-up branch must survive: every
// tool_use needs a paired tool_result for the history to stay replayable, and
// with no text at all the run ends with the budget error instead of hanging.
type pathologicalProvider struct {
	calls int
}

func (p *pathologicalProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *pathologicalProvider) Stream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("pbad-%d", p.calls), Name: "wecho"}},
			Stop:      "tool_calls",
			Done:      true,
		}
	}()
	return ch, nil
}

// TestRun_ToolCallBudget_RefusesPostWrapUpToolCalls: a provider that keeps
// emitting tool calls even for a tool-less request must get refusal results
// (pairing invariant) and a terminal budget error — never an unbounded loop.
func TestRun_ToolCallBudget_RefusesPostWrapUpToolCalls(t *testing.T) {
	provider := &pathologicalProvider{}
	a := New(AgentConfig{
		LLMProvider:  provider,
		Tools:        newRegistryWithNoOpTool("wecho"),
		MaxToolCalls: 1,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want budget error for a provider that ignores the tool-less wrap-up request")
	}
	if !strings.Contains(err.Error(), "tool call budget") {
		t.Fatalf("error = %v, want tool call budget error", err)
	}

	// Pairing invariant: every tool_use ID on any assistant message has a
	// matching tool_result message in the history.
	wantCalls := map[string]bool{}
	gotResults := map[string]bool{}
	for _, msg := range result.Messages {
		for _, call := range msg.ToolCalls {
			wantCalls[call.ID] = true
		}
		if msg.ToolResult != nil {
			gotResults[msg.ToolResult.CallID] = true
		}
	}
	for id := range wantCalls {
		if !gotResults[id] {
			t.Fatalf("tool_use %q has no matching tool_result (history would be unreplayable)", id)
		}
	}
}
