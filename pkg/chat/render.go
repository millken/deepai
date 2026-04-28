package chat

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// pendingToolLine tracks an in-progress tool call whose start line has been printed.
type pendingToolLine struct {
	id string // tool call ID
}

// Renderer handles terminal output for agent events.
// All public methods are safe to call from multiple goroutines.
type Renderer struct {
	out               io.Writer
	styles            Styles
	turn              int
	start             time.Time
	thinking          bool
	mu                sync.Mutex
	pendingStartLines []pendingToolLine
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
	r.mu.Lock()
	defer r.mu.Unlock()
	switch evt.Type {
	case agent.AgentEventTextChunk:
		if evt.Text != "" {
			r.clearThinking()
			fmt.Fprint(r.out, evt.Text)
		}

	case agent.AgentEventToolCallStart:
		r.clearThinking()
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

// TurnStart prints the user message and assistant header, then shows a
// "Thinking..." indicator so the user knows the model is working.
func (r *Renderer) TurnStart(turn int, userInput string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn = turn
	r.start = time.Now()
	r.thinking = true

	// Show user message
	fmt.Fprintf(r.out, "%s %s\n", r.styles.UserPrompt.Render("  You:"), userInput)

	// Show thinking indicator
	fmt.Fprint(r.out, r.styles.Dim.Render("  Thinking..."))
}

// TurnEnd prints usage statistics for the completed turn.
func (r *Renderer) TurnEnd(usage *agent.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, r.styles.Dim.Render("  Interrupted."))
}

// RenderHeartbeat prints a lightweight progress hint when a turn is still
// running but has had no visible activity for a while.
func (r *Renderer) RenderHeartbeat(elapsed, idle time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearThinking()
	fmt.Fprintln(r.out, r.styles.Dim.Render(
		fmt.Sprintf("  Still running... %.0fs elapsed (last activity %.0fs ago). Press Ctrl+C to stop.",
			elapsed.Seconds(), idle.Seconds())))
}

// clearThinking replaces the "Thinking..." indicator with a newline on first output.
// Caller must hold r.mu.
func (r *Renderer) clearThinking() {
	if !r.thinking {
		return
	}
	r.thinking = false
	// Move cursor to beginning of "Thinking..." line, clear to end of line, reset cursor
	fmt.Fprint(r.out, "\r\033[K")
}

// RenderSubagentEvent displays subagent lifecycle events in the REPL.
// Called from the subagent pool goroutine; protected by r.mu.
func (r *Renderer) RenderSubagentEvent(evt subagent.TaskEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch evt.Type {
	case "task_started":
		desc := evt.Description
		if len([]rune(desc)) > 80 {
			desc = string([]rune(desc)[:77]) + "..."
		}
		fmt.Fprintln(r.out, r.styles.Dim.Render(fmt.Sprintf("  ↳ [subagent] %s", desc)))
	case "task_failed":
		errMsg := evt.Error
		if errMsg == "" {
			errMsg = evt.Message
		}
		fmt.Fprintln(r.out, r.styles.Error.Render(fmt.Sprintf("  ↳ [subagent] failed: %s", errMsg)))
	case "task_completed":
		// completion is shown by the tool_call_end event from the parent agent
	}
}

// PrintHistory renders the full conversation history to w.
func PrintHistory(w io.Writer, messages []models.Message) {
	styles := DefaultStyles()
	sep := styles.Dim.Render("  " + strings.Repeat("─", 60))

	turnNum := 0
	for i, msg := range messages {
		switch msg.Role {
		case models.RoleHuman:
			turnNum++
			if i > 0 {
				fmt.Fprintln(w, sep)
			}
			fmt.Fprintf(w, "%s %s\n", styles.UserPrompt.Render(fmt.Sprintf("  [%d] You:", turnNum)), msg.Content)
		case models.RoleAI:
			if msg.Content != "" {
				content := msg.Content
				if len(content) > 2000 {
					content = content[:2000] + "... [truncated]"
				}
				fmt.Fprintf(w, "%s %s\n", styles.Assistant.Render("  AI:"), content)
			}
			for _, tc := range msg.ToolCalls {
				preview := toolArgsPreview(tc.Arguments)
				if preview != "" {
					fmt.Fprintln(w, styles.ToolCall.Render(fmt.Sprintf("    ⚙ %s(%s)", tc.Name, preview)))
				} else {
					fmt.Fprintln(w, styles.ToolCall.Render(fmt.Sprintf("    ⚙ %s", tc.Name)))
				}
			}
		case models.RoleTool:
			// skip raw tool results in history view
		}
	}
	if len(messages) > 0 {
		fmt.Fprintln(w, sep)
	}
}

func (r *Renderer) renderToolStart(evt agent.AgentEvent) {
	var id, name, preview string
	if evt.ToolEvent != nil {
		id = evt.ToolEvent.ID
		name = evt.ToolEvent.Name
		preview = toolArgsPreview(evt.ToolEvent.Arguments)
	} else if evt.ToolCall != nil {
		id = evt.ToolCall.ID
		name = evt.ToolCall.Name
		preview = toolArgsPreview(evt.ToolCall.Arguments)
	}
	if name == "" {
		return
	}
	var line string
	if preview != "" {
		line = fmt.Sprintf("  ⚙ %s(%s)…", name, preview)
	} else {
		line = fmt.Sprintf("  ⚙ %s…", name)
	}
	fmt.Fprintln(r.out, r.styles.ToolCall.Render(line))
	r.pendingStartLines = append(r.pendingStartLines, pendingToolLine{id: id})
}

func (r *Renderer) renderToolEnd(evt agent.AgentEvent) {
	if evt.ToolEvent == nil {
		return
	}
	te := evt.ToolEvent

	// Build collapsed result line.
	icon := "✓"
	useErrorStyle := te.Error != ""
	if useErrorStyle {
		icon = "✗"
	}
	var detail string
	if te.DurationMS > 0 {
		detail = fmt.Sprintf(" (%.1fs)", float64(te.DurationMS)/1000)
	}
	if te.Error != "" {
		detail += " " + te.Error
	} else if te.ResultPreview != "" {
		preview := te.ResultPreview
		if lipgloss.Width(preview) > 120 {
			preview = truncateWidth(preview, 117) + "..."
		}
		detail += " → " + preview
	}
	line := fmt.Sprintf("  %s %s%s", icon, te.Name, detail)
	var rendered string
	if useErrorStyle {
		rendered = r.styles.Error.Render(line)
	} else {
		rendered = r.styles.ToolResult.Render(line)
	}

	// Find the matching pending start line by tool ID.
	idx := -1
	for i, p := range r.pendingStartLines {
		if p.id == te.ID {
			idx = i
			break
		}
	}

	if idx >= 0 {
		// linesAbove = how many lines up from the current cursor position
		// to the start line that was printed for this tool call.
		linesAbove := len(r.pendingStartLines) - idx
		// Move up, clear the start line, write result, then return to original position.
		fmt.Fprintf(r.out, "\033[%dA\r\033[K", linesAbove)
		fmt.Fprintln(r.out, rendered)
		if linesAbove > 1 {
			fmt.Fprintf(r.out, "\033[%dB", linesAbove-1)
		}
		r.pendingStartLines = append(r.pendingStartLines[:idx], r.pendingStartLines[idx+1:]...)
	} else {
		fmt.Fprintln(r.out, rendered)
	}
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
	line := fmt.Sprintf("  Context compacted: %d -> %d messages (%.0f%% of %d)",
		cs.MessagesBefore, cs.MessagesAfter, cs.Ratio*100, cs.ContextWindow)
	if cs.MessagesBefore == cs.MessagesAfter {
		line = fmt.Sprintf("  Context compacted: %d messages (content trimmed, %.0f%% of %d)",
			cs.MessagesAfter, cs.Ratio*100, cs.ContextWindow)
	}
	fmt.Fprintln(r.out, r.styles.Compaction.Render(line))
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
