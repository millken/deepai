package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// addSubagentUsage rolls a completed subagent's token consumption into the
// parent run's usage accumulator (M2-2 12b), so RunResult.Usage (TUI stats)
// and the parent's own token budget check (a.maxTokensBudget, above) include
// the full subagent tree, not just the parent's own LLM calls. Liberal in
// what it accepts: result.Data["subagent_usage"]'s value is the concrete
// *subagent.TokenUsage type populated by the task tool
// (pkg/tools/subagent.go), but a type switch keeps this robust rather than a
// hard cast. No-op when the key is absent, nil, or a different type.
func addSubagentUsage(dst *Usage, result models.ToolResult) {
	if dst == nil || len(result.Data) == 0 {
		return
	}
	raw, ok := result.Data["subagent_usage"]
	if !ok {
		return
	}
	switch v := raw.(type) {
	case *subagent.TokenUsage:
		if v == nil {
			return
		}
		dst.InputTokens += v.PromptTokens
		dst.OutputTokens += v.CompletionTokens
		dst.TotalTokens += v.TotalTokens
	}
}

func toolMessageContent(result models.ToolResult) string {
	s := result.Content
	if result.Error != "" {
		s = result.Error
	}
	if len(s) > maxToolContentBytes {
		s = s[:maxToolContentBytes] + fmt.Sprintf("\n... [truncated: %d bytes total]", len(s))
	}
	return s
}

// appendToolResultMessage appends a tool result message to runMessages.
// Returns the new slice (append may reallocate).
func appendToolResultMessage(runMessages []models.Message, sessionID string, result models.ToolResult) []models.Message {
	return append(runMessages, models.Message{
		ID:         newMessageID("tool"),
		SessionID:  sessionID,
		Role:       models.RoleTool,
		Content:    toolMessageContent(result),
		ToolResult: &result,
		CreatedAt:  time.Now().UTC(),
	})
}

// offloadIfNeeded checks if a tool result exceeds the offload threshold.
// If so, it writes the full content to a file under offloadDir and replaces
// result.Content with a compact reference (path + first/last 50 lines).
// Returns true if offload occurred. Errors during file write are logged but
// non-fatal — the result stays in-context (degraded but functional).
// A handler that set models.ToolDataNoOffload in Data opts its result out —
// for tools whose contract is delivering large content INTO context (code_map
// include_content), offloading would defeat the entire call; those handlers
// enforce their own output budget instead.
func (a *Agent) offloadIfNeeded(result *models.ToolResult, offloadDir string) bool {
	if offloadDir == "" || len(result.Content) <= offloadThresholdBytes {
		return false
	}
	if v, ok := result.Data[models.ToolDataNoOffload].(bool); ok && v {
		return false
	}
	// CallID is unique per invocation, making a safe filename.
	callID := result.CallID
	if callID == "" {
		callID = newMessageID("offload")
	}
	offloadPath := filepath.Join(offloadDir, callID+".txt")

	if err := os.MkdirAll(offloadDir, 0700); err != nil {
		a.logger.Warn("offload mkdir failed", "dir", offloadDir, "err", err)
		return false
	}
	if err := os.WriteFile(offloadPath, []byte(result.Content), 0644); err != nil {
		a.logger.Warn("offload write failed", "path", offloadPath, "err", err)
		return false
	}

	result.Content = buildOffloadedContent(result.Content, result.ToolName, offloadPath)
	return true
}

