package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

var messageSeq uint64

var agentRequestSeq uint64

// turnStreamResult accumulates one turn's streamed response, whether it
// finished normally (err == nil) or was cut short by a provider error, a
// context-overflow chunk, or the stream idle watchdog (err != nil in all
// three cases — the caller in Run distinguishes them the same way it always
// has, via isContextOverflowError/normalizeRunError on the returned err).
type turnStreamResult struct {
	text       string
	toolCalls  []models.ToolCall
	usage      *llm.Usage
	stopReason string
	err        error
}

// consumeStream drains one turn's stream channel, emitting
// AgentEventTextChunk events as text deltas arrive, and enforcing the stream
// idle watchdog (plan #8): if no chunk of any kind arrives within
// a.streamIdleTimeout, the per-request ctx is cancelled (via cancel, which
// MUST be the CancelFunc paired with the ctx passed to the a.llm.Stream call
// that produced stream), and a *TimeoutError is returned so the normal
// timeout handling (isContextOverflowError / normalizeRunError, and any
// errors.As(err, *TimeoutError) check further up the stack — see
// pkg/agent/subagent.go's retry logic) applies exactly as it would for the
// pre-existing total-duration requestTimeout. cancel is deferred so the
// per-request ctx never outlives this call on any exit path.
//
// Both "stream went bad" exits (a chunk carrying Err, and the idle timer
// firing) drain the SAME way, deliberately: in a background goroutine,
// never in-place. An earlier version of this code drained the chunk.Err
// case synchronously (`for range stream {}` in this call's own goroutine),
// on the assumption that a provider which has already surfaced an error is
// about to unwind and close its channel shortly. That assumption isn't
// guaranteed — a provider goroutine that errors and then wedges without
// closing hung this call, and Run, forever, exactly like an idle stream
// would (caught by review, reproduced with a probe provider). A single
// best-effort background goroutine mops up either case instead: harmless if
// the provider (or its underlying HTTP transport, which does respect ctx
// cancellation) eventually unwinds and closes the channel, and bounded (one
// goroutine per exit, not an accumulating leak) even in the pathological
// case where it never does.
func (a *Agent) consumeStream(stream <-chan llm.StreamChunk, cancel context.CancelFunc, emit func(AgentEvent), aiMessageID string) turnStreamResult {
	defer cancel()

	idleTimeout := a.streamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()

	var (
		textBuilder strings.Builder
		toolCalls   []models.ToolCall
		streamUsage *llm.Usage
		stopReason  string
	)

	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return turnStreamResult{
					text:       textBuilder.String(),
					toolCalls:  toolCalls,
					usage:      streamUsage,
					stopReason: stopReason,
				}
			}
			// Any chunk at all — including one carrying an error — proves
			// the stream is still alive, so the idle window resets here,
			// before inspecting what the chunk actually contains.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleTimeout)

			if chunk.Err != nil {
				cancel()
				// Drain in the background rather than in-place: a provider
				// that has already surfaced an error is EXPECTED to unwind
				// and close its channel shortly (that was the original
				// assumption for draining synchronously here, mirroring the
				// pre-existing overflow-branch drain), but that is not
				// guaranteed — a provider goroutine that errors and then
				// wedges without closing would hang this call, and Run,
				// forever (review finding: proved with a probe provider).
				// Same treatment as the idle-timer exit below: bounded to
				// one goroutine per error, harmless once the provider (or
				// its underlying HTTP transport, which does respect ctx
				// cancellation) actually unwinds.
				go func() {
					for range stream {
					}
				}()
				return turnStreamResult{
					text:       textBuilder.String(),
					toolCalls:  toolCalls,
					usage:      streamUsage,
					stopReason: stopReason,
					err:        chunk.Err,
				}
			}
			if chunk.Delta != "" {
				textBuilder.WriteString(chunk.Delta)
				emit(AgentEvent{Type: AgentEventTextChunk, MessageID: aiMessageID, Text: chunk.Delta})
			}
			if len(chunk.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, chunk.ToolCalls)
			}
			if chunk.Usage != nil {
				streamUsage = chunk.Usage
			}
			if chunk.Done {
				stopReason = chunk.Stop
				if chunk.Message != nil {
					if textBuilder.Len() == 0 && chunk.Message.Content != "" {
						textBuilder.WriteString(chunk.Message.Content)
					}
					if len(toolCalls) == 0 && len(chunk.Message.ToolCalls) > 0 {
						toolCalls = append(toolCalls, chunk.Message.ToolCalls...)
					}
				}
			}
		case <-idleTimer.C:
			cancel()
			// See the func doc: draining here must not block, since a
			// provider that has gone idle may never send anything again.
			go func() {
				for range stream {
				}
			}()
			return turnStreamResult{
				text:       textBuilder.String(),
				toolCalls:  toolCalls,
				usage:      streamUsage,
				stopReason: stopReason,
				err: &TimeoutError{
					Duration: idleTimeout,
					// Duration is NOT repeated in Message: TimeoutError's
					// own formatter appends "after <Duration>" already, so
					// including it here too would read as
					// "no data for 50ms after 50ms".
					Message: "stream idle timeout: no data received",
				},
			}
		}
	}
}

