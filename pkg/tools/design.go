package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/orchestrator"
)

func DesignTaskTool(pool taskPool) models.Tool {
	return models.Tool{
		Name:        "design_task",
		Description: "Produce a vetted implementation plan via a design panel: several proposer subagents independently draft approaches (from diverse angles, in parallel), then a judge subagent critiques them and synthesizes one consolidated plan. Use BEFORE implementing something non-trivial; feed the returned plan to implement_task. Read-only — writes no files.",
		Groups:      []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":    map[string]any{"type": "string", "description": "The task to design an approach for"},
				"proposers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Proposer agent types (default [architect, coder]). List more (or repeats) for a wider panel; each gets a different angle."},
				"judge":     map[string]any{"type": "string", "description": "Judge agent type that synthesizes the final plan (default architect)"},
			},
			"required": []any{"prompt"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			if pool == nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("subagent pool is required")
			}
			prompt, _ := call.Arguments["prompt"].(string)
			if strings.TrimSpace(prompt) == "" {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("prompt is required")
			}
			judge, _ := call.Arguments["judge"].(string)

			res, err := orchestrator.Design(ctx, orchestrator.DesignConfig{
				Proposers: stringsFromArg(call.Arguments["proposers"]),
				Judge:     strings.TrimSpace(judge),
			}, prompt, poolRunner{pool: pool})
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed, Error: err.Error()}, err
			}

			out := map[string]any{
				"plan":           res.Plan,
				"rationale":      res.Rationale,
				"proposal_count": len(res.Proposals),
			}
			data, _ := json.Marshal(out)
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: string(data)}, nil
		},
	}
}
