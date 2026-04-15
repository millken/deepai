package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func writeTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditFileHandler(t *testing.T) {
	path := writeTestFile(t, "line1\nline2\nline3\n")

	result, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-1",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":       path,
			"old_string": "line2",
			"new_string": "line2-edited",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}

	got, _ := os.ReadFile(path)
	want := "line1\nline2-edited\nline3\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

func TestEditFileHandler_ReplaceAll(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\nbaz\nfoo\n")

	result, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-2",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":        path,
			"old_string":  "foo",
			"new_string":  "qux",
			"replace_all": true,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}

	got, _ := os.ReadFile(path)
	want := "qux\nbar\nqux\nbaz\nqux\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

func TestEditFileHandler_NotFound(t *testing.T) {
	path := writeTestFile(t, "line1\nline2\n")

	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-3",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":       path,
			"old_string": "not-exist",
			"new_string": "whatever",
		},
	})
	if err == nil {
		t.Fatal("expected error when old_string not found")
	}
}

func TestEditFileHandler_MultipleMatch(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\n")

	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-4",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":       path,
			"old_string": "foo",
			"new_string": "baz",
		},
	})
	if err == nil {
		t.Fatal("expected error when old_string matches multiple times without replace_all")
	}
}

func TestEditFileHandler_MissingPath(t *testing.T) {
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-5",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"old_string": "a",
			"new_string": "b",
		},
	})
	if err == nil {
		t.Fatal("expected error when path is missing")
	}
}

func TestEditFileHandler_MissingOldString(t *testing.T) {
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-6",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":       "/tmp/x",
			"new_string": "b",
		},
	})
	if err == nil {
		t.Fatal("expected error when old_string is missing")
	}
}

func TestEditFileHandler_IdenticalStrings(t *testing.T) {
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:     "edit-7",
		Name:   "edit_file",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":       "/tmp/x",
			"old_string": "same",
			"new_string": "same",
		},
	})
	if err == nil {
		t.Fatal("expected error when old_string and new_string are identical")
	}
}
