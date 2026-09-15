package builtin

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// Hashline editing (docs/HASHLINE_EDIT_DESIGN.md): read_file's numbered output
// prefixes every line with "N:hhhhhh" — a 1-based line number and a 6-hex
// content hash. edit_file's hash mode takes that prefix back as
// start_hash/end_hash and replaces the inclusive line range without restating
// the old text. The hash is the content check; the line number is only a
// disambiguation hint between lines with identical content — it never
// authorizes a write on its own.

// lineHash returns the 6 lowercase hex digits of FNV-1a 64 (low 24 bits) over
// the line's bytes with the trailing "\r" removed (splitFileLines already
// removes the "\n"). Stripping the CR makes LF and CRLF files hash alike, so a
// prefix copied before a line-ending conversion still resolves.
func lineHash(line string) string {
	h := fnv.New64a()
	h.Write([]byte(strings.TrimSuffix(line, "\r")))
	return fmt.Sprintf("%06x", h.Sum64()&0xffffff)
}

// writeHashNumberedLine renders one "N:hhhhhh<TAB>content" line of read_file's
// numbered output. width right-aligns the line number like today's plain
// numbering, so the prefix stays column-stable within one read.
func writeHashNumberedLine(b *strings.Builder, width, lineno int, line string) {
	fmt.Fprintf(b, "%*d:%s\t%s\n", width, lineno, lineHash(line), line)
}

// editByHashRange is edit_file's hash mode: locate the inclusive line range
// named by start/end hash refs against the file as it is on disk right now,
// then replace it with newStr (empty = delete the range). No old text is read
// from the arguments; the hashes are recomputed from the file, so the tool
// stays stateless.
func editByHashRange(ctx context.Context, call models.ToolCall, path, displayPath, startRef, endRef, newStr string) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(fmt.Errorf("read failed: %w", err))
	}
	content := string(data)
	lines := splitFileLines(content)
	if len(lines) == 0 {
		return fail(fmt.Errorf("%s is empty; use write_file", displayPath))
	}

	s, e, err := resolveHashRange(lines, startRef, endRef, displayPath)
	if err != nil {
		return fail(err)
	}
	from, to, _, _, err := lineWindow(content, s, e, displayPath)
	if err != nil {
		return fail(err)
	}
	oldText := content[from:to]
	startHash, endHash := lineHash(lines[s-1]), lineHash(lines[e-1])

	replacement := mendTrailingNewline(content, conformLineEndings(newStr, content), e == len(lines))

	updated := content[:from] + replacement + content[to:]
	if writeErr := os.WriteFile(path, []byte(updated), filePerm(path, 0644)); writeErr != nil {
		return fail(fmt.Errorf("write failed: %w", writeErr))
	}

	span := lineSpanLabel(s, e)
	var msg string
	if replacement == "" {
		msg = fmt.Sprintf("Deleted %s (%s..%s) in %s", span, startHash, endHash, displayPath)
	} else if newLines := splitFileLines(replacement); len(newLines) <= maxNewLineRefs {
		// §17.4: the refs use post-edit line numbers and are copy-pastable
		// straight back into start_hash/end_hash, so chained edits to the
		// just-written lines need no re-read.
		msg = fmt.Sprintf("Replaced %s (%s..%s) in %s with %d lines: %s",
			span, startHash, endHash, displayPath, len(newLines),
			strings.Join(newLineRefs(replacement, s), " "))
	} else {
		msg = fmt.Sprintf("Replaced %s (%s..%s) in %s with %d lines (%s..%s)",
			span, startHash, endHash, displayPath, len(newLines),
			lineHash(newLines[0]), lineHash(newLines[len(newLines)-1]))
	}
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  msg,
		Data: map[string]any{
			"start_line": s,
			"end_line":   e,
			"old_text":   oldText,
			"start_hash": startHash,
			"end_hash":   endHash,
		},
	}, nil
}

// maxNewLineRefs caps the per-line N:hhhhhh listing in success replies
// (HASHLINE_EDIT_DESIGN §17.4 Q10): beyond it the reply falls back to the
// start..end pair so it stays within the compact tool-result budget.
const maxNewLineRefs = 8

// mendTrailingNewline applies §7's trailing-newline rule to a conformed
// replacement or insertion body: a non-empty body that does not cover EOF
// must end with a newline or its last line would merge with the next one; a
// body covering EOF follows the file's own final-newline state — append only
// when the file ended with one, and never strip what the model sent.
func mendTrailingNewline(content, body string, coversEOF bool) string {
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	if coversEOF && !strings.HasSuffix(content, "\n") {
		return body
	}
	return body + fileNewline(content)
}

