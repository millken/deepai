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

// offloadIfNeeded checks if a tool result exceeds the offload threshold.
// If so, it writes the full content to a file under offloadDir and replaces
// result.Content with a compact reference (path + first/last 50 lines).
// Returns true if offload occurred. Errors during file write are logged but
// non-fatal — the result stays in-context (degraded but functional).
func (a *Agent) offloadIfNeeded(result *models.ToolResult, offloadDir string) bool {
	if offloadDir == "" || len(result.Content) <= offloadThresholdBytes {
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

// allParallelSafe reports whether every call in the batch resolves to a
// registered tool that has declared ParallelSafe=true. Unknown tools and any
// false flag short-circuit to false so the safe (sequential) path is taken.
func (a *Agent) allParallelSafe(calls []models.ToolCall) bool {
	if a.tools == nil {
		return false
	}
	for _, c := range calls {
		t := a.tools.Get(c.Name)
		if t == nil || !t.ParallelSafe {
			return false
		}
	}
	return true
}
