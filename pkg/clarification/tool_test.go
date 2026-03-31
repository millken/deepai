package clarification

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestAskClarificationTool(t *testing.T) {
	manager := NewManager(1)
	tool := AskClarificationTool(manager)

	result, err := tool.Handler(WithThreadID(context.Background(), "thread-3"), models.ToolCall{
		ID:   "call-1",
		Name: tool.Name,
		Arguments: map[string]any{
			"question": "Which approach should I use?",
			"options": []any{
				map[string]any{"label": "Fast", "value": "fast"},
				map[string]any{"label": "Thorough", "value": "thorough"},
			},
			"required": true,
		},
	})
	if err != nil {
		t.Fatalf("tool handler error = %v", err)
	}
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}

	id, _ := result.Data["id"].(string)
	if id == "" {
		t.Fatal("tool result missing clarification id")
	}

	item, ok := manager.Get(id)
	if !ok {
		t.Fatal("clarification not stored in manager")
	}
	if item.ThreadID != "thread-3" {
		t.Fatalf("thread_id = %q, want thread-3", item.ThreadID)
	}
}
