package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func readCall(t *testing.T, args map[string]any) models.ToolResult {
	t.Helper()
	res, err := ReadFileHandler(context.Background(), models.ToolCall{ID: "r", Name: "read_file", Arguments: args})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	return res
}

// edits was specified as hash-only, and the session history shows what models
// actually send: several {old_string, new_string} objects, the natural reading
// of "multi-hunk". That hard-failed with nothing written, and the model fell
// back to one edit_file per hunk. These tests pin the accepted shape.

func TestEdits_OldStringHunks(t *testing.T) {
	content := "package main\n\nfunc alpha() int {\n\treturn 1\n}\n\nfunc beta() int {\n\treturn 2\n}\n"
	path := writeTestFile(t, content)

	res, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "func alpha() int {\n\treturn 1\n}", "new_string": "func alpha() int {\n\treturn 10\n}"},
		map[string]any{"old_string": "\treturn 2", "new_string": "\treturn 20"},
	})
	if err != nil {
		t.Fatalf("old_string hunks must apply: %v", err)
	}
	want := "package main\n\nfunc alpha() int {\n\treturn 10\n}\n\nfunc beta() int {\n\treturn 20\n}\n"
	if got := readBack(t, path); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !strings.Contains(res.Content, "Applied 2 edits") {
		t.Fatalf("summary should count both hunks: %q", res.Content)
	}
}

func TestEdits_MixedHashAndOldStringHunks(t *testing.T) {
	content := "alpha\nbeta\ngamma\ndelta\n"
	path := writeTestFile(t, content)

	if _, err := multiEditCall(t, path, []any{
		map[string]any{"start_hash": "1:" + lineHash("alpha"), "new_string": "ALPHA"},
		map[string]any{"old_string": "gamma", "new_string": "GAMMA"},
	}); err != nil {
		t.Fatalf("a hash hunk and an old_string hunk must coexist: %v", err)
	}
	if got, want := readBack(t, path), "ALPHA\nbeta\nGAMMA\ndelta\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEdits_OldStringHunkIsAtomicOnAmbiguity(t *testing.T) {
	content := "dup\nkeep\ndup\n"
	path := writeTestFile(t, content)

	_, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "keep", "new_string": "KEEP"},
		map[string]any{"old_string": "dup", "new_string": "DUP"},
	})
	if err == nil {
		t.Fatal("a repeated old_string must fail, not replace the first match")
	}
	if !strings.Contains(err.Error(), "edits[1]") || !strings.Contains(err.Error(), "matches 2 times") {
		t.Fatalf("error must name the hunk and the count: %v", err)
	}
	if got := readBack(t, path); got != content {
		t.Fatalf("the good hunk must not land either: %q", got)
	}
}

func TestEdits_OldStringMissReportsNearestMiss(t *testing.T) {
	content := "func f() {\n\t// a real comment\n}\n"
	path := writeTestFile(t, content)

	_, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "func f() {\n\t// a remembered comment\n}", "new_string": "func f() {}\n"},
	})
	if err == nil {
		t.Fatal("a retyped old_string must fail")
	}
	if !strings.Contains(err.Error(), "differs at line") {
		t.Fatalf("hunk misses should carry the near-miss hint like old_string mode: %v", err)
	}
}

func TestEdits_OldStringHunksOverlapRejected(t *testing.T) {
	content := "alpha beta gamma\n"
	path := writeTestFile(t, content)

	_, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "alpha beta", "new_string": "X"},
		map[string]any{"old_string": "beta gamma", "new_string": "Y"},
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping byte spans must be rejected: %v", err)
	}
	if got := readBack(t, path); got != content {
		t.Fatalf("nothing should be written: %q", got)
	}
}

func TestEdits_TwoOldStringHunksInOneLine(t *testing.T) {
	// Disjoint spans inside one line are legal: old_string hunks are judged by
	// byte span, not by the whole lines a hash hunk would replace.
	content := "left middle right\n"
	path := writeTestFile(t, content)

	if _, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "left", "new_string": "LEFT"},
		map[string]any{"old_string": "right", "new_string": "RIGHT"},
	}); err != nil {
		t.Fatalf("disjoint spans in one line must apply: %v", err)
	}
	if got, want := readBack(t, path), "LEFT middle RIGHT\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEdits_HunkWithNoLocatorNamesOldString(t *testing.T) {
	path := writeTestFile(t, "alpha\n")
	_, err := multiEditCall(t, path, []any{map[string]any{"new_string": "beta"}})
	if err == nil || !strings.Contains(err.Error(), "old_string") {
		t.Fatalf("the error must offer old_string as a locator: %v", err)
	}
}

func TestEdits_OldStringHunkTolerantMatchIsReported(t *testing.T) {
	content := "func f() {\n\tif ok && ready {\n\t\treturn\n\t}\n}\n"
	path := writeTestFile(t, content)

	// Spaces where the file has a tab: the whitespace layer must still catch it
	// inside a hunk, and say so, since that note is what Phase 3 counts.
	res, err := multiEditCall(t, path, []any{
		map[string]any{"old_string": "    if ok && ready {", "new_string": "\tif ok && ready && fresh {"},
	})
	if err != nil {
		t.Fatalf("whitespace-tolerant match must work inside a hunk: %v", err)
	}
	if !strings.Contains(res.Content, "whitespace-tolerant match") {
		t.Fatalf("the tolerance note must reach the reply: %q", res.Content)
	}
	if !strings.Contains(readBack(t, path), "ok && ready && fresh") {
		t.Fatalf("edit did not land: %q", readBack(t, path))
	}
}

// --- read side: a whole-file read carries hashes too ---

func TestReadFile_WholeFileIsNumberedByDefault(t *testing.T) {
	path := writeTestFile(t, "alpha\nbeta\n")
	res := readCall(t, map[string]any{"path": path})
	if want := numberedFileText("alpha", "beta"); res.Content != want {
		t.Fatalf("whole-file read = %q, want %q", res.Content, want)
	}
	// The prefix it printed must be usable as an edit locator with no re-read.
	if _, err := hashEditCall(t, path, "1:"+lineHash("alpha"), "", "ALPHA"); err != nil {
		t.Fatalf("hash from a whole-file read must resolve: %v", err)
	}
}

func TestReadFile_WholeFileRawOnRequest(t *testing.T) {
	path := writeTestFile(t, "alpha\nbeta\n")
	res := readCall(t, map[string]any{"path": path, "line_numbers": false})
	if res.Content != "alpha\nbeta\n" {
		t.Fatalf("line_numbers=false must still return raw text: %q", res.Content)
	}
}
