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

		// M5-1 §2.3 routing, resolved BEFORE calling Execute (post-review
		// fix): Execute renders the skill body, which runs any `!`command``
		// dynamic injection in it (pkg/skill/render.go) — a REAL side
		// effect, not just text assembly. Resolving the routing decision
		// first lets branches 3/4 return without ever calling Execute, so a
		// caller with no right to this skill's body (the main agent, or an
		// unrelated subagent) can't trigger those side effects merely by
		// attempting the call — previously, Execute ran unconditionally,
		// so even the Failed branch 4 had already executed the command by
		// the time it refused.
		sk := executor.registry.Get(name)
		caller := CallerAgentTypeFromContext(ctx)

		if sk != nil && sk.Meta.IsFork() {
			switch caller {
			case sk.Meta.Agent:
				// Already running inside the bound agent's own subagent
				// (e.g. document-editor calling docx-polish on itself) —
				// falls through below to the same Execute + inline-return
				// path a non-fork skill takes.
			case "":
				// Main agent: never hand it the body, and never even render
				// it. Route it to the task tool instead, with the target
				// agent type it needs and the skill name to pass through
				// task's `skill` argument.
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusCompleted,
					Content: fmt.Sprintf(
						"Skill %q runs in a subagent. Call task with agent_type=%q, skill=%q, and put the user's request in prompt.",
						name, sk.Meta.Agent, name,
					),
					// skill_name is deliberately NOT set here (required-fix
					// 3): react.go:1031 unconditionally marks
					// Data["skill_name"] as the ActiveSkill regardless of
					// whether system_prompt is present, which would tag a
					// skill the main agent never actually loaded as active
					// and carry that mislabel across the session (memory
					// fencing/fact-source attribution).
					Data: map[string]any{
						"fork_agent_type": sk.Meta.Agent,
					},
					Duration: time.Since(start),
				}, nil
			default:
				// Some OTHER subagent tried to delegate to a fork skill
				// bound to a different agent type. Letting the handler
				// spawn a task here itself would need the pool (which this
				// package deliberately does not have —
				// AGENT_CAPABILITY_DESIGN.md §2.3) and would bypass
				// filterTaskTool's recursion guard, so refuse instead —
				// without ever calling Execute, so this rejected subagent
				// cannot trigger the skill's side effects either.
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error: fmt.Sprintf(
						"skill %q is bound to agent %q; subagents cannot delegate — do the work within your own role or report back what is needed",
						name, sk.Meta.Agent,
					),
					Duration: time.Since(start),
				}, nil
			}
		}

		// Non-fork skill, OR a fork skill whose caller already IS the bound
		// agent: proceed to Execute (render + run dynamic injection) and
		// return the body inline — react.go's AppendSystemPrompt gate reads
		// Data["system_prompt"]'s PRESENCE (a value of "" is equally safe:
		// react.go only acts when the string is also non-empty), so nothing
		// downstream of this call depends on which of these two cases it
		// was.
		cfg, err := executor.Execute(ctx, name, args)
		if err != nil {
			// sk == nil (unknown skill) reaches here too — this is
			// deliberately the ONLY place that error surfaces, so its
			// wording (Execute's own "skill %q not found") is unchanged
			// from before this fix; the model self-corrects from it.
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