func resolveModel(model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	if model := strings.TrimSpace(os.Getenv("DEFAULT_LLM_MODEL")); model != "" {
		return model
	}
	return "gpt-4.1-mini"
}

func newMessageID(prefix string) string {
	seq := atomic.AddUint64(&messageSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UTC().UnixNano(), seq)
}

func newAgentRequestID() string {
	seq := atomic.AddUint64(&agentRequestSeq, 1)
	return fmt.Sprintf("req_%d_%d", time.Now().UTC().UnixNano(), seq)
}

func normalizeRunError(ctx context.Context, err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	// Only map to agent timeout when the run context itself hit deadline.
	// Keep inner/tool deadline errors intact so users see the real source.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &TimeoutError{
			Duration: timeout,
			Message:  "agent request timed out",
		}
	}
	return err
}

func mergeToolCalls(existing, incoming []models.ToolCall) []models.ToolCall {
	if len(existing) == 0 {
		return append([]models.ToolCall(nil), incoming...)
	}

	indexByID := make(map[string]int, len(existing))
	for i, call := range existing {
		indexByID[call.ID] = i
	}

	for _, call := range incoming {
		if call.ID != "" {
			if idx, ok := indexByID[call.ID]; ok {
				if existing[idx].Name == "" {
					existing[idx].Name = call.Name
				}
				if len(call.Arguments) > 0 {
					existing[idx].Arguments = call.Arguments
				}
				if call.Status != "" {
					existing[idx].Status = call.Status
				}
				continue
			}
			indexByID[call.ID] = len(existing)
		}
		existing = append(existing, call)
	}

	return existing
}

func accumulateUsage(dst *Usage, src *llm.Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
}

func cloneUsage(src *Usage) *Usage {
	if src == nil {
		return nil
	}
	out := *src
	return &out
}

// toolSchemaTokens estimates the token cost of the tool definitions attached to
// every request (name + description + JSON schema). These are invisible to
// estimateTokens, which only inspects messages, so they must be added to any
// context-window estimate or the agent will compact later than the real payload
// warrants. Uses the same calibrated bytesPerToken ratio as estimateTokens.
func (a *Agent) toolSchemaTokens() int {
	return int(float64(a.toolSchemaBytes()) / bytesPerToken)
}

// toolSchemaBytes estimates the byte size of all registered tool schemas
// (name + description + JSON input schema). Used by token estimation and by the
// Phase 0 metrics framework.
func (a *Agent) toolSchemaBytes() int {
	if a == nil || a.tools == nil {
		return 0
	}
	totalBytes := 0
	for _, t := range a.tools.List() {
		totalBytes += len(t.Name) + len(t.Description) + 10 // per-tool framing
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				totalBytes += len(b)
			}
		}
	}
	return totalBytes
}

