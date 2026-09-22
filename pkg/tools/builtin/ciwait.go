package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
)

const (
	ciWaitPollInterval = 20 * time.Second
	ciWaitDefaultTTL   = 900 * time.Second
	ciWaitGhTTL        = 60 * time.Second
)

type ciWaitOutput struct {
	Status         string  `json:"status"`
	Target         string  `json:"target"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	Output         string  `json:"output"`
}

// CIWaitHandler polls `gh pr checks` / `gh run view` until CI finishes, so the
// model spends one tool call on the wait instead of a turn-burning sleep loop
// (observed: runs of 6-10 consecutive `sleep 75; gh pr checks` bash calls).
func CIWaitHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	res := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	pr := 0
	switch v := args["pr"].(type) {
	case float64:
		pr = int(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return res, fmt.Errorf("pr must be a number, got %q", v)
		}
		pr = n
	}
	runID := ""
	switch v := args["run_id"].(type) {
	case float64:
		runID = strconv.FormatInt(int64(v), 10)
	case string:
		runID = strings.TrimSpace(v)
	}
	if pr == 0 && runID == "" {
		return res, fmt.Errorf("pr or run_id is required")
	}
	if pr != 0 && runID != "" {
		return res, fmt.Errorf("pr and run_id are mutually exclusive")
	}
	repo, _ := args["repo"].(string)

	timeout := ciWaitDefaultTTL
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	deadline := time.Now().Add(timeout)
	start := time.Now()
	target := fmt.Sprintf("pr #%d", pr)
	if runID != "" {
		target = "run " + runID
	}

	var lastOutput string
	for {
		var done, ok bool
		var out string
		var err error
		if runID != "" {
			done, ok, out, err = pollRunView(ctx, runID, repo)
		} else {
			done, ok, out, err = pollPrChecks(ctx, pr, repo)
		}
		if err != nil {
			return res, err
		}
		lastOutput = out

		if done {
			status := "success"
			if !ok {
				status = "failed"
			}
			data, _ := json.Marshal(ciWaitOutput{
				Status: status, Target: target,
				ElapsedSeconds: time.Since(start).Seconds(),
				Output:         truncateOutput(lastOutput, 8*1024),
			})
			res.Content = string(data)
			return res, nil
		}

		if time.Now().After(deadline) {
			data, _ := json.Marshal(ciWaitOutput{
				Status: "timeout", Target: target,
				ElapsedSeconds: time.Since(start).Seconds(),
				Output:         truncateOutput(lastOutput, 8*1024),
			})
			res.Status = models.CallStatusFailed
			res.Error = fmt.Sprintf("CI still pending on %s after %.0fs; last state:\n%s",
				target, time.Since(start).Seconds(), truncateOutput(lastOutput, 4*1024))
			res.Content = string(data)
			return res, nil
		}

		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(ciWaitPollInterval):
		}
	}
}

func ghCmd(ctx context.Context, args string) (stdout, stderr string, exitCode int, err error) {
	result, err := sandbox.ExecDirect(ctx, "gh "+args, ciWaitGhTTL)
	if err != nil {
		return "", "", -1, fmt.Errorf("gh failed to run: %w", err)
	}
	return result.Stdout(), result.Stderr(), result.ExitCode(), nil
}

func repoFlag(repo string) string {
	if repo == "" {
		return ""
	}
	return " --repo " + strconv.Quote(repo)
}

func pollPrChecks(ctx context.Context, pr int, repo string) (done, ok bool, out string, err error) {
	out, stderr, code, err := ghCmd(ctx, fmt.Sprintf("pr checks %d%s", pr, repoFlag(repo)))
	if err != nil {
		return false, false, out, err
	}
	// gh pr checks exit codes: 0 all passed, 8 pending, 1 at least one failed —
	// and also 1 for lookup errors, which carry a diagnostic on stderr.
	// 127 means gh itself is missing; reporting that as a failed CI verdict
	// would send the model off to debug a phantom failure.
	switch {
	case code == 0:
		return true, true, out, nil
	case code == 8:
		return false, false, out, nil
	case code == 127:
		return false, false, out, fmt.Errorf("gh is not installed or not on PATH")
	case code == 1 && isGhLookupError(stderr):
		return false, false, out, fmt.Errorf("gh pr checks: %s", strings.TrimSpace(stderr))
	default:
		return true, false, out, nil
	}
}

func pollRunView(ctx context.Context, runID, repo string) (done, ok bool, out string, err error) {
	out, _, _, err = ghCmd(ctx, fmt.Sprintf("run view %s%s --json status,conclusion", runID, repoFlag(repo)))
	if err != nil {
		return false, false, out, err
	}
	done, ok, err = parseRunState(out)
	return done, ok, out, err
}

func parseRunState(stdout string) (done, ok bool, err error) {
	var state struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal([]byte(stdout), &state); err != nil {
		return false, false, fmt.Errorf("cannot parse gh run view output: %w", err)
	}
	if state.Status != "completed" {
		return false, false, nil
	}
	return true, state.Conclusion == "success", nil
}

func isGhLookupError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "could not resolve") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no pull requests found")
}

func CIWaitTool() models.Tool {
	return models.Tool{
		Name: "ci_wait",
		Description: "Wait for GitHub Actions CI to finish on a pull request or workflow run, polling gh in the background. Use it right after git push or gh pr create — one call replaces the `sleep N; gh pr checks` bash loop that burns a turn per poll. " +
			"Returns the final checks summary (status success/failed with the gh output), or reports the last pending state when timeout is reached.",
		Groups: []string{"builtin"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pr":      map[string]any{"type": "number", "description": "Pull request number to wait on (its checks)"},
				"run_id":  map[string]any{"type": "string", "description": "Workflow run id to wait on (mutually exclusive with pr)"},
				"repo":    map[string]any{"type": "string", "description": "Repository as owner/name; defaults to the current directory's repository"},
				"timeout": map[string]any{"type": "number", "description": "Max seconds to wait (default 900); on timeout the last pending state is reported"},
			},
		},
		Handler: CIWaitHandler,
	}
}
