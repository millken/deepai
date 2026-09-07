package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// The three legal status values for a TodoItem. Any other string is rejected
// by TodoWriteHandler — an enum, not free text, because the state machine
// this tool exists to enforce (at most one thing "in progress" at a time)
// only works if the model can't invent a fourth state that bypasses it.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoDone       = "done"
)

// maxTodoItems and maxTodoContentBytes bound the cost of the full-table
// replace semantics (see TodoWriteTool's doc comment): every write, AND the
// per-request turn-injection re-render (pkg/agent's formatTodoNote), resends
// the WHOLE list, every time — so an unbounded list means an unbounded
// per-request cost for the life of the plan, not just a one-time write.
// Review fix #5: a list this long (or an item this verbose) is itself the
// symptom of ignoring the "aim for roughly 3-7 items" granularity guidance
// in todoUsagePrompt, not evidence the task genuinely needs it, so both are
// rejected outright with guidance to split the work up — same posture as
// the two-in_progress rejection below. 30 items and 500 bytes are generous
// (a real plan is usually well under both); they exist to catch a model that
// has stopped using the tool as intended, not to constrain normal use.
const (
	maxTodoItems        = 30
	maxTodoContentBytes = 500
)

// TodoItem is one entry of the agent's current task list. It is the element
// type of ToolResult.Data["todos"] returned by TodoWriteHandler, consumed
// directly (same concrete Go type, no re-marshaling) by pkg/agent's turn
// injection and pkg/chat's TUI rendering.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoWriteTool replaces the agent's entire task list. This is deliberately
// a full-table-replace, not an incremental add/update/remove API: an
// incremental API lets a model patch one item and silently let the rest of
// the plan go stale in its own "memory" of what it wrote earlier. Forcing a
// full resend on every call means the model must re-derive and re-state the
// COMPLETE plan each time — which is the actual mechanism that catches a
// model that has started to drift, not just a nicer progress display for
// the human watching.
func TodoWriteTool() models.Tool {
	return models.Tool{
		Name: "todo_write",
		Description: "Replace your ENTIRE task list with the one given here (not an incremental patch — resend every " +
			"item every time, including ones already done or not yet started). Use this to write out a plan before " +
			"starting a multi-step task, then call it again after each step to move it to done and the next one to " +
			"in_progress. At most one item may be in_progress at a time. When the task is fully complete, call this " +
			"once more with every item marked done — do not clear the list (pass an empty array) to signal " +
			"completion; an empty list is indistinguishable from a task that was never planned. Pass an empty list " +
			"only to actually abandon the current plan (e.g. the user changed direction). Aim for roughly 3-7 " +
			"items (hard limit 30) — if a task needs more, it needs fewer, higher-level steps, not one item per action.",
		Groups:       []string{"builtin", "planning"},
		ParallelSafe: false, // mutates the agent's shared todo-list state
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "The full, complete task list — every item, not just the one that changed.",
					"maxItems":    maxTodoItems,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{
								"type":        "string",
								"description": "Short description of one task",
								"maxLength":   maxTodoContentBytes,
							},
							"status": map[string]any{"type": "string", "enum": []any{TodoPending, TodoInProgress, TodoDone}},
						},
						"required": []any{"content", "status"},
					},
				},
			},
			"required": []any{"todos"},
		},
		Handler: TodoWriteHandler,
	}
}

// TodoWriteHandler validates and normalizes the replacement todo list. It is
// a pure function of its arguments — it holds no state of its own and reads
// none — because full-table-replace semantics mean every call is entirely
// self-contained; the caller (react.go's tool-result handling) is what
// carries the returned list onto Agent/SessionCarry state and rebuilds the
// turn injection, mirroring how the "skill" tool's result is applied.
func TodoWriteHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	raw, _ := call.Arguments["todos"].([]any)
	// Reject an oversized list before touching any item: see maxTodoItems'
	// doc comment — this cost is paid on EVERY subsequent request (the turn
	// injection re-renders the whole list every time), not just this call.
	if len(raw) > maxTodoItems {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name},
			fmt.Errorf("todos has %d items, over the %d-item limit — split the work into fewer, higher-level "+
				"steps (aim for roughly 3-7) instead of listing every individual action", len(raw), maxTodoItems)
	}
	todos := make([]TodoItem, 0, len(raw))
	inProgress := 0
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name},
				fmt.Errorf("todos[%d] must be an object with content and status", i)
		}
		content, _ := m["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name},
				fmt.Errorf("todos[%d].content is required and must be non-empty", i)
		}
		if len(content) > maxTodoContentBytes {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name},
				fmt.Errorf("todos[%d].content is %d bytes, over the %d-byte limit — shorten it to a short task "+
					"description, not a full explanation", i, len(content), maxTodoContentBytes)
		}
		status, _ := m["status"].(string)
		switch status {
		case TodoPending, TodoInProgress, TodoDone:
		default:
			return models.ToolResult{CallID: call.ID, ToolName: call.Name},
				fmt.Errorf("todos[%d].status = %q, must be one of %q, %q, %q", i, status, TodoPending, TodoInProgress, TodoDone)
		}
		if status == TodoInProgress {
			inProgress++
		}
		todos = append(todos, TodoItem{Content: content, Status: status})
	}
	// At most one in_progress: two things simultaneously "in progress" is
	// itself the symptom this tool exists to catch (a model that has lost
	// track of what it's doing right now), so it is rejected outright
	// rather than merely discouraged in the description text.
	if inProgress > 1 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name},
			fmt.Errorf("at most one todo may be in_progress at a time, got %d — finish or park the current one before starting another", inProgress)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  RenderTodoList(todos),
		Data:     map[string]any{"todos": todos},
	}, nil
}

// RenderTodoList formats a todo list as a plain-text checklist, shared by
// TodoWriteHandler's ToolResult.Content (what the model sees in its own
// transcript) and pkg/agent's turn-injection formatting (what the model
// sees on every SUBSEQUENT request, regardless of whether this call's
// tool_result is still in the visible history window).
func RenderTodoList(todos []TodoItem) string {
	if len(todos) == 0 {
		return "(task list is empty)"
	}
	var b strings.Builder
	for i, t := range todos {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s %s", i+1, TodoMarker(t.Status), t.Content)
	}
	return b.String()
}

// TodoMarker returns the checklist marker for one status. Shared by
// RenderTodoList (plain text, in this package) and pkg/chat's TUI rendering
// (which colors the same three markers instead of just printing them) — the
// one exported implementation both use instead of each reinventing the
// three-state convention.
func TodoMarker(status string) string {
	switch status {
	case TodoDone:
		return "[x]"
	case TodoInProgress:
		return "[~]"
	default:
		return "[ ]"
	}
}
