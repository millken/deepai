package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const (
	defaultCompactionThreshold = 0.75
	defaultCompactionKeepTail  = 6
	compactToolResultKeep      = 300
	compactAssistantTextKeep   = 200
	// summarizedToolResultPrefix / compactAssistantTextSuffix are the markers
	// that let compaction recognize its OWN output and leave it alone. See
	// summarizeOldToolResult for why re-summarizing is destructive.
	summarizedToolResultPrefix = "[tool result: "
	compactAssistantTextSuffix = " [...]"
	// compactToolCallArgsKeep is the max JSON byte size for a compacted
	// tool call's Arguments. Arguments larger than this are replaced with a
	// placeholder so compaction actually saves space, while small Arguments
	// are preserved verbatim (some providers reject nil Arguments).
	compactToolCallArgsKeep = 512
	// maxToolContentBytes is a hard cap applied when storing tool results in
	// runMessages. Prevents individual bash/web-fetch outputs from inflating
	// the context beyond any provider's practical limit.
	maxToolContentBytes = 50_000
	// offloadThresholdBytes is the size above which a tool result is written
	// to disk and replaced in-context with a summary + first/last 50 lines.
	// Per design doc §9 (l0_source_guard.offload_threshold_bytes).
	offloadThresholdBytes = 24 * 1024
)

// bytesPerToken is the single calibrated bytes-per-token ratio used by every
// fallback byte-based token estimate in this package (estimateTokens,
// toolSchemaTokens, and metrics.go's estimateInputTokens). Derived in commit
// 695bd80 from content-weighted measurement of real sessions: 58.7% code
// (~3.5 B/tok) + 28.9% JSON (~3.0) + 12.4% text (~3.0) ≈ 3.3 bytes/token,
// about 9% more accurate than a flat /3. Before this constant, three call
// sites used two different literals (/3 vs /3.3); provider-reported token
// counts usually override the heuristic, but the fallback ratio decides the
// very first request of every turn (provider anchors reset at every
// compaction), so the disagreement mattered. Migrating the trigger's fallback
// path from the uncalibrated /3 to /3.3 moves the effective compaction point
// LATER (fewer estimated tokens per byte, ~9% for large ASCII-heavy
// histories) — compactionThreshold (0.75) was not re-tuned to compensate:
// 0.75 against this calibrated ratio is the intended real threshold, not an
// artifact carried over from the old ratio.
const bytesPerToken = 3.3

