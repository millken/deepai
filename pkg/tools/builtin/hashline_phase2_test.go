package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// --- P2-A: grep carries hashes; grep prefix edits without a read ---

func grepCall(t *testing.T, args map[string]any) models.ToolResult {
	t.Helper()
	res, err := GrepHandler(context.Background(), models.ToolCall{ID: "g", Name: "grep", Arguments: args})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	return res
}

func TestGrep_FlatAndContextCarryHashes(t *testing.T) {
	content := "line1\nline2\ntarget line\nline4\nline5\n"
	path := writeTestFile(t, content)

	flat := grepCall(t, map[string]any{"pattern": "target", "path": path})
	wantFlat := path + ":3:" + lineHash("target line") + ": target line\n"
	if flat.Content != wantFlat {
		t.Fatalf("flat = %q, want %q", flat.Content, wantFlat)
	}
	matches, ok := flat.Data["matches"].([]grepMatch)
	if !ok || len(matches) != 1 || matches[0].Hash != lineHash("target line") {
		t.Fatalf("Data[matches] lost the hash (displayGrepMatches must copy it): %v", flat.Data["matches"])
	}

	ctxRes := grepCall(t, map[string]any{"pattern": "target", "path": path, "context": float64(1)})
	for _, want := range []string{
		":2:" + lineHash("line2") + ": line2\n",
		":3:" + lineHash("target line") + ": target line\n",
		":4:" + lineHash("line4") + ": line4\n",
	} {
		if !strings.Contains(ctxRes.Content, want) {
			t.Errorf("context output missing %q:\n%s", want, ctxRes.Content)
		}
	}
}

func TestGrep_PrefixEditsWithoutRead(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	path := writeTestFile(t, content)
	res := grepCall(t, map[string]any{"pattern": "beta", "path": path})

	// Copy the whole grep line into start_hash, the way a sloppy model would.
	line := strings.TrimSuffix(res.Content, "\n")
	if _, err := hashEditCall(t, path, line, "", "BETA"); err != nil {
		t.Fatalf("whole grep line pasted into start_hash must resolve (D4): %v", err)
	}
	if got := readBack(t, path); got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("got %q", got)
	}
}

func TestGrep_HashMatchesReadFileOnCRLF(t *testing.T) {
	content := "one\r\ntwo\r\n"
	path := writeTestFile(t, content)
	res := grepCall(t, map[string]any{"pattern": "two", "path": path})
	// read_file hashes the CR-stripped body; grep must agree.
	if !strings.Contains(res.Content, ":2:"+lineHash("two\r")+": ") {
		t.Fatalf("grep hash disagrees with read_file on CRLF: %q (want hash %s)", res.Content, lineHash("two"))
	}
}

func TestGrep_NoPhantomLastLine(t *testing.T) {
	path := writeTestFile(t, "a\n")
	// "x*" matches the empty string; before Q13 the trailing newline produced
	// a phantom empty line 2 that read_file/edit_file do not recognize.
	res := grepCall(t, map[string]any{"pattern": "x*", "path": path})
	if strings.Contains(res.Content, ":2:") {
		t.Fatalf("phantom last line reported: %q", res.Content)
	}
}

// --- D4: over-copied refs ---

func TestParseHashRef_OverpasteForms(t *testing.T) {
	h := lineHash("beta")
	for _, ref := range []string{
		"12:" + h,
		"  12:" + h,
		"12:" + h + "\tbeta",
		"12:" + h + ": beta",
		"file.go:12:" + h + ": beta",
		"file.go:12:" + h + ": see 99:deadbe: x",
	} {
		hint, hash, err := parseHashRef("start_hash", ref)
		if err != nil || hint != 12 || hash != h {
			t.Errorf("parseHashRef(%q) = (%d, %q, %v), want (12, %q, nil)", ref, hint, hash, err, h)
		}
	}
	// The fake hash inside pasted content must not win when it is the only
	// candidate either: preceded by a space, it has no valid left boundary.
	if _, _, err := parseHashRef("start_hash", "see 99:deadbe: x"); err == nil {
		t.Error("content-embedded fake hash must not parse as a ref")
	}
}

// --- P2-B: multi-hunk ---

