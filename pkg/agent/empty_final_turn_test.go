package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// toolThenEmptyProvider streams exactly one tool-call turn (turn 0), then on
// every subsequent turn ends the stream normally (Stop: "stop", Done: true)
// with NO text and NO tool calls — the exact shape the investigation
// reproduced byte-for-byte against the real incident: a normal (non
// wrap-up) final turn that just happens to produce nothing.
type toolThenEmptyProvider struct {
	mu   sync.Mutex
	made int
}

func (p *toolThenEmptyProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *toolThenEmptyProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	p.made++
	n := p.made
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if n == 1 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "call-1", Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
			return
		}
		// Turn 1+: stream ends NORMALLY (Stop: "stop") with empty content
		// and no tool calls — not a wrap-up turn (no tool budget, no ctx
		// deadline in this test), not a provider error, not a truncation.
		ch <- llm.StreamChunk{Delta: "", Stop: "stop", Done: true}
	}()
	return ch, nil
}

// TestRun_NormalFinalTurn_EmptyOutput_IsAnError is the RED test for defect A
// (M6 latency-investigation brief): react.go's len(toolCalls)==0 branch
// unconditionally returned (assistantMessage.Content, nil) for a plain,
// non-wrap-up final turn — the empty-content guard lived entirely inside
// `if wrapUp`. A real eval run hit exactly this: turn 0 called a tool, turn
// 1 ended the stream normally with empty content and no tool calls, well
// before any deadline or tool-call budget — and Run reported success with
// FinalOutput="" and err==nil.
//
// No wrap-up trigger is armed here: AgentConfig.MaxToolCalls is left at 0
// (unlimited) and ctx carries no deadline, so wrapUp never flips true —
// this reaches the plain "normal final turn" path, not either wrap-up path.
func TestRun_NormalFinalTurn_EmptyOutput_IsAnError(t *testing.T) {
	provider := &toolThenEmptyProvider{}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})

	if err == nil {
		t.Fatalf("Run() error = nil, want an error — a normal final turn produced no tool calls AND no output (result: %+v)", result)
	}
	if result == nil || result.FinalOutput != "" {
		t.Fatalf("FinalOutput = %q, want empty", result.FinalOutput)
	}
	if result.WoundDownReason != "" {
		t.Fatalf("WoundDownReason = %q, want empty — this must NOT look like a wrap-up", result.WoundDownReason)
	}
	if result.BudgetExhausted {
		t.Fatal("BudgetExhausted = true, want false — no wrap-up trigger armed in this test")
	}
	// Must not borrow either wrap-up path's wording — those are reserved
	// for when wrapUp is actually true (deadline / tool-budget).
	if strings.Contains(err.Error(), "tool call budget") || strings.Contains(strings.ToLower(err.Error()), "deadline") || strings.Contains(strings.ToLower(err.Error()), "wall-clock") {
		t.Fatalf("err = %q, must not borrow wrap-up wording — this is a plain final turn, not a wrap-up", err.Error())
	}
}
