package chat

import (
	"fmt"
	"io"
	"strings"
	"time"

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
func (r *Renderer) RenderEvent(evt agent.AgentEvent) {
	switch evt.Type {
	case agent.AgentEventTextChunk, agent.AgentEventChunk:
		if evt.Text != "" {
			fmt.Fprint(r.out, evt.Text)
		}

	case agent.AgentEventToolCall, agent.AgentEventToolCallStart:
		r.renderToolStart(evt)

	case agent.AgentEventToolCallEnd:
		r.renderToolEnd(evt)

	case agent.AgentEventToolResult:
		r.renderToolResult(evt)

	case agent.AgentEventError:
		r.renderError(evt)

	case agent.AgentEventEnd:
		// End of stream — newline handled by caller.

	case agent.AgentEventCompact:
		r.renderCompact(evt)
	}
}

// TurnStart resets the turn timer and prints the assistant header.
func (r *Renderer) TurnStart(turn int) {
	r.turn = turn
	r.start = time.Now()
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
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		detail += " -> " + preview
	}
	if te.Error != "" {
		detail += " " + r.styles.Error.Render("ERROR: "+te.Error)
	}
	line := fmt.Sprintf("  [%s] done%s", te.Name, detail)
	fmt.Fprintln(r.out, r.styles.ToolResult.Render(line))
}

func (r *Renderer) renderToolResult(evt agent.AgentEvent) {
	if evt.Result == nil {
		return
	}
	content := evt.Result.Content
	if len(content) > 200 {
		content = content[:200] + "..."
	}
	fmt.Fprintln(r.out, r.styles.ToolResult.Render("  "+content))
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
				if len(s) > 80 {
					return s[:80] + "..."
				}
				return s
			}
		}
	}
	// Fallback: show first key.
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		return k + "=" + s
	}
	return ""
}