func multiEditCall(t *testing.T, path string, edits []any) (models.ToolResult, error) {
	t.Helper()
	return EditFileHandler(context.Background(), models.ToolCall{
		ID: "me", Name: "edit_file",
		Arguments: map[string]any{"path": path, "edits": edits},
	})
}

func replaceHunk(t *testing.T, content string, s, e int, newStr string) map[string]any {
	t.Helper()
	return map[string]any{
		"start_hash": prefixFor(t, content, s),
		"end_hash":   prefixFor(t, content, e),
		"new_string": newStr,
	}
}

const threeFuncs = "func A() {\n\ta()\n}\n\nfunc B() {\n\tb()\n}\n\nfunc C() {\n\tc()\n}\n"

func TestMultiHunk_OutOfOrderDisjointHunks(t *testing.T) {
	path := writeTestFile(t, threeFuncs)
	res, err := multiEditCall(t, path, []any{
		replaceHunk(t, threeFuncs, 9, 11, "func C() {\n\tc2()\n}"),
		replaceHunk(t, threeFuncs, 1, 3, "func A() {\n\ta2()\n}"),
		map[string]any{"after_hash": prefixFor(t, threeFuncs, 7), "new_string": "// tail of B"},
	})
	if err != nil {
		t.Fatalf("multi-hunk: %v", err)
	}
	want := "func A() {\n\ta2()\n}\n\nfunc B() {\n\tb()\n}\n// tail of B\n\nfunc C() {\n\tc2()\n}\n"
	if got := readBack(t, path); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Reply lists details in file order regardless of send order.
	if !strings.Contains(res.Content, "Applied 3 edits") ||
		strings.Index(res.Content, "lines 1-3") > strings.Index(res.Content, "after line 7") {
		t.Fatalf("reply not in file order: %q", res.Content)
	}
	hunks, ok := res.Data["hunks"].([]map[string]any)
	if !ok || len(hunks) != 3 {
		t.Fatalf("Data[hunks] = %v", res.Data["hunks"])
	}
	if old, _ := hunks[0]["old_text"].(string); old != "func A() {\n\ta()\n}\n" {
		t.Fatalf("hunks[0].old_text = %q", old)
	}
}

func TestMultiHunk_PostEditRefsHitTheEditedFile(t *testing.T) {
	path := writeTestFile(t, threeFuncs)
	res, err := multiEditCall(t, path, []any{
		replaceHunk(t, threeFuncs, 1, 3, "func A() {\n\tx()\n\ty()\n\tz()\n}"), // 3 → 5 lines, Δ=+2
		replaceHunk(t, threeFuncs, 5, 7, "func B() {\n\tb2()\n}"),              // Δ=0
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "; new lines: ") {
		t.Fatalf("want per-line refs (8 total new lines): %q", res.Content)
	}
	refField := res.Content[strings.Index(res.Content, "; new lines: ")+len("; new lines: "):]
	edited := readBack(t, path)
	editedLines := splitFileLines(edited)
	for _, ref := range strings.Fields(refField) {
		hint, hash, err := parseHashRef("ref", ref)
		if err != nil {
			t.Fatalf("reply ref %q unparseable: %v", ref, err)
		}
		if hint < 1 || hint > len(editedLines) || lineHash(editedLines[hint-1]) != hash {
			t.Errorf("reply ref %q does not hit the edited file (line %d)", ref, hint)
		}
	}
	// And the refs are directly chainable: edit one just-written line, no read.
	first := strings.Fields(refField)[1] // "2:hash(\tx())" after Δ accounting
	if _, err := hashEditCall(t, path, first, "", "\tX()"); err != nil {
		t.Fatalf("chained edit from reply ref failed: %v", err)
	}
}

func TestMultiHunk_AdjacentAndInsertAtReplaceBoundary(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	path := writeTestFile(t, content)
	// after line 5 + replace 6-10: the insertion's zero-width byte point is the
	// same byte as the replacement's start; line-based overlap must allow it
	// and the splice must put the insertion first (N6).
	if _, err := multiEditCall(t, path, []any{
		replaceHunk(t, content, 6, 10, "L6to10"),
		map[string]any{"after_hash": prefixFor(t, content, 5), "new_string": "inserted"},
	}); err != nil {
		t.Fatalf("adjacent insert+replace: %v", err)
	}
	want := "l1\nl2\nl3\nl4\nl5\ninserted\nL6to10\n"
	if got := readBack(t, path); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMultiHunk_OverlapAndAnchorViolationsRejectAll(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\n"
	path := writeTestFile(t, content)
	_, err := multiEditCall(t, path, []any{
		replaceHunk(t, content, 2, 4, "x"),
		replaceHunk(t, content, 4, 5, "y"),                                        // overlaps at line 4
		map[string]any{"after_hash": prefixFor(t, content, 3), "new_string": "z"}, // anchor inside 2-4
	})
	if err == nil {
		t.Fatal("overlapping hunks must reject")
	}
	for _, want := range []string{"edits[1]", "edits[2]", "nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if got := readBack(t, path); got != content {
		t.Fatal("file must be untouched")
	}
}

func TestMultiHunk_ResolutionErrorsReportedTogetherZeroWrite(t *testing.T) {
	content := "l1\nl2\nl3\n"
	path := writeTestFile(t, content)
	_, err := multiEditCall(t, path, []any{
		map[string]any{"start_hash": "1:aaaaaa", "new_string": "x"},              // bad hash
		map[string]any{"start_hash": prefixFor(t, content, 2)},                   // missing new_string
		map[string]any{"end_hash": prefixFor(t, content, 3), "new_string": "y"},  // end without start
		map[string]any{"after_hash": prefixFor(t, content, 1), "new_string": ""}, // empty insert
	})
	if err == nil {
		t.Fatal("want all four hunks rejected")
	}
	for _, want := range []string{"edits[0]", "edits[1]", "edits[2]", "edits[3]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q (Q14: report all at once): %v", want, err)
		}
	}
	if got := readBack(t, path); got != content {
		t.Fatal("file must be untouched")
	}
}

