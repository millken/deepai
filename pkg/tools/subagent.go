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

// AgentOption advertises one available agent type in the task tool description.
// Local to pkg/tools to avoid an agent↔tools import cycle; callers convert from
// pkg/agent.AgentInfo.
type AgentOption struct {
	Type        string
	Description string
}

func TaskTool(pool taskPool, agents []AgentOption) models.Tool {
	desc := "Spawn a bounded subagent, stream lifecycle updates, and return its final result."
	if extras := formatAgentOptions(agents); extras != "" {
		desc += " " + extras
	}
	return models.Tool{
		Name:        "task",
		Description: desc,
		Groups:      []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description":   map[string]any{"type": "string", "description": "Short description of the delegated task"},
				"prompt":        map[string]any{"type": "string", "description": "Detailed instructions for the subagent"},
				"subagent_type": map[string]any{"type": "string", "description": "Deprecated: use agent_type instead"},
				"agent_type":    map[string]any{"type": "string", "description": "Agent type (e.g. coder, bash, security-reviewer). Takes precedence over subagent_type."},
				"model":         map[string]any{"type": "string", "description": "Model alias for this subagent (e.g. 'fast', 'smart'). Optional; defaults to the agent type config or the main model."},
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
			model, _ := call.Arguments["model"].(string)

			// Resolve agent type: agent_type > subagent_type > general-purpose
			agentType, _ := call.Arguments["agent_type"].(string)
			if agentType == "" {
				agentType = string(parseSubagentType(call.Arguments["subagent_type"]))
			}

			task, err := pool.StartTask(ctx, strings.TrimSpace(description), strings.TrimSpace(prompt), subagent.SubagentConfig{
				AgentType: agentType,
				MaxTurns:  maxTurns,
				Model:     strings.TrimSpace(model),
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

// formatAgentOptions renders the available-agent list for the task tool
// description: "Available agent_type values: type1 — desc; type2 — desc."
// Descriptions are capped to keep the tool description bounded.
func formatAgentOptions(agents []AgentOption) string {
	if len(agents) == 0 {
		return ""
	}
	const maxDesc = 100
	parts := make([]string, 0, len(agents))
	for _, a := range agents {
		d := strings.TrimSpace(a.Description)
		d = strings.ReplaceAll(d, "\n", " ")
		if len([]rune(d)) > maxDesc {
			d = string([]rune(d)[:maxDesc-1]) + "…"
		}
		if d == "" {
			parts = append(parts, a.Type)
		} else {
			parts = append(parts, a.Type+" — "+d)
		}
	}
	return "Available agent_type values: " + strings.Join(parts, "; ") + "."
}
