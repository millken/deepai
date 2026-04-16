package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func AskUserQuestionHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	ui := tools.UserInteractionFromContext(ctx)
	if ui == nil {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  "No user interaction available. Proceed with your best judgment without asking the user.",
		}, nil
	}

	question, _ := call.Arguments["question"].(string)
	if strings.TrimSpace(question) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("question is required")
	}

	var options []string
	if raw, ok := call.Arguments["options"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					options = append(options, s)
				}
			}
		}
	}

	answer, err := ui.AskQuestion(ctx, question, options)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("ask user failed: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  answer,
	}, nil
}

func AskUserQuestionTool() models.Tool {
	return models.Tool{
		Name:        "ask_user",
		Description: "Ask the user a question when you need clarification or a decision. Use this when requirements are ambiguous or multiple valid approaches exist. In non-interactive mode, you will be told to decide on your own.",
		Groups:      []string{"builtin"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question to ask the user"},
				"options": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional list of choices for the user",
				},
			},
			"required": []any{"question"},
		},
		Handler: AskUserQuestionHandler,
	}
}
