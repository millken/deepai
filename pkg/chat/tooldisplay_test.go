package chat

import (
	"strings"
	"testing"

	builtin "github.com/millken/deepai/pkg/tools/builtin"
)

// TestRenderToolDiff_TodoWrite is the RED test for M5's TUI requirement: the
// todo_write tool result must not fall through to the opaque
// ResultPreview/raw-JSON rendering every other non-edit tool gets — it must
// render as a readable checklist with the three statuses visually
// distinguished (pending/in_progress/done), following the same
// renderToolDiff dispatch edit_file/write_file already use.
func TestRenderToolDiff_TodoWrite(t *testing.T) {
	m := newTUIModel(BannerInfo{})

	data := map[string]any{
		"todos": []builtin.TodoItem{
			{Content: "write tests", Status: builtin.TodoDone},
			{Content: "implement the tool", Status: builtin.TodoInProgress},
			{Content: "wire up the TUI", Status: builtin.TodoPending},
		},
	}
	d := m.renderToolDiff("todo_write", nil, data)
	if d == "" {
		t.Fatal("renderToolDiff(\"todo_write\", ...) = \"\", want a rendered checklist")
	}
	if !strings.Contains(d, "write tests") || !strings.Contains(d, "implement the tool") || !strings.Contains(d, "wire up the TUI") {
		t.Fatalf("rendered todo list missing task content:\n%s", d)
	}
	// Three distinct statuses must produce three distinct markers, not the
	// same glyph repeated (that would defeat "visually distinguished").
	doneMarker := builtin.TodoMarker(builtin.TodoDone)
	inProgressMarker := builtin.TodoMarker(builtin.TodoInProgress)
	pendingMarker := builtin.TodoMarker(builtin.TodoPending)
	if !strings.Contains(d, doneMarker) || !strings.Contains(d, inProgressMarker) || !strings.Contains(d, pendingMarker) {
		t.Fatalf("rendered todo list missing one of the three status markers (%q/%q/%q):\n%s",
			doneMarker, inProgressMarker, pendingMarker, d)
	}

	// Empty list: still a readable message, not "" (which would fall back
	// to the opaque ResultPreview/raw-JSON path in toolEndLine).
	empty := m.renderToolDiff("todo_write", nil, map[string]any{"todos": []builtin.TodoItem{}})
	if empty == "" {
		t.Fatal("renderToolDiff(\"todo_write\", empty list) = \"\", want a non-empty \"plan cleared\"-style message")
	}
}

// TestRenderToolDiff_EditFileHashModeUsesOldText guards HASHLINE_EDIT_DESIGN
// §9: a hash-mode edit_file call carries no old_string in its arguments, so
// the diff must fall back to the handler's Data["old_text"] — otherwise the
// replacement renders as pure additions and the user sees something other
// than what happened on disk.
func TestRenderToolDiff_EditFileHashModeUsesOldText(t *testing.T) {
	m := newTUIModel(BannerInfo{})

	args := map[string]any{
		"path":       "foo.go",
		"start_hash": "12:a3f2b1",
		"end_hash":   "13:9c01d4",
		"new_string": "kept line\nnew line",
	}
	data := map[string]any{
		"start_line": 12,
		"old_text":   "kept line\nremoved line\n",
	}
	d := m.renderToolDiff("edit_file", args, data)
	if !strings.Contains(d, "removed line") {
		t.Fatalf("hash-mode diff lost the old text (no '-' rows):\n%s", d)
	}
	if !strings.Contains(d, "new line") {
		t.Fatalf("hash-mode diff lost the new text:\n%s", d)
	}
}

// TestRenderToolDiff_EditFileMultiHunk guards HASHLINE_EDIT_DESIGN §17.5: a
// multi-hunk edit renders one stacked diff block per Data["hunks"] entry, and
// a result without hunks data (older transcript) falls back to "" so the
// generic preview path takes over instead of crashing or lying.
func TestRenderToolDiff_EditFileMultiHunk(t *testing.T) {
	m := newTUIModel(BannerInfo{})

	args := map[string]any{"path": "foo.go", "edits": []any{}}
	data := map[string]any{
		"hunks": []map[string]any{
			{"start_line": 2, "old_text": "old-a\n", "new_string": "new-a"},
			{"start_line": 9, "old_text": "", "new_string": "inserted-b"},
		},
	}
	d := m.renderToolDiff("edit_file", args, data)
	for _, want := range []string{"old-a", "new-a", "inserted-b"} {
		if !strings.Contains(d, want) {
			t.Fatalf("multi-hunk diff missing %q:\n%s", want, d)
		}
	}

	if got := m.renderToolDiff("edit_file", args, map[string]any{}); got != "" {
		t.Fatalf("missing hunks data must fall back to the generic preview, got:\n%s", got)
	}
}