// buildOffloadedContent creates the in-context replacement for an offloaded
// result: a reference header, first 50 lines, last 50 lines, and an omission
// notice for the middle.
func buildOffloadedContent(original, toolName, offloadPath string) string {
	lines := strings.Split(original, "\n")
	totalLines := len(lines)

	var b strings.Builder
	fmt.Fprintf(&b, "[offloaded: full output (%d bytes, %d lines) saved to %s]\n",
		len(original), totalLines, offloadPath)

	const headLimit = 50
	const tailLimit = 50

	if totalLines <= headLimit+tailLimit {
		// Content fits in head+tail, include everything (still offloaded for recovery).
		b.WriteString("\n")
		b.WriteString(original)
		return b.String()
	}

	b.WriteString("\n--- first 50 lines ---\n")
	b.WriteString(strings.Join(lines[:headLimit], "\n"))
	fmt.Fprintf(&b, "\n\n... (%d lines omitted) ...\n\n", totalLines-headLimit-tailLimit)
	b.WriteString("--- last 50 lines ---\n")
	b.WriteString(strings.Join(lines[totalLines-tailLimit:], "\n"))

	return b.String()
}

func newToolCallEvent(call models.ToolCall, result *models.ToolResult) *ToolCallEvent {
	event := &ToolCallEvent{
		ID:            call.ID,
		Name:          call.Name,
		Arguments:     cloneArguments(call.Arguments),
		ArgumentsText: formatToolArguments(call.Arguments),
		Status:        call.Status,
		RequestedAt:   formatEventTime(call.RequestedAt),
		StartedAt:     formatEventTime(call.StartedAt),
		CompletedAt:   formatEventTime(call.CompletedAt),
	}
	if result != nil {
		event.Result = cloneToolResult(result)
		event.ResultPreview = toolResultPreview(*result)
		event.Error = result.Error
		event.DurationMS = result.Duration.Milliseconds()
		if event.Status == "" {
			event.Status = result.Status
		}
		if event.CompletedAt == "" {
			event.CompletedAt = formatEventTime(result.CompletedAt)
		}
	}
	return event
}

func newToolEventFromResult(call models.ToolCall, result models.ToolResult) *ToolCallEvent {
	return newToolCallEvent(call, &result)
}

func cloneArguments(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	return out
}

func cloneToolResult(result *models.ToolResult) *models.ToolResult {
	if result == nil {
		return nil
	}
	copyResult := *result
	if len(result.Data) > 0 {
		copyResult.Data = make(map[string]any, len(result.Data))
		for k, v := range result.Data {
			copyResult.Data[k] = v
		}
	}
	return &copyResult
}

func formatToolArguments(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func toolResultPreview(result models.ToolResult) string {
	content := strings.TrimSpace(result.Content)
	if content == "" {
		content = strings.TrimSpace(result.Error)
	}
	if content == "" && len(result.Data) > 0 {
		raw, err := json.Marshal(result.Data)
		if err == nil {
			content = string(raw)
		}
	}
	content = strings.ReplaceAll(content, "\n", " ")
	if len(content) > 240 {
		return content[:240] + "..."
	}
	return content
}

func formatEventTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

// runOneTool executes a single tool call with the standard sandbox/thread/UI
// context plumbing and normalizes errors into a Failed ToolResult so callers
// can treat success and failure uniformly.
func (a *Agent) runOneTool(ctx context.Context, sessionID string, call models.ToolCall) models.ToolResult {
	toolStarted := time.Now().UTC()
	toolCtx := tools.WithSandbox(ctx, a.sandbox)
	toolCtx = tools.WithThreadID(toolCtx, sessionID)
	// Window-aware handlers (code_map include_content) size their output
	// budget against the actual model window instead of a global constant —
	// a 1M-window model can afford a far larger pull than a 128k one.
	if a.contextWindow > 0 {
		toolCtx = tools.WithContextWindow(toolCtx, a.contextWindow)
	}
	if a.userInteraction != nil {
		toolCtx = tools.WithUserInteraction(toolCtx, a.userInteraction)
	}
	result, err := a.tools.Execute(toolCtx, call)
	if err != nil {
		err = normalizeRunError(ctx, err, a.requestTimeout)
		// H1: preserve the tool's own result.Data (e.g. the task tool sets
		// Data["subagent_usage"] on its timed-out/cancelled/failed branches
		// before returning an error) instead of discarding it along with the
		// rest of the original result — otherwise addSubagentUsage never
		// sees a failed subagent's token consumption.
		result = models.ToolResult{
			CallID:      call.ID,
			ToolName:    call.Name,
			Status:      models.CallStatusFailed,
			Error:       err.Error(),
			Data:        result.Data,
			CompletedAt: time.Now().UTC(),
		}
	}
	result.Duration = time.Since(toolStarted)
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now().UTC()
	}
	return result
}

