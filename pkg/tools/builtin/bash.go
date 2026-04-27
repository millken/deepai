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

	output := &BashOutput{
		Stdout:   result.Stdout(),
		Stderr:   result.Stderr(),
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

func BashTool() models.Tool {
	return models.Tool{
		Name:        "bash",
		Description: "Execute shell commands. Returns stdout, stderr, and exit code as JSON.",
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
