package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// Round-2 review: a relative path read must land on the same tracker key as
// an absolute-path edit — EvalSymlinks alone returns a still-relative path.
func TestEditReadGate_RelativeAndAbsoluteSameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	gateWrite(t, path, "alpha\nbeta\n")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	ctx := gateCtx(t)
	if _, err := ReadFileHandler(ctx, gateReadCall("a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit via absolute path after relative read should pass, got %v", err)
	}
}

// Round-2 review: reading a 0-byte file hands the model the whole file (the
// empty text); the gate must count it — both the default numbered read and a
// range read return early with empty content.
func TestEditReadGate_EmptyFileReadCounts(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "empty.txt")
	gateWrite(t, path, "")
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("empty-file read should satisfy the gate; want a non-gate error, got %v", err)
	}
}

func TestEditReadGate_EmptyFileRangeReadCounts(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "empty.txt")
	gateWrite(t, path, "")
	if _, err := ReadFileHandler(ctx, models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": float64(1)},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("empty-file range read should satisfy the gate; want a non-gate error, got %v", err)
	}
}
