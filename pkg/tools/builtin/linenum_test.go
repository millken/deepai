package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// The failure this file guards against: read_file's range/line_numbers modes
// emit "<lineno>\t<content>" per line, the model copies that output verbatim
// into edit_file's old_string, and every match path fails on the numeric
// prefix. See TestEditFile_OldStringCopiedFromRangeRead for the end-to-end case.

func TestEditFile_OldStringCopiedFromRangeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gradient.zig")
	src := "const std = @import(\"std\");\n\npub fn lerp(a: f32, b: f32, t: f32) f32 {\n    return a + (b - a) * t;\n}\n"
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	read, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": float64(3), "end_line": float64(5)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verbatim copy of what the model saw, prefixes and all.
	if _, err := editCall(t, path, strings.TrimSuffix(read.Content, "\n"),
		"pub fn lerp(a: f32, b: f32, t: f32) f32 {\n    return b;\n}", false); err != nil {
		t.Fatalf("edit with line-numbered old_string failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "const std = @import(\"std\");\n\npub fn lerp(a: f32, b: f32, t: f32) f32 {\n    return b;\n}\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

func TestEditFile_LineNumberedOldAndNewString(t *testing.T) {
	path := writeTestFile(t, "alpha\nbeta\ngamma\n")

	// Model numbered both sides (it often echoes the read format back).
	if _, err := editCall(t, path, "1\talpha\n2\tbeta", "1\talpha\n2\tBETA", false); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_PaddedLineNumbersStripped(t *testing.T) {
	path := writeTestFile(t, "a\nb\nc\n")

	// read_file right-aligns numbers, so wide files yield leading spaces.
	if _, err := editCall(t, path, "  2\tb\n  3\tc", "b2\nc2", false); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nb2\nc2\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_TSVContentPrefersLiteralMatch(t *testing.T) {
	// File genuinely contains "<number>\t<value>" rows. A literal match must win
	// so stripping never corrupts real tab-separated data.
	path := writeTestFile(t, "1\tapple\n2\tbanana\n3\tcherry\n")
	if _, err := editCall(t, path, "2\tbanana", "2\tblueberry", false); err != nil {
		t.Fatalf("literal TSV edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "1\tapple\n2\tblueberry\n3\tcherry\n" {
		t.Fatalf("TSV content corrupted: %q", string(got))
	}
}

func TestEditFile_NonConsecutiveNumbersNotStripped(t *testing.T) {
	// Not a read_file transcript (numbers jump), so stripping must not kick in
	// and invent a match against the bare text.
	path := writeTestFile(t, "apple\nbanana\ncherry\n")
	if _, err := editCall(t, path, "7\tapple\n19\tbanana", "x\ny", false); err == nil {
		t.Fatal("expected no match for non-consecutive numbering")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "apple\nbanana\ncherry\n" {
		t.Fatalf("file must be untouched, got %q", string(got))
	}
}

func TestEditFile_PartiallyNumberedNotStripped(t *testing.T) {
	// Only some lines carry a prefix — not a read_file transcript.
	path := writeTestFile(t, "apple\nbanana\ncherry\n")
	if _, err := editCall(t, path, "1\tapple\nbanana", "x\ny", false); err == nil {
		t.Fatal("expected no match when only some lines are numbered")
	}
}

func TestEditFile_NotFoundErrorMentionsLineNumberPrefix(t *testing.T) {
	path := writeTestFile(t, "hello world\n")
	_, err := editCall(t, path, "nonexistent snippet here", "x", false)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
	if !strings.Contains(err.Error(), "line number") {
		t.Fatalf("error should warn about line-number prefixes: %v", err)
	}
}

func TestReadFile_RangeHonorsLineNumbersFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{
			"path": path, "start_line": float64(2), "end_line": float64(4),
			"line_numbers": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "b\nc\nd\n" {
		t.Fatalf("got %q, want clean range content", res.Content)
	}
}

func TestReadFile_RangeStillNumbersByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": float64(2), "end_line": float64(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "2\tb\n3\tc\n" {
		t.Fatalf("default range mode must stay numbered, got %q", res.Content)
	}
}
