package clarification

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestAskClarificationToolAutonomousSkipsUI(t *testing.T) {
	// Autonomous mode must short-circuit even when a CLI UserInteraction is
	// attached to the context, otherwise unattended runs would block on stdin.
	tool := AskClarificationToolWithMode(true)

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
	// Non-interactive mode (no UI attached, not autonomous): the "prompt"
	// alias must still satisfy the question requirement and fall through to
	// the best-judgment fallback content.
	tool := AskClarificationToolWithMode(false)

	result, err := tool.Handler(context.Background(), models.ToolCall{
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
	if !strings.Contains(result.Content, "No user interaction available") {
		t.Fatalf("expected non-interactive fallback content, got %q", result.Content)
	}
}

type failingUI struct{ t *testing.T }

func (f failingUI) AskQuestion(_ context.Context, _ string, _ []string) (string, error) {
	f.t.Fatal("autonomous mode must not invoke UI.AskQuestion")
	return "", nil
}
