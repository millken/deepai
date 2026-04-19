package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

type taskPool interface {
	StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error)
	Wait(ctx context.Context, taskID string) (*subagent.Task, error)
}

func TaskTool(pool taskPool) models.Tool {
	return models.Tool{
		Name:        "task",
		Description: "Spawn a bounded subagent, stream lifecycle updates, and return its final result.",
		Groups:      []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description":   map[string]any{"type": "string", "description": "Short description of the delegated task"},
				"prompt":        map[string]any{"type": "string", "description": "Detailed instructions for the subagent"},
				"subagent_type": map[string]any{"type": "string", "description": "Deprecated: use agent_type instead"},
				"agent_type":    map[string]any{"type": "string", "description": "Agent type (e.g. coder, bash, security-reviewer). Takes precedence over subagent_type."},
				"max_turns":     map[string]any{"type": "integer", "description": "Optional max turns override"},
			},
			"required": []any{"description", "prompt"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			if pool == nil {
				return models.ToolResult{}, fmt.Errorf("subagent pool is required")
			}

			description, _ := call.Arguments["description"].(string)
			prompt, _ := call.Arguments["prompt"].(string)
			maxTurns := intFromArg(call.Arguments["max_turns"])

			// Resolve agent type: agent_type > subagent_type > general-purpose
			agentType, _ := call.Arguments["agent_type"].(string)
			if agentType == "" {
				agentType = string(parseSubagentType(call.Arguments["subagent_type"]))
			}

			task, err := pool.StartTask(ctx, strings.TrimSpace(description), strings.TrimSpace(prompt), subagent.SubagentConfig{
				AgentType: agentType,
				MaxTurns:  maxTurns,
			})
			if err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			completed, err := pool.Wait(ctx, task.ID)
			if err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			result := models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Content:  completed.Result,
			}
			switch completed.Status {
			case subagent.TaskStatusCompleted:
				result.Status = models.CallStatusCompleted
			case subagent.TaskStatusTimedOut:
				result.Status = models.CallStatusFailed
				result.Error = completed.Error
				return result, fmt.Errorf("subagent task timed out: %s", completed.Error)
			default:
				result.Status = models.CallStatusFailed
				result.Error = completed.Error
				if result.Error == "" {
					result.Error = fmt.Sprintf("subagent task ended with status %s", completed.Status)
				}
				return result, fmt.Errorf("%s", result.Error)
			}
			return result, nil
		},
	}
}

func parseSubagentType(raw any) subagent.SubagentType {
	value, _ := raw.(string)
	switch strings.TrimSpace(value) {
	case string(subagent.SubagentBash):
		return subagent.SubagentBash
	default:
		return subagent.SubagentGeneralPurpose
	}
}

func intFromArg(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
