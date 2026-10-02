package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func gateCtx(t *testing.T) context.Context {
	t.Helper()
	return tools.WithReadTracker(context.Background(), tools.NewReadTracker())
}

func gateReadCall(path string) models.ToolCall {
	return models.ToolCall{ID: "r", Name: "read_file", Arguments: map[string]any{"path": path}}
}

func gateEditCall(path string) models.ToolCall {
	return models.ToolCall{
		ID: "e", Name: "edit_file",
		Arguments: map[string]any{"path": path, "old_string": "alpha", "new_string": "gamma"},
	}
}

func TestEditReadGate_BlocksUnreadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(gateCtx(t), gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error, got %v", err)
	}
}

func TestEditReadGate_HashModeAlsoGated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(gateCtx(t), models.ToolCall{
		ID: "e", Name: "edit_file",
		Arguments: map[string]any{"path": path, "start_hash": "1:abc123", "end_hash": "1:abc123", "new_string": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("want unread gate error ahead of hash resolution, got %v", err)
	}
}

func TestEditReadGate_AllowsAfterRead(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit after read should pass, got %v", err)
	}
}

func TestEditReadGate_BlocksExternalChange(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("alpha\nbeta changed a lot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("want stale-read gate error, got %v", err)
	}
}

func TestEditReadGate_AllowsAfterWriteFile(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, err := WriteFileHandler(ctx, models.ToolCall{
		ID: "w", Name: "write_file",
		Arguments: map[string]any{"path": path, "content": "alpha\nbeta\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("edit after write_file should pass, got %v", err)
	}
}

func TestEditReadGate_SecondEditAfterEditAllowed(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatal(err)
	}
	second := models.ToolCall{
		ID: "e2", Name: "edit_file",
		Arguments: map[string]any{"path": path, "old_string": "beta", "new_string": "delta"},
	}
	if _, err := EditFileHandler(ctx, second); err != nil {
		t.Fatalf("follow-up edit on own write should pass, got %v", err)
	}
}

func TestEditReadGate_NoTrackerKeepsLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EditFileHandler(context.Background(), gateEditCall(path)); err != nil {
		t.Fatalf("no tracker in ctx must keep legacy behavior, got %v", err)
	}
}
