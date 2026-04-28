package clarification

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
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

func TestAskClarificationToolAutonomousSkipsUI(t *testing.T) {
	// Autonomous mode must short-circuit even when a CLI UserInteraction is
	// attached to the context, otherwise unattended runs would block on stdin.
	tool := AskClarificationToolWithMode(nil, true)

	ctx := tools.WithUserInteraction(context.Background(), failingUI{t: t})
	result, err := tool.Handler(ctx, models.ToolCall{
		ID:        "call-auto",
		Name:      tool.Name,
		Arguments: map[string]any{"question": "Pick one"},
	})
	if err != nil {
		t.Fatalf("autonomous handler error = %v", err)
	}
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	if !strings.Contains(result.Content, "Autonomous mode") {
		t.Fatalf("expected autonomous-mode content, got %q", result.Content)
	}
}

func TestAskClarificationTool_PromptAlias(t *testing.T) {
	manager := NewManager(1)
	tool := AskClarificationTool(manager)

	result, err := tool.Handler(WithThreadID(context.Background(), "thread-prompt-alias"), models.ToolCall{
		ID:   "call-prompt-alias",
		Name: tool.Name,
		Arguments: map[string]any{
			"prompt": "Need one answer",
		},
	})
	if err != nil {
		t.Fatalf("tool handler error = %v", err)
	}
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
}

type failingUI struct{ t *testing.T }

func (f failingUI) AskQuestion(_ context.Context, _ string, _ []string) (string, error) {
	f.t.Fatal("autonomous mode must not invoke UI.AskQuestion")
	return "", nil
}
