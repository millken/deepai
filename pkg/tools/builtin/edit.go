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

// EditFileHandler replaces text in a file, in one of two modes. Hash mode
// (start_hash/end_hash copied from read_file's "N:hhhhhh" prefixes) replaces
// an inclusive line range without restating the old text — see hashline.go.
// old_string mode replaces an exact substring; to make it tolerant of common
// AI failure modes (tab vs space, CRLF vs LF, collapsed whitespace runs,
// pasted-back line numbers), the handler retries with normalized matching
// when the literal match fails. Optional start_line/end_line confine the
// search — and therefore the uniqueness check and replace_all — to a line
// window, which is the cheap way to disambiguate short repeated snippets.
func EditFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, _ := args["path"].(string)

	if strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}

	displayPath := strings.TrimSpace(path)
	path = resolveWritablePath(ctx, path)

	// Multi-hunk mode dispatches first (HASHLINE_EDIT_DESIGN §17.2 D5): each
	// hunk carries its own new_string, so the top-level key checks below do
	// not apply; every other top-level locator argument is ignored.
	if editsRaw, hasEdits := args["edits"]; hasEdits {
		return editByHunks(ctx, call, path, displayPath, editsRaw)
	}

	// A missing key is not an empty string: in hash mode "" means "delete the
	// range", so a model that forgot new_string must get an error, not a
	// silent deletion.
	newRaw, hasNew := args["new_string"]
	newStr, newIsString := newRaw.(string)
	if !hasNew || !newIsString {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("new_string is required (pass an empty string to delete the range)")
	}

	// Mode dispatch runs before the old_string checks: in hash modes
	// old_string is not needed (and ignored if sent), as is replace_all —
	// a hash range means "this one place".
	startHashArg, _ := args["start_hash"].(string)
	endHashArg, _ := args["end_hash"].(string)
	afterHashArg, _ := args["after_hash"].(string)
	hasStart := strings.TrimSpace(startHashArg) != ""
	hasAfter := strings.TrimSpace(afterHashArg) != ""
	if hasAfter && hasStart {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("after_hash and start_hash are mutually exclusive; use start_hash/end_hash to replace a range, after_hash to insert after a line")
	}
	if hasAfter {
		if newStr == "" {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("new_string must not be empty when inserting with after_hash")
		}
		return editByInsertAfter(ctx, call, path, displayPath, afterHashArg, newStr)
	}
	if hasStart {
		return editByHashRange(ctx, call, path, displayPath, startHashArg, endHashArg, newStr)
	}
	if strings.TrimSpace(endHashArg) != "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("end_hash without start_hash; set start_hash (copy the N:hhhhhh prefix)")
	}

	oldStr, _ := args["old_string"].(string)
	if oldStr == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string is required")
	}
	if newStr == oldStr {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string and new_string are identical")
	}

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

	candidates := editCandidates(oldStr, newStr)

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

	// Say WHERE the text diverged when the file has an obvious near-miss.
	// Without it the model is told only that its string is absent, and the
	// observed failure mode is retrying the same wrong text: nearly every
	// real miss is one paraphrased line in the middle of an otherwise exact
	// block (a comment retyped from memory, a call site "remembered" as
	// something the file never said), and quoting both sides is what makes
	// that fixable in one step.
	if hint := nearestMissHint(content, oldStr); hint != "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf(
			"old_string not found in %s. %s Re-read that range with read_file and copy the file's own text — do not retype it",
			displayPath, hint,
		)
	}
	return models.ToolResult{CallID: call.ID, ToolName: call.Name}, oldStringNotFoundErr(displayPath, oldStr)
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