// maybeCompact is Run()'s per-turn context-compaction trigger: it clears a
// previous stall once enough new material has accumulated, then — unless
// still stalled — measures the assembled prompt against a.contextWindow and,
// if over a.compactionThreshold, compacts runMessages (flushing memory
// synchronously first), re-derives promptView from the compacted messages,
// escalates to a smaller tail if one compaction pass wasn't enough, and
// records stall state + emits an AgentEventCompact if the ratio is still not
// back under threshold afterward. Pure extraction of that block from Run:
// inputs/outputs are explicit (runMessages/promptView in and out), but the
// compaction state itself (compactionStalled, compactionStalledAt,
// lastInputTokens, lastTokenCountMsgs) stays on Agent, exactly as it did
// before — moving it into a separate contextManager type was considered and
// deliberately downscoped (see the plan note) since ~10 fields worth of
// churn buys nothing a method extraction doesn't already give.
//
// turn is only used for log/debug fields; emit is Run's per-request event
// closure (stamps RequestID/SessionID before delivering to a.events).
//
// M4-2 metering invariant: promptView in is the aged view WITHOUT
// a.turnInjection appended (see react.go's call site); every estimate taken
// here — the initial ratio check and, if compaction fires, the after-
// compaction re-check(s) — measures appendTurnInjection(view, a.turnInjection),
// i.e. the SAME bytes the request this turn will actually send, and the
// promptView this function RETURNS already has the injection appended
// exactly once, whether or not compaction fired. Callers must not append it
// again.
func (a *Agent) maybeCompact(ctx context.Context, sessionID string, turn int, runMessages, promptView []models.Message, systemPrompt string, emit func(AgentEvent)) ([]models.Message, []models.Message) {
	// Clear a previous stall once new material has slid into the
	// compactable region: enough messages have been appended since the
	// stall point that the messages protected as the tail back then are
	// no longer the tail now, so a fresh compaction attempt has new
	// ground to work with instead of just re-deriving the same
	// inconclusive result.
	if a.compactionStalled && len(runMessages) >= a.compactionStalledAt+a.compactionKeepTail {
		a.setCompactionStall(false, a.compactionStalledAt)
	}

	// Context compaction: compress old messages when approaching context window.
	// Skipped entirely while compactionStalled is set: a previous turn
	// already discovered that compacting doesn't bring the ratio back
	// under threshold, and re-evaluating and re-compacting every
	// subsequent turn before enough new material has accumulated (see
	// the unstall check above) would only thrash (repeated synchronous
	// memory flushes and re-deriving the same inconclusive compaction)
	// for no benefit.
	if a.contextWindow > 0 && !a.compactionStalled {
		// Measure the assembled prompt + aged view + turn injection (what the
		// request below actually sends), not canonical messages or the base
		// system prompt — otherwise compaction can fire too late (assembled
		// prompt bigger than base) or too early (aged view much smaller than
		// canonical, so compacting on the canonical estimate destroys history
		// the provider was never even going to see) or too late again (the
		// turn injection's own bytes — date + memory — omitted from the
		// estimate). Tool schemas are sent on every request too;
		// estimateContextTokens adds them internally.
		estimated := a.estimateContextTokens(appendTurnInjection(promptView, a.turnInjection), systemPrompt)
		ratio := float64(estimated) / float64(a.contextWindow)
		if ratio >= a.compactionThreshold {
			before := len(runMessages)
			compacted, didCompact := compactMessages(runMessages, a.compactionKeepTail)
			if didCompact {
				// Flush memory synchronously before compaction to guarantee no
				// data loss. This blocks while the LLM extracts, but compaction
				// is infrequent and losing information is worse than the
				// latency cost. Moved inside didCompact (rather than gated only
				// on ratio >= threshold) so a trigger turn where
				// compactMessages finds nothing left to compact — already at
				// the head/tail floor — doesn't still pay a 30s-timeout flush
				// for a compaction that's not going to happen.
				if a.memoryService != nil && a.memoryExtractor != nil {
					// Cancel any queued update for this session so the
					// stale async job won't overwrite our sync flush.
					a.memoryService.CancelPendingUpdates(sessionID)
					flushCtx, flushCancel := context.WithTimeout(ctx, 30*time.Second)
					if skillName := a.ActiveSkill(); skillName != "" {
						_ = a.memoryService.UpdateWithFactSource(flushCtx, sessionID, runMessages, a.memoryExtractor, "skill:"+skillName)
					} else {
						_ = a.memoryService.UpdateWith(flushCtx, sessionID, runMessages, a.memoryExtractor)
					}
					flushCancel()
				}

				runMessages = compacted
				a.setTokenAnchor(0, 0)
				// Canonical messages changed; the view sent to the provider
				// must be re-derived from them. Injection re-appended (via
				// appendTurnInjection, exactly once) only for the estimate
				// here — promptView itself stays un-injected until the final
				// return below, matching the pre-compaction shape.
				promptView = buildPromptView(runMessages, a.aging, a.contextWindow)

				afterEstimated := a.estimateContextTokens(appendTurnInjection(promptView, a.turnInjection), systemPrompt)
				if afterEstimated > a.contextWindow {
					for tail := a.compactionKeepTail - 1; tail >= 2; tail-- {
						c2, ok := compactMessages(runMessages, tail)
						if ok {
							runMessages = c2
						}
						promptView = buildPromptView(runMessages, a.aging, a.contextWindow)
						afterEstimated = a.estimateContextTokens(appendTurnInjection(promptView, a.turnInjection), systemPrompt)
						if afterEstimated <= a.contextWindow {
							break
						}
					}
				}

				afterRatio := float64(afterEstimated) / float64(a.contextWindow)
				a.logger.Debug("context compaction", "turn", turn, "before_msgs", before, "after_msgs", len(runMessages), "before_tokens", estimated, "after_tokens", afterEstimated, "before_ratio", fmt.Sprintf("%.2f", ratio), "after_ratio", fmt.Sprintf("%.2f", afterRatio))
				emit(AgentEvent{
					Type: AgentEventCompact,
					CompactStats: &CompactStats{
						MessagesBefore: before,
						MessagesAfter:  len(runMessages),
						InputTokens:    estimated,
						AfterTokens:    afterEstimated,
						ContextWindow:  a.contextWindow,
						Ratio:          ratio,
						AfterRatio:     afterRatio,
					},
				})
				if afterRatio >= a.compactionThreshold {
					a.setCompactionStall(true, len(runMessages))
					a.logger.Warn("context compaction did not drop the ratio under threshold; suppressing "+
						"further compaction attempts until enough new messages accumulate to make the "+
						"current tail compactable again", "turn", turn, "after_tokens", afterEstimated,
						"context_window", a.contextWindow, "threshold", a.compactionThreshold)
				}
			}
		}
	}

	// M4-2: the turn injection is appended here — exactly once — regardless
	// of whether compaction fired above, so every caller of maybeCompact
	// receives a promptView that is already the FINAL view to send (see the
	// doc comment on this function and on appendTurnInjection).
	return runMessages, appendTurnInjection(promptView, a.turnInjection)
}

