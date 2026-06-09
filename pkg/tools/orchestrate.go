package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/orchestrator"
	pkgsandbox "github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/subagent"
)

type poolRunner struct {
	pool taskPool
}

func (r poolRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	task, err := r.pool.StartTask(ctx, description, prompt, subagent.SubagentConfig{AgentType: agentType})
	if err != nil {
		return "", err
	}
	done, err := r.pool.Wait(ctx, task.ID)
	if err != nil {
		return "", err
	}
	if done.Status != subagent.TaskStatusCompleted {
		msg := done.Error
		if msg == "" {
			msg = fmt.Sprintf("subagent ended with status %s", done.Status)
		}
		return "", fmt.Errorf("%s", msg)
	}
	return done.Result, nil
}

type cmdVerifier struct {
	command string
	workDir string
	timeout time.Duration
}

func (v cmdVerifier) Verify(ctx context.Context) (orchestrator.VerifyResult, error) {
	if strings.TrimSpace(v.command) == "" {
		return orchestrator.VerifyResult{Ran: false, Output: "(no verify command configured)"}, nil
	}
	res, err := pkgsandbox.ExecDirect(ctx, withWorkDir(v.workDir, v.command), v.timeout)
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	out := strings.TrimSpace(res.Stdout() + "\n" + res.Stderr())
	return orchestrator.VerifyResult{Ran: true, Passed: res.ExitCode() == 0, Output: out}, nil
}

type gitDiffer struct {
	workDir string
}

func (d gitDiffer) Diff(ctx context.Context) (string, error) {
	cmd := "git add -N -A . >/dev/null 2>&1; git --no-pager diff"
	res, err := pkgsandbox.ExecDirect(ctx, withWorkDir(d.workDir, cmd), 30*time.Second)
	if err != nil {
		return "", err
	}
	return res.Stdout(), nil
}

func withWorkDir(dir, cmd string) string {
	if strings.TrimSpace(dir) == "" {
		return cmd
	}
	return fmt.Sprintf("cd %s && %s", shellQuote(dir), cmd)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ImplementTaskTool(pool taskPool, workDir string) models.Tool {
	return models.Tool{
		Name:        "implement_task",
		Description: "Autonomously implement a coding task with an implement→verify→review→fix loop. A coder subagent makes the change, an optional verify_command (build/test) runs, a reviewer subagent judges the diff, and the loop repeats with feedback until both pass or max_rounds is reached. Use for self-contained changes you want driven to completion without step-by-step guidance.",
		Groups:      []string{"agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":         map[string]any{"type": "string", "description": "Detailed task for the coder to implement"},
				"verify_command": map[string]any{"type": "string", "description": "Shell command that must exit 0 for success (e.g. 'go build ./... && go test ./...'). Optional; if omitted, success relies on the reviewer only."},
				"reviewer_type":  map[string]any{"type": "string", "description": "Reviewer agent type (default arch-reviewer; e.g. security-reviewer, perf-reviewer)"},
				"max_rounds":     map[string]any{"type": "integer", "description": "Max implement→verify→review→fix rounds (default 4)"},
				"require_verification": map[string]any{"type": "boolean", "description": "If true, only declare success when verify_command actually ran and passed (review alone never suffices). Requires verify_command."},
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
			reviewerType, _ := call.Arguments["reviewer_type"].(string)
			requireVerification, _ := call.Arguments["require_verification"].(bool)

			if requireVerification && strings.TrimSpace(verifyCmd) == "" {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("require_verification needs a verify_command")
			}

			cfg := orchestrator.Config{
				MaxRounds:           intFromArg(call.Arguments["max_rounds"]),
				ReviewerType:        strings.TrimSpace(reviewerType),
				RequireVerification: requireVerification,
			}

			res, err := orchestrator.Run(ctx, cfg, prompt,
				poolRunner{pool: pool},
				cmdVerifier{command: verifyCmd, workDir: workDir, timeout: 5 * time.Minute},
				gitDiffer{workDir: workDir},
			)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed, Error: err.Error()}, err
			}

			content, _ := json.Marshal(summarizeOrchestration(res))
			status := models.CallStatusCompleted
			if !res.Done {
				status = models.CallStatusFailed
			}
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: status, Content: string(content)}, nil
		},
	}
}

func summarizeOrchestration(res *orchestrator.Result) map[string]any {
	out := map[string]any{
		"done":     res.Done,
		"verified": res.Verified,
		"reason":   res.Reason,
		"rounds":   len(res.Rounds),
	}
	if n := len(res.Rounds); n > 0 {
		last := res.Rounds[n-1]
		out["verify_ran"] = last.VerifyRan
		out["verify_passed"] = last.VerifyPassed
		out["review_pass"] = last.Verdict.Pass
		if last.Verdict.Summary != "" {
			out["review_summary"] = last.Verdict.Summary
		}
		if !res.Done && len(last.Verdict.Issues) > 0 {
			out["open_issues"] = last.Verdict.Issues
		}
		out["final_implementation"] = last.ImplSummary
	}
	return out
}
