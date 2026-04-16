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

func TestReadFileHandlerResolvesThreadVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-file-tool"
	target := filepath.Join(root, "threads", threadID, "user-data", "uploads", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := ReadFileHandler(ctx, models.ToolCall{
		ID:   "call-1",
		Name: "read_file",
		Arguments: map[string]any{
			"path": "/mnt/user-data/uploads/notes.txt",
		},
	})
	if err != nil {
		t.Fatalf("ReadFileHandler() error = %v", err)
	}
	if result.Content != "hello" {
		t.Fatalf("content=%q want hello", result.Content)
	}
}

func TestWriteFileHandlerWritesToResolvedVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-write-tool"
	ctx := tools.WithThreadID(context.Background(), threadID)
	_, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-2",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": "created",
		},
	})
	if err != nil {
		t.Fatalf("WriteFileHandler() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "threads", threadID, "user-data", "uploads", "out.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "created" {
		t.Fatalf("content=%q want created", string(data))
	}
}

func TestWriteFileHandlerAppendsContent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-append-tool"
	ctx := tools.WithThreadID(context.Background(), threadID)
	_, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-append",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": "hello",
		},
	})
	if err != nil {
		t.Fatalf("initial WriteFileHandler() error = %v", err)
	}
	_, err = WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-append-2",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": " world",
			"append":  true,
		},
	})
	if err != nil {
		t.Fatalf("append WriteFileHandler() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "threads", threadID, "user-data", "uploads", "out.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("content=%q want hello world", string(data))
	}
}

func TestGlobHandlerResolvesVirtualPattern(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-glob-tool"
	dir := filepath.Join(root, "threads", threadID, "user-data", "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := GlobHandler(ctx, models.ToolCall{
		ID:   "call-3",
		Name: "glob",
		Arguments: map[string]any{
			"pattern": "/mnt/user-data/uploads/*.txt",
		},
	})
	if err != nil {
		t.Fatalf("GlobHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "a.txt") || !strings.Contains(result.Content, "b.txt") {
		t.Fatalf("glob result=%q", result.Content)
	}
}

func TestGlobHandlerHonorsRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	result, err := GlobHandler(context.Background(), models.ToolCall{
		ID:   "call-4",
		Name: "glob",
		Arguments: map[string]any{
			"root":    root,
			"pattern": "nested/*.txt",
		},
	})
	if err != nil {
		t.Fatalf("GlobHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "note.txt") {
		t.Fatalf("glob result=%q, want note.txt", result.Content)
	}
}
