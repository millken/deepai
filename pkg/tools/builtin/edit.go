package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// EditFileHandler replaces an exact substring in a file. To make it tolerant
// of common AI failure modes (tab vs space, CRLF vs LF, collapsed whitespace
// runs, pasted-back line numbers), the handler retries with normalized
// matching when the literal match fails. Optional start_line/end_line confine
// the search — and therefore the uniqueness check and replace_all — to a line
// window, which is the cheap way to disambiguate short repeated snippets.
func EditFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	if strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	if oldStr == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string is required")
	}
	if newStr == oldStr {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string and new_string are identical")
	}

	displayPath := strings.TrimSpace(path)
	path = resolveWritablePath(ctx, path)
	replaceAll, _ := args["replace_all"].(bool)

	startLine, hasStart, err := optionalLineArg(args, "start_line")
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}
	endLine, hasEnd, err := optionalLineArg(args, "end_line")
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read failed: %w", err)
	}
	content := string(data)

	// An optional line window scopes matching, so a short old_string that repeats
	// elsewhere in the file still resolves uniquely without replace_all.
	winStart, winEnd := 0, len(content)
	var firstLine, lastLine int
	if hasStart || hasEnd {
		winStart, winEnd, firstLine, lastLine, err = lineWindow(content, startLine, endLine, displayPath)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
		}
	}
	region := content[winStart:winEnd]
	// A range covering the whole file is not a window: there is no "outside".
	windowed := winStart > 0 || winEnd < len(content)
	// Match counts reported to the model are region-scoped, so name the region.
	// The bounds come from lineWindow rather than a newline recount, which
	// undercounts the final line of a file that does not end in a newline.
	scope := displayPath
	if windowed {
		scope = fmt.Sprintf("lines %d-%d of %s", firstLine, lastLine, displayPath)
	}

	type candidate struct {
		oldS, newS string
		note       string
	}
	candidates := []candidate{{oldStr, newStr, ""}}
	if uOld, uNew := unescapeLiteral(oldStr), unescapeLiteral(newStr); uOld != oldStr && uOld != uNew {
		candidates = append(candidates, candidate{uOld, uNew, "escape-normalized"})
	}
	// read_file renders ranges and outlines as "<lineno>\t<content>"; models
	// routinely paste that back verbatim. Try again with the prefixes removed,
	// but only after literal matching failed, so genuine tab-separated data is
	// never rewritten by this path.
	if sOld, ok := stripLineNumberPrefixes(oldStr); ok && sOld != oldStr {
		sNew, newStripped := stripLineNumberPrefixes(newStr)
		// If new_string carries prefixes but does not strip cleanly — deleting a
		// line makes its numbering jump, which is the common case — there is no
		// safe replacement text. Using it as-is would write the visible line
		// numbers into the file while old_string matched the real text, i.e.
		// silent corruption reported as success. Skip the candidate and let the
		// edit fail instead.
		if newStripped || !looksLineNumbered(newStr) {
			if sOld != sNew {
				candidates = append(candidates, candidate{sOld, sNew, "line-number prefixes stripped"})
			}
		}
	}

	for _, c := range candidates {
		updatedRegion, n, offset, kind, err := applyEdit(region, c.oldS, c.newS, replaceAll, scope)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
		}
		if n == 0 {
			continue
		}
		updated := content[:winStart] + updatedRegion + content[winEnd:]
		offset += winStart
		if writeErr := os.WriteFile(path, []byte(updated), filePerm(path, 0644)); writeErr != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("write failed: %w", writeErr)
		}
		notes := []string{}
		if kind != "" {
			notes = append(notes, kind)
		}
		if c.note != "" {
			notes = append(notes, c.note)
		}
		msg := fmt.Sprintf("Replaced %d occurrence(s) in %s", n, displayPath)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, ", ") + ")"
		}
		// start_line lets the TUI render the diff with real file line numbers
		// (1-based, in original-file coordinates at the first replacement).
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  msg,
			Data:     map[string]any{"start_line": 1 + strings.Count(content[:offset], "\n")},
		}, nil
	}

	// A windowed miss whose text sits elsewhere in the file is a range mistake,
	// not a quoting mistake. Say where it really is, or the model just retries
	// the same range.
	if windowed {
		for _, c := range candidates {
			if idx := strings.Index(content, c.oldS); idx >= 0 {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf(
					"old_string not found within %s, but it matches at line %d; move or widen the range, or drop start_line/end_line to search the whole file",
					scope, 1+strings.Count(content[:idx], "\n"),
				)
			}
		}
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf(
		"old_string not found in %s; send the file's own text: drop the line number prefix that read_file adds (\"12<TAB>\") or grep adds (\"file.go:12: \"), and use real newlines and tabs (not escaped \\n/\\t), then retry edit_file",
		displayPath,
	)
}