// fileNewline returns the newline style of content's own line endings.
func fileNewline(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// lineSpanLabel renders a 1-based inclusive line range for result messages.
func lineSpanLabel(s, e int) string {
	if e > s {
		return fmt.Sprintf("lines %d-%d", s, e)
	}
	return fmt.Sprintf("line %d", s)
}

// newLineRefs renders post-edit "N:hhhhhh" refs for each line of body, whose
// first line lands on 1-based line startLine after the edit.
func newLineRefs(body string, startLine int) []string {
	ls := splitFileLines(body)
	refs := make([]string, len(ls))
	for i, ln := range ls {
		refs[i] = fmt.Sprintf("%d:%s", startLine+i, lineHash(ln))
	}
	return refs
}

// editByInsertAfter is edit_file's pure-insert mode (§17.3): place newStr
// after the single line named by afterRef, restating nothing. Prepending
// before line 1 is not expressible (there is no line 0); that case uses
// old_string or write_file.
func editByInsertAfter(ctx context.Context, call models.ToolCall, path, displayPath, afterRef, newStr string) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(fmt.Errorf("read failed: %w", err))
	}
	content := string(data)
	lines := splitFileLines(content)
	if len(lines) == 0 {
		return fail(fmt.Errorf("%s is empty; use write_file", displayPath))
	}
	s, e, err := resolveHashRange(lines, afterRef, "", displayPath)
	if err != nil {
		return fail(err)
	}
	if s != e {
		return fail(fmt.Errorf("after_hash %q resolved to lines %d-%d, not a single anchor line; copy one N:hhhhhh prefix", afterRef, s, e))
	}
	anchor := s

	_, to, _, _, err := lineWindow(content, anchor, anchor, displayPath)
	if err != nil {
		return fail(err)
	}
	body := mendTrailingNewline(content, conformLineEndings(newStr, content), anchor == len(lines))
	// Inserting after a final line that has no newline needs a separator
	// first, or the insertion would merge with the anchor. The separator is
	// plumbing, not content: the file keeps its no-final-newline state unless
	// the model's own new_string ends with one (§17.3 N7).
	sep := ""
	if anchor == len(lines) && !strings.HasSuffix(content, "\n") {
		sep = fileNewline(content)
	}

	updated := content[:to] + sep + body + content[to:]
	if writeErr := os.WriteFile(path, []byte(updated), filePerm(path, 0644)); writeErr != nil {
		return fail(fmt.Errorf("write failed: %w", writeErr))
	}

	k := len(splitFileLines(body))
	msg := fmt.Sprintf("Inserted %d lines after line %d (%s) in %s", k, anchor, lineHash(lines[anchor-1]), displayPath)
	if k <= maxNewLineRefs {
		msg += ": " + strings.Join(newLineRefs(body, anchor+1), " ")
	}
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  msg,
		Data: map[string]any{
			"start_line": anchor + 1,
			"old_text":   "",
		},
	}, nil
}

// resolvedHunk is one entry of a multi-hunk edit after resolution against the
// original file: line coordinates, the half-open byte span [from,to) (from==to
// for an insertion), and the mended body ready to splice.
type resolvedHunk struct {
	idx    int // position in the model's edits array, for error attribution
	insert bool
	raw    bool // resolved from old_string: from/to is an exact byte span, not whole lines
	note   string
	s, e   int // replace: inclusive range; insert: s==e==anchor line
	from   int
	to     int
	sep    string // insert-only: separator before body at EOF-without-newline
	body   string
	rawNew string // as the model sent it, for the TUI diff
	k      int    // line count of body (0 = deletion)
}

