package chat

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

const maxDiffLines = 16

// renderToolDiff returns a colored +/- block for file-editing tools, derived
// from the tool-call arguments, or "" when the tool isn't an edit/write.
func (m *tuiModel) renderToolDiff(name string, args map[string]any) string {
	switch name {
	case "edit_file":
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		return m.diffBlock(diffSplit(oldStr), diffSplit(newStr))
	case "write_file":
		content, _ := args["content"].(string)
		return m.diffBlock(nil, diffSplit(content))
	}
	return ""
}

func diffSplit(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// diffBlock renders removed lines (red, "-") followed by added lines (green,
// "+"), indented under the tool result and capped at maxDiffLines.
func (m *tuiModel) diffBlock(oldLines, newLines []string) string {
	total := len(oldLines) + len(newLines)
	if total == 0 {
		return ""
	}
	var b strings.Builder
	shown := 0
	write := func(prefix, text string, del bool) {
		if shown >= maxDiffLines {
			return
		}
		if shown > 0 {
			b.WriteString("\n")
		}
		st := m.styles.DiffAdd
		if del {
			st = m.styles.DiffDel
		}
		line := "      " + prefix + " " + text
		if lipgloss.Width(line) > 120 {
			line = truncateWidth(line, 117) + "..."
		}
		b.WriteString(st.Render(line))
		shown++
	}
	for _, l := range oldLines {
		write("-", l, true)
	}
	for _, l := range newLines {
		write("+", l, false)
	}
	if more := total - shown; more > 0 {
		b.WriteString("\n")
		b.WriteString(m.styles.Dim.Render(fmt.Sprintf("      … (%d more lines)", more)))
	}
	return b.String()
}