func TestMultiHunk_EmptyArrayRejected(t *testing.T) {
	path := writeTestFile(t, "a\n")
	if _, err := multiEditCall(t, path, []any{}); err == nil {
		t.Fatal("empty edits array must error")
	}
}

func TestMultiHunk_SummaryCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 16; i++ {
		b.WriteString("unique line ")
		b.WriteByte(byte('a' + i))
		b.WriteByte('\n')
	}
	content := b.String()
	path := writeTestFile(t, content)
	var edits []any
	for i := 1; i <= 8; i++ {
		edits = append(edits, replaceHunk(t, content, i*2-1, i*2-1, "r"))
	}
	res, err := multiEditCall(t, path, edits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "+5 more") {
		t.Fatalf("summary must cap at 3 details (+5 more): %q", res.Content)
	}
	// 8 hunks × 1 new line each = 8 total new lines: still within Q10's cap,
	// so the per-line refs are listed even though the summary is folded.
	idx := strings.Index(res.Content, "; new lines: ")
	if idx < 0 {
		t.Fatalf("8 total new lines must list per-line refs: %q", res.Content)
	}
	if got := len(strings.Fields(res.Content[idx+len("; new lines: "):])); got != 8 {
		t.Fatalf("want 8 refs, got %d: %q", got, res.Content)
	}
}

// --- P2-C: after_hash ---

func insertCall(t *testing.T, path, afterRef string, newStr any) (models.ToolResult, error) {
	t.Helper()
	args := map[string]any{"path": path, "after_hash": afterRef}
	if newStr != nil {
		args["new_string"] = newStr
	}
	return EditFileHandler(context.Background(), models.ToolCall{ID: "ins", Name: "edit_file", Arguments: args})
}

func TestInsertAfter_MidFile(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	res, err := insertCall(t, path, prefixFor(t, content, 2), "2.5a\n2.5b")
	if err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\ntwo\n2.5a\n2.5b\nthree\n" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(res.Content, "Inserted 2 lines after line 2") ||
		!strings.Contains(res.Content, "3:"+lineHash("2.5a")) {
		t.Fatalf("reply = %q", res.Content)
	}
	if sl, _ := res.Data["start_line"].(int); sl != 3 {
		t.Fatalf("Data[start_line] = %v", res.Data["start_line"])
	}
}

