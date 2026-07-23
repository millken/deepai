package chat

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

const (
	mdMinWidth = 40
	mdMaxWidth = 110
)

func (m *tuiModel) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	w := m.width - 2
	if w < mdMinWidth {
		w = mdMinWidth
	}
	if w > mdMaxWidth {
		w = mdMaxWidth
	}
	if m.mdRenderer == nil || m.mdWidth != w {
		// Use minimal ANSI styling to avoid terminal state corruption
		// - No emoji (can cause encoding issues)
		// - No background colors (reduce ANSI sequence complexity)
		// - Standard style with minimal overhead
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle("notty"), // Use no-style to avoid ANSI issues
			glamour.WithWordWrap(w),
		)
		if err != nil {
			return s // Fallback to plain text
		}
		m.mdRenderer = r
		m.mdWidth = w
	}
	out, err := m.mdRenderer.Render(s)
	if err != nil {
		return s // Fallback to plain text on error
	}
	return strings.Trim(out, "\n")
}

// tailLines returns the last n lines of s, prefixed with an ellipsis marker when
// earlier lines were dropped. Used to bound the live streaming region while the
// full message is rendered to scrollback on completion.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
