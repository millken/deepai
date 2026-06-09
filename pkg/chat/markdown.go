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
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(w),
			glamour.WithEmoji(),
		)
		if err != nil {
			return ""
		}
		m.mdRenderer = r
		m.mdWidth = w
	}
	out, err := m.mdRenderer.Render(s)
	if err != nil {
		return ""
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
