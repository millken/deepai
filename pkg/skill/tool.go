package skill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// SkillTool returns a models.Tool that allows an LLM to invoke skills.
// Register this tool in the agent's tool registry alongside other tools.
func SkillTool(executor *Executor) models.Tool {
	return models.Tool{
		Name:        "skill",
		Description: "调用专业技能。当用户请求匹配某个技能时使用。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "技能名称",
					"enum":        executor.registry.AvailableNames(),
				},
				"arguments": map[string]any{
					"type":        "string",
					"description": "传递给技能的参数",
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

		// Return the rendered system prompt content as the result.
		// The caller (agent framework) should use this to configure and run the agent.
		var parts []string
		parts = append(parts, fmt.Sprintf("技能 %s 已加载", name))
		if cfg.RunInSubagent {
			parts = append(parts, fmt.Sprintf("运行模式: subagent (%s)", cfg.AgentType))
		}
		if len(cfg.AllowedTools) > 0 {
			parts = append(parts, fmt.Sprintf("免审批工具: %s", strings.Join(cfg.AllowedTools, ", ")))
		}
		if cfg.Model != "" {
			parts = append(parts, fmt.Sprintf("模型: %s", cfg.Model))
		}

		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusCompleted,
			Content:  strings.Join(parts, "\n"),
			Data: map[string]any{
				"skill_name":      name,
				"system_prompt":   cfg.SystemPrompt,
				"allowed_tools":   cfg.AllowedTools,
				"model":           cfg.Model,
				"max_turns":       cfg.MaxTurns,
				"temperature":     cfg.Temperature,
				"effort":          cfg.Effort,
				"run_in_subagent": cfg.RunInSubagent,
				"agent_type":      cfg.AgentType,
			},
			Duration: time.Since(start),
		}, nil
	}
}
