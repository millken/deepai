package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func editCall(t *testing.T, path, oldS, newS string, replaceAll bool) (models.ToolResult, error) {
	t.Helper()
	return EditFileHandler(context.Background(), models.ToolCall{
		ID:   "edit",
		Name: "edit_file",
		Arguments: map[string]any{
			"path":        path,
			"old_string":  oldS,
			"new_string":  newS,
			"replace_all": replaceAll,
		},
	})
}

func TestEditFile_LiteralEscapeSequences(t *testing.T) {
	path := writeTestFile(t, "func foo() {\n\treturn 1\n}\n")

	// old_string arrives with literal backslash-n/backslash-t (two chars each),
	// as weaker models sometimes emit. The file has real newlines/tabs.
	res, err := editCall(t, path, "func foo() {\\n\\treturn 1\\n}", "func foo() {\n\treturn 2\n}", false)
	if err != nil {
		t.Fatalf("escape-normalized edit failed: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("result error: %s", res.Error)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "func foo() {\n\treturn 2\n}\n" {
		t.Fatalf("unexpected file content: %q", string(got))
	}
}

func TestEditFile_EscapedBothOldAndNew(t *testing.T) {
	path := writeTestFile(t, "a\nb\nc\n")
	if _, err := editCall(t, path, "a\\nb\\nc", "x\\ny\\nz", false); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "x\ny\nz\n" {
		t.Fatalf("escaped new_string not unescaped: %q", string(got))
	}
}

func TestEditFile_CRLFPreserved(t *testing.T) {
	path := writeTestFile(t, "line1\r\nline2\r\nline3\r\n")

	// Needle uses LF; file uses CRLF. Whitespace-tolerant match should locate it
	// and the written replacement must stay CRLF, not introduce mixed endings.
	if _, err := editCall(t, path, "line1\nline2\nline3", "alpha\nbeta\ngamma", false); err != nil {
		t.Fatalf("CRLF edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha\r\nbeta\r\ngamma\r\n" {
		t.Fatalf("line endings not preserved: %q", string(got))
	}
}

func TestEditFile_NotFoundGivesActionableError(t *testing.T) {
	path := writeTestFile(t, "hello world\n")
	res, err := editCall(t, path, "nonexistent snippet here", "x", false)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
	if !strings.Contains(err.Error(), "retry edit_file") {
		t.Fatalf("error not actionable: %v", err)
	}
	_ = res
}

func TestEditFile_LiteralBackslashContentStillMatches(t *testing.T) {
	// File genuinely contains backslash-n as two characters (e.g. a Go string
	// literal). The literal match must win; no spurious unescaping.
	path := writeTestFile(t, `msg := "a\nb"`+"\n")
	if _, err := editCall(t, path, `"a\nb"`, `"a\tb"`, false); err != nil {
		t.Fatalf("literal-backslash edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `msg := "a\tb"`+"\n" {
		t.Fatalf("literal backslash content corrupted: %q", string(got))
	}
}
