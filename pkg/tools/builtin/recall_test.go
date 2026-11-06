package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestRecallOutputPath_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := recallOutputPath(dir, ""); err == nil {
		t.Fatal("empty ref")
	}
	if _, err := recallOutputPath(dir, ".."); err == nil {
		t.Fatal("..")
	}
	if _, err := recallOutputPath(dir, "nested/id"); err == nil {
		t.Fatal("nested id")
	}
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if _, err := recallOutputPath(dir, outside); err == nil {
		t.Fatal("absolute path outside the directory")
	}
	inside := filepath.Join(dir, "call-1.txt")
	if err := os.WriteFile(inside, []byte("full"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := recallOutputPath(dir, "call-1")
	if err != nil || got != inside {
		t.Fatalf("bare id: path=%q err=%v", got, err)
	}
	got, err = recallOutputPath(dir, inside)
	if err != nil || got != inside {
		t.Fatalf("absolute path: path=%q err=%v", got, err)
	}

	tool := RecallOutputTool(dir)
	res, err := tool.Handler(context.Background(), models.ToolCall{
		ID: "c", Name: "recall_output", Arguments: map[string]any{"ref": "call-1"},
	})
	if err != nil || res.Content != "full" {
		t.Fatalf("content=%q err=%v", res.Content, err)
	}
}