func TestInsertAfter_LastLineEOFStates(t *testing.T) {
	// File WITH final newline: insertion becomes the new last content and the
	// file keeps ending with a newline.
	withNL := "one\ntwo\n"
	path := writeTestFile(t, withNL)
	if _, err := insertCall(t, path, prefixFor(t, withNL, 2), "three"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\ntwo\nthree\n" {
		t.Fatalf("with final newline: got %q", got)
	}

	// File WITHOUT final newline: a separator is added (or the insertion
	// would merge with the anchor), no trailing newline is invented.
	withoutNL := "one\ntwo"
	path = writeTestFile(t, withoutNL)
	if _, err := insertCall(t, path, prefixFor(t, withoutNL, 2), "three"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\ntwo\nthree" {
		t.Fatalf("without final newline: got %q", got)
	}

	// N7: the model's own explicit trailing newline is content, kept as sent.
	path = writeTestFile(t, withoutNL)
	if _, err := insertCall(t, path, prefixFor(t, withoutNL, 2), "x\n"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\ntwo\nx\n" {
		t.Fatalf("explicit trailing newline must be kept: got %q", got)
	}
}

func TestInsertAfter_AmbiguousAnchorNeedsHint(t *testing.T) {
	content := "a\n}\nb\n}\n"
	path := writeTestFile(t, content)
	if _, err := insertCall(t, path, lineHash("}"), "x"); err == nil {
		t.Fatal("repeated bare-hash anchor must reject")
	}
	if _, err := insertCall(t, path, prefixFor(t, content, 4), "x"); err != nil {
		t.Fatalf("hinted anchor must resolve: %v", err)
	}
	if got := readBack(t, path); got != "a\n}\nb\n}\nx\n" {
		t.Fatalf("got %q", got)
	}
}

func TestInsertAfter_ErrorCases(t *testing.T) {
	content := "one\ntwo\n"
	path := writeTestFile(t, content)
	if _, err := insertCall(t, path, prefixFor(t, content, 1), ""); err == nil {
		t.Fatal("empty new_string insert must error")
	}
	if _, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "ins", Name: "edit_file",
		Arguments: map[string]any{
			"path": path, "after_hash": prefixFor(t, content, 1),
			"start_hash": prefixFor(t, content, 2), "new_string": "x",
		},
	}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatal("after_hash + start_hash must error")
	}
	empty := writeTestFile(t, "")
	if _, err := insertCall(t, empty, "1:abcdef", "x"); err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("empty file must point at write_file, got %v", err)
	}
	if got := readBack(t, path); got != content {
		t.Fatal("file must be untouched by rejected inserts")
	}
}

// --- P2-D: chaining from single-edit replies; cap behavior ---

func TestChainedEditFromReplyRefs(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	res, err := hashEditCall(t, path, prefixFor(t, content, 2), "", "TWO-A\nTWO-B")
	if err != nil {
		t.Fatal(err)
	}
	// Reply: "Replaced line 2 (h..h) in f with 2 lines: 2:xxxxxx 3:yyyyyy"
	idx := strings.Index(res.Content, "lines: ")
	if idx < 0 {
		t.Fatalf("reply lacks per-line refs: %q", res.Content)
	}
	refs := strings.Fields(res.Content[idx+len("lines: "):])
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %v", refs)
	}
	if _, err := hashEditCall(t, path, refs[1], "", "TWO-B2"); err != nil {
		t.Fatalf("chained edit of a just-written line must need no read: %v", err)
	}
	if got := readBack(t, path); got != "one\nTWO-A\nTWO-B2\nthree\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyRefsCappedAtEight(t *testing.T) {
	content := "one\ntwo\n"
	path := writeTestFile(t, content)
	nine := strings.TrimSuffix(strings.Repeat("n\n", 9), "\n")
	res, err := hashEditCall(t, path, prefixFor(t, content, 1), "", nine)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "lines: 1:") {
		t.Fatalf("9 new lines must fall back to endpoint hashes: %q", res.Content)
	}
	if !strings.Contains(res.Content, "with 9 lines (") {
		t.Fatalf("want v1 endpoint form: %q", res.Content)
	}
}
