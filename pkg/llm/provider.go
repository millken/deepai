package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// reconnectHeartbeatInterval is how often a provider emits a Progress chunk
// while it is backing off and re-establishing a failed stream. By that point
// the channel has already been handed to the agent, whose stream-idle watchdog
// (pkg/agent/streaming.go) cannot tell a reconnect apart from a dead stream —
// so a silent reconnect that outlasts the idle window kills a perfectly
// healthy request. Comfortably shorter than the agent's 2-minute default; a
// var so tests can shrink it.
var reconnectHeartbeatInterval = 15 * time.Second

// heartbeatDuring runs fn while emitting Progress chunks on ch, so a blocking
// reconnect never looks like a dead stream. It returns only after the
// heartbeat goroutine has stopped, which is what makes it safe for a caller
// that closes ch afterwards: no send can outlive this call.
func heartbeatDuring(ch chan<- StreamChunk, model string, fn func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(reconnectHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Abandon the send if fn finished meanwhile — otherwise a full
				// channel would pin this goroutine past close(ch).
				select {
				case ch <- StreamChunk{Model: model, Progress: true}:
				case <-done:
					return
				}
			}
		}
	}()
	fn()
	close(done)
	wg.Wait()
}

// LLMProvider describes the minimal contract implemented by each model backend.
type LLMProvider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// ChatRequest is the provider-agnostic request payload.
type ChatRequest struct {
	Model           string           `json:"model"`
	Messages        []models.Message `json:"messages"`
	Tools           []models.Tool    `json:"tools,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxTokens       *int             `json:"max_tokens,omitempty"`
	SystemPrompt    string           `json:"system_prompt,omitempty"`
	// ImageDetail controls the vision detail level for OpenAI-compatible
	// providers. "low" (default) uses a single tile (~170 tokens); "high" uses
	// multiple tiles for finer detail. Anthropic ignores this field (it uses
	// automatic rescaling).
	ImageDetail string            `json:"image_detail,omitempty"`
	OnChunk     func(StreamChunk) `json:"-"`
}

// ChatResponse is the normalized provider response.
type ChatResponse struct {
	Model   string         `json:"model,omitempty"`
	Message models.Message `json:"message"`
	Usage   Usage          `json:"usage,omitempty"`
	Stop    string         `json:"stop,omitempty"`
}

// StreamChunk is a normalized streaming delta.
type StreamChunk struct {
	Model     string            `json:"model,omitempty"`
	Delta     string            `json:"delta,omitempty"`
	ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
	Message   *models.Message   `json:"message,omitempty"`
	Usage     *Usage            `json:"usage,omitempty"`
	Stop      string            `json:"stop,omitempty"`
	Done      bool              `json:"done,omitempty"`
	Err       error             `json:"-"`
	// Progress marks a chunk sent for exactly one reason: to prove the
	// underlying stream is still actively producing bytes when those bytes
	// have nowhere else to go yet — the running case is a large tool-call
	// argument, streamed to this provider as many "input_json_delta"/
	// "arguments" fragments that accumulate into a builder and are only
	// turned into a models.ToolCall once the argument is complete. Before
	// this field existed, that entire accumulation window was invisible to
	// the consumer: no chunk of any kind crossed the channel, so
	// pkg/agent's stream idle watchdog (which resets its timer on ANY
	// received chunk, before looking at what the chunk contains) saw
	// nothing and could time out a request that was actually busy.
	//
	// A Progress chunk carries no payload — Delta, ToolCalls, Usage, Done,
	// and Err are all left at their zero value — by construction, not by
	// convention enforced elsewhere: every provider that sends one MUST
	// leave every other field zero, or a consumer that (correctly, per this
	// doc) treats Progress as "nothing to look at here" will silently drop
	// real content. Consumers that only care whether the stream is alive
	// (i.e. everyone today) can ignore this field entirely: simply
	// receiving the chunk already resets an idle timer, since that already
	// happens unconditionally before any field is inspected. The field
	// exists so a heartbeat-only chunk is self-describing — distinguishable
	// from a StreamChunk{} sent (or zero-valued) by mistake — rather than
	// overloading Delta/ToolCalls/Done with an empty value that already
	// means something else on every other chunk.
	Progress bool `json:"-"`
}

// Usage tracks token counts when a provider returns them.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

func (r ChatRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages are required")
	}
	return nil
}

// newToolArgsJSONError wraps a tool-call arguments JSON parse failure
// (anthropic.go's content_block_stop handling and openai_compat.go's
// assembleToolCalls both hit this) with an explanation of the most likely
// cause and what to do about it, instead of surfacing the bare
// json.Unmarshal error ("unexpected end of JSON input") on its own.
//
// The non-retryable path is the only place this fires with a message a
// human or the calling agent ever sees: the retryable path (see both call
// sites) already retries silently on the same condition and never
// constructs this error at all. By far the most common cause reaching this
// non-retryable path is the response hitting its output token limit while
// still emitting one large tool-call argument, which truncates the
// streamed JSON mid-string; a malformed-but-complete JSON argument from the
// model is possible but far rarer; it is not worth naming as a
// second explanation. This package is provider-agnostic and has no
// knowledge of individual tools (it must not name a specific tool such as
// docx_write here), so the advice stays generic: shrink the argument, or —
// if the tool that was called offers one — use its file-based parameter
// instead of inlining a large value.
func newToolArgsJSONError(toolName string, cause error) error {
	return fmt.Errorf(
		"tool %q: invalid arguments JSON: %w — this usually means the response hit its output token limit "+
			"while emitting a large tool-call argument, cutting the argument off mid-stream before it could "+
			"be parsed; retry with a smaller argument, or, if this tool has a file-based parameter, use that "+
			"instead of inlining a large value",
		toolName, cause)
}