// compactMessages applies heuristic compression to old messages.
// It preserves: system messages, the first human message, and the last keepTail messages.
// Messages in the middle ("old" region) are summarized:
//   - Tool results → "[tool result: {name}, {first N chars}...]"
//   - Assistant with tool calls → keep tool_calls (ID+Name only, strip Arguments), add summary text
//   - Assistant with text → first N chars + "[...]"
//
// Compacted messages preserve SessionID and critical struct fields (ToolResult, ToolCalls)
// to maintain provider mapping integrity and session persistence validation.
//
// Returns the compacted slice and whether any compaction occurred.
func compactMessages(messages []models.Message, keepTail int) ([]models.Message, bool) {
	n := len(messages)
	if n <= keepTail+2 {
		return messages, false
	}

	tailStart := n - keepTail
	if tailStart < 2 {
		tailStart = 2
	}

	// Find protected head: system messages + first human message.
	headEnd := 0
	for i, msg := range messages {
		if msg.Role == models.RoleSystem {
			headEnd = i + 1
			continue
		}
		if msg.Role == models.RoleHuman {
			headEnd = i + 1
			break
		}
		break
	}

	if headEnd >= tailStart {
		return messages, false
	}

	// Don't cut in the middle of a tool_call/tool_result chain.
	// If the tail would start with a tool result, move back to include
	// the preceding assistant message that issued the tool calls.
	for tailStart > headEnd && messages[tailStart].Role == models.RoleTool {
		tailStart--
	}

	var result []models.Message
	compacted := false

	// Phase 1: preserve head verbatim.
	result = append(result, messages[:headEnd]...)

	// Phase 2: compact the middle region.
	for i := headEnd; i < tailStart; i++ {
		msg := messages[i]
		switch msg.Role {
		case models.RoleTool:
			compactedMsg := compactToolMessage(msg)
			result = append(result, compactedMsg)
			if compactedMsg.Content != msg.Content {
				compacted = true
			}

		case models.RoleAI:
			compactedMsg := compactAssistantMessage(msg)
			result = append(result, compactedMsg)
			if compactedMsg.Content != msg.Content || len(compactedMsg.ToolCalls) != len(msg.ToolCalls) {
				compacted = true
			}

		default:
			result = append(result, msg)
		}
	}

	// Phase 3: append tail verbatim.
	result = append(result, messages[tailStart:]...)

	return result, compacted
}

// compactToolMessage creates a compressed copy of a tool result message.
// Preserves SessionID, ToolResult (CallID/ToolName/Status), truncates Content.
func compactToolMessage(msg models.Message) models.Message {
	summary := summarizeOldToolResult(msg)
	out := models.Message{
		ID:        msg.ID,
		SessionID: msg.SessionID,
		Role:      models.RoleTool,
		Content:   summary,
	}
	if msg.ToolResult != nil {
		out.ToolResult = &models.ToolResult{
			CallID:   msg.ToolResult.CallID,
			ToolName: msg.ToolResult.ToolName,
			Status:   msg.ToolResult.Status,
		}
	}
	return out
}

// compactAssistantMessage creates a compressed copy of an assistant message.
// For messages with tool_calls: keeps ToolCalls with ID+Name+Status and
// preserves Arguments (some providers reject tool_calls with nil Arguments),
// truncating large Arguments to a placeholder to save space.
// For text-only messages: truncates content.
func compactAssistantMessage(msg models.Message) models.Message {
	out := models.Message{
		ID:        msg.ID,
		SessionID: msg.SessionID,
		Role:      models.RoleAI,
	}

	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]models.ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			out.ToolCalls[i] = models.ToolCall{
				ID:     tc.ID,
				Name:   tc.Name,
				Status: tc.Status,
			}
			argsJSON, err := json.Marshal(tc.Arguments)
			if err != nil || len(argsJSON) > compactToolCallArgsKeep {
				out.ToolCalls[i].Arguments = map[string]any{"_compacted": true}
			} else {
				out.ToolCalls[i].Arguments = cloneArguments(tc.Arguments)
			}
		}
		if strings.TrimSpace(msg.Content) == "" {
			names := make([]string, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				names = append(names, tc.Name)
			}
			out.Content = fmt.Sprintf("[Called %s]", strings.Join(names, ", "))
		} else {
			out.Content = compactAssistantText(msg.Content)
		}
	} else {
		out.Content = compactAssistantText(msg.Content)
	}

	return out
}

