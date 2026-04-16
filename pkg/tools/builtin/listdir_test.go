package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestListDirHandler(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "subdir"), 0755)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package main"), 0644)

	result, err := ListDirHandler(context.Background(), models.ToolCall{
		ID:     "ls-1",
		Name:   "list_dir",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}

	// Should list all entries
	if !contains(result.Content, "a.txt") {
		t.Errorf("missing a.txt, got: %s", result.Content)
	}
	if !contains(result.Content, "b.go") {
		t.Errorf("missing b.go, got: %s", result.Content)
	}
	if !contains(result.Content, "subdir") {
		t.Errorf("missing subdir, got: %s", result.Content)
	}
}

func TestListDirHandler_DirsFirst(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "zzz.txt"), []byte("file"), 0644)
	os.MkdirAll(filepath.Join(root, "aaa"), 0755)

	result, err := ListDirHandler(context.Background(), models.ToolCall{
		ID:     "ls-2",
		Name:   "list_dir",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), result.Content)
	}
	// Directory should come first
	if !strings.Contains(lines[0], "drw-") || !strings.Contains(lines[0], "aaa") {
		t.Errorf("expected dir 'aaa' first, got: %s", lines[0])
	}
	if !strings.Contains(lines[1], "-rw-") || !strings.Contains(lines[1], "zzz.txt") {
		t.Errorf("expected file 'zzz.txt' second, got: %s", lines[1])
	}
}

func TestListDirHandler_NotDir(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	os.WriteFile(filePath, []byte("hello"), 0644)

	_, err := ListDirHandler(context.Background(), models.ToolCall{
		ID:     "ls-3",
		Name:   "list_dir",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": filePath,
		},
	})
	if err == nil {
		t.Fatal("expected error when listing a file instead of directory")
	}
}

func TestListDirHandler_DefaultPath(t *testing.T) {
	result, err := ListDirHandler(context.Background(), models.ToolCall{
		ID:     "ls-4",
		Name:   "list_dir",
		Status: models.CallStatusPending,
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should list current directory without error
	if result.Content == "" {
		t.Error("expected non-empty output for current directory")
	}
}

func TestListDirHandler_EmptyDir(t *testing.T) {
	root := t.TempDir()

	result, err := ListDirHandler(context.Background(), models.ToolCall{
		ID:     "ls-5",
		Name:   "list_dir",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "" {
		t.Errorf("expected empty output for empty dir, got: %s", result.Content)
	}
}
