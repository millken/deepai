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
	desc := "Spawn a subagent, stream lifecycle updates, and return its final result. Multiple task calls issued in one turn run concurrently."
	if extras := formatAgentOptions(agents); extras != "" {
		desc += " " + extras
	}
	return models.Tool{
		Name: "task",
		// ParallelSafe: a batch of task calls issued in one assistant turn is
		// safe to run concurrently. There is no local concurrency limiter
		// (pkg/subagent/pool.go documents why — the provider's own rate
		// limiting governs parallelism); each subagent gets its own isolated
		// tool registry/agent instance, and this handler only calls
		// StartTask+Wait — it holds no shared mutable state of its own that
		// concurrent invocations could race on.
		//
		// This is a handler-level thread-safety guarantee only: spawned
		// subagents share the working tree and the git index, so concurrent
		// git operations across subagents (e.g. git_auto_commit staging or
		// committing at the same time) can still interleave and race at the
		// filesystem/git level. That hazard is mitigated by prompt-level
		// guidance (delegationStrategy's "Parallel delegation" section tells
		// the model not to fan out concurrent git operations); it is an
		// accepted risk, not something this handler enforces.
		ParallelSafe: true,
		Description:  desc,
		Groups:       []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description":    map[string]any{"type": "string", "description": "Short description of the delegated task"},
				"prompt":         map[string]any{"type": "string", "description": "Detailed instructions for the subagent"},
				"subagent_type":  map[string]any{"type": "string", "description": "Deprecated: use agent_type instead"},
				"agent_type":     map[string]any{"type": "string", "description": "Agent type (e.g. coder, bash, security-reviewer). Takes precedence over subagent_type."},
				"model":          map[string]any{"type": "string", "description": "Model alias for this subagent (e.g. 'fast', 'smart'). Optional; defaults to the agent type config or the main model."},
				"max_tool_calls": map[string]any{"type": "integer", "description": "Optional cap on the number of tool calls this subagent may execute (0 = no cap). On exhaustion the subagent wraps up with a final answer instead of failing."},
				"max_turns":      map[string]any{"type": "integer", "description": "Deprecated alias for max_tool_calls"},
				"token_budget":   map[string]any{"type": "integer", "description": "Optional max total tokens for this subagent; 0 = unlimited"},
				"context_files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Repo-relative or absolute file paths whose contents are injected into the subagent's first message. Use for the files the subagent must read anyway; keeps its context focused.",
				},
			},
			"required": []any{"description", "prompt"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			if pool == nil {
				return models.ToolResult{}, fmt.Errorf("subagent pool is required")
			}

			description, _ := call.Arguments["description"].(string)
			prompt, _ := call.Arguments["prompt"].(string)
			// Key presence (not a 0 sentinel) decides the legacy fallback: an
			// explicit max_tool_calls — including an explicit 0 ("no cap") —
			// always wins; max_turns is read only when the new key is absent,
			// mirroring the YAML loader's precedence.
			maxToolCalls := 0
			if raw, ok := call.Arguments["max_tool_calls"]; ok && raw != nil {
				maxToolCalls = intFromArg(raw)
			} else if raw, ok := call.Arguments["max_turns"]; ok && raw != nil {
				maxToolCalls = intFromArg(raw)
			}
			model, _ := call.Arguments["model"].(string)
			tokenBudget := intFromArg(call.Arguments["token_budget"])

			// Parent-budget passthrough (plan §M2.2 carry-forward): a parent
			// agent running under its own MaxTokensBudget injects its
			// remaining budget into ctx once per tool-dispatch batch
			// (react.go's Run, via tools.WithRemainingTokenBudget) so a
			// subagent spawned here can't be handed an unlimited budget
			// underneath a budget-constrained parent. The parent remaining
			// caps an explicit token_budget arg, and becomes the default
			// when no explicit arg was given — i.e. effective budget = min
			// of whichever of {explicit, parent remaining} are nonzero. A
			// parent with no remaining budget in play at all (ctx never
			// carries one, e.g. the parent itself has no MaxTokensBudget)
			// leaves tokenBudget completely unchanged.
			//
			// A PRESENT-but-zero remaining is different from "no budget in
			// play": it means the parent's own budget is already exhausted
			// (spent mid-turn, before this batch's dispatch), and folding
			// that in as a "default" would leave tokenBudget at 0 —
			// unlimited — exactly backwards. Refuse the call outright
			// instead of ever calling pool.StartTask, so an exhausted parent
			// can't fan out subagents with no cap at all.
			if remaining, ok := RemainingTokenBudgetFromContext(ctx); ok {
				if remaining <= 0 {
					refuseErr := fmt.Errorf("parent token budget exhausted; no budget available for subagents")
					return models.ToolResult{
						CallID:   call.ID,
						ToolName: call.Name,
						Status:   models.CallStatusFailed,
						Error:    refuseErr.Error(),
					}, refuseErr
				}
				if tokenBudget <= 0 || remaining < tokenBudget {
					tokenBudget = remaining
				}
			}

			// Resolve agent type: agent_type > subagent_type > (empty, which the
			// executor defaults to general-purpose). Both arguments pass through
			// VERBATIM: deciding which types exist needs the project/plugin/
			// builtin agent catalog, which lives in pkg/agent, and the executor
			// rejects an unknown type there. Coercing an unrecognized value to
			// general-purpose here would silently downgrade a typo into a run
			// with the wrong — and less restricted — profile.
			agentType := strings.TrimSpace(stringArg(call.Arguments["agent_type"]))
			if agentType == "" {
				agentType = strings.TrimSpace(stringArg(call.Arguments["subagent_type"]))
			}

			contextFiles, err := stringsFromArg(call.Arguments["context_files"])
			if err != nil {
				// Reject rather than silently coerce/skip: a non-string entry
				// is a model mistake (e.g. a number or object where a path
				// string belongs) and hiding it would let the subagent run
				// with a silently incomplete context bundle.
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			task, err := pool.StartTask(ctx, strings.TrimSpace(description), strings.TrimSpace(prompt), subagent.SubagentConfig{
				AgentType:    agentType,
				MaxToolCalls: maxToolCalls,
				Model:        strings.TrimSpace(model),
				TokenBudget:  tokenBudget,
				ContextFiles: contextFiles,
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
			// Surface the subagent's token consumption for react.go's parent-run
			// roll-up (M2-2 12a/12b). Nil-safe: a task that never reported usage
			// (or never reached the executor) leaves Data unset. Content is left
			// byte-for-byte untouched — the usage summary lives only in Data, so
			// model-visible text stays clean.
			if completed.Usage != nil {
				result.Data = map[string]any{"subagent_usage": completed.Usage}
			}
			switch completed.Status {
			case subagent.TaskStatusCompleted:
				result.Status = models.CallStatusCompleted
			case subagent.TaskStatusTimedOut:
				result.Status = models.CallStatusFailed
				result.Error = completed.Error
				return result, fmt.Errorf("subagent task timed out: %s", completed.Error)
			case subagent.TaskStatusCancelled:
				result.Status = models.CallStatusFailed
				result.Error = "subagent task cancelled"
				return result, fmt.Errorf("subagent task cancelled")
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

// stringArg returns a string argument, or "" when it is absent or of another
// type. A non-string agent type is treated as absent rather than rejected: the
// executor's default (general-purpose) is the documented behavior for an omitted
// type, and there is nothing a caller could mean by a numeric agent type.
func stringArg(raw any) string {
	value, _ := raw.(string)
	return value
}

// stringsFromArg converts a JSON array argument (decoded as []any) into a
// []string. A non-string entry is rejected outright (rather than silently
// coerced or dropped) so a model mistake — e.g. a number or object where a
// path string belongs — surfaces as a failed tool call instead of hiding
// behind a silently incomplete context bundle. raw == nil (argument omitted)
// returns (nil, nil).
func stringsFromArg(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("context_files must be an array of strings, got %T", raw)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("context_files[%d] must be a string, got %v (%T)", i, item, item)
		}
		out = append(out, s)
	}
	return out, nil
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
