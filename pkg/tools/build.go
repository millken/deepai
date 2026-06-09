package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/orchestrator"
)

func BuildTaskTool(pool taskPool, workDir string) models.Tool {
	return models.Tool{
		Name:        "build_task",
		Description: "End-to-end autonomous build: a design panel (parallel proposers + judge) produces a vetted plan, which is then fed deterministically into the implement→verify→review→fix loop. One call drives discussion→design→implementation→verification with no step-by-step guidance. Use for non-trivial tasks you want taken from idea to verified change in one shot.",
		Groups:      []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":         map[string]any{"type": "string", "description": "The task to design and implement"},
				"verify_command": map[string]any{"type": "string", "description": "Shell command that must exit 0 for success (e.g. 'go build ./... && go test ./...'). Optional; without it success relies on the reviewer."},
				"proposers":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Design proposer agent types (default [architect, coder])."},
				"judge":          map[string]any{"type": "string", "description": "Design judge agent type (default architect)."},
				"reviewers":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Reviewer agent types that vote during implementation (default arch-reviewer)."},
				"review_policy":  map[string]any{"type": "string", "description": "Reviewer vote policy: 'unanimous' (default) or 'majority'."},
				"review_model":   map[string]any{"type": "string", "description": "Optional distinct model for reviewers (reduces self-review bias)."},
				"max_rounds":     map[string]any{"type": "integer", "description": "Max implement→verify→review→fix rounds (default 4)."},
				"max_agent_calls": map[string]any{"type": "integer", "description": "Cap on coder+reviewer invocations in the implement phase. 0 = unlimited."},
				"require_verification": map[string]any{"type": "boolean", "description": "If true, only succeed when verify_command actually ran and passed. Requires verify_command."},
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
			verifyCmd, _ := call.Arguments["verify_command"].(string)
			judge, _ := call.Arguments["judge"].(string)
			reviewModel, _ := call.Arguments["review_model"].(string)
			reviewPolicy, _ := call.Arguments["review_policy"].(string)
			requireVerification, _ := call.Arguments["require_verification"].(bool)

			if requireVerification && strings.TrimSpace(verifyCmd) == "" {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("require_verification needs a verify_command")
			}

			implCfg := orchestrator.Config{
				MaxRounds:           intFromArg(call.Arguments["max_rounds"]),
				Reviewers:           stringsFromArg(call.Arguments["reviewers"]),
				MajorityReview:      strings.EqualFold(strings.TrimSpace(reviewPolicy), "majority"),
				MaxAgentCalls:       intFromArg(call.Arguments["max_agent_calls"]),
				RequireVerification: requireVerification,
			}
			cfg := orchestrator.BuildConfig{
				Design: orchestrator.DesignConfig{
					Proposers: stringsFromArg(call.Arguments["proposers"]),
					Judge:     strings.TrimSpace(judge),
				},
				Implement: implCfg,
			}

			reviewerSet := make(map[string]struct{})
			for _, r := range implCfg.Reviewers {
				reviewerSet[r] = struct{}{}
			}

			res, err := orchestrator.Build(ctx, cfg, prompt,
				poolRunner{pool: pool, reviewModel: strings.TrimSpace(reviewModel), reviewerTypes: reviewerSet},
				cmdVerifier{command: verifyCmd, workDir: workDir, timeout: 5 * time.Minute},
				gitDiffer{workDir: workDir},
			)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed, Error: err.Error()}, err
			}

			out := map[string]any{}
			if res.Design != nil {
				out["plan"] = res.Design.Plan
				out["proposal_count"] = len(res.Design.Proposals)
			}
			done := false
			if res.Implement != nil {
				done = res.Implement.Done
				out["done"] = res.Implement.Done
				out["verified"] = res.Implement.Verified
				out["reason"] = res.Implement.Reason
				out["rounds"] = len(res.Implement.Rounds)
				out["agent_calls"] = res.Implement.AgentCalls
			}
			data, _ := json.Marshal(out)
			status := models.CallStatusCompleted
			if !done {
				status = models.CallStatusFailed
			}
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: status, Content: string(data)}, nil
		},
	}
}
