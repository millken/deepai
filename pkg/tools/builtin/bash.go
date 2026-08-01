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
	data, _ := json.Marshal(output)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

type BashOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
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
	headSize := int(float64(availableBytes) * 0.7)  // 70% for head
	tailSize := availableBytes - headSize              // 30% for tail
	
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
