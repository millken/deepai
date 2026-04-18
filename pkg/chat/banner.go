package chat

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
)

// BannerInfo holds data displayed in the startup banner.
type BannerInfo struct {
	Version    string
	Provider   string
	Model      string
	ToolCount  int
	SkillCount int
	SkillNames []string
	SessionID  string
}

// RenderBanner prints the startup banner.
func RenderBanner(w io.Writer, info BannerInfo) {
	styles := DefaultStyles()

	width := 50
	border := strings.Repeat("─", width)

	fmt.Fprintln(w)
	fmt.Fprintln(w, styles.Separator.Render("  ┌"+border+"┐"))

	title := styles.Banner.Render("  deepai")
	if info.Version != "" {
		title += styles.BannerDim.Render(" v" + info.Version)
	}
	fmt.Fprintln(w, "  │"+padCenter(title, width)+"│")

	fmt.Fprintln(w, "  ├"+border+"┤")

	fmt.Fprintln(w, fmtLine("Provider", info.Provider, width))
	fmt.Fprintln(w, fmtLine("Model", info.Model, width))
	fmt.Fprintln(w, fmtLine("Session", info.SessionID, width))

	toolsLine := fmt.Sprintf("%d tools", info.ToolCount)
	if info.SkillCount > 0 {
		toolsLine += fmt.Sprintf(", %d skills", info.SkillCount)
		if len(info.SkillNames) > 0 {
			names := strings.Join(info.SkillNames, ", ")
			if len(names) > 30 {
				names = names[:30] + "..."
			}
			toolsLine += " (" + names + ")"
		}
	}
	fmt.Fprintln(w, fmtLine("Loaded", toolsLine, width))

	fmt.Fprintln(w, "  └"+border+"┘")
	fmt.Fprintln(w)

	helpLine := styles.Dim.Render("  Type your prompt, /help for commands, Ctrl+C to interrupt, Ctrl+D to exit")
	fmt.Fprintln(w, helpLine)
	fmt.Fprintln(w)
}

func fmtLine(label, value string, width int) string {
	l := "  " + label + ": "
	if value == "" {
		value = "-"
	}
	content := l + value
	if len(content) > width {
		content = content[:width-3] + "..."
	}
	return styles(l, value) + " │"
}

func padCenter(s string, width int) string {
	n := lipgloss.Width(s)
	if n >= width {
		return s
	}
	padding := width - n
	left := padding / 2
	right := padding - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

func styles(label, value string) string {
	s := DefaultStyles()
	return "  " + s.Dim.Render(label) + s.Assistant.Render(value)
}
