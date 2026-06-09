package chat

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"github.com/millken/deepai/pkg/models"
)

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

// toolArgsPreview builds a short string from tool call arguments.
func toolArgsPreview(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// Show the first string-type argument as preview.
	for _, key := range []string{"command", "path", "file_path", "pattern", "query", "description", "prompt"} {
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
