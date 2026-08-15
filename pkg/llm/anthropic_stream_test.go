package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// fakeAnthropicDecoder replays a canned list of SSE events (and, once
// exhausted, a canned terminal error) without any real HTTP/SSE wire
// encoding — ssestream.Decoder is an exported interface and ssestream.
// NewStream is an exported constructor, so this drives the SAME
// AnthropicProvider.consumeStream production code real traffic does, just
// fed synthetic events.
type fakeAnthropicDecoder struct {
	events []ssestream.Event
	idx    int
	err    error
}

func (f *fakeAnthropicDecoder) Next() bool {
	if f.idx >= len(f.events) {
		return false
	}
	f.idx++
	return true
}
func (f *fakeAnthropicDecoder) Event() ssestream.Event { return f.events[f.idx-1] }
func (f *fakeAnthropicDecoder) Close() error           { return nil }
func (f *fakeAnthropicDecoder) Err() error             { return f.err }

func anthropicSSEEvent(t *testing.T, eventType string, payload map[string]any) ssestream.Event {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture event: %v", err)
	}
	return ssestream.Event{Type: eventType, Data: data}
}

func toolUseDeltaEvents(t *testing.T, toolID, toolName string, fragments []string) []ssestream.Event {
	t.Helper()
	events := []ssestream.Event{
		anthropicSSEEvent(t, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use", "id": toolID, "name": toolName, "input": map[string]any{},
			},
		}),
	}
	for _, frag := range fragments {
		events = append(events, anthropicSSEEvent(t, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
		}))
	}
	events = append(events,
		anthropicSSEEvent(t, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		anthropicSSEEvent(t, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use"},
			"usage": map[string]any{"output_tokens": 7},
		}),
	)
	return events
}

func drainStreamChunks(ch <-chan StreamChunk) []StreamChunk {
	var out []StreamChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

// TestAnthropicConsumeStream_ToolArgumentDeltasSendProgressNoText is the RED
// test for the Anthropic side of the fix: pkg/llm/anthropic.go's
// input_json_delta handling used to accumulate into the tool-call builder
// and forward NOTHING to the channel — the whole span during which a large
// tool-call argument (e.g. write_file's markdown body) streams in was
// invisible to any consumer. It must now send one StreamChunk{Progress:
// true} per non-empty fragment, carrying no text and disturbing nothing
// else, and the final assembled message/tool call must be unaffected.
func TestAnthropicConsumeStream_ToolArgumentDeltasSendProgressNoText(t *testing.T) {
	fragments := []string{
		`{"path":"design.md","con`,
		`tent":"# Title\n\nSome very long body `,
		`text here."}`,
	}
	decoder := &fakeAnthropicDecoder{events: toolUseDeltaEvents(t, "tool_1", "write_file", fragments)}
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](decoder, nil)

	p := &AnthropicProvider{provider: "anthropic"}
	ch := make(chan StreamChunk, 64)
	retry := p.consumeStream(context.Background(), ch, stream, "test-model", false)
	close(ch)
	chunks := drainStreamChunks(ch)

	if retry {
		t.Fatal("consumeStream returned retry=true, want false (stream completed cleanly)")
	}

	var progressCount int
	for i, c := range chunks {
		if c.Delta != "" {
			t.Fatalf("chunk[%d] carried a text delta %q, want none — this stream never emitted text", i, c.Delta)
		}
		if c.Progress {
			progressCount++
			if len(c.ToolCalls) != 0 || c.Usage != nil || c.Done || c.Err != nil || c.Message != nil {
				t.Fatalf("chunk[%d] is a Progress chunk but also carries a payload: %+v", i, c)
			}
		}
	}
	if progressCount != len(fragments) {
		t.Fatalf("progress chunk count = %d, want %d (one per input_json_delta fragment)", progressCount, len(fragments))
	}

	final := chunks[len(chunks)-1]
	if !final.Done {
		t.Fatalf("final chunk Done = false, want true; chunks = %+v", chunks)
	}
	if final.Message == nil || final.Message.Content != "" {
		t.Fatalf("final message = %+v, want Content empty (no text was ever streamed)", final.Message)
	}
	if len(final.ToolCalls) != 1 || final.ToolCalls[0].Name != "write_file" {
		t.Fatalf("final tool calls = %+v, want exactly one write_file call", final.ToolCalls)
	}
	if got, _ := final.ToolCalls[0].Arguments["path"].(string); got != "design.md" {
		t.Fatalf("tool call args[path] = %q, want %q", got, "design.md")
	}
	if got, _ := final.ToolCalls[0].Arguments["content"].(string); got == "" {
		t.Fatalf("tool call args[content] is empty, want the assembled fragments")
	}
}

// TestAnthropicConsumeStream_ProgressDoesNotEnableRetryAfterError pins the
// "do not change emitted's semantics" constraint: input_json_delta already
// set emitted=true before this fix (unrelated to the Progress send added
// alongside it), so a stream that has already accumulated tool-argument
// bytes and then hits a retryable transport error must NOT retry — retrying
// after real output was already produced would duplicate content for the
// caller. Adding the Progress send must not change this.
func TestAnthropicConsumeStream_ProgressDoesNotEnableRetryAfterError(t *testing.T) {
	events := toolUseDeltaEvents(t, "tool_1", "write_file", []string{`{"path":"a.md"}`})
	// Drop the trailing content_block_stop/message_delta events — the
	// decoder now runs dry and reports a transient error, as if the
	// connection dropped mid-argument.
	events = events[:len(events)-2]
	decoder := &fakeAnthropicDecoder{events: events, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](decoder, nil)

	p := &AnthropicProvider{provider: "anthropic"}
	ch := make(chan StreamChunk, 64)
	retry := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
	close(ch)
	chunks := drainStreamChunks(ch)

	if retry {
		t.Fatal("consumeStream retried after a tool-argument delta had already streamed — emitted must gate this false, exactly as before this fix")
	}
	var sawErr bool
	for _, c := range chunks {
		if c.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the transient error to be surfaced as a chunk since emitted blocked the retry")
	}
}