// editByHunks is edit_file's multi-hunk mode (§17.2): every hunk resolves
// against the SAME original file (hashes computed once), all validation runs
// before any byte is written, and resolution errors are reported together —
// one retry fixes them all. A hunk locates its target by hash range, insertion
// anchor, or old_string — models reach for edits as "several old_string edits
// at once", and rejecting that shape wrote nothing and pushed them back to
// one call per hunk. Overlap is judged by line numbers (by byte span between
// two old_string hunks); application splices half-open byte spans in one
// ascending pass, zero-width insertions first at equal offsets.
func editByHunks(ctx context.Context, call models.ToolCall, path, displayPath string, editsRaw any) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}
	list, ok := editsRaw.([]any)
	if !ok || len(list) == 0 {
		return fail(fmt.Errorf("edits must be a non-empty array of {start_hash, end_hash, new_string}, {after_hash, new_string} or {old_string, new_string} objects"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(fmt.Errorf("read failed: %w", err))
	}
	content := string(data)
	lines := splitFileLines(content)
	if len(lines) == 0 {
		return fail(fmt.Errorf("%s is empty; use write_file", displayPath))
	}
	total := len(lines)

	var errs []string
	addErr := func(i int, format string, a ...any) {
		errs = append(errs, fmt.Sprintf("edits[%d]: %s", i, fmt.Sprintf(format, a...)))
	}
	var hunks []resolvedHunk
	for i, raw := range list {
		obj, ok := raw.(map[string]any)
		if !ok {
			addErr(i, "must be an object")
			continue
		}
		startRef, _ := obj["start_hash"].(string)
		endRef, _ := obj["end_hash"].(string)
		afterRef, _ := obj["after_hash"].(string)
		oldStr, _ := obj["old_string"].(string)
		hasStart := strings.TrimSpace(startRef) != ""
		hasEnd := strings.TrimSpace(endRef) != ""
		hasAfter := strings.TrimSpace(afterRef) != ""
		hasOld := oldStr != ""
		newRaw, hasNew := obj["new_string"]
		newStr, newIsString := newRaw.(string)
		switch {
		case !hasNew || !newIsString:
			addErr(i, "new_string is required (pass an empty string to delete the range)")
			continue
		case hasAfter && hasStart:
			addErr(i, "after_hash and start_hash are mutually exclusive")
			continue
		case hasAfter && newStr == "":
			addErr(i, "new_string must not be empty when inserting")
			continue
		case !hasAfter && !hasStart && hasEnd:
			addErr(i, "end_hash without start_hash; set start_hash (copy the N:hhhhhh prefix)")
			continue
		case !hasAfter && !hasStart && !hasOld:
			addErr(i, "needs start_hash (replace), after_hash (insert) or old_string (exact text)")
			continue
		}
		// A hash ref wins over old_string in the same hunk, matching the
		// top-level dispatch: the hash is checked against the file, the text
		// is whatever the model remembered.
		if !hasAfter && !hasStart {
			from, to, body, note, lerr := locateOldString(content, oldStr, newStr, displayPath)
			if lerr != nil {
				addErr(i, "%v", lerr)
				continue
			}
			ls, le := 1+strings.Count(content[:from], "\n"), 1+strings.Count(content[:to-1], "\n")
			hunks = append(hunks, resolvedHunk{
				idx: i, raw: true, note: note, s: ls, e: le,
				from: from, to: to, body: body, rawNew: newStr,
			})
			continue
		}
		if hasAfter {
			s, e, rerr := resolveHashRange(lines, afterRef, "", displayPath)
			if rerr != nil {
				addErr(i, "%v", rerr)
				continue
			}
			if s != e {
				addErr(i, "after_hash resolved to lines %d-%d, not a single anchor line", s, e)
				continue
			}
			hunks = append(hunks, resolvedHunk{idx: i, insert: true, s: s, e: s, rawNew: newStr})
		} else {
			s, e, rerr := resolveHashRange(lines, startRef, endRef, displayPath)
			if rerr != nil {
				addErr(i, "%v", rerr)
				continue
			}
			hunks = append(hunks, resolvedHunk{idx: i, s: s, e: e, rawNew: newStr})
		}
	}
	if len(errs) > 0 {
		return fail(fmt.Errorf("%s — nothing was written", strings.Join(errs, "; ")))
	}

	// Overlap by LINE NUMBERS (§17.2 N6): replace ranges pairwise disjoint,
	// an insertion anchor never inside a replaced range (the anchor would be
	// consumed), and no two insertions share an anchor (their order would be
	// undefined). Byte intervals cannot express "insert after 5 + replace
	// 6-10 are adjacent", so they are not what is checked here.
	for a := 0; a < len(hunks); a++ {
		for b := a + 1; b < len(hunks); b++ {
			ha, hb := hunks[a], hunks[b]
			switch {
			case !ha.insert && !hb.insert:
				// Two old_string hunks carry exact byte spans, so two edits
				// inside one long line are legal; anything involving a hash
				// hunk is judged by whole lines, which is the unit it replaces.
				if ha.raw && hb.raw {
					if ha.from < hb.to && hb.from < ha.to {
						addErr(hb.idx, "old_string span (lines %d-%d) overlaps edits[%d] (lines %d-%d)", hb.s, hb.e, ha.idx, ha.s, ha.e)
					}
				} else if ha.s <= hb.e && hb.s <= ha.e {
					addErr(hb.idx, "lines %d-%d overlap edits[%d] (lines %d-%d)", hb.s, hb.e, ha.idx, ha.s, ha.e)
				}
			case ha.insert != hb.insert:
				ins, rep := ha, hb
				if hb.insert {
					ins, rep = hb, ha
				}
				if rep.s <= ins.s && ins.s <= rep.e {
					addErr(ins.idx, "insertion anchor line %d is inside edits[%d]'s replaced range %d-%d", ins.s, rep.idx, rep.s, rep.e)
				}
			default:
				if ha.s == hb.s {
					addErr(hb.idx, "duplicate insertion anchor line %d with edits[%d]", hb.s, ha.idx)
				}
			}
		}
	}
	if len(errs) > 0 {
		return fail(fmt.Errorf("%s — nothing was written", strings.Join(errs, "; ")))
	}

	hasFinalNL := strings.HasSuffix(content, "\n")
	for i := range hunks {
		h := &hunks[i]
		if h.raw {
			// An old_string span is not line-aligned, so neither the trailing
			// newline rule nor a line count applies to it.
			continue
		}
		from, to, _, _, werr := lineWindow(content, h.s, h.e, displayPath)
		if werr != nil {
			return fail(werr)
		}
		h.body = mendTrailingNewline(content, conformLineEndings(h.rawNew, content), h.e == total)
		h.k = len(splitFileLines(h.body))
		if h.insert {
			h.from, h.to = to, to
			if h.s == total && !hasFinalNL {
				h.sep = fileNewline(content)
			}
		} else {
			h.from, h.to = from, to
		}
	}

	// Ascending single-pass splice over half-open byte spans; a zero-width
	// insertion at the same offset as a replacement's start goes first.
	sortHunksByOffset(hunks)
	var out strings.Builder
	cursor := 0
	for _, h := range hunks {
		out.WriteString(content[cursor:h.from])
		out.WriteString(h.sep)
		out.WriteString(h.body)
		cursor = h.to
	}
	out.WriteString(content[cursor:])
	if writeErr := os.WriteFile(path, []byte(out.String()), filePerm(path, 0644)); writeErr != nil {
		return fail(fmt.Errorf("write failed: %w", writeErr))
	}

	// Summary in file order, capped at 3 details (§17.2 C21); post-edit line
	// numbers accumulate each hunk's line delta over the hunks above it
	// (§17.4). Deletions contribute no refs.
	var details []string
	var refs []string
	totalNew := 0
	deltaSum := 0
	dataHunks := make([]map[string]any, 0, len(hunks))
	for _, h := range hunks {
		switch {
		case h.raw:
			// No N:hhhhhh refs for a raw span: the hashes of the rewritten
			// lines are only knowable for whole-line replacements, and a wrong
			// ref is worse than none. The line delta is counted from the bytes
			// so later hunks' refs still land on post-edit line numbers.
			verb := "replaced"
			if h.body == "" {
				verb = "deleted"
			}
			details = append(details, fmt.Sprintf("%s old_string at %s", verb, lineSpanLabel(h.s, h.e)))
			deltaSum += strings.Count(h.body, "\n") - strings.Count(content[h.from:h.to], "\n")
		case h.insert:
			details = append(details, fmt.Sprintf("inserted %d lines after line %d", h.k, h.s))
			refs = append(refs, newLineRefs(h.body, h.s+1+deltaSum)...)
			totalNew += h.k
			deltaSum += h.k
		case h.body == "":
			details = append(details, fmt.Sprintf("deleted %s", lineSpanLabel(h.s, h.e)))
			deltaSum -= h.e - h.s + 1
		default:
			details = append(details, fmt.Sprintf("replaced %s with %d lines", lineSpanLabel(h.s, h.e), h.k))
			refs = append(refs, newLineRefs(h.body, h.s+deltaSum)...)
			totalNew += h.k
			deltaSum += h.k - (h.e - h.s + 1)
		}
		startLine := h.s
		if h.insert {
			startLine = h.s + 1
		}
		dataHunks = append(dataHunks, map[string]any{
			"start_line": startLine,
			"end_line":   h.e,
			"old_text":   content[h.from:h.to],
			"new_string": h.rawNew,
		})
	}
	shown := details
	if len(shown) > 3 {
		shown = append(append([]string{}, details[:3]...), fmt.Sprintf("+%d more", len(details)-3))
	}
	msg := fmt.Sprintf("Applied %d edits in %s: %s", len(hunks), displayPath, strings.Join(shown, ", "))
	if notes := hunkNotes(hunks); notes != "" {
		msg += " (" + notes + ")"
	}
	if totalNew > 0 && totalNew <= maxNewLineRefs {
		msg += "; new lines: " + strings.Join(refs, " ")
	}
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  msg,
		Data: map[string]any{
			"start_line": hunks[0].s,
			"hunks":      dataHunks,
		},
	}, nil
}