// optionalLineArg reads a 1-based line argument that may arrive as a JSON
// number or a stringified number. A present-but-unparseable value is an error
// rather than a silent fallback: ignoring it would widen a scoped edit into a
// whole-file edit.
func optionalLineArg(args map[string]any, key string) (int, bool, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case float64:
		return int(v), true, nil
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("%s must be a line number, got %q", key, v.String())
		}
		return int(n), true, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, false, fmt.Errorf("%s must be a line number, got %q", key, v)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("%s must be a line number, got %T", key, raw)
	}
}

// lineWindow converts a 1-based inclusive line range into a byte span of
// content, returning the span plus the resolved first/last line numbers so
// callers can describe the window without recounting newlines. The span covers
// whole lines, including line end's terminating newline. start<=0 means "from
// line 1"; end<=0 means "through EOF". Reversed bounds are swapped and an
// over-long end is clamped, matching read_file.
func lineWindow(content string, start, end int, displayPath string) (from, to, firstLine, lastLine int, err error) {
	starts := lineStartOffsets(content)
	total := len(starts)
	if total == 0 {
		return 0, 0, 0, 0, fmt.Errorf("%s is empty; drop start_line/end_line", displayPath)
	}
	if start > 0 && end > 0 && start > end {
		start, end = end, start
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if start > total {
		return 0, 0, 0, 0, fmt.Errorf("start_line %d is past the end of %s (%d lines)", start, displayPath, total)
	}

	from = starts[start-1]
	to = len(content)
	if end < total {
		to = starts[end]
	}
	return from, to, start, end, nil
}

// lineStartOffsets returns the byte offset of each line's first character. A
// trailing newline does not open a new line, so "a\nb\n" yields two entries.
func lineStartOffsets(content string) []int {
	if content == "" {
		return nil
	}
	offsets := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && i+1 < len(content) {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// stripLineNumberPrefixes removes a leading "<optional spaces><digits><TAB>"
// from every line of s, reporting whether s is a read_file transcript at all.
// Every line must carry a prefix and the numbers must ascend by one, so real
// tab-separated data (whose first column rarely counts consecutively across the
// exact span being edited) is left alone.
func stripLineNumberPrefixes(s string) (string, bool) {
	if s == "" {
		return s, false
	}
	lines := strings.Split(s, "\n")
	trailingNewline := false
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		trailingNewline = true
	}
	if len(lines) == 0 {
		return s, false
	}

	out := make([]string, len(lines))
	prev := 0
	for i, ln := range lines {
		body := strings.TrimSuffix(ln, "\r")
		hadCR := body != ln
		n, rest, ok := splitLineNumberPrefix(body)
		if !ok || (i > 0 && n != prev+1) {
			return s, false
		}
		prev = n
		if hadCR {
			rest += "\r"
		}
		out[i] = rest
	}

	result := strings.Join(out, "\n")
	if trailingNewline {
		result += "\n"
	}
	return result, true
}

// looksLineNumbered reports whether any line of s carries a read_file-style
// "<spaces><digits><TAB>" prefix. Used to tell "plain replacement text" apart
// from "a transcript whose numbering did not parse", which must never be
// written to a file verbatim.
func looksLineNumbered(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if _, _, ok := splitLineNumberPrefix(strings.TrimSuffix(line, "\r")); ok {
			return true
		}
	}
	return false
}

// splitLineNumberPrefix parses "<spaces><digits><TAB><rest>", returning the
// parsed number and the remainder. The TAB is required: a space separator would
// make ordinary numbered prose ("1. step") look like a transcript.
func splitLineNumberPrefix(line string) (num int, rest string, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	start := i
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		num = num*10 + int(line[i]-'0')
		i++
	}
	if i == start || i >= len(line) || line[i] != '\t' {
		return 0, "", false
	}
	return num, line[i+1:], true
}

// applyEdit returns the rewritten content, the number of replacements, the
// byte offset of the first replacement (for line-number reporting), an optional
// match-kind note, and any error.
func applyEdit(content, oldS, newS string, replaceAll bool, displayPath string) (updated string, n int, offset int, kind string, err error) {
	if count := strings.Count(content, oldS); count > 0 {
		if !replaceAll && count > 1 {
			return "", 0, 0, "", fmt.Errorf(
				"old_string matches %d times in %s; provide more context to make it unique, or set replace_all=true",
				count, displayPath,
			)
		}
		if replaceAll {
			updated = strings.ReplaceAll(content, oldS, newS)
		} else {
			updated = strings.Replace(content, oldS, newS, 1)
		}
		return updated, count, strings.Index(content, oldS), "", nil
	}

	normOld := normalizeWhitespace(oldS)
	if len(strings.TrimSpace(normOld)) >= 8 {
		spans := findWhitespaceTolerantSpans(content, oldS)
		if len(spans) > 0 {
			if !replaceAll && len(spans) > 1 {
				return "", 0, 0, "", fmt.Errorf(
					"old_string matches %d locations in %s after whitespace normalization; provide more context or set replace_all=true",
					len(spans), displayPath,
				)
			}
			conformed := conformLineEndings(newS, content)
			updated = replaceSpans(content, spans, conformed, replaceAll)
			count := 1
			if replaceAll {
				count = len(spans)
			}
			return updated, count, spans[0][0], "whitespace-tolerant match", nil
		}
	}

	return "", 0, 0, "", nil
}

