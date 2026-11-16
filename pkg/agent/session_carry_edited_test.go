package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func editToolCall(tool, pathArg, path string) models.ToolCall {
	return models.ToolCall{ID: "c1", Name: tool, Arguments: map[string]any{pathArg: path}}
}

func editToolResult(tool string, status models.CallStatus) models.ToolResult {
	return models.ToolResult{CallID: "c1", ToolName: tool, Status: status}
}

func TestRecordEditedFile(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("abs(%q): %v", p, err)
		}
		return a
	}

	tests := []struct {
		name   string
		call   models.ToolCall
		result models.ToolResult
		want   []string
	}{
		{
			name:   "completed edit_file recorded",
			call:   editToolCall("edit_file", "path", "/tmp/x/a.go"),
			result: editToolResult("edit_file", models.CallStatusCompleted),
			want:   []string{"/tmp/x/a.go"},
		},
		{
			name:   "completed write_file recorded",
			call:   editToolCall("write_file", "path", "/tmp/x/b.go"),
			result: editToolResult("write_file", models.CallStatusCompleted),
			want:   []string{"/tmp/x/b.go"},
		},
		{
			name:   "failed edit produced no change",
			call:   editToolCall("edit_file", "path", "/tmp/x/a.go"),
			result: editToolResult("edit_file", models.CallStatusFailed),
			want:   nil,
		},
		{
			name:   "non-edit tool ignored",
			call:   editToolCall("read_file", "path", "/tmp/x/a.go"),
			result: editToolResult("read_file", models.CallStatusCompleted),
			want:   nil,
		},
		{
			name:   "file_path alias accepted",
			call:   editToolCall("write_file", "file_path", "/tmp/x/c.go"),
			result: editToolResult("write_file", models.CallStatusCompleted),
			want:   []string{"/tmp/x/c.go"},
		},
		{
			name:   "missing path skipped",
			call:   models.ToolCall{ID: "c1", Name: "edit_file", Arguments: map[string]any{}},
			result: editToolResult("edit_file", models.CallStatusCompleted),
			want:   nil,
		},
		{
			name:   "relative path normalized to absolute",
			call:   editToolCall("edit_file", "path", "rel/d.go"),
			result: editToolResult("edit_file", models.CallStatusCompleted),
			want:   []string{abs("rel/d.go")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{session: NewSessionCarry()}
			a.recordEditedFile(tt.call, tt.result)
			got := a.session.EditedFiles()
			if len(got) != len(tt.want) {
				t.Fatalf("EditedFiles() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("EditedFiles() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// Criteria 17/18/19: run_code forwards the paths its script actually wrote
// via Data["edited_paths"]; the run_code branch must sit BEFORE the completed
// check so a FAILED script's already-landed writes still reach the review gate.
func TestRecordEditedFileRunCodeForwarding(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("abs(%q): %v", p, err)
		}
		return a
	}

	tests := []struct {
		name   string
		status models.CallStatus
		paths  []string
		want   []string
	}{
		{
			name:   "completed run_code paths recorded",
			status: models.CallStatusCompleted,
			paths:  []string{"/tmp/x/a.go", "rel/b.go"},
			// EditedFiles() returns sorted paths; the abs cwd prefix sorts before /tmp.
			want: []string{abs("rel/b.go"), "/tmp/x/a.go"},
		},
		{
			name:   "failed run_code still records landed writes",
			status: models.CallStatusFailed,
			paths:  []string{"/tmp/x/c.go"},
			want:   []string{"/tmp/x/c.go"},
		},
		{
			name:   "empty edited_paths ignored",
			status: models.CallStatusCompleted,
			paths:  nil,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{session: NewSessionCarry()}
			data := map[string]any{"outcome": "ok"}
			if tt.paths != nil {
				data["edited_paths"] = tt.paths
			}
			a.recordEditedFile(models.ToolCall{ID: "c1", Name: "run_code"}, models.ToolResult{
				CallID: "c1", ToolName: "run_code", Status: tt.status, Data: data,
			})
			got := a.session.EditedFiles()
			if len(got) != len(tt.want) {
				t.Fatalf("EditedFiles() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("EditedFiles() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// Non-string entries in edited_paths are skipped, not fatal.
func TestRecordEditedFileRunCodeSkipsNonStrings(t *testing.T) {
	a := &Agent{session: NewSessionCarry()}
	a.recordEditedFile(models.ToolCall{ID: "c1", Name: "run_code"}, models.ToolResult{
		CallID: "c1", ToolName: "run_code", Status: models.CallStatusFailed,
		Data: map[string]any{"edited_paths": []any{"/tmp/x/ok.go", 42, nil}},
	})
	got := a.session.EditedFiles()
	if len(got) != 1 || got[0] != "/tmp/x/ok.go" {
		t.Fatalf("EditedFiles() = %v, want [/tmp/x/ok.go]", got)
	}
}

func TestRecordEditedFileNilSessionIsNoop(t *testing.T) {
	a := &Agent{} // no carried session (e.g. subagent runs)
	a.recordEditedFile(
		editToolCall("edit_file", "path", "/tmp/a.go"),
		editToolResult("edit_file", models.CallStatusCompleted),
	)
}

func TestEditedFilesDedupSortAndClear(t *testing.T) {
	s := NewSessionCarry()
	for _, p := range []string{"/b.go", "/a.go", "/b.go"} {
		s.RecordEditedFile(p)
	}
	got := s.EditedFiles()
	want := []string{"/a.go", "/b.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("EditedFiles() = %v, want %v", got, want)
	}

	s.ClearEditedFiles()
	if got := s.EditedFiles(); got != nil {
		t.Fatalf("EditedFiles() after clear = %v, want nil", got)
	}
	// Recording after a clear starts a fresh set.
	s.RecordEditedFile("/c.go")
	if got := s.EditedFiles(); len(got) != 1 || got[0] != "/c.go" {
		t.Fatalf("EditedFiles() after clear+record = %v, want [/c.go]", got)
	}
}

func TestEditedFilesNilCarry(t *testing.T) {
	var s *SessionCarry
	if got := s.EditedFiles(); got != nil {
		t.Fatalf("nil carry EditedFiles() = %v, want nil", got)
	}
	s.ClearEditedFiles() // must not panic
}

// A fatal parallel batch's tail results really executed (their goroutines
// ran before the observation loop), so appendRemaining must feed them into
// edited-file attribution too — otherwise edits from a fatal batch escape
// the review gate.
func TestAppendRemainingRecordsEditedFiles(t *testing.T) {
	a := &Agent{session: NewSessionCarry()}
	batch := newToolBatchState(context.Background(), a, "sess", 1, newToolCallBreaker(), &Usage{}, func(AgentEvent) {}, nil)

	calls := []models.ToolCall{
		editToolCall("write_file", "path", "/tmp/tail.go"),
		editToolCall("read_file", "path", "/tmp/ignored.go"),
	}
	results := []models.ToolResult{
		editToolResult("write_file", models.CallStatusCompleted),
		editToolResult("read_file", models.CallStatusCompleted),
	}
	batch.appendRemaining(calls, results)

	got := a.session.EditedFiles()
	if len(got) != 1 || got[0] != "/tmp/tail.go" {
		t.Fatalf("EditedFiles() = %v, want [/tmp/tail.go]", got)
	}
}
