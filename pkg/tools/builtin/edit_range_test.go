package builtin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// edit_file's optional start_line/end_line scope matching to a line window, so
// a short old_string that repeats across the file still resolves uniquely
// without replace_all.

func editRangeCall(t *testing.T, path, oldS, newS string, extra map[string]any) (models.ToolResult, error) {
	t.Helper()
	args := map[string]any{"path": path, "old_string": oldS, "new_string": newS}
	for k, v := range extra {
		args[k] = v
	}
	return EditFileHandler(context.Background(), models.ToolCall{ID: "edit", Name: "edit_file", Arguments: args})
}

func TestEditFile_RangeDisambiguatesRepeatedText(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\nbaz\nfoo\n")

	// "foo" matches 3x file-wide, once inside lines 3-4.
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": float64(3), "end_line": float64(4),
	}); err != nil {
		t.Fatalf("ranged edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nqux\nbaz\nfoo\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_RangeStillRejectsAmbiguityInsideWindow(t *testing.T) {
	path := writeTestFile(t, "foo\nfoo\nbar\nfoo\nfoo\nfoo\n")
	_, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": float64(1), "end_line": float64(2),
	})
	if err == nil {
		t.Fatal("expected ambiguity error for 2 matches inside the window")
	}
	// The count is window-scoped (2, not 5), so the message must name the window
	// rather than the whole file.
	if !strings.Contains(err.Error(), "lines 1-2") {
		t.Fatalf("ambiguity error should name the search window: %v", err)
	}
}

func TestEditFile_RangeExcludesMatchOutsideWindow(t *testing.T) {
	path := writeTestFile(t, "alpha\nbeta\ngamma\n")

	_, err := editRangeCall(t, path, "gamma", "GAMMA", map[string]any{
		"start_line": float64(1), "end_line": float64(2),
	})
	if err == nil {
		t.Fatal("expected error: match lies outside the window")
	}
	// The error must point at where it actually matches, or the model just
	// retries the same range forever.
	if !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("error should report the real match location: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("file must be untouched, got %q", string(got))
	}
}

func TestEditFile_RangeStartLineOnlyRunsToEOF(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": float64(2),
	}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nqux\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_RangeEndLineOnlyStartsAtOne(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"end_line": float64(2),
	}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "qux\nbar\nfoo\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_RangeReplaceAllScopedToWindow(t *testing.T) {
	path := writeTestFile(t, "foo\nfoo\nfoo\nfoo\n")
	res, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": float64(2), "end_line": float64(3), "replace_all": true,
	})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nqux\nqux\nfoo\n" {
		t.Fatalf("replace_all leaked outside the window: %q", string(got))
	}
	if !strings.Contains(res.Content, "2 occurrence") {
		t.Fatalf("unexpected summary: %q", res.Content)
	}
}

func TestEditFile_RangeReportsWholeFileStartLine(t *testing.T) {
	path := writeTestFile(t, "a\nb\nc\ntarget\ne\n")
	res, err := editRangeCall(t, path, "target", "hit", map[string]any{
		"start_line": float64(3), "end_line": float64(5),
	})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	// Data start_line drives the TUI diff; it must be file coordinates, not
	// window-relative.
	if got := dataLine(res); got != 4 {
		t.Fatalf("start_line = %d, want 4 (whole-file coordinates)", got)
	}
}

func TestEditFile_RangeMatchSpanningWindowEdgeIsRejected(t *testing.T) {
	path := writeTestFile(t, "alpha\nbeta\ngamma\n")
	if _, err := editRangeCall(t, path, "beta\ngamma", "x", map[string]any{
		"start_line": float64(1), "end_line": float64(2),
	}); err == nil {
		t.Fatal("expected error: old_string runs past end_line")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("file must be untouched, got %q", string(got))
	}
}

func TestEditFile_RangeBeyondEOF(t *testing.T) {
	path := writeTestFile(t, "a\nb\n")
	_, err := editRangeCall(t, path, "a", "x", map[string]any{"start_line": float64(99)})
	if err == nil {
		t.Fatal("expected error for start_line past EOF")
	}
	if !strings.Contains(err.Error(), "2 lines") {
		t.Fatalf("error should state the real line count: %v", err)
	}
}

func TestEditFile_RangeSwappedBoundsAreNormalized(t *testing.T) {
	// read_file normalizes reversed ranges; edit_file matches that behaviour.
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": float64(3), "end_line": float64(2),
	}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nqux\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_RangeEndLineClampedToEOF(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\n")
	if _, err := editRangeCall(t, path, "bar", "baz", map[string]any{
		"start_line": float64(1), "end_line": float64(999),
	}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbaz\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_RangeComposesWithLineNumberStripping(t *testing.T) {
	// The full round trip: read a range, paste the numbered output back, and
	// scope the edit to the same range.
	path := writeTestFile(t, "foo\nbar\nfoo\nbaz\n")
	read, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": float64(3), "end_line": float64(4)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := editRangeCall(t, path, strings.TrimSuffix(read.Content, "\n"), "qux\nbaz",
		map[string]any{"start_line": float64(3), "end_line": float64(4)}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nqux\nbaz\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_UnparseableRangeIsRejected(t *testing.T) {
	// Silently ignoring a bad range would turn a scoped edit into a whole-file
	// edit — the one failure mode this feature must never have.
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": "three",
	}); err == nil {
		t.Fatal("expected error for unparseable start_line")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nfoo\n" {
		t.Fatalf("file must be untouched, got %q", string(got))
	}
}

func TestEditFile_NumericStringRangeAccepted(t *testing.T) {
	// Some providers stringify numeric arguments.
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editRangeCall(t, path, "foo", "qux", map[string]any{
		"start_line": "3",
	}); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo\nbar\nqux\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditFile_NoRangeArgsKeepsWholeFileBehaviour(t *testing.T) {
	path := writeTestFile(t, "foo\nbar\nfoo\n")
	if _, err := editCall(t, path, "foo", "qux", false); err == nil {
		t.Fatal("without a range, repeated text must still be ambiguous")
	}
}
