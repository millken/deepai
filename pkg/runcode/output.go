package runcode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// Rendering budgets (§6.2/§6.3).
const (
	writesLineMaxBytes = 600 // single-line writes: cap so the offload tail preview keeps it whole
	failureTextMax     = 16 << 10
	resultPreviewMax   = 2 << 10 // return-value preview inside the rendered text
)

// Render builds the run_code ToolResult Content/Error from a RunOutput (§6.3):
//
//	run_code ok — 9 tool calls in 2.4s (grep×3, read_file×4, …)
//	result:
//	{…}
//	logs:
//	…
//	writes: edit_file a.go [completed]; …
//
// The writes: line is LAST and single-line ≤600B because the offload preview
// keeps only the tail 15 lines/800B (toolexec.go offloadPreview) — in exactly
// the big-result case where the summary matters most, this line must survive.
// Handler contract (§6.2): never return err — Error carries the same full
// text as Content on failure (toolMessageContent shows Error INSTEAD OF
// Content, and runOneTool discards Content on err).
func Render(out RunOutput, script string) models.ToolResult {
	var b strings.Builder
	outcomeWord := string(out.Outcome)
	status := models.CallStatusCompleted
	if out.Outcome != OutcomeOK {
		status = models.CallStatusFailed
	}

	first := fmt.Sprintf("run_code %s — %d tool calls in %.1fs%s",
		outcomeWord, len(out.Calls), out.Elapsed.Seconds(), toolHistogram(out.Calls))
	b.WriteString(first)

	if out.Outcome == OutcomeOK {
		b.WriteString("\nresult:\n")
		b.WriteString(previewResult(out.Value))
	} else {
		b.WriteString("\nerror:\n")
		errText := out.Err
		if errText == "" {
			errText = outcomeText(out.Outcome)
		} else {
			errText = errText + "\n(" + outcomeText(out.Outcome) + ")"
		}
		b.WriteString(errText)
	}

	if out.Logs != "" {
		b.WriteString("\nlogs:\n")
		b.WriteString(strings.TrimRight(out.Logs, "\n"))
	}
	b.WriteString("\n")
	b.WriteString(writesLine(out.Calls))

	content := b.String()
	res := models.ToolResult{
		Status:  status,
		Content: content,
		Data:    resultData(out, script),
	}
	if status == models.CallStatusFailed {
		// Failure text never goes through offload (offload touches Content),
		// so cap it here: logs first, then the error preview (§6.2). The
		// writes: line is exempt (its own ≤600B cap governs).
		res.Content = capFailureText(content, writesLine(out.Calls))
		res.Error = res.Content
	}
	return res
}

func outcomeText(o Outcome) string {
	switch o {
	case OutcomeTimeout:
		return "script timed out"
	case OutcomeCallLimit:
		return "tool call limit exceeded"
	case OutcomeMemory:
		return "script exceeded the memory limit"
	case OutcomeCancelled:
		return "cancelled"
	default:
		return "script failed"
	}
}

func previewResult(v any) string {
	if v == nil {
		return "undefined"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > resultPreviewMax {
		return models.TruncateBytes(string(b), resultPreviewMax) + fmt.Sprintf("… [truncated: %d bytes total]", len(b))
	}
	return string(b)
}

func capFailureText(content, writes string) string {
	if len(content) <= failureTextMax {
		return content
	}
	head := models.TruncateBytes(content, failureTextMax-len(writes)-1)
	return head + "\n… [truncated]\n" + writes
}

// toolHistogram renders "(grep×3, read_file×4)" for the summary line.
func toolHistogram(calls []JournalEntry) string {
	if len(calls) == 0 {
		return ""
	}
	counts := map[string]int{}
	var order []string
	for _, c := range calls {
		if _, seen := counts[c.Tool]; !seen {
			order = append(order, c.Tool)
		}
		counts[c.Tool]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, t := range order {
		parts = append(parts, fmt.Sprintf("%s×%d", t, counts[t]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// writesLine renders every STARTED non-parallel-safe call with its status
// (§6.2/§6.3): "writes: edit_file a.go [completed]; bash \"go test\" [interrupted]".
// No writes at all renders "writes: none" so "nothing ran" stays
// distinguishable from "truncated away". Over-budget keeps the head and ends
// with "… +k more (N writes total)" — the count is always visible.
func writesLine(calls []JournalEntry) string {
	type witem struct {
		text   string
		status JournalStatus
	}
	var items []witem
	for _, c := range calls {
		if !c.Write {
			continue
		}
		items = append(items, witem{writeItemText(c), c.Status})
	}
	if len(items) == 0 {
		return "writes: none"
	}
	render := func(items []witem) string {
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = fmt.Sprintf("%s [%s]", it.text, it.status)
		}
		return "writes: " + strings.Join(parts, "; ")
	}
	total := len(items)
	shown := items
	for len(render(shown)) > writesLineMaxBytes && len(shown) > 1 {
		shown = shown[:len(shown)-1]
	}
	line := render(shown)
	if len(shown) < total {
		more := total - len(shown)
		suffix := fmt.Sprintf("… +%d more (%d writes total)", more, total)
		room := writesLineMaxBytes - len(suffix)
		if len(line) > room {
			line = models.TruncateBytes(line, room)
		}
		line += suffix
	}
	// A single over-budget item (one write with a huge path) must still fit:
	// the 600B cap governs the line unconditionally, else one log line can
	// push it out of the offload tail preview (review #6).
	if len(line) > writesLineMaxBytes {
		line = models.TruncateBytes(line, writesLineMaxBytes-len("…")) + "…"
	}
	return line
}

func writeItemText(c JournalEntry) string {
	switch c.Tool {
	case "edit_file", "write_file":
		p, _ := c.args["path"].(string)
		if p == "" {
			p, _ = c.args["file_path"].(string)
		}
		return c.Tool + " " + p
	default:
		cmd, _ := c.args["command"].(string)
		if cmd == "" {
			return c.Tool
		}
		return fmt.Sprintf("%s %q", c.Tool, models.TruncateBytes(cmd, 60))
	}
}

// resultData builds the structured Data (§6.3): script, journal, outcome,
// edited_paths (consumed by recordEditedFile — §4.5), byte counts.
func resultData(out RunOutput, script string) map[string]any {
	journal := make([]map[string]any, 0, len(out.Calls))
	for _, c := range out.Calls {
		journal = append(journal, map[string]any{
			"seq":          c.Seq,
			"tool":         c.Tool,
			"args_preview": c.ArgsPreview,
			"status":       string(c.Status),
			"ms":           c.MS,
			"bytes":        c.Bytes,
			"write":        c.Write,
		})
	}
	data := map[string]any{
		"script":  script,
		"journal": journal,
		"outcome": string(out.Outcome),
	}
	if edited := editedPaths(out.Calls); len(edited) > 0 {
		data["edited_paths"] = edited
	}
	if out.Value != nil {
		if b, err := json.Marshal(out.Value); err == nil {
			data["return_bytes"] = len(b)
		}
	}
	if out.Logs != "" {
		data["log_bytes"] = len(out.Logs)
	}
	return data
}