// toolBatchState holds the per-batch bookkeeping that Run()'s parallel and
// serial tool-execution paths used to duplicate: usage rollup, offload,
// message-history append, Phase-0 metrics, AgentEventToolResult/
// ToolCallEnd emission, and circuit-breaker observation. handleResult
// applies all of it, in the exact order both paths relied on: addSubagentUsage
// → offloadIfNeeded → appendToolResultMessage → metrics record →
// AgentEventToolResult/ToolCallEnd emission → breaker.observe. One
// implementation, two call sites — mirroring the "one implementation, two
// call sites" toolCallBreaker.observe's own doc comment already promises for
// just the breaker slice of this.
//
// Path-specific bookkeeping stays at the call site and is NOT covered here:
// parallel's upfront start-event emission, wg/results indexing, and cap-
// refusal pre-fill; serial's inline execution, per-call ctx.Err check, and
// the skill Data["system_prompt"] special-case. Those differ enough between
// the two paths (goroutine fan-out vs. inline sequential execution) that
// folding them into this struct would cost the simpler path the complexity
// of the other for no shared benefit.
type toolBatchState struct {
	a         *Agent
	sessionID string
	turn      int
	breaker   *toolCallBreaker
	usage     *Usage
	emit      func(AgentEvent)

	// runMessages is the batch's working copy of Run's canonical message
	// history. Call sites read it back (b.runMessages) after the fatal path
	// and after the whole batch to keep their own local variable in sync —
	// it is not returned per-call because handleResult's callers also need
	// it for other purposes within the loop (e.g. indexing the
	// just-appended message for its ID).
	runMessages []models.Message

	// batchClean tracks whether every call in the batch passed validation;
	// finishBatch only resets the breaker's global consecutive-failure
	// counter if this stayed true for the whole batch.
	batchClean bool

	// pendingHints accumulates breaker hint messages during the batch; they
	// are appended to runMessages only after the batch's last tool result
	// (via finishBatch, or flushPendingHints on a fatal/early-return path)
	// so a hint (RoleHuman) never lands between the assistant tool_calls
	// message that started this batch and any of its tool results (M1-7).
	pendingHints []models.Message
}

// newToolBatchState starts a new batch's bookkeeping, seeded with the
// canonical runMessages as of the top of this batch (a.k.a. Run's local
// runMessages variable at the point the batch begins).
func newToolBatchState(a *Agent, sessionID string, turn int, breaker *toolCallBreaker, usage *Usage, emit func(AgentEvent), runMessages []models.Message) *toolBatchState {
	return &toolBatchState{
		a:           a,
		sessionID:   sessionID,
		turn:        turn,
		breaker:     breaker,
		usage:       usage,
		emit:        emit,
		runMessages: runMessages,
		batchClean:  true,
	}
}