// hunkNotes joins the distinct normalization notes the old_string hunks needed,
// in first-seen order. They are reported for the same reason old_string mode
// reports them: each note is one tolerance layer proving it is still load-bearing.
func hunkNotes(hunks []resolvedHunk) string {
	var seen []string
	for _, h := range hunks {
		if h.note == "" {
			continue
		}
		dup := false
		for _, s := range seen {
			if s == h.note {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, h.note)
		}
	}
	return strings.Join(seen, "; ")
}

// sortHunksByOffset orders hunks for the splice: ascending byte offset,
// zero-width insertions before a replacement starting at the same byte.
func sortHunksByOffset(hunks []resolvedHunk) {
	for i := 1; i < len(hunks); i++ {
		for j := i; j > 0; j-- {
			a, b := hunks[j-1], hunks[j]
			if b.from < a.from || (b.from == a.from && b.insert && !a.insert) {
				hunks[j-1], hunks[j] = b, a
			} else {
				break
			}
		}
	}
}

// parseHashRef parses a start_hash/end_hash argument: the bare 6-hex hash, or
// an over-copied prefix from either numbered source. Models paste the whole
// span they saw, not a precisely extracted N:hhhhhh, so both shapes must
// resolve (HASHLINE_EDIT_DESIGN §17.1 D4):
//
//	read_file: "  12:a3f2b1<TAB>content"        (TAB-separated)
//	grep:      "file.go:12:a3f2b1: content"     (colon-separated)
//
// Everything from the first TAB on is cut, then locateHashRef finds the first
// line:hash pair with proper boundaries. hint is 0 when no line number came
// with the hash.
func parseHashRef(field, ref string) (hint int, hash string, err error) {
	s := ref
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s != "" {
		if isSixHex(s) {
			return 0, s, nil
		}
		if h, hx, ok := locateHashRef(s); ok {
			return h, hx, nil
		}
	}
	return 0, "", fmt.Errorf("%s %q is not a line hash; copy the whole N:hhhhhh prefix from read_file or grep output — do not invent hashes or send a bare line number", field, ref)
}

