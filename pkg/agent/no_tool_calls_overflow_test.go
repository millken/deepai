package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// inputOverflowThenSuccessProvider simulates a provider that rejects the
// first request with a genuine INPUT context-window overflow (no tool
// calls, no text — just a bare stop reason) and then completes normally
// once the caller compacts and retries.
type inputOverflowThenSuccessProvider struct {
	callCount int
}

func (p *inputOverflowThenSuccessProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *inputOverflowThenSuccessProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "model_context_window_exceeded"}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Done: true, Stop: "stop", Delta: "all done"}
	}()
	return ch, nil
}

// TestNoToolCalls_InputContextOverflow_CompactsAndRetries guards the M3
// finding at react.go's no-tool-calls branch: the old gate
// (didCompact && len(compacted) < len(runMessages)) was provably always
// false because compactMessages preserves message count, so a genuine
// input-context-overflow stop reason with no tool calls always fell straight
// through to the misleading "compaction cannot reduce further" warning and
// ended the run on attempt 1 — even though compactOnOverflow (fixed
// elsewhere for the chunk.Err path) can actually shrink the estimated token
// size and allow a retry.
//
// RED (today): the run ends after attempt 1 with FinalOutput == "" and the
// provider is never asked a second time.
// GREEN (after the fix): the run retries, the provider's second response
// completes normally, and FinalOutput contains "all done".
func TestNoToolCalls_InputContextOverflow_CompactsAndRetries(t *testing.T) {
	provider := &inputOverflowThenSuccessProvider{}
	msgs := []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "start"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleAI, Content: strings.Repeat("x", 4000)})
	}

	a := New(AgentConfig{
		LLMProvider:        provider,
		MaxTurns:           10,
		CompactionKeepTail: 6,
	})

	result, err := a.Run(context.Background(), "s1", msgs)
	if err != nil {
		t.Fatalf("expected no fatal error, got: %v", err)
	}
	if provider.callCount < 2 {
		t.Fatalf("expected the provider to be retried after compaction, got only %d call(s)", provider.callCount)
	}
	if !strings.Contains(result.FinalOutput, "all done") {
		t.Fatalf("expected FinalOutput to contain the retried completion, got %q", result.FinalOutput)
	}
}

// outputTruncationNoToolCallsProvider simulates a provider that truncates
// its OUTPUT (max_tokens) rather than rejecting the input. Compacting
// history cannot help this case, so the run must end after a single
// attempt — this is the control for the fix above: only genuine
// input-overflow stop reasons should trigger a compact-and-retry.
type outputTruncationNoToolCallsProvider struct {
	callCount int
}

func (p *outputTruncationNoToolCallsProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *outputTruncationNoToolCallsProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Done: true, Stop: "max_tokens", Delta: "truncated output"}
	}()
	return ch, nil
}

func TestNoToolCalls_OutputTruncation_DoesNotRetry(t *testing.T) {
	provider := &outputTruncationNoToolCallsProvider{}
	msgs := []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "start"}}

	a := New(AgentConfig{
		LLMProvider:        provider,
		MaxTurns:           10,
		CompactionKeepTail: 6,
	})

	result, err := a.Run(context.Background(), "s1", msgs)
	if err != nil {
		t.Fatalf("expected no fatal error, got: %v", err)
	}
	if provider.callCount != 1 {
		t.Fatalf("output truncation must not trigger a compact-and-retry: expected 1 call, got %d", provider.callCount)
	}
	if !strings.Contains(result.FinalOutput, "truncated output") {
		t.Fatalf("expected FinalOutput to contain the single attempt's text, got %q", result.FinalOutput)
	}
}
