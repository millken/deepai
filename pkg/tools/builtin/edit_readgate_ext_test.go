package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func gateGrepCall(path string, extra map[string]any) models.ToolCall {
	args := map[string]any{"pattern": "alpha", "path": path}
	for k, v := range extra {
		args[k] = v
	}
	return models.ToolCall{ID: "g", Name: "grep", Arguments: args}
}

func gateMapCall(path string, extra map[string]any) models.ToolCall {
	args := map[string]any{"path": path}
	for k, v := range extra {
		args[k] = v
	}
	return models.ToolCall{ID: "cm", Name: "code_map", Arguments: args}
}

func gateWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A grep hit hands the model real line text and a real hash, so per the
// file:line:hash: contract it must satisfy the read gate without a read_file.
func TestEditReadGate_GrepHitCountsAsRead(t *testing.T) {
	ctx := gateCtx(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	gateWrite(t, path, "alpha\nbeta\n")
	if _, err := GrepHandler(ctx, gateGrepCall(dir, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit after grep hit should pass, got %v", err)
	}
}

// files_only returns names and counts only — no line text, no hashes — so it
// must not satisfy the gate.
func TestEditReadGate_GrepFilesOnlyDoesNotCount(t *testing.T) {
	ctx := gateCtx(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	gateWrite(t, path, "alpha\nbeta\n")
	if _, err := GrepHandler(ctx, gateGrepCall(dir, map[string]any{"files_only": true})); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error after files_only grep, got %v", err)
	}
}

func TestEditReadGate_GrepNoMatchDoesNotCount(t *testing.T) {
	ctx := gateCtx(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	gateWrite(t, path, "alpha\nbeta\n")
	if _, err := GrepHandler(ctx, gateGrepCall(dir, map[string]any{"pattern": "zzz"})); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error after no-match grep, got %v", err)
	}
}

// A binary read tells the model nothing editable; it must not count.
func TestEditReadGate_BinaryReadDoesNotCount(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "blob.bin")
	gateWrite(t, path, "alpha\x00\x01\x02beta\x00")
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error after binary read, got %v", err)
	}
}

// The structural outline (head+symbols+tail) hides most of the file; an
// old_string edit against unseen lines must stay blocked.
func TestEditReadGate_OutlineReadDoesNotCount(t *testing.T) {
	old := ReadFileOutlineThreshold
	ReadFileOutlineThreshold = 10
	defer func() { ReadFileOutlineThreshold = old }()

	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "big.go")
	var b strings.Builder
	b.WriteString("package big\n\nconst Alpha = \"alpha\"\n")
	for i := 0; i < 12; i++ {
		b.WriteString("// filler line to push past the outline threshold\n")
	}
	gateWrite(t, path, b.String())
	res, err := ReadFileHandler(ctx, gateReadCall(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "structural outline") && !strings.Contains(res.Content, "outline") {
		t.Fatalf("expected outline response, got: %.120s", res.Content)
	}
	_, err = EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error after outline read, got %v", err)
	}
}

// Range reads return the file's own text for the requested span; that has
// always satisfied the gate and must keep doing so.
func TestEditReadGate_RangeReadStillCounts(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	gateWrite(t, path, "alpha\nbeta\ngamma\n")
	if _, err := ReadFileHandler(ctx, models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": 1, "end_line": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit after range read should pass, got %v", err)
	}
}

// include_content inlines a file's full source — editing right after must
// work without a read_file round-trip.
func TestEditReadGate_CodeMapContentCountsAsRead(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "solo.go")
	gateWrite(t, path, "package solo\n\nconst Alpha = \"alpha\"\n")
	if _, err := CodeMapHandler(ctx, gateMapCall(path, map[string]any{"include_content": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit after code_map include_content should pass, got %v", err)
	}
}

func TestEditReadGate_CodeMapTreeDoesNotCount(t *testing.T) {
	ctx := gateCtx(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	gateWrite(t, path, "package a\n\nconst Alpha = \"alpha\"\n")
	if _, err := CodeMapHandler(ctx, gateMapCall(dir, nil)); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error after tree code_map, got %v", err)
	}
}

// A file listed without content (budget exhausted) never reached the model;
// it must not be stamped.
func TestEditReadGate_CodeMapBudgetListedOnlyDoesNotCount(t *testing.T) {
	ctx := gateCtx(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	big := filepath.Join(dir, "b.go")
	gateWrite(t, a, "package a\nconst Alpha = \"alpha\"\n")
	gateWrite(t, big, "package b\n\nconst Beta = \"alpha\"\n\n// padding to exceed the content budget\n// more padding\n// even more padding\n")
	if _, err := CodeMapHandler(ctx, gateMapCall(dir, map[string]any{"include_content": true, "max_total_bytes": 40})); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(a)); err != nil {
		t.Fatalf("edit on included file should pass, got %v", err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(big))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error on budget-listed-only file, got %v", err)
	}
}

// Reading via a symlink and editing via the real path (or vice versa) is one
// file and must resolve to one tracker key.
func TestEditReadGate_SymlinkSameKey(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	gateWrite(t, real, "alpha\nbeta\n")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ctx := gateCtx(t)
	if _, err := ReadFileHandler(ctx, gateReadCall(link)); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(real)); err != nil {
		t.Fatalf("edit via real path after read via symlink should pass, got %v", err)
	}
}

// A size-only-stable rewrite (mtime moved, size did not) must not produce a
// "was N bytes, now N bytes" non-explanation — the message has to show the
// mtime change so the model does not dismiss it as a false positive.
func TestEditReadGate_StaleMessageShowsModification(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	gateWrite(t, path, "aaaa\nbbbb\n")
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	gateWrite(t, path, "cccc\ndddd\n") // same size, different bytes, fresh mtime
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("want stale gate error, got %v", err)
	}
	if !strings.Contains(err.Error(), "modified") {
		t.Fatalf("stale error should mention modification time, got: %v", err)
	}
}