// handleResult applies the shared per-result bookkeeping for one (call,
// result) pair. runningCall is the Status=Running snapshot previously
// emitted for this call (parallel: runningCalls[i]; serial: runningCall);
// handleResult derives the ToolCallEnd "completed" call from it.
//
// By the time this returns, the observation's hints are already folded into
// b.pendingHints and b.batchClean is already updated — callers only need to
// react to the returned obs.fatalErr. Fatal-path choreography (append
// whatever remaining tool_results the rest of the batch needs, THEN flush
// pendingHints, THEN emit+return the fatal error) is deliberately left to
// the caller via appendRemaining/appendSynthesizedFailures + flushPendingHints
// below: the parallel path already has computed results for the rest of the
// batch (its goroutines ran before the observation loop even starts), the
// serial path has none and must synthesize placeholders, and unifying those
// two shapes at this layer would cost the cheaper path the complexity of the
// more expensive one for no benefit.
func (b *toolBatchState) handleResult(call models.ToolCall, result models.ToolResult, runningCall models.ToolCall) breakerObservation {
	addSubagentUsage(b.usage, result)
	offloaded := b.a.offloadIfNeeded(&result, b.a.offloadDir)
	b.runMessages = appendToolResultMessage(b.runMessages, b.sessionID, result)
	if b.a.metrics != nil {
		// M1.2: Enhanced metrics collection
		argsHash := computeArgsHash(call.Arguments)
		filePath := extractPathFromArgs(result.ToolName, call.Arguments)
		durationMs := result.Duration.Milliseconds()

		b.a.metrics.RecordToolResult(ToolResultMetric{
			Turn:        b.turn,
			ToolName:    result.ToolName,
			ResultBytes: len(result.Content),
			ArgsHash:    argsHash,
			Path:        filePath,
			Offloaded:   offloaded,
			DurationMs:  durationMs,
		})
	}
	toolMessage := b.runMessages[len(b.runMessages)-1]
	b.emit(AgentEvent{
		Type:      AgentEventToolResult,
		MessageID: toolMessage.ID,
		Result:    &result,
		ToolEvent: newToolEventFromResult(call, result),
	})
	completed := runningCall
	completed.Status = result.Status
	completed.CompletedAt = result.CompletedAt
	b.emit(AgentEvent{
		Type:      AgentEventToolCallEnd,
		MessageID: toolMessage.ID,
		ToolCall:  &completed,
		Result:    &result,
		ToolEvent: newToolEventFromResult(completed, result),
	})

	// Edited-file attribution for the adversarial-review gate — like the
	// breaker below, both execution paths feed every executed pair through
	// this one site (fatal parallel tails go through appendRemaining's
	// mirror call).
	b.a.recordEditedFile(call, result)

	// Circuit-breaker bookkeeping — one implementation, both call sites feed
	// every (call, result) pair through in batch order (see
	// toolCallBreaker.observe for the combined repeat-call/validation logic).
	obs := b.breaker.observe(b.sessionID, call, result)
	if len(obs.hintMessages) > 0 {
		b.pendingHints = append(b.pendingHints, obs.hintMessages...)
	}
	if obs.validationFailure {
		b.batchClean = false
	}
	return obs
}

// appendRemaining appends already-computed results for the rest of a fatal
// PARALLEL batch: their tool.Execute calls already ran on other goroutines
// before the observation loop began (the whole batch runs concurrently
// before per-result observation starts), so real results exist and only
// need the usage-rollup + offload treatment handleResult gives normal
// results. Metrics/events are deliberately skipped for these, matching the
// original inline tail loop's documented scope: only the tool_result
// pairing invariant (every tool_use ID on the batch's assistant message
// needs a matching tool_result) is required for correctness here — plus
// edited-file attribution, because these edits really executed and the
// review gate must not under-report them. calls[i] pairs with results[i].
func (b *toolBatchState) appendRemaining(calls []models.ToolCall, results []models.ToolResult) {
	for i := range results {
		addSubagentUsage(b.usage, results[i])
		b.a.offloadIfNeeded(&results[i], b.a.offloadDir)
		if i < len(calls) {
			b.a.recordEditedFile(calls[i], results[i])
		}
		b.runMessages = appendToolResultMessage(b.runMessages, b.sessionID, results[i])
	}
}

