package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// Reasoning models reached over an OpenAI-compatible endpoint (DeepSeek, Qwen,
// GLM — all three route to OpenAICompatProvider, see registry.go) stream their
// whole thinking phase as `delta.reasoning_content`, which the openai-go SDK
// does not model as a typed field. Before this fix consumeStream inspected only
// Content and ToolCalls, so a model that reasoned for longer than the agent's
// 2-minute idle window produced total channel silence and the watchdog in
// pkg/agent/streaming.go cancelled a perfectly healthy request with
// "stream idle timeout: no data received after 2m0s". The Anthropic provider
// already solves the identical problem for thinking_delta (anthropic.go).

// reasoningDeltaEvents models a reasoning stream: N thinking fragments carrying
// no content, then the real answer, then finish_reason.
func reasoningDeltaEvents(t *testing.T, field string, fragments []string, answer string) []ssestream.Event {
	t.Helper()
	var events []ssestream.Event
	for _, frag := range fragments {
		events = append(events, openAIChunkEvent(t, map[string]any{
			"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{field: frag, "content": ""},
				"finish_reason": "",
			}},
		}))
	}
	events = append(events, openAIChunkEvent(t, map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": answer}, "finish_reason": "",
		}},
	}))
	events = append(events, openAIChunkEvent(t, map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}))
	return events
}

func TestOpenAICompatConsumeStream_ReasoningContentSendsProgress(t *testing.T) {
	fragments := []string{"Let me think. ", "The user wants X, ", "so I should Y."}
	decoder := &fakeOpenAIDecoder{events: reasoningDeltaEvents(t, "reasoning_content", fragments, "Answer.")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 64)
	retry, stripRE := p.consumeStream(context.Background(), ch, stream, "test-model", false)
	close(ch)
	chunks := drainStreamChunks(ch)

	if retry || stripRE {
		t.Fatalf("consumeStream returned (retry=%v, stripReasoningEffort=%v), want (false, false)", retry, stripRE)
	}

	var progressCount int
	for i, c := range chunks {
		if !c.Progress {
			continue
		}
		progressCount++
		// Reasoning is a liveness signal only; it must not leak into the
		// assistant's visible text, matching the Anthropic provider.
		if c.Delta != "" || len(c.ToolCalls) != 0 || c.Usage != nil || c.Done || c.Err != nil || c.Message != nil {
			t.Fatalf("chunk[%d] is a Progress chunk but also carries a payload: %+v", i, c)
		}
	}
	if progressCount != len(fragments) {
		t.Fatalf("progress chunk count = %d, want %d (one per non-empty reasoning fragment)", progressCount, len(fragments))
	}

	final := chunks[len(chunks)-1]
	if !final.Done {
		t.Fatalf("final chunk Done = false, want true; chunks = %+v", chunks)
	}
	if final.Message == nil || final.Message.Content != "Answer." {
		t.Fatalf("final message = %+v, want Content %q — reasoning must not pollute it", final.Message, "Answer.")
	}
}

func TestOpenAICompatConsumeStream_ReasoningFieldAlias(t *testing.T) {
	// OpenRouter and some gateways name the same field "reasoning".
	decoder := &fakeOpenAIDecoder{events: reasoningDeltaEvents(t, "reasoning", []string{"thinking..."}, "Done.")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 64)
	p.consumeStream(context.Background(), ch, stream, "test-model", false)
	close(ch)

	var progressCount int
	for _, c := range drainStreamChunks(ch) {
		if c.Progress {
			progressCount++
		}
	}
	if progressCount != 1 {
		t.Fatalf("progress chunk count = %d, want 1 for the \"reasoning\" field alias", progressCount)
	}
}

func TestOpenAICompatConsumeStream_EmptyReasoningSendsNoProgress(t *testing.T) {
	// A keep-alive-shaped empty delta must not manufacture busywork.
	decoder := &fakeOpenAIDecoder{events: reasoningDeltaEvents(t, "reasoning_content", []string{"", ""}, "Answer.")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 64)
	p.consumeStream(context.Background(), ch, stream, "test-model", false)
	close(ch)

	for _, c := range drainStreamChunks(ch) {
		if c.Progress {
			t.Fatal("empty reasoning fragments must not emit Progress chunks")
		}
	}
}

func TestOpenAICompatConsumeStream_ReasoningOnlyStreamDoesNotRetryAfterError(t *testing.T) {
	// A stream that reasoned for minutes and then dropped has already produced
	// (and billed) output. Re-running it transparently would double the cost and
	// the latency, so emitted must be set by reasoning too — same rule the
	// Anthropic provider applies to thinking_delta.
	events := reasoningDeltaEvents(t, "reasoning_content", []string{"long thought"}, "")
	events = events[:1] // drop the answer and finish_reason chunks
	decoder := &fakeOpenAIDecoder{events: events, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 64)
	retry, _ := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
	close(ch)

	if retry {
		t.Fatal("consumeStream retried a stream that had already streamed reasoning output")
	}
	var sawErr bool
	for _, c := range drainStreamChunks(ch) {
		if c.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the transient error to be surfaced as a chunk")
	}
}
