package clarification

import (
	"context"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// AskClarificationToolWithMode returns the clarification tool. When autonomous
// is true, the tool short-circuits to best-judgment content even if a CLI
// UserInteraction is attached, so unattended runs never block on stdin.
func AskClarificationToolWithMode(autonomous bool) models.Tool {
	return models.Tool{
		Name:        "ask_clarification",
		Description: "Request clarification from the user when requirements are ambiguous or confirmation is required. In CLI mode, blocks until the user answers. In non-interactive mode, proceeds with best judgment.",
		Groups:      []string{"builtin", "interaction"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type": map[string]any{
					"type":        "string",
					"description": "Clarification type: choice, text, or confirm. Defaults to choice when options are provided, otherwise text.",
				},
				"question": map[string]any{
					"type":        "string",
					"description": "Question to present to the user.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Alias of question (deprecated).",
				},
				"options": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":    map[string]any{"type": "string"},
							"label": map[string]any{"type": "string"},
							"value": map[string]any{"type": "string"},
						},
					},
				},
				"default": map[string]any{
					"type":        "string",
					"description": "Default answer or selected option value.",
				},
				"required": map[string]any{
					"type":        "boolean",
					"description": "Whether the user must answer before work continues.",
				},
			},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			question, _ := call.Arguments["question"].(string)
			if strings.TrimSpace(question) == "" {
				question, _ = call.Arguments["prompt"].(string)
			}
			if strings.TrimSpace(question) == "" {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    "question is required",
				}, fmt.Errorf("question is required")
			}

			// Autonomous mode: never block on user input; the agent must proceed
			// with its best judgment so the run can complete unattended.
			if autonomous {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusCompleted,
					Content:  "Autonomous mode: no user interaction. Proceed with your best judgment and document the assumption in your final answer.",
				}, nil
			}

			// CLI mode: synchronous — ask user directly via stdin
			if ui := tools.UserInteractionFromContext(ctx); ui != nil {
				var optionLabels []string
				if rawOptions, ok := call.Arguments["options"].([]any); ok {
					for _, raw := range rawOptions {
						if m, ok := raw.(map[string]any); ok {
							if label, _ := m["label"].(string); label != "" {
								optionLabels = append(optionLabels, label)
							}
						}
					}
				}
				answer, err := ui.AskQuestion(ctx, question, optionLabels)
				if err != nil {
					return models.ToolResult{
						CallID:   call.ID,
						ToolName: call.Name,
						Status:   models.CallStatusFailed,
						Error:    err.Error(),
					}, err
				}
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusCompleted,
					Content:  answer,
				}, nil
			}

			// Non-interactive mode: no user input available
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusCompleted,
				Content:  "No user interaction available. Proceed with your best judgment without asking the user.",
			}, nil
		},
	}
}
