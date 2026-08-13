package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
)

// M2.3: Bash output size limit to prevent extreme results
const BashMaxOutputBytes = 50 * 1024 // 50KB hard limit

func BashHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments

	cmd, ok := args["command"].(string)
	if !ok || strings.TrimSpace(cmd) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("command is required")
	}

	// Default timeout is generous to cover common build/test commands without
	// forcing the AI to set timeout explicitly every call.
	timeout := 300 * time.Second
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	result, err := sandbox.ExecDirect(ctx, cmd, timeout)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("bash failed: %w", err)
	}

	// M2.3: Apply output size limit
	stdout := result.Stdout()
	stderr := result.Stderr()

	// Check total output size
	totalSize := len(stdout) + len(stderr)
	if totalSize > BashMaxOutputBytes {
		// Truncate output to fit within limit
		stdout = truncateOutput(stdout, BashMaxOutputBytes)
		stderr = truncateOutput(stderr, BashMaxOutputBytes-len(stdout))
	}

	output := &BashOutput{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: result.ExitCode(),
	}
	if result.TimedOut() {
		output.TimedOut = true
		output.DurationSeconds = result.Duration().Seconds()
	}
	data, _ := json.Marshal(output)
	res := models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}
	if result.TimedOut() {
		// Status makes the failure legible to the circuit-breaker, the provider
		// (is_error) and the UI; Error carries the explanation, since
		// toolMessageContent shows Error INSTEAD OF Content — so it has to hold
		// the partial output too, or the model would lose it.
		res.Status = models.CallStatusFailed
		res.Error = timeoutMessage(timeout, result.Duration(), stdout, stderr)
	}
	return res, nil
}

// timeoutMessage explains a killed command to the model.
//
// A silent hang is the worst case for an agent: exit code -1 is also what
// "failed to start" looks like, and with no output there is nothing to reason
// about, so the model's only obvious move is to run it again — 8 times, 120s
// apiece, in session 20260812_093415_fc6e. The message therefore states what
// happened, that re-running verbatim will hang again, and what the actual
// alternatives are.
func timeoutMessage(limit, elapsed time.Duration, stdout, stderr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "command TIMED OUT after %.0fs (limit %.0fs) and its process group was killed.",
		elapsed.Seconds(), limit.Seconds())

	partial := strings.TrimSpace(stdout + "\n" + stderr)
	if partial == "" {
		b.WriteString(" It produced NO output at all before the kill, so there is nothing to inspect" +
			" — and re-running the same command will hang the same way." +
			" Do something different: raise the timeout, run a narrower subset," +
			" add a flag that makes it non-interactive or more verbose," +
			" or check whether it is waiting on something unavailable (network, a prompt, a lock).")
	} else {
		b.WriteString(" Output captured before the kill (incomplete):\n")
		b.WriteString(partial)
	}
	return b.String()
}

type BashOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	// TimedOut distinguishes "killed for running too long" from an exit code
	// of -1 for any other reason. DurationSeconds is only set alongside it.
	TimedOut        bool    `json:"timed_out,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
}

// truncateOutput truncates output to fit within maxBytes, preserving head 70% and tail 30%
func truncateOutput(output string, maxBytes int) string {
	if len(output) <= maxBytes {
		return output
	}

	// Reserve space for truncation message
	truncationMsg := "\n... (output truncated)"
	msgSize := len(truncationMsg)
	if maxBytes <= msgSize {
		return output[:maxBytes]
	}

	availableBytes := maxBytes - msgSize
	headSize := int(float64(availableBytes) * 0.7) // 70% for head
	tailSize := availableBytes - headSize          // 30% for tail

	if len(output) > headSize+tailSize {
		return output[:headSize] + truncationMsg + output[len(output)-tailSize:]
	}

	// If output is smaller than expected, just truncate to maxBytes
	return output[:maxBytes-msgSize] + truncationMsg
}

func BashTool() models.Tool {
	return models.Tool{
		Name:        "bash",
		Description: "Execute shell commands for building, running, testing, package managers, git, and any task without a dedicated tool. Do NOT use bash for file operations — the dedicated file tools are more reliable in the sandbox. Output is limited to 50KB to prevent context overflow; large outputs are truncated with head 70% + tail 30% preserved. Returns stdout, stderr, and exit code as JSON.",
		Groups:      []string{"builtin"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute"},
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (default 300)"},
			},
			"required": []any{"command"},
		},
		Handler: BashHandler,
	}
}
