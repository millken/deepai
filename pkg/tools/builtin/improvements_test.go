package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestEditFileHandler_TabVsSpaceFallback(t *testing.T) {
	// File uses a tab; the AI passes 4 spaces — should still match.
	path := filepath.Join(t.TempDir(), "tabs.txt")
	if err := os.WriteFile(path, []byte("func foo() {\n\treturn 42\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "edit", Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "    return 42",
			"new_string": "\treturn 43",
		},
	})
	if err != nil {
		t.Fatalf("expected whitespace-tolerant match, got error: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "func foo() {\n\treturn 43\n}\n" {
		t.Fatalf("unexpected content: %q", string(got))
	}
}

func TestEditFileHandler_CRLFFallback(t *testing.T) {
	// File on disk has CRLF endings, AI sends LF.
	path := filepath.Join(t.TempDir(), "crlf.txt")
	if err := os.WriteFile(path, []byte("alpha line one\r\nbeta line two\r\ngamma line three\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "edit", Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "beta line two",
			"new_string": "BETA REPLACED",
		},
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got, _ := os.ReadFile(path)
	if want := "alpha line one\r\nBETA REPLACED\r\ngamma line three\r\n"; string(got) != want {
		t.Fatalf("got %q want %q", string(got), want)
	}
}

func TestEditFileHandler_PreservesExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hello\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "edit", Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "echo hello",
			"new_string": "echo world",
		},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("permission bits dropped, got %o want 0755", info.Mode().Perm())
	}
}

func TestReadFileHandler_LineRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{
			"path":       path,
			"start_line": float64(2),
			"end_line":   float64(4),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "2\tb\n3\tc\n4\td\n"
	if res.Content != want {
		t.Fatalf("got %q want %q", res.Content, want)
	}
}

func TestReadFileHandler_LineNumbersOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ln.txt")
	if err := os.WriteFile(path, []byte("x\ny\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{
			"path":         path,
			"line_numbers": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "1\tx\n2\ty\n3\t\n" {
		t.Fatalf("got %q", res.Content)
	}
}

func TestWriteFileHandler_PreservesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	path := filepath.Join(t.TempDir(), "k.sh")
	if err := os.WriteFile(path, []byte("old\n"), 0750); err != nil {
		t.Fatal(err)
	}
	_, err := WriteFileHandler(context.Background(), models.ToolCall{
		ID: "w", Name: "write_file",
		Arguments: map[string]any{"path": path, "content": "new\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0750 {
		t.Fatalf("perm got %o want 0750", info.Mode().Perm())
	}
}

func TestFindHandler_NameOptional(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := FindHandler(context.Background(), models.ToolCall{
		ID: "f", Name: "find",
		Arguments: map[string]any{"path": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == "No files found." || res.Content == "" {
		t.Fatalf("expected entries when name omitted, got %q", res.Content)
	}
}