// TestAnthropicConsumeStream_NoOutputYetStillRetries is the control for the
// test above: a stream that has produced NO tool-argument bytes (or any
// other output) yet, hitting the same retryable error, must still retry —
// that pre-existing behavior must survive the Progress send being added
// elsewhere in this function.
func TestAnthropicConsumeStream_NoOutputYetStillRetries(t *testing.T) {
	decoder := &fakeAnthropicDecoder{events: nil, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](decoder, nil)

	p := &AnthropicProvider{provider: "anthropic"}
	ch := make(chan StreamChunk, 4)
	retry := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
	close(ch)

	if !retry {
		t.Fatal("consumeStream did not retry a transient error before any output was emitted — unrelated to this fix, but must be unaffected by it")
	}
}

// TestAnthropicConsumeStream_ThinkingDeltasSendProgress is the RED test for
// the reasoning-model idle-timeout false positive: a thinking stream (GLM and
// Claude extended thinking via the anthropic endpoint) can emit minutes of
// thinking_delta/signature_delta events before the first text or tool token.
// Those events carry no message content, but they ARE stream activity — the
// provider must forward one Progress chunk per non-empty delta so pkg/agent's
// idle watchdog sees the reasoning phase instead of cancelling a perfectly
// healthy request ("stream idle timeout") once a hard problem thinks longer
// than the idle window.
func TestAnthropicConsumeStream_ThinkingDeltasSendProgress(t *testing.T) {
	thinkingFrags := []string{"Let me ", "think about ", "this hard problem."}
	events := []ssestream.Event{
		anthropicSSEEvent(t, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"role": "assistant", "model": "test-model",
				"usage": map[string]any{"input_tokens": 11},
			},
		}),
		anthropicSSEEvent(t, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking"},
		}),
	}
	for _, frag := range thinkingFrags {
		events = append(events, anthropicSSEEvent(t, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": frag},
		}))
	}
	events = append(events,
		anthropicSSEEvent(t, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": "sig-1"},
		}),
		anthropicSSEEvent(t, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		anthropicSSEEvent(t, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "text_delta", "text": "final answer"},
		}),
		anthropicSSEEvent(t, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 7},
		}),
		anthropicSSEEvent(t, "message_stop", map[string]any{"type": "message_stop"}),
	)

	decoder := &fakeAnthropicDecoder{events: events}
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](decoder, nil)
	p := &AnthropicProvider{provider: "anthropic"}
	ch := make(chan StreamChunk, 64)
	retry := p.consumeStream(context.Background(), ch, stream, "test-model", false)
	close(ch)
	chunks := drainStreamChunks(ch)

	if retry {
		t.Fatal("consumeStream returned retry=true, want false (stream completed cleanly)")
	}

	progressCount := 0
	for i, c := range chunks {
		if c.Progress {
			progressCount++
			if len(c.ToolCalls) != 0 || c.Usage != nil || c.Done || c.Err != nil || c.Message != nil || c.Delta != "" {
				t.Fatalf("chunk[%d] is a Progress chunk but also carries a payload: %+v", i, c)
			}
		}
	}
	// One per thinking fragment plus one for the signature delta.
	if want := len(thinkingFrags) + 1; progressCount != want {
		t.Fatalf("progress chunk count = %d, want %d (one per thinking_delta + one signature_delta)", progressCount, want)
	}

	final := chunks[len(chunks)-1]
	if !final.Done || final.Message == nil || final.Message.Content != "final answer" {
		t.Fatalf("final chunk = %+v, want Done with text 'final answer' (thinking must not leak into content)", final)
	}
	if final.Message.Content == "Let me " {
		t.Fatal("thinking text leaked into the message content")
	}
}

// TestAnthropicConsumeStream_ThinkingDeltasBlockRetry pins the emitted flag
// for the thinking case: a stream that has streamed minutes of thinking
// deltas and then dies with a retryable transport error must NOT be retried —
// the reasoning tokens were already produced and billed, and re-running would
// re-bill the entire thinking phase (up to 4 attempts) for one response.
func TestAnthropicConsumeStream_ThinkingDeltasBlockRetry(t *testing.T) {
	events := []ssestream.Event{
		anthropicSSEEvent(t, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking"},
		}),
		anthropicSSEEvent(t, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "long reasoning..."},
		}),
	}
	// No terminal events: the decoder runs dry mid-thinking and reports a
	// transient error, as if the connection dropped mid-reasoning.
	decoder := &fakeAnthropicDecoder{events: events, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](decoder, nil)

	p := &AnthropicProvider{provider: "anthropic"}
	ch := make(chan StreamChunk, 64)
	retry := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
	close(ch)
	chunks := drainStreamChunks(ch)

	if retry {
		t.Fatal("consumeStream retried after thinking deltas had already streamed — emitted must gate this false (re-running would re-bill the whole thinking phase)")
	}
	var sawErr bool
	for _, c := range chunks {
		if c.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the transient error to be surfaced as a chunk since emitted blocked the retry")
	}
}
