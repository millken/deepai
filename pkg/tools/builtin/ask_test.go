package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// stubUserInteraction implements tools.UserInteraction for testing.
type stubUserInteraction struct {
	answer string
	err    error
}

func (s *stubUserInteraction) AskQuestion(_ context.Context, question string, options []string) (string, error) {
	return s.answer, s.err
}

func TestAskUserQuestionHandler_Interactive(t *testing.T) {
	ui := &stubUserInteraction{answer: "use approach A"}
	ctx := tools.WithUserInteraction(context.Background(), ui)

	result, err := AskUserQuestionHandler(ctx, models.ToolCall{
		ID:     "ask-1",
		Name:   "ask_user",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"question": "Which approach?",
			"options":  []any{"approach A", "approach B"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "use approach A" {
		t.Errorf("expected user answer, got: %s", result.Content)
	}
}

func TestAskUserQuestionHandler_NonInteractive(t *testing.T) {
	// No UserInteraction in context
	result, err := AskUserQuestionHandler(context.Background(), models.ToolCall{
		ID:     "ask-2",
		Name:   "ask_user",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"question": "Which approach?",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "best judgment") {
		t.Errorf("expected fallback message, got: %s", result.Content)
	}
}

func TestAskUserQuestionHandler_MissingQuestion(t *testing.T) {
	ui := &stubUserInteraction{}
	ctx := tools.WithUserInteraction(context.Background(), ui)

	_, err := AskUserQuestionHandler(ctx, models.ToolCall{
		ID:     "ask-3",
		Name:   "ask_user",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"options": []any{"A", "B"},
		},
	})
	if err == nil {
		t.Fatal("expected error when question is missing")
	}
}
