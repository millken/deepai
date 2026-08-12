package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// fakeOpenAIDecoder replays a canned list of SSE data payloads (and, once
// exhausted, a canned terminal error) without any real HTTP/SSE wire
// encoding — ssestream.Decoder is an exported interface and ssestream.
// NewStream is an exported constructor, so this drives the SAME
// OpenAICompatProvider.consumeStream production code real traffic does,
// just fed synthetic chunks.
type fakeOpenAIDecoder struct {
	events []ssestream.Event
	idx    int
	err    error
}

func (f *fakeOpenAIDecoder) Next() bool {
	if f.idx >= len(f.events) {
		return false
	}
	f.idx++
	return true
}
func (f *fakeOpenAIDecoder) Event() ssestream.Event { return f.events[f.idx-1] }
func (f *fakeOpenAIDecoder) Close() error           { return nil }
func (f *fakeOpenAIDecoder) Err() error             { return f.err }

func openAIChunkEvent(t *testing.T, payload map[string]any) ssestream.Event {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture chunk: %v", err)
	}
	return ssestream.Event{Data: data}
}

// toolCallDeltaEvents models the real OpenAI-compat wire shape: the first
// chunk carries the tool call's id/name with an empty arguments string, then
// several chunks carry only argument fragments, then a final chunk reports
// finish_reason.
func toolCallDeltaEvents(t *testing.T, toolID, toolName string, fragments []string) []ssestream.Event {
	t.Helper()
	events := []ssestream.Event{
		openAIChunkEvent(t, map[string]any{
			"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": toolID, "type": "function",
					"function": map[string]any{"name": toolName, "arguments": ""},
				}}},
				"finish_reason": "",
			}},
		}),
	}
	for _, frag := range fragments {
		events = append(events, openAIChunkEvent(t, map[string]any{
			"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "function": map[string]any{"arguments": frag},
				}}},
				"finish_reason": "",
			}},
		}))
	}
	events = append(events, openAIChunkEvent(t, map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	}))
	return events
}

// TestOpenAICompatConsumeStream_ToolArgumentDeltasSendProgressNoText is the
// RED test for the OpenAI-compat side of the fix: pkg/llm/openai_compat.go's
// tool-call delta handling used to accumulate `tc.Function.Arguments` into
// the builder and forward NOTHING to the channel — the user's deployments
// are mostly OpenAI-compatible, so this path matters at least as much as the
// Anthropic one. It must now send one StreamChunk{Progress: true} per
// non-empty argument fragment, carrying no text, and the final assembled
// message/tool call must be unaffected.
func TestOpenAICompatConsumeStream_ToolArgumentDeltasSendProgressNoText(t *testing.T) {
	fragments := []string{
		`{"path":"design.md","con`,
		`tent":"# Title\n\nSome very long body `,
		`text here."}`,
	}
	decoder := &fakeOpenAIDecoder{events: toolCallDeltaEvents(t, "tool_1", "write_file", fragments)}
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
		t.Fatalf("progress chunk count = %d, want %d (one per non-empty argument fragment; the id/name-only initial delta carries an empty arguments string and must not count)", progressCount, len(fragments))
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

// TestOpenAICompatConsumeStream_ProgressDoesNotEnableRetryAfterError pins the
// "do not change emitted's semantics" constraint on the OpenAI-compat side:
// a tool-call delta already set emitted=true before this fix, so a stream
// that accumulated tool-argument bytes and then hits a retryable transport
// error must NOT retry.
func TestOpenAICompatConsumeStream_ProgressDoesNotEnableRetryAfterError(t *testing.T) {
	events := toolCallDeltaEvents(t, "tool_1", "write_file", []string{`{"path":"a.md"}`})
	// Drop the trailing finish_reason chunk — the decoder now runs dry and
	// reports a transient error, as if the connection dropped mid-argument.
	events = events[:len(events)-1]
	decoder := &fakeOpenAIDecoder{events: events, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 64)
	retry, _ := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
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

// TestOpenAICompatConsumeStream_NoOutputYetStillRetries is the control for
// the test above: a stream that produced no output yet, hitting the same
// retryable error, must still retry.
func TestOpenAICompatConsumeStream_NoOutputYetStillRetries(t *testing.T) {
	decoder := &fakeOpenAIDecoder{events: nil, err: errors.New("connection reset by peer")}
	stream := ssestream.NewStream[openai.ChatCompletionChunk](decoder, nil)

	p := &OpenAICompatProvider{provider: "openai-compat"}
	ch := make(chan StreamChunk, 4)
	retry, _ := p.consumeStream(context.Background(), ch, stream, "test-model", true /* retryable */)
	close(ch)

	if !retry {
		t.Fatal("consumeStream did not retry a transient error before any output was emitted — unrelated to this fix, but must be unaffected by it")
	}
}
