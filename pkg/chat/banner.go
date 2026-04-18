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

	innerWidth := 50
	border := strings.Repeat("─", innerWidth)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ┌"+border+"┐")

	title := styles.Banner.Render("deepai")
	if info.Version != "" {
		title += styles.BannerDim.Render(" v" + info.Version)
	}
	fmt.Fprintln(w, "  │"+padCenter(title, innerWidth)+"│")

	fmt.Fprintln(w, "  ├"+border+"┤")

	fmt.Fprintln(w, boxLine("Provider", info.Provider, innerWidth))
	fmt.Fprintln(w, boxLine("Model", info.Model, innerWidth))
	fmt.Fprintln(w, boxLine("Session", info.SessionID, innerWidth))

	toolsLine := fmt.Sprintf("%d tools", info.ToolCount)
	if info.SkillCount > 0 {
		toolsLine += fmt.Sprintf(", %d skills", info.SkillCount)
		if len(info.SkillNames) > 0 {
			names := strings.Join(info.SkillNames, ", ")
			if lipgloss.Width(names) > 30 {
				names = truncateWidth(names, 27) + "..."
			}
			toolsLine += " (" + names + ")"
		}
	}
	fmt.Fprintln(w, boxLine("Loaded", toolsLine, innerWidth))

	fmt.Fprintln(w, "  └"+border+"┘")
	fmt.Fprintln(w)

	helpLine := styles.Dim.Render("  Type your prompt, /help for commands, Ctrl+C to interrupt, Ctrl+D to exit")
	fmt.Fprintln(w, helpLine)
	fmt.Fprintln(w)
}

// boxLine renders a label: value line inside the box, e.g. "  │ Provider: openai    │".
func boxLine(label, value string, innerWidth int) string {
	if value == "" {
		value = "-"
	}
	s := DefaultStyles()
	labelPart := " " + label + ": "
	labelW := lipgloss.Width(labelPart)
	remaining := innerWidth - labelW
	if remaining < 3 {
		remaining = 3
	}
	if lipgloss.Width(value) > remaining {
		value = truncateWidth(value, remaining-3) + "..."
	}
	content := s.Dim.Render(labelPart) + s.Assistant.Render(value)
	// Pad to fill inner width.
	if w := lipgloss.Width(content); w < innerWidth {
		content += strings.Repeat(" ", innerWidth-w)
	}
	return "  │" + content + "│"
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