func unescapeLiteral(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func conformLineEndings(s, content string) string {
	if !strings.Contains(content, "\r\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// normalizeWhitespace collapses CRLF/CR to LF and runs of horizontal
// whitespace (space, tab) to a single space. Used only for matching.
func normalizeWhitespace(s string) string {
	out, _ := normalizeWithIndex(s)
	return out
}

// normalizeWithIndex returns the normalized byte string and a slice mapping
// each normalized byte position back to its source byte offset in s.
func normalizeWithIndex(s string) (string, []int) {
	nb := make([]byte, 0, len(s))
	origIdx := make([]int, 0, len(s))
	prevWS := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				continue // skip the CR; the LF will be appended next loop
			}
			nb = append(nb, '\n')
			origIdx = append(origIdx, i)
			prevWS = false
			continue
		}
		if c == ' ' || c == '\t' {
			if !prevWS {
				nb = append(nb, ' ')
				origIdx = append(origIdx, i)
				prevWS = true
			}
			continue
		}
		prevWS = false
		nb = append(nb, c)
		origIdx = append(origIdx, i)
	}
	return string(nb), origIdx
}

// findWhitespaceTolerantSpans returns non-overlapping byte spans in content
// whose whitespace-normalized form equals normalizeWhitespace(needle).
func findWhitespaceTolerantSpans(content, needle string) [][2]int {
	target := normalizeWhitespace(needle)
	if target == "" {
		return nil
	}
	norm, origIdx := normalizeWithIndex(content)
	var spans [][2]int
	for i := 0; i+len(target) <= len(norm); {
		if norm[i:i+len(target)] == target {
			start := origIdx[i]
			end := origIdx[i+len(target)-1] + 1
			// If the matched normalized span ends with our collapsed-space
			// marker, extend `end` over any trailing original whitespace bytes
			// so the replacement consumes them too.
			if target[len(target)-1] == ' ' {
				for end < len(content) && (content[end] == ' ' || content[end] == '\t') {
					end++
				}
			}
			spans = append(spans, [2]int{start, end})
			i += len(target)
			continue
		}
		i++
	}
	return spans
}

// replaceSpans applies newStr to each given byte span in content. When
// replaceAll is false, only the first span is replaced.
func replaceSpans(content string, spans [][2]int, newStr string, replaceAll bool) string {
	if len(spans) == 0 {
		return content
	}
	if !replaceAll {
		spans = spans[:1]
	}
	var b strings.Builder
	b.Grow(len(content) + len(spans)*len(newStr))
	cursor := 0
	for _, sp := range spans {
		if sp[0] < cursor {
			continue
		}
		b.WriteString(content[cursor:sp[0]])
		b.WriteString(newStr)
		cursor = sp[1]
	}
	b.WriteString(content[cursor:])
	return b.String()
}

// filePerm returns the existing file's mode bits, falling back to def when
// the file doesn't exist or cannot be stat-ed. Preserves executable bits and
// other permission information on rewrite.
func filePerm(path string, def os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return def
}

func EditFileTool() models.Tool {
	return models.Tool{
		Name: "edit_file",
		Description: "Replace exact text in a file for in-place edits. old_string must uniquely match (use replace_all for multiple matches) and must be the file's own text: " +
			"strip the line number + TAB prefix that read_file (ranges, line_numbers, outlines) and grep add before matching. " +
			"Optional start_line/end_line (1-based, inclusive) scope the search to that line window, so a short old_string that repeats elsewhere in the file still resolves uniquely without replace_all — prefer this over padding old_string with context. " +
			"Falls back to whitespace-tolerant matching (tab vs space, CRLF vs LF, collapsed runs) when literal match fails. " +
			"Fails safely if no match or ambiguous match; on failure, re-read the file with read_file (line_numbers=false for a clean span) and retry this tool.",
		Groups: []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to edit"},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to find (must be unique unless replace_all is set)"},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring a unique match (confined to start_line/end_line when set)"},
				"start_line":  map[string]any{"type": "number", "description": "1-based inclusive first line to search; restricts matching to this window"},
				"end_line":    map[string]any{"type": "number", "description": "1-based inclusive last line to search; pairs with start_line (defaults to EOF)"},
			},
			"required": []any{"path", "old_string", "new_string"},
		},
		Handler: EditFileHandler,
	}
}