// splitLineNumberPrefix parses "<spaces><digits><TAB><rest>" or the hashline
// form "<spaces><digits>:<6 hex><TAB><rest>", returning the parsed number and
// the remainder. The TAB is required: a space separator would make ordinary
// numbered prose ("1. step") look like a transcript. Recognizing the hash
// form here — rather than only in stripLineNumberPrefixes — matters because
// looksLineNumbered shares this parser: it must flag a pasted-back hashline
// transcript whose numbering no longer strips cleanly, or that new_string
// would be written to the file verbatim, prefixes and all.
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
	if i == start || i >= len(line) {
		return 0, "", false
	}
	if line[i] == ':' && i+7 < len(line) && isSixHex(line[i+1:i+7]) && line[i+7] == '\t' {
		return num, line[i+8:], true
	}
	if line[i] != '\t' {
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

// locateOldString resolves an old_string hunk to a byte span of content
// without writing anything, running the same normalization ladder and
// whitespace fallback as old_string mode. It exists so edits can carry raw
// hunks alongside hash hunks (the observed failure was models sending
// {old_string, new_string} objects in edits and getting nothing written);
// multi-hunk resolution must happen for every hunk before the first byte is
// spliced, so applyEdit's replace-in-place shape does not fit. replace_all has
// no meaning here — a hunk is one place — so a repeated match is an error.
func locateOldString(content, oldStr, newStr, scope string) (from, to int, body, note string, err error) {
	for _, c := range editCandidates(oldStr, newStr) {
		if n := strings.Count(content, c.oldS); n > 0 {
			if n > 1 {
				return 0, 0, "", "", fmt.Errorf(
					"old_string matches %d times in %s; add surrounding context to make it unique (replace_all does not apply inside edits)",
					n, scope,
				)
			}
			i := strings.Index(content, c.oldS)
			return i, i + len(c.oldS), c.newS, c.note, nil
		}
		if len(strings.TrimSpace(normalizeWhitespace(c.oldS))) < 8 {
			continue
		}
		spans := findWhitespaceTolerantSpans(content, c.oldS)
		if len(spans) == 0 {
			continue
		}
		if len(spans) > 1 {
			return 0, 0, "", "", fmt.Errorf(
				"old_string matches %d locations in %s after whitespace normalization; add surrounding context to make it unique",
				len(spans), scope,
			)
		}
		notes := "whitespace-tolerant match"
		if c.note != "" {
			notes = c.note + ", " + notes
		}
		return spans[0][0], spans[0][1], conformLineEndings(c.newS, content), notes, nil
	}

	if hint := nearestMissHint(content, oldStr); hint != "" {
		return 0, 0, "", "", fmt.Errorf(
			"old_string not found in %s. %s Re-read that range with read_file and copy the file's own text — or copy the N:hhhhhh prefixes into start_hash/end_hash instead",
			scope, hint,
		)
	}
	return 0, 0, "", "", oldStringNotFoundErr(scope, oldStr)
}

// oldStringNotFoundErr is the shared no-near-miss fallback for old_string
// mode and edits[] hunks. It must tell two misses apart: a transcript pasted
// back with its line-number prefixes still on, and text nothing in the file
// resembles — retyped from memory or never written. One shared message sent
// the model down the wrong fix for the second kind (observed in session
// history: it kept resending text that was never in the file).
func oldStringNotFoundErr(scope, oldStr string) error {
	if looksLineNumbered(oldStr) {
		return fmt.Errorf(
			"old_string not found in %s; send the file's own text (drop the line-number prefix that read_file adds (\"12:a3f2b1<TAB>\") or grep adds (\"file.go:12:a3f2b1: \"), and use real newlines and tabs), or copy that prefix into start_hash/end_hash instead",
			scope,
		)
	}
	return fmt.Errorf(
		"old_string not found in %s and no line in the file resembles it — the text may never have been written, or it changed since your last read. Re-read with read_file and edit from what is actually there — or copy the N:hhhhhh prefixes into start_hash/end_hash instead",
		scope,
	)
}

// editCandidate is one (old, new) pair to try against the file, with the note
// that names the normalization that produced it. The ladder is ordered: the
// literal strings first, so a file whose real text contains "\\n" or a numbered
// column is never rewritten by a normalization that merely looked plausible.
type editCandidate struct {
	oldS, newS string
	note       string
}

// editCandidates builds the normalization ladder shared by old_string mode and
// the old_string hunks inside edits: literal, then escape-normalized (the model
// wrote the two-character "\\n"), then with read_file's numbered prefixes
// stripped (the model pasted the transcript back).
func editCandidates(oldStr, newStr string) []editCandidate {
	candidates := []editCandidate{{oldStr, newStr, ""}}
	if uOld, uNew := unescapeLiteral(oldStr), unescapeLiteral(newStr); uOld != oldStr && uOld != uNew {
		candidates = append(candidates, editCandidate{uOld, uNew, "escape-normalized"})
	}
	// read_file renders ranges and outlines as "<lineno>:<hash>\t<content>";
	// models routinely paste that back verbatim. Try again with the prefixes
	// removed, but only after literal matching failed, so genuine tab-separated
	// data is never rewritten by this path.
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
				candidates = append(candidates, editCandidate{sOld, sNew, "line-number prefixes stripped"})
			}
		}
	}
	return candidates
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
		Description: "Replace text in a file. Hash mode (preferred after read_file or grep): copy the whole N:hhhhhh prefix of the first and last line of the range into start_hash/end_hash and send only new_string — do not restate the old text; the inclusive line range is replaced (empty new_string deletes it). " +
			"after_hash inserts new_string after that line instead (cannot insert before line 1). " +
			"edits applies several hunks in one call against the same read: an array of {start_hash, end_hash, new_string}, {after_hash, new_string} or {old_string, new_string} (mixable — prefer hash hunks after read_file; old_string only for raw spans); hunks must not overlap, and the call is atomic — any bad hunk means nothing is written. " +
			"old_string mode (when you have no hashes — raw spans): old_string must be the file's own exact text and uniquely match (use replace_all for multiple matches); strip the line-number prefix that read_file's numbered output adds before matching (grep's file:line:hash: prefix is not stripped). " +
			"Optional start_line/end_line (1-based, inclusive) scope an old_string search to that line window, so a short old_string that repeats elsewhere still resolves uniquely without replace_all — prefer this over padding old_string with context. " +
			"old_string falls back to whitespace-tolerant matching (tab vs space, CRLF vs LF, collapsed runs) when literal match fails. " +
			"All modes fail safely on missing or ambiguous targets; on failure, re-read the file with read_file and copy hashes or text from its output.",
		Groups: []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "File path to edit"},
				"start_hash": map[string]any{"type": "string", "description": "Hash mode: first line of the range — copy the whole N:hhhhhh prefix from read_file or grep"},
				"end_hash":   map[string]any{"type": "string", "description": "Hash mode: last line of the range (defaults to start_hash for a single line)"},
				"after_hash": map[string]any{"type": "string", "description": "Insert mode: N:hhhhhh prefix of the line to insert new_string after"},
				"edits": map[string]any{
					"type":        "array",
					"description": "Multi-hunk mode: non-overlapping hunks applied atomically against the same read. Each hunk locates its target by start_hash/end_hash (replace a line range, preferred — copy the N:hhhhhh prefix from your read), after_hash (insert) or old_string (exact text, must be unique; only when you have no hashes); new_string is always required",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"start_hash": map[string]any{"type": "string"},
							"end_hash":   map[string]any{"type": "string"},
							"after_hash": map[string]any{"type": "string"},
							"old_string": map[string]any{"type": "string", "description": "Exact text to replace in this hunk, when you have no hashes; must match uniquely"},
							"new_string": map[string]any{"type": "string"},
						},
					},
				},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to find (must be unique unless replace_all is set); not needed in hash modes"},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text (empty string deletes the hash range)"},
				"replace_all": map[string]any{"type": "boolean", "description": "old_string mode: replace all occurrences instead of requiring a unique match (confined to start_line/end_line when set)"},
				"start_line":  map[string]any{"type": "number", "description": "old_string mode: 1-based inclusive first line to search; restricts matching to this window"},
				"end_line":    map[string]any{"type": "number", "description": "old_string mode: 1-based inclusive last line to search; pairs with start_line (defaults to EOF)"},
			},
			"required": []any{"path"},
		},
		Handler: EditFileHandler,
	}
}

