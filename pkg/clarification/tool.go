package clarification

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

func AskClarificationTool(manager *Manager) models.Tool {
	return models.Tool{
		Name:        "ask_clarification",
		Description: "Request clarification from the user when requirements are ambiguous or confirmation is required.",
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
			"required": []any{"question"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			if manager == nil {
				err := fmt.Errorf("clarification manager is required")
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			req, err := parseRequest(call.Arguments)
			if err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			item, err := manager.Request(ctx, req)
			if err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			content := fmt.Sprintf("Clarification requested with ID %s. Ask the user to answer it before continuing with assumptions.", item.ID)
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusCompleted,
				Content:  content,
				Data: map[string]any{
					"id":         item.ID,
					"thread_id":  item.ThreadID,
					"type":       item.Type,
					"question":   item.Question,
					"options":    item.Options,
					"default":    item.Default,
					"required":   item.Required,
					"created_at": item.CreatedAt.Format(time.RFC3339Nano),
				},
			}, nil
		},
	}
}

func parseRequest(args map[string]any) (ClarificationRequest, error) {
	req := ClarificationRequest{
		Type:     strings.TrimSpace(stringValue(args["type"])),
		Question: strings.TrimSpace(stringValue(args["question"])),
		Default:  strings.TrimSpace(stringValue(args["default"])),
		Required: boolValue(args["required"]),
	}

	if rawOptions, ok := args["options"].([]any); ok {
		req.Options = make([]ClarificationOption, 0, len(rawOptions))
		for _, raw := range rawOptions {
			optionMap, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			req.Options = append(req.Options, ClarificationOption{
				ID:    strings.TrimSpace(stringValue(optionMap["id"])),
				Label: strings.TrimSpace(stringValue(optionMap["label"])),
				Value: strings.TrimSpace(stringValue(optionMap["value"])),
			})
		}
	}

	if req.Question == "" {
		return ClarificationRequest{}, fmt.Errorf("question is required")
	}
	return req, nil
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}
