package skill

import (
	"context"
	"fmt"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// SkillTool returns a models.Tool that allows an LLM to invoke skills.
// Register this tool in the agent's tool registry alongside other tools.
func SkillTool(executor *Executor) models.Tool {
	return models.Tool{
		Name:        "skill",
		Description: "Invoke a specialized skill. Use when the user request matches a skill's domain.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Skill name",
					"enum":        executor.registry.AvailableNames(),
				},
				"arguments": map[string]any{
					"type":        "string",
					"description": "Arguments to pass to the skill",
				},
			},
			"required": []string{"name"},
		},
		Groups:  []string{"skill"},
		Handler: makeSkillHandler(executor),
	}
}

// SkillToolWithRegistry is a convenience that creates a SkillTool from a Registry.
func SkillToolWithRegistry(registry *Registry) models.Tool {
	return SkillTool(NewExecutor(registry))
}

func makeSkillHandler(executor *Executor) models.ToolHandler {
	return func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		name, _ := call.Arguments["name"].(string)
		if name == "" {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    "missing required argument: name",
			}, nil
		}

		args, _ := call.Arguments["arguments"].(string)

		start := time.Now()
		cfg, err := executor.Execute(ctx, name, args)
		if err != nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    err.Error(),
				Duration: time.Since(start),
			}, nil
		}

		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusCompleted,
			Content:  fmt.Sprintf("Skill %q loaded.", name),
			Data: map[string]any{
				"skill_name":    name,
				"system_prompt": cfg.SystemPrompt,
			},
			Duration: time.Since(start),
		}, nil
	}
}