// nearestMissHint locates the text old_string was probably aiming at and
// reports the FIRST line where the two diverge, quoting both sides.
//
// Every analyzable edit_file miss in this project's own session history was
// the same shape: the block was anchored correctly and matched exactly for
// several lines, then one line differed because the model retyped it from
// memory instead of copying it ("// would panic there rather than in
// production." for "// would panic there.", errNoProductRow() for
// sql.ErrNoRows). None of them were quoting problems, which is all the
// generic error talks about — so the model read the advice, found nothing to
// fix, and resent the same string.
//
// This only ever explains the failure. It never edits the near-miss it
// found: writing text the caller did not send would be silent corruption
// dressed up as success.
func nearestMissHint(content, oldStr string) string {
	oldLines := strings.Split(strings.TrimSuffix(oldStr, "\n"), "\n")
	anchorIdx := 0
	for anchorIdx < len(oldLines) && strings.TrimSpace(oldLines[anchorIdx]) == "" {
		anchorIdx++
	}
	if anchorIdx == len(oldLines) {
		return ""
	}
	anchor := strings.TrimSpace(oldLines[anchorIdx])

	fileLines := strings.Split(content, "\n")
	best, bestScore := -1, 0.0
	for i, line := range fileLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == anchor {
			best, bestScore = i, 1
			break
		}
		if score := diceSimilarity(trimmed, anchor); score > bestScore {
			best, bestScore = i, score
		}
	}
	// Below this the "nearest" line is not recognizably the same line, and
	// pointing at it would send the model to the wrong place.
	if best < 0 || bestScore < 0.7 {
		return ""
	}

	start := best - anchorIdx
	if start < 0 {
		start = 0
	}
	for k, want := range oldLines {
		var got string
		atEOF := start+k >= len(fileLines)
		if !atEOF {
			got = fileLines[start+k]
		}
		// Whitespace-only differences are not what to report: applyEdit's
		// whitespace-tolerant pass already accepts those, so a miss that
		// reaches here differs in something that actually matters.
		if !atEOF && (want == got || normalizeWhitespace(want) == normalizeWhitespace(got)) {
			continue
		}
		if atEOF {
			return fmt.Sprintf(
				"The closest text starts at line %d and matches your first %d line(s), then your old_string runs past the end of the file.",
				start+1, k,
			)
		}
		if k == 0 {
			return fmt.Sprintf(
				"The closest line is %d — you sent %q, the file has %q.",
				start+k+1, want, got,
			)
		}
		return fmt.Sprintf(
			"The closest text starts at line %d: it matches your first %d line(s), then differs at line %d — you sent %q, the file has %q.",
			start+1, k, start+k+1, want, got,
		)
	}
	return ""
}

// diceSimilarity scores two strings by shared adjacent rune pairs (Sørensen-
// Dice), in [0,1]. Cheap, order-aware enough to tell "the same line, retyped"
// from "a different line", and it degrades gracefully on CJK text, where
// whole-word tokenization would not.
func diceSimilarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		if a == b {
			return 1
		}
		return 0
	}
	counts := make(map[[2]rune]int, len(ra))
	for i := 0; i+1 < len(ra); i++ {
		counts[[2]rune{ra[i], ra[i+1]}]++
	}
	shared := 0
	for i := 0; i+1 < len(rb); i++ {
		key := [2]rune{rb[i], rb[i+1]}
		if counts[key] > 0 {
			counts[key]--
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(ra)-1+len(rb)-1)
}
