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

	// §7: the span includes line e's newline (or runs to EOF on the last
	// line), so a non-empty replacement that does not end in a newline must
	// get one appended or its last line would merge with line e+1. On the
	// last line, follow the file's own final-newline state instead — append
	// only when the file ended with one; never invent one it never had.
	replacement := conformLineEndings(newStr, content)
	if replacement != "" && !strings.HasSuffix(replacement, "\n") {
		nl := "\n"
		if strings.Contains(content, "\r\n") {
			nl = "\r\n"
		}
		if e < len(lines) || strings.HasSuffix(content, "\n") {
			replacement += nl
		}
	}

	updated := content[:from] + replacement + content[to:]
	if writeErr := os.WriteFile(path, []byte(updated), filePerm(path, 0644)); writeErr != nil {
		return fail(fmt.Errorf("write failed: %w", writeErr))
	}

	span := fmt.Sprintf("line %d", s)
	if e > s {
		span = fmt.Sprintf("lines %d-%d", s, e)
	}
	var msg string
	if replacement == "" {
		msg = fmt.Sprintf("Deleted %s (%s..%s) in %s", span, startHash, endHash, displayPath)
	} else {
		newLines := splitFileLines(replacement)
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

// parseHashRef parses a start_hash/end_hash argument: either the bare 6-hex
// hash or the whole read_file prefix "N:hhhhhh". Models are told to copy the
// entire prefix, so leading pad spaces and an over-copied TAB (with or without
// trailing content) are tolerated: everything from the first TAB on is cut.
// hint is 0 when no line number was given.
func parseHashRef(field, ref string) (hint int, hash string, err error) {
	s := ref
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	badRef := func() error {
		return fmt.Errorf("%s %q is not a line hash; copy the whole N:hhhhhh prefix from read_file's numbered output — do not invent hashes or send a bare line number", field, ref)
	}
	if s == "" {
		return 0, "", badRef()
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		numPart, hexPart := s[:i], s[i+1:]
		for _, c := range numPart {
			if c < '0' || c > '9' {
				return 0, "", badRef()
			}
		}
		if numPart == "" || !isSixHex(hexPart) {
			return 0, "", badRef()
		}
		n := 0
		for _, c := range numPart {
			n = n*10 + int(c-'0')
		}
		if n <= 0 {
			return 0, "", badRef()
		}
		return n, hexPart, nil
	}
	if !isSixHex(s) {
		return 0, "", badRef()
	}
	return 0, s, nil
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