// compactAssistantText truncates historical assistant text, and — like
// summarizeOldToolResult — is idempotent: text already carrying the truncation
// marker is returned untouched rather than re-cut. Re-cutting rune-safely would
// eat a few bytes of real text per pass and append a second marker every time
// compaction ran.
func compactAssistantText(content string) string {
	if strings.HasSuffix(content, compactAssistantTextSuffix) {
		return content
	}
	if len(content) <= compactAssistantTextKeep {
		return content // short enough, keep verbatim
	}
	return truncateRuneSafe(content, compactAssistantTextKeep) + compactAssistantTextSuffix
}

// summarizeOldToolResult rewrites a historical tool result down to its first
// compactToolResultKeep bytes, wrapped in a marker naming the tool.
//
// It MUST be idempotent. Compaction runs again on every turn that is over
// threshold, and the escalation loop in maybeCompact re-runs it over
// already-compacted messages with smaller tails, so a summary that summarized
// its own output would nest: with a 25-byte wrapper and only the first 300
// bytes kept, twelve passes leave nothing but "[tool result: read_file,"
// prefixes and the payload is gone — permanently, since the compacted messages
// become the canonical history and get persisted. The model then re-reads the
// files it can no longer see, which pushes the context back over threshold,
// which compacts again: the read loop observed in session
// 20260812_093415_fc6e (1136 read_file calls over 7 distinct files).
//
// Returning msg.Content unchanged for an already-summarized message also keeps
// compactMessages honest about whether it actually compacted anything, which is
// what lets the compaction-stall guard in maybeCompact engage instead of
// re-compacting the same tail every turn forever.
func summarizeOldToolResult(msg models.Message) string {
	if isSummarizedToolResult(msg.Content) {
		// Sessions compacted by the buggy version carry nested placeholders
		// whose payload is already gone — 300 bytes of "[tool result:
		// read_file, [tool result: read_file, …" per message, ~110k tokens of
		// pure noise across the session that started this investigation. They
		// cannot be repaired, but they can stop costing context: collapse them
		// to one honest line. A single-level summary is left exactly as is,
		// which is what keeps this function idempotent.
		if strings.Count(msg.Content, summarizedToolResultPrefix) > 1 {
			return fmt.Sprintf("%s%s, content lost to an earlier compaction bug]",
				summarizedToolResultPrefix, toolNameOf(msg))
		}
		return msg.Content
	}
	toolName := toolNameOf(msg)
	// truncateRuneSafe, not a raw byte slice: cutting mid-rune emits invalid
	// UTF-8, which strict providers reject outright (same reason aging.go uses
	// it — CJK tool output makes this the common case, not the edge case).
	content := truncateRuneSafe(msg.Content, compactToolResultKeep)
	return fmt.Sprintf("%s%s, %s...]", summarizedToolResultPrefix, toolName, content)
}

// toolNameOf names the tool a result came from, for summaries and markers.
func toolNameOf(msg models.Message) string {
	if msg.ToolResult != nil && msg.ToolResult.ToolName != "" {
		return msg.ToolResult.ToolName
	}
	return "unknown"
}

// isSummarizedToolResult reports whether content is already the output of
// summarizeOldToolResult — either a truncated summary or the collapsed marker
// for a legacy nested one. Requiring the closing bracket as well as the prefix
// keeps a genuine tool result that merely happens to start with those bytes
// from being mistaken for a summary; if one ever does slip through, the only
// consequence is that it is left verbatim instead of being truncated.
func isSummarizedToolResult(content string) bool {
	return strings.HasPrefix(content, summarizedToolResultPrefix) &&
		strings.HasSuffix(content, "]")
}

func resolveCompactionThreshold(v float64) float64 {
	if v > 0 && v <= 1 {
		return v
	}
	return defaultCompactionThreshold
}

func resolveCompactionKeepTail(v int) int {
	if v > 0 {
		return v
	}
	return defaultCompactionKeepTail
}

// estimateTokens estimates token count from the current message history.
// Always computes from actual message bytes to catch context growth
// that lastInputTokens (a lagging indicator) would miss.
func estimateTokens(messages []models.Message, systemPrompt string, _ int) int {
	totalBytes := len(systemPrompt)
	for _, msg := range messages {
		// msg.Content already contains the tool result text (same bytes as
		// ToolResult.Content), so only count it once via msg.Content.
		size := len(msg.Content)
		for _, tc := range msg.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			size += len(tc.ID) + len(tc.Name) + len(argsJSON) + 20 // overhead
		}
		if msg.ToolResult != nil {
			size += len(msg.ToolResult.CallID) + len(msg.ToolResult.ToolName)
			size += len(msg.ToolResult.Error)
			size += 20
		}
		totalBytes += size + 30 // role/ID/metadata overhead per message
	}
	// Calibrated bytes/token ratio (see bytesPerToken) rather than a flat
	// per-language guess.
	tokens := int(float64(totalBytes) / bytesPerToken)
	if tokens == 0 && totalBytes > 0 {
		return 1
	}
	return tokens
}