// locateHashRef scans s for the FIRST "<digits>:<6 hex>" whose left neighbor
// is the start of the string or ':' and whose right neighbor is ':', TAB, or
// the end of the string. The boundaries keep it from biting hash-shaped noise
// inside pasted content ("see 99:deadbe: x" — preceded by a space, skipped)
// while accepting a whole pasted grep line ("file.go:12:a3f2b1: content").
func locateHashRef(s string) (hint int, hash string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		if i > 0 && s[i-1] != ':' {
			continue
		}
		j, n := i, 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			n = n*10 + int(s[j]-'0')
			j++
		}
		if j >= len(s) || s[j] != ':' || n <= 0 {
			i = j
			continue
		}
		hexEnd := j + 1 + 6
		if hexEnd > len(s) || !isSixHex(s[j+1:hexEnd]) {
			i = j
			continue
		}
		if hexEnd < len(s) && s[hexEnd] != ':' && s[hexEnd] != '\t' {
			i = j
			continue
		}
		return n, s[j+1 : hexEnd], true
	}
	return 0, "", false
}

// isSixHex reports whether s is exactly 6 lowercase hex digits — the only
// hash shape read_file emits, so anything else is a copy error, not a hash.
func isSixHex(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveHashRange resolves start/end hash refs against the current file's
// lines into a 1-based inclusive line range, per HASHLINE_EDIT_DESIGN §5.2.
// Every returned range has both endpoint hashes matching the file as it is on
// disk right now; the line-number hints only choose between spans whose
// endpoint content is identical.
func resolveHashRange(lines []string, startRef, endRef, displayPath string) (int, int, error) {
	startHint, startHash, err := parseHashRef("start_hash", startRef)
	if err != nil {
		return 0, 0, err
	}
	endHint, endHash := startHint, startHash
	if strings.TrimSpace(endRef) != "" {
		endHint, endHash, err = parseHashRef("end_hash", endRef)
		if err != nil {
			return 0, 0, err
		}
	}

	hashes := make([]string, len(lines))
	for i, ln := range lines {
		hashes[i] = lineHash(ln)
	}
	var starts, ends []int
	for i, h := range hashes {
		if h == startHash {
			starts = append(starts, i+1)
		}
		if h == endHash {
			ends = append(ends, i+1)
		}
	}
	if len(starts) == 0 {
		return 0, 0, missingHashErr("start_hash", startHash, hashes, lines, displayPath)
	}
	if len(ends) == 0 {
		return 0, 0, missingHashErr("end_hash", endHash, hashes, lines, displayPath)
	}

	type span struct{ s, e int }
	var spans []span
	for _, s := range starts {
		for _, e := range ends {
			if s <= e {
				spans = append(spans, span{s, e})
			}
		}
	}
	if len(spans) == 0 {
		return 0, 0, fmt.Errorf(
			"start_hash is after end_hash (start_hash %s at line %d, end_hash %s at line %d); swap them",
			startHash, starts[0], endHash, ends[len(ends)-1],
		)
	}
	if len(spans) == 1 {
		return spans[0].s, spans[0].e, nil
	}

	// Exact hit: both hints name lines whose content still hashes to the given
	// values — the common case right after a read.
	if startHint > 0 && endHint > 0 && startHint <= endHint &&
		endHint <= len(lines) && hashes[startHint-1] == startHash && hashes[endHint-1] == endHash {
		return startHint, endHint, nil
	}

	// Nearest hash-matching span to the hints. A side without a hint does not
	// score (scoring it as |s-0| would systematically favor spans near the top
	// of the file). Ties reject: guessing between equally-near spans is exactly
	// the silent mis-write this tool refuses.
	if startHint > 0 || endHint > 0 {
		best, bestScore, tied := 0, 0, false
		for i, sp := range spans {
			score := 0
			if startHint > 0 {
				score += abs(sp.s - startHint)
			}
			if endHint > 0 {
				score += abs(sp.e - endHint)
			}
			if i == 0 || score < bestScore {
				best, bestScore, tied = i, score, false
			} else if score == bestScore {
				tied = true
			}
		}
		if !tied {
			return spans[best].s, spans[best].e, nil
		}
	}

	if startHash == endHash && (startHint == 0 || endHint == 0) {
		return 0, 0, fmt.Errorf(
			"hash %s occurs %d times in %s (lines %s); pass start_hash as N:hhhhhh (the whole read_file prefix), or use old_string",
			startHash, len(starts), displayPath, joinInts(starts, 5),
		)
	}
	list := make([]string, 0, len(spans))
	for i, sp := range spans {
		if i == 5 {
			list = append(list, "…")
			break
		}
		list = append(list, fmt.Sprintf("%d-%d", sp.s, sp.e))
	}
	return 0, 0, fmt.Errorf(
		"hash range %s..%s matches %d spans in %s (lines %s); copy the full N:hhhhhh prefixes (line+hash) from read_file, not just the hex",
		startHash, endHash, len(spans), displayPath, strings.Join(list, ", "),
	)
}

// missingHashErr reports a hash absent from the file, adding a "closest hash"
// pointer when some line's hash differs in exactly one hex character — a
// likely copy typo. It only ever hints; it never edits what it found.
func missingHashErr(field, want string, hashes, lines []string, displayPath string) error {
	msg := fmt.Sprintf(
		"%s %s not in %s (%d lines). Re-read with read_file and copy the N:hhhhhh prefix from the transcript — do not invent hashes",
		field, want, displayPath, len(lines),
	)
	for i, h := range hashes {
		if hexCharDiff(h, want) == 1 {
			return fmt.Errorf("%s (closest hash is %s at line %d)", msg, h, i+1)
		}
	}
	return fmt.Errorf("%s", msg)
}

// hexCharDiff counts differing character positions between two equal-length
// hash strings (6 hex chars, not bits); -1 when lengths differ.
func hexCharDiff(a, b string) int {
	if len(a) != len(b) {
		return -1
	}
	n := 0
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// joinInts renders up to max line numbers, eliding the rest.
func joinInts(nums []int, max int) string {
	parts := make([]string, 0, len(nums))
	for i, n := range nums {
		if i == max {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%d", n))
	}
	return strings.Join(parts, ", ")
}
