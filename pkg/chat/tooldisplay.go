package chat

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

const maxDiffLines = 16

// diffOp is one row of a line-level diff: kind is ' ' (context), '-' (removed),
// or '+' (added).
type diffOp struct {
	kind byte
	text string
}

// renderToolDiff returns a Claude-style change block — a "path  +A -R" header
// followed by a line-numbered, syntax-marked diff — for file-editing tools, or
// "" when the tool isn't an edit/write. data carries the tool result's side
// channel (e.g. "start_line" for real file line numbers).
func (m *tuiModel) renderToolDiff(name string, args, data map[string]any) string {
	path, _ := args["path"].(string)
	startLine := dataInt(data, "start_line")
	switch name {
	case "edit_file":
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		return m.diffBlock(path, lineDiff(diffSplit(oldStr), diffSplit(newStr)), startLine)
	case "write_file":
		newStr, _ := args["content"].(string)
		var ops []diffOp
		for _, l := range diffSplit(newStr) {
			ops = append(ops, diffOp{'+', l})
		}
		return m.diffBlock(path, ops, startLine)
	}
	return ""
}

func dataInt(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	switch v := data[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func diffSplit(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// maxLCSCells bounds the LCS matrix size (n*m). Beyond it, lineDiff falls back
// to a plain remove-all-then-add-all diff so a pathological large edit can't
// spike memory or freeze the Bubble Tea render goroutine. 1M cells ≈ 8 MB.
const maxLCSCells = 1_000_000

// lineDiff computes a line-level diff between old and new via LCS, so lines the
// model included only for context appear as unchanged rows rather than being
// duplicated as a remove + add pair.
func lineDiff(oldLines, newLines []string) []diffOp {
	n, mm := len(oldLines), len(newLines)
	if n > 0 && mm > 0 && n*mm > maxLCSCells {
		return fallbackDiff(oldLines, newLines)
	}
	// dp[i][j] = LCS length of oldLines[i:] and newLines[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, mm+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := mm - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < mm {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, diffOp{' ', oldLines[i]})
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', oldLines[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', newLines[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', oldLines[i]})
	}
	for ; j < mm; j++ {
		ops = append(ops, diffOp{'+', newLines[j]})
	}
	return ops
}

// fallbackDiff is the pre-LCS behavior: all old lines as removed, then all new
// lines as added. Used when the LCS matrix would exceed maxLCSCells.
func fallbackDiff(oldLines, newLines []string) []diffOp {
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for _, l := range oldLines {
		ops = append(ops, diffOp{'-', l})
	}
	for _, l := range newLines {
		ops = append(ops, diffOp{'+', l})
	}
	return ops
}

// diffBlock renders the change header and a line-numbered diff. Context rows are
// dim, removed rows red, added rows green; +/- rows get a full-width background
// bar. Capped at maxDiffLines with a "… (N more lines)" footer. startLine is the
// 1-based file line of the first row (0 = unknown → no line numbers).
func (m *tuiModel) diffBlock(path string, ops []diffOp, startLine int) string {
	if len(ops) == 0 {
		return ""
	}
	added, removed := 0, 0
	for _, op := range ops {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}

	var b strings.Builder
	b.WriteString(m.diffHeader(path, added, removed))

	// Body width for the full-width highlight bar on changed rows.
	bodyWidth := m.width - 6
	if bodyWidth < 40 || m.width == 0 {
		bodyWidth = 80
	}
	if bodyWidth > 160 {
		bodyWidth = 160
	}

	addBg := m.styles.DiffAdd.Background(lipgloss.Color("#14321B"))
	delBg := m.styles.DiffDel.Background(lipgloss.Color("#3A1A20"))

	oldNo, newNo := startLine, startLine
	shown := 0
	for _, op := range ops {
		if shown >= maxDiffLines {
			break
		}
		var num int
		switch op.kind {
		case ' ':
			num, oldNo, newNo = newNo, oldNo+1, newNo+1
		case '-':
			num, oldNo = oldNo, oldNo+1
		case '+':
			num, newNo = newNo, newNo+1
		}

		gutter := "    "
		if startLine > 0 {
			gutter = fmt.Sprintf("%4d", num)
		}
		text := op.text
		body := fmt.Sprintf("%s %c %s", gutter, op.kind, text)
		if w := lipgloss.Width(body); w > bodyWidth {
			body = truncateWidth(body, bodyWidth-1) + "…"
		}

		b.WriteString("\n      ")
		switch op.kind {
		case ' ':
			b.WriteString(m.styles.Dim.Render(body))
		case '-':
			b.WriteString(delBg.Render(padRightWidth(body, bodyWidth)))
		case '+':
			b.WriteString(addBg.Render(padRightWidth(body, bodyWidth)))
		}
		shown++
	}
	if more := len(ops) - shown; more > 0 {
		b.WriteString("\n")
		b.WriteString(m.styles.Dim.Render(fmt.Sprintf("      … (%d more lines)", more)))
	}
	return b.String()
}

// diffHeader renders the "path  +A -R" summary line.
func (m *tuiModel) diffHeader(path string, added, removed int) string {
	if path == "" {
		path = "(file)"
	}
	parts := []string{m.styles.Bold.Render("      " + path)}
	if added > 0 {
		parts = append(parts, m.styles.DiffAdd.Render(fmt.Sprintf("+%d", added)))
	}
	if removed > 0 {
		parts = append(parts, m.styles.DiffDel.Render(fmt.Sprintf("-%d", removed)))
	}
	return strings.Join(parts, "  ")
}

// padRightWidth pads s with spaces to the given display width (no-op if already
// at or over width). Used so the diff highlight bar spans the full row.
func padRightWidth(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