// estimateContextTokens returns the best estimate of how many tokens the next
// request will occupy. Callers MUST pass the same two things the request
// actually sends: the message VIEW (the aged prompt view when aging is
// enabled, not necessarily canonical runMessages) and the fully ASSEMBLED
// system prompt (BuildSystemPrompt's output — base prompt, the file-op rule,
// tool recommendations, the delegation prompt + catalog, and plan-mode text —
// not the bare a.systemPrompt). BuildSystemPrompt does NOT layer memory
// injections (M4-2 removed that — those, plus the date, now ride the
// separate per-Run trailing turn injection appended via appendTurnInjection;
// see buildTurnInjection's doc comment): every call site in this package
// passes view WITH that injection already appended (never systemPrompt with
// it baked in), which is how the injection's bytes get counted at all here.
// Measuring anything else (canonical messages, or the base prompt alone, or
// systemPrompt without also appending the injection to view) either compacts
// too late — the assembled prompt is bigger than the base one — or compacts
// unnecessarily — canonical history can be far bigger than the aged view
// that is actually sent, permanently destroying history the provider never
// even saw.
//
// It prefers the provider's own reported input-token count from the previous
// response — accurate for the model's real tokenizer, which the byte
// heuristic underestimates for CJK/multi-byte text — plus a byte estimate of
// any messages appended since that count was taken. The anchor
// (lastInputTokens/lastTokenCountMsgs) is reset to zero at every compaction
// site via setTokenAnchor — the SINGLE write mechanism for both fields (see
// its doc comment); every reset in this package, including
// compactOnOverflow's, goes through it — so whenever the anchor is set, the
// first lastTokenCountMsgs entries of view are exactly what SOME request
// counted (buildPromptView never changes message count, only content, so
// indices into view and into the canonical slice it was derived from always
// agree). "Some request" rather than "this Agent's own previous request" is
// deliberate: M4-3 (SessionCarry) lets New() PRIME a brand-new Agent's
// anchor from a carried session — i.e. from a DIFFERENT Agent's prior Run —
// so this invariant holds across the REPL's per-turn Agent churn, not only
// within a single Run (setTokenAnchor mirrors onto the session on every
// write, so the two can never disagree). Falls back to the pure byte
// heuristic (plus tool schemas) before the first response, right after a
// compaction reset, or when nothing was ever carried.
func (a *Agent) estimateContextTokens(view []models.Message, systemPrompt string) int {
	heuristic := estimateTokens(view, systemPrompt, 0) + a.toolSchemaTokens()
	if a.lastInputTokens <= 0 || a.lastTokenCountMsgs <= 0 || len(view) == 0 || a.lastTokenCountMsgs > len(view)-1 {
		return heuristic
	}
	// lastInputTokens already covers the system prompt, tool schemas, the
	// first lastTokenCountMsgs CANONICAL messages, AND the trailing turn
	// injection (M4-2) that was appended to THAT request's view — react.go
	// sets lastTokenCountMsgs from len(runMessages) (canonical, never
	// includes the injection), but the provider's real count it pairs with
	// (lastInputTokens) was for a view that DID have a.turnInjection appended
	// at the tail. Since a.turnInjection is constant per activeSource segment
	// (recomputed only on a mid-Run skill load; that one turn's estimate is
	// off by the injection size delta until the next response re-anchors), that
	// already-counted copy stands in for "the current injection's cost" —
	// the delta below must therefore span only view[lastTokenCountMsgs :
	// len(view)-1], i.e. the genuinely new canonical growth since the
	// anchor, EXCLUDING the freshly re-appended injection at view's tail
	// (view[len(view)-1]). Including it would double-count the injection's
	// bytes every single turn (once via lastInputTokens, once via delta),
	// inflating the estimate and tripping compaction early.
	delta := estimateTokens(view[a.lastTokenCountMsgs:len(view)-1], "", 0)
	if provider := a.lastInputTokens + delta; provider > heuristic {
		return provider
	}
	return heuristic
}

// isInputContextOverflow reports whether stopReason indicates the provider
// rejected the REQUEST because the input (prompt + history) exceeded the
// model's context window. This is the only overflow shape compacting
// conversation history can actually help with, since compaction shrinks
// input, not output.
func isInputContextOverflow(stopReason string) bool {
	switch stopReason {
	case "model_context_window_exceeded":
		return true
	}
	return false
}

// isOutputTruncation reports whether stopReason indicates the provider
// truncated its OUTPUT (hit the max-tokens/generation-length limit) rather
// than rejecting the input. Compacting conversation history cannot help
// here — there's nothing to retry into; the request itself fit fine, only
// the response was cut short.
func isOutputTruncation(stopReason string) bool {
	switch stopReason {
	case "max_tokens", "length":
		return true
	}
	return false
}