// appendSynthesizedRefusals appends synthesized "not executed" placeholder
// results for calls that were never dispatched (circuit-breaker abort,
// budget-exhausted wrap-up refusal). One implementation for every refusal
// site so the tool_use/tool_result pairing invariant lives in one place; the
// per-site reason string is the only difference.
func appendSynthesizedRefusals(runMessages []models.Message, sessionID string, calls []models.ToolCall, reason string) []models.Message {
	for _, call := range calls {
		synthetic := models.ToolResult{
			CallID:      call.ID,
			ToolName:    call.Name,
			Status:      models.CallStatusFailed,
			Error:       reason,
			CompletedAt: time.Now().UTC(),
		}
		runMessages = appendToolResultMessage(runMessages, sessionID, synthetic)
	}
	return runMessages
}

// appendSynthesizedFailures appends synthesized "not executed" placeholder
// results for the rest of a fatal SERIAL batch. Unlike appendRemaining, the
// serial path never executed these calls, so there are no real results —
// only a placeholder is needed to keep every tool_use ID paired with a
// tool_result.
func (b *toolBatchState) appendSynthesizedFailures(remaining []models.ToolCall) {
	b.runMessages = appendSynthesizedRefusals(b.runMessages, b.sessionID, remaining,
		"not executed: batch aborted by circuit breaker")
}

// flushPendingHints appends any breaker hints accumulated so far to
// runMessages. Call ONLY after appendRemaining/appendSynthesizedFailures on
// a fatal or other early-return path (never before them) — a hint
// (RoleHuman) must never land between the assistant tool_calls message that
// started this batch and any of its tool results (M1-7).
//
// Idempotent by construction: pendingHints is cleared once appended, so a
// second call (e.g. a future early-return path that flushes and then still
// falls through to finishBatch) is a no-op instead of duplicating every hint
// message in runMessages.
func (b *toolBatchState) flushPendingHints() {
	if len(b.pendingHints) > 0 {
		b.runMessages = append(b.runMessages, b.pendingHints...)
		b.pendingHints = nil
	}
}

// finishBatch flushes pending hints and, if every call in the batch passed
// validation, resets the breaker's global consecutive-validation-failure
// counter. Call only on the clean end-of-batch path — the fatal path has
// its own append-then-flush choreography via flushPendingHints above.
func (b *toolBatchState) finishBatch() {
	b.flushPendingHints()
	if b.batchClean {
		b.breaker.resetOnCleanBatch()
	}
}

// toolSegment is a half-open run [start,end) of a batch that executes as a
// unit: concurrently when parallel, otherwise one call at a time.
type toolSegment struct {
	start, end int
	parallel   bool
}

// callParallelSafe fails closed: an unregistered tool is never assumed safe
// to run alongside anything else.
func (a *Agent) callParallelSafe(c models.ToolCall) bool {
	if a.tools == nil {
		return false
	}
	t := a.tools.Get(c.Name)
	return t != nil && t.ParallelSafe
}

// partitionToolCalls splits a batch into maximal runs of CONSECUTIVE
// parallel-safe calls, with every unsafe call standing alone.
//
// Requiring the whole batch to be parallel-safe meant one
// bash call alongside four task calls ran all five strictly sequentially —
// four subagents serialized by an unrelated command in the same turn.
// Partitioning restores concurrency where it is safe while preserving the
// batch's relative order exactly: segments run in order, so a call never
// moves ahead of or behind a neighbour it might depend on. Claude Code
// partitions identically (services/tools/toolOrchestration.ts,
// partitionToolCalls: "a single non-read-only tool, or multiple consecutive
// read-only tools").
func (a *Agent) partitionToolCalls(calls []models.ToolCall) []toolSegment {
	var segs []toolSegment
	for i, c := range calls {
		safe := a.callParallelSafe(c)
		if safe && len(segs) > 0 && segs[len(segs)-1].parallel && segs[len(segs)-1].end == i {
			segs[len(segs)-1].end = i + 1
			continue
		}
		segs = append(segs, toolSegment{start: i, end: i + 1, parallel: safe})
	}
	return segs
}
