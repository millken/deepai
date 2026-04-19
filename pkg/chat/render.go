package chat

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"github.com/millken/deepai/pkg/agent"
)

// Renderer handles terminal output for agent events.
type Renderer struct {
	out    io.Writer
	styles Styles
	turn   int
	start  time.Time
}

// NewRenderer creates a renderer writing to w.
func NewRenderer(w io.Writer) *Renderer {
	return &Renderer{
		out:    w,
		styles: DefaultStyles(),
	}
}

// RenderEvent outputs a single agent event to the terminal.
// Only consumes the primary event type from each agent co-emission pair:
//   - text_chunk (not chunk)
//   - tool_call_start (not tool_call)
//   - tool_call_end (not tool_result)
func (r *Renderer) RenderEvent(evt agent.AgentEvent) {
	switch evt.Type {
	case agent.AgentEventTextChunk:
		if evt.Text != "" {
			fmt.Fprint(r.out, evt.Text)
		}

	case agent.AgentEventToolCallStart:
		r.renderToolStart(evt)

	case agent.AgentEventToolCallEnd:
		r.renderToolEnd(evt)

	case agent.AgentEventError:
		r.renderError(evt)

	case agent.AgentEventCompact:
		r.renderCompact(evt)

		// Ignored: AgentEventChunk, AgentEventToolCall, AgentEventToolResult,
		// AgentEventEnd — redundant duplicates or handled by caller.
	}
}

// TurnStart resets the turn timer and prints the assistant header.
func (r *Renderer) TurnStart(turn int) {
	r.turn = turn
	r.start = time.Now()
	fmt.Fprintln(r.out, r.styles.Dim.Render(fmt.Sprintf("  ── Assistant (turn %d) ──", turn)))
}

// TurnEnd prints usage statistics for the completed turn.
func (r *Renderer) TurnEnd(usage *agent.Usage) {
	fmt.Fprintln(r.out)
	elapsed := time.Since(r.start)

	var parts []string
	parts = append(parts, fmt.Sprintf("Turn %d", r.turn))
	if usage != nil {
		parts = append(parts, fmt.Sprintf("Tokens: %d in / %d out", usage.InputTokens, usage.OutputTokens))
	}
	parts = append(parts, fmt.Sprintf("%.1fs", elapsed.Seconds()))

	fmt.Fprintln(r.out, r.styles.Stats.Render("  "+strings.Join(parts, " | ")))
}

// RenderInterrupted shows an interruption message.
func (r *Renderer) RenderInterrupted() {
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, r.styles.Dim.Render("  Interrupted."))
}

func (r *Renderer) renderToolStart(evt agent.AgentEvent) {
	var name, preview string
	if evt.ToolEvent != nil {
		name = evt.ToolEvent.Name
		preview = toolArgsPreview(evt.ToolEvent.Arguments)
	} else if evt.ToolCall != nil {
		name = evt.ToolCall.Name
		preview = toolArgsPreview(evt.ToolCall.Arguments)
	}
	if name == "" {
		return
	}
	line := fmt.Sprintf("  [%s] %s", name, preview)
	fmt.Fprintln(r.out, r.styles.ToolCall.Render(line))
}

func (r *Renderer) renderToolEnd(evt agent.AgentEvent) {
	if evt.ToolEvent == nil {
		return
	}
	te := evt.ToolEvent
	var detail string
	if te.DurationMS > 0 {
		detail = fmt.Sprintf(" (%.1fs)", float64(te.DurationMS)/1000)
	}
	if te.ResultPreview != "" {
		preview := te.ResultPreview
		if lipgloss.Width(preview) > 120 {
			preview = truncateWidth(preview, 117) + "..."
		}
		detail += " -> " + preview
	}
	if te.Error != "" {
		detail += " " + r.styles.Error.Render("ERROR: "+te.Error)
	}
	line := fmt.Sprintf("  [%s] done%s", te.Name, detail)
	fmt.Fprintln(r.out, r.styles.ToolResult.Render(line))
}

func (r *Renderer) renderError(evt agent.AgentEvent) {
	msg := evt.Err
	if evt.Error != nil {
		msg = evt.Error.Message
	}
	fmt.Fprintln(r.out, r.styles.Error.Render("  Error: "+msg))
}

func (r *Renderer) renderCompact(evt agent.AgentEvent) {
	if evt.CompactStats == nil {
		return
	}
	cs := evt.CompactStats
	fmt.Fprintln(r.out, r.styles.Compaction.Render(
		fmt.Sprintf("  Context compacted: %d -> %d messages (%.0f%% of %d)",
			cs.MessagesBefore, cs.MessagesAfter, cs.Ratio*100, cs.ContextWindow)))
}

// toolArgsPreview builds a short string from tool call arguments.
func toolArgsPreview(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// Show the first string-type argument as preview.
	for _, key := range []string{"command", "description", "path", "query", "prompt"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				if lipgloss.Width(s) > 80 {
					return truncateWidth(s, 77) + "..."
				}
				return s
			}
		}
	}
	// Fallback: show first key.
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if lipgloss.Width(s) > 60 {
			s = truncateWidth(s, 57) + "..."
		}
		return k + "=" + s
	}
	return ""
}

// truncateWidth truncates a string to max display width (not bytes).
func truncateWidth(s string, maxW int) string {
	runes := []rune(s)
	w, start := 0, 0
	for _, r := range runes {
		rw := runewidth.RuneWidth(r)
		if w+rw > maxW {
			return string(runes[:start])
		}
		w += rw
		start++
	}
	return s
}

// RenderPipelineResult renders a pipeline result with color-coded severity.
func (r *Renderer) RenderPipelineResult(result *agent.OrchestratorResult) {
	if result == nil {
		return
	}

	verdictStyle := r.styles.ReviewPass
	verdictText := "PASS"
	if result.Verdict != "pass" {
		verdictStyle = r.styles.ReviewFail
		verdictText = "ISSUES FOUND"
	}
	fmt.Fprintf(r.out, "  Verdict: %s  (rounds=%d)\n", verdictStyle.Render(verdictText), result.Rounds)

	if len(result.Reviews) == 0 {
		return
	}
	fmt.Fprintln(r.out, "  Reviews:")
	for key, review := range result.Reviews {
		reviewVerdict := r.styles.ReviewPass.Render("pass")
		if review.Verdict != "pass" {
			reviewVerdict = r.styles.ReviewFail.Render(review.Verdict)
		}
		fmt.Fprintf(r.out, "    [%s] %s %s\n", key, reviewVerdict, r.styles.Dim.Render("- "+review.Summary))

		for _, issue := range review.Issues {
			var severityStr string
			switch issue.Severity {
			case "critical":
				severityStr = r.styles.SeverityCrit.Render("CRITICAL")
			case "warning":
				severityStr = r.styles.SeverityWarn.Render("WARNING")
			default:
				severityStr = r.styles.SeveritySugg.Render(strings.ToUpper(issue.Severity))
			}
			loc := ""
			if issue.File != "" {
				loc = issue.File
				if issue.Line > 0 {
					loc += fmt.Sprintf(":%d", issue.Line)
				}
			}
			msg := issue.Message
			if loc != "" {
				msg = loc + ": " + msg
			}
			fmt.Fprintf(r.out, "      %s %s\n", severityStr, msg)
			if issue.Suggestion != "" {
				fmt.Fprintf(r.out, "        %s %s\n", r.styles.Dim.Render("->"), issue.Suggestion)
			}
		}
	}
}