// isContextOverflowError detects context-window overflow surfaced as a
// transport-level error (typically HTTP 400 from OpenAI-compatible providers
// such as DeepSeek/Qwen/GLM that don't expose stopReason in this case).
// Matched substrings are intentionally specific to avoid false positives on
// generic "too long" complaints.
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	needles := []string{
		"context_length_exceeded",       // openai
		"context length",                // openai variants
		"maximum context length",        // openai
		"context window",                // generic
		"model_context_window_exceeded", // anthropic stop reason surfaced as error
		"prompt is too long",            // anthropic
		"input is too long",             // anthropic / qwen
		"request too large",             // openai
		"reduce the length",             // openai "please reduce the length..."
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// compactOnOverflow tries to shrink runMessages and reports whether the caller
// should continue the outer loop (i.e. retry the LLM request).
//
// compactMessages rewrites message *content* but preserves message *count*, so
// the retry must be gated on the estimated token size dropping — gating on
// len() (as a previous version did) was always false, leaving this reactive
// backstop dead and turning every provider context-overflow into a hard
// failure. Progressively smaller tails are tried so a payload dominated by a
// big tail can still be reduced.
//
// systemPrompt is the fully assembled prompt used for the request that just
// overflowed (the same value the main compaction trigger uses), so this
// reactive path's before/after comparison uses the identical estimator
// (estimateContextTokens: heuristic + tool schemas, or provider anchor +
// delta) as the proactive one instead of a bare a.systemPrompt with no
// schema accounting. It's a relative before/after comparison, so absolute
// agreement with the main path matters less than internal consistency here.
//
// The provider anchor (lastInputTokens/lastTokenCountMsgs) is invalidated
// before either estimate: it describes the last SUCCESSFUL request (the
// request that just overflowed errored out before reporting any usage), so
// by the time we're here it is stale — it doesn't cover whatever grew the
// payload enough to overflow. Left alone it is worse than merely stale —
// since compactMessages preserves message *count*, the anchor's cutoff index
// stays in-bounds for the compacted messages too, so both "before" and
// "after" would resolve to the identical stale provider count (lastInputTokens
// plus a zero delta, since the tail beyond the anchor is now empty on both
// sides). before==after then holds for every candidate tail, "after < before"
// is never true, and this whole reactive backstop silently goes dead — the
// common case for any provider that reports input_tokens (DeepSeek, Qwen,
// GLM), not a rare edge case.
func (a *Agent) compactOnOverflow(runMessages []models.Message, systemPrompt string, turn int, where string) ([]models.Message, bool) {
	// M4 final-phase review F-M4-1: use setTokenAnchor (not a direct field
	// assignment) so the reset also reaches the carried session, if any —
	// otherwise the next REPL turn's New() would revive this stale
	// pre-overflow anchor from the session even though this Agent's own
	// copy was correctly invalidated.
	a.setTokenAnchor(0, 0)

	// Measure the same aged VIEW + turn injection the main compaction trigger
	// (and the request that just overflowed — maybeCompact appends
	// a.turnInjection to every view it hands to Run) actually sends, not raw
	// canonical bytes — compactMessages below still mutates and returns
	// canonical messages. Without the injection here, "before" would
	// under-count relative to what actually overflowed; since it is the same
	// constant-per-Run message on both sides of the comparison, omitting it
	// wouldn't change which candidate tail wins, but including it keeps this
	// reactive path's absolute numbers (logged below) consistent with the
	// proactive trigger's.
	before := a.estimateContextTokens(appendTurnInjection(buildPromptView(runMessages, a.aging, a.contextWindow), a.turnInjection), systemPrompt)
	for _, tail := range []int{a.compactionKeepTail, 4, 2} {
		if tail <= 0 {
			continue
		}
		compacted, didCompact := compactMessages(runMessages, tail)
		if !didCompact {
			continue
		}
		after := a.estimateContextTokens(appendTurnInjection(buildPromptView(compacted, a.aging, a.contextWindow), a.turnInjection), systemPrompt)
		if after < before {
			a.logger.Warn("compacting after "+where+" context overflow", "turn", turn, "tail", tail, "before_tokens", before, "after_tokens", after)
			return compacted, true
		}
	}
	a.logger.Warn("context overflow and compaction cannot reduce further", "where", where, "turn", turn, "messages", len(runMessages))
	return runMessages, false
}

func newAgentError(err error) *AgentError {
	if err == nil {
		return nil
	}
	agentErr := &AgentError{
		Message: err.Error(),
	}
	switch {
	case errors.Is(err, context.Canceled):
		agentErr.Code = "context_canceled"
		agentErr.Suggestion = "Retry the run if the cancellation was unintended."
		agentErr.Retryable = true
	case errors.Is(err, context.DeadlineExceeded):
		agentErr.Code = "deadline_exceeded"
		agentErr.Suggestion = "Retry with a longer timeout or lower max_tokens."
		agentErr.Retryable = true
	case strings.Contains(strings.ToLower(err.Error()), "max turns"):
		agentErr.Code = "max_turns_exceeded"
		agentErr.Suggestion = "Increase max turns or simplify the request."
	case strings.Contains(strings.ToLower(err.Error()), "token budget"):
		agentErr.Code = "token_budget_exceeded"
		agentErr.Suggestion = "Increase token budget or simplify the request."
	case strings.Contains(strings.ToLower(err.Error()), "api key"):
		agentErr.Code = "provider_auth"
		agentErr.Suggestion = "Verify the provider credentials and base URL."
	default:
		agentErr.Code = "run_error"
		agentErr.Suggestion = "Retry the run or inspect the previous tool and model events."
		agentErr.Retryable = true
	}
	return agentErr
}
