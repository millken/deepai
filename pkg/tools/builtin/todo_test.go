package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// --- M5 todo tool: RED tests written before TodoWriteTool/TodoWriteHandler
// exist (see /Users/millken/github.com/millken/deepai's M5 brief). The tool
// is a full-table-replace: every call resends the ENTIRE list, never an
// incremental add/update — that's the whole point (it forces the model to
// re-articulate its complete plan instead of drifting via one-line patches).

func callTodoWrite(t *testing.T, todos []any) (models.ToolResult, error) {
	t.Helper()
	return TodoWriteHandler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "todo_write",
		Arguments: map[string]any{
			"todos": todos,
		},
	})
}

func TestTodoWriteHandler_ValidListReturnsData(t *testing.T) {
	result, err := callTodoWrite(t, []any{
		map[string]any{"content": "write tests", "status": "in_progress"},
		map[string]any{"content": "implement", "status": "pending"},
	})
	if err != nil {
		t.Fatalf("TodoWriteHandler() error = %v", err)
	}
	todos, ok := result.Data["todos"].([]TodoItem)
	if !ok {
		t.Fatalf("result.Data[\"todos\"] type = %T, want []TodoItem", result.Data["todos"])
	}
	if len(todos) != 2 {
		t.Fatalf("len(todos) = %d, want 2", len(todos))
	}
	if todos[0].Content != "write tests" || todos[0].Status != TodoInProgress {
		t.Fatalf("todos[0] = %+v, want {write tests in_progress}", todos[0])
	}
	if !strings.Contains(result.Content, "write tests") {
		t.Fatalf("result.Content = %q, want it to mention the task", result.Content)
	}
}

func TestTodoWriteHandler_EmptyListIsLegal(t *testing.T) {
	result, err := callTodoWrite(t, []any{})
	if err != nil {
		t.Fatalf("TodoWriteHandler() error = %v, want nil (empty list clears the plan)", err)
	}
	todos, ok := result.Data["todos"].([]TodoItem)
	if !ok {
		t.Fatalf("result.Data[\"todos\"] type = %T, want []TodoItem", result.Data["todos"])
	}
	if len(todos) != 0 {
		t.Fatalf("len(todos) = %d, want 0", len(todos))
	}
}

func TestTodoWriteHandler_RejectsEmptyContent(t *testing.T) {
	_, err := callTodoWrite(t, []any{
		map[string]any{"content": "   ", "status": "pending"},
	})
	if err == nil {
		t.Fatal("TodoWriteHandler() error = nil, want a validation error for empty content")
	}
}

func TestTodoWriteHandler_RejectsBadStatus(t *testing.T) {
	_, err := callTodoWrite(t, []any{
		map[string]any{"content": "do a thing", "status": "done_ish"},
	})
	if err == nil {
		t.Fatal("TodoWriteHandler() error = nil, want a validation error for an unrecognized status")
	}
}

// TestTodoWriteHandler_RejectsMultipleInProgress is the RED test for the
// design's hardest invariant: at most one todo may be in_progress. Two
// simultaneously "in progress" items is itself the symptom of a model that
// has lost track of what it's doing right now — the whole reason this tool
// exists — so this is rejected outright rather than merely discouraged.
func TestTodoWriteHandler_RejectsMultipleInProgress(t *testing.T) {
	_, err := callTodoWrite(t, []any{
		map[string]any{"content": "a", "status": "in_progress"},
		map[string]any{"content": "b", "status": "in_progress"},
	})
	if err == nil {
		t.Fatal("TodoWriteHandler() error = nil, want a rejection for two in_progress items")
	}
}

// TestTodoWriteHandler_RejectsTooManyItems is the RED test for review fix #5:
// nothing bounded the list size, so a model that writes a 60-item plan would
// have every one of those 60 items resent, in full, on every single
// subsequent request (formatTodoNote/RenderTodoList render the whole list
// every time — see promptbuild.go). A list this long is itself the symptom
// (it means the "aim for roughly 3-7 items" granularity guidance in
// todoUsagePrompt was ignored, not that the task genuinely needs 60 tracked
// steps), so it's rejected with guidance to split the work up, the same way
// the two-in_progress case above is rejected rather than merely discouraged.
func TestTodoWriteHandler_RejectsTooManyItems(t *testing.T) {
	todos := make([]any, maxTodoItems+1)
	for i := range todos {
		todos[i] = map[string]any{"content": fmt.Sprintf("item %d", i), "status": "pending"}
	}
	_, err := callTodoWrite(t, todos)
	if err == nil {
		t.Fatalf("TodoWriteHandler() error = nil, want a rejection for a list of %d items (limit %d)", len(todos), maxTodoItems)
	}
}

// TestTodoWriteHandler_AllowsExactlyMaxItems checks the cap is inclusive —
// exactly maxTodoItems must still succeed, only maxTodoItems+1 is rejected.
func TestTodoWriteHandler_AllowsExactlyMaxItems(t *testing.T) {
	todos := make([]any, maxTodoItems)
	for i := range todos {
		todos[i] = map[string]any{"content": fmt.Sprintf("item %d", i), "status": "pending"}
	}
	_, err := callTodoWrite(t, todos)
	if err != nil {
		t.Fatalf("TodoWriteHandler() error = %v, want nil at exactly the %d-item cap", err, maxTodoItems)
	}
}

// TestTodoWriteHandler_RejectsOverlongContent is the RED test for review fix
// #5's content-length half: an unbounded content string has the identical
// "resent every request, forever" cost as an unbounded item count.
func TestTodoWriteHandler_RejectsOverlongContent(t *testing.T) {
	_, err := callTodoWrite(t, []any{
		map[string]any{"content": strings.Repeat("a", maxTodoContentBytes+1), "status": "pending"},
	})
	if err == nil {
		t.Fatalf("TodoWriteHandler() error = nil, want a rejection for content over %d bytes", maxTodoContentBytes)
	}
}

func TestTodoWriteHandler_AllowsExactlyMaxContentLength(t *testing.T) {
	_, err := callTodoWrite(t, []any{
		map[string]any{"content": strings.Repeat("a", maxTodoContentBytes), "status": "pending"},
	})
	if err != nil {
		t.Fatalf("TodoWriteHandler() error = %v, want nil at exactly the %d-byte content cap", err, maxTodoContentBytes)
	}
}

func TestTodoWriteTool_NotParallelSafe(t *testing.T) {
	tool := TodoWriteTool()
	if tool.ParallelSafe {
		t.Fatal("TodoWriteTool().ParallelSafe = true, want false (it mutates shared agent state)")
	}
	if tool.Name != "todo_write" {
		t.Fatalf("tool.Name = %q, want todo_write", tool.Name)
	}
	if tool.Handler == nil {
		t.Fatal("tool.Handler is nil")
	}
}
