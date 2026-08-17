package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const (
	acpWorkspaceVirtualPath = "/mnt/acp-workspace"
	acpPromptPlaceholder    = "{{prompt}}"
)

type ACPAgentConfig struct {
	Description string            `json:"description" yaml:"description"`
	Command     string            `json:"command" yaml:"command"`
	Args        []string          `json:"args" yaml:"args"`
	Env         map[string]string `json:"env" yaml:"env"`
	Model       string            `json:"model" yaml:"model"`
}

func InvokeACPAgentTool(agents map[string]ACPAgentConfig) models.Tool {
	available := make([]string, 0, len(agents))
	lines := make([]string, 0, len(agents))
	for name, cfg := range agents {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		available = append(available, name)
		description := strings.TrimSpace(cfg.Description)
		if description == "" {
			description = "External agent"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", name, description))
	}

	description := "Invoke a configured external ACP-style coding agent and return its final stdout.\n\n" +
		"Available agents:\n" + strings.Join(lines, "\n") + "\n\n" +
		"IMPORTANT: These agents run in an isolated workspace. Do not reference /mnt/user-data paths in the prompt. " +
		"Results written by the external agent are available under /mnt/acp-workspace/ (read-only to the main agent)."

	return models.Tool{
		Name:        "invoke_acp_agent",
		Description: description,
		Groups:      []string{"builtin", "agent"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent":  map[string]any{"type": "string", "description": "Configured ACP agent name", "enum": stringSliceToAny(available)},
				"prompt": map[string]any{"type": "string", "description": "Self-contained task prompt for the external agent"},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Optional timeout in seconds (default 300)",
				},
			},
			"required": []any{"agent", "prompt"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			agentName, _ := call.Arguments["agent"].(string)
			prompt, _ := call.Arguments["prompt"].(string)
			timeout := acpTimeoutFromArgs(call.Arguments["timeout"])

			result := models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
			}

			cfg, ok := agents[strings.TrimSpace(agentName)]
			if !ok {
				result.Status = models.CallStatusFailed
				result.Error = fmt.Sprintf("unknown agent %q", strings.TrimSpace(agentName))
				return result, fmt.Errorf("%s", result.Error)
			}

			output, err := invokeACPAgentCommand(ctx, cfg, strings.TrimSpace(prompt), ThreadIDFromContext(ctx), timeout)
			if err != nil {
				result.Status = models.CallStatusFailed
				result.Error = err.Error()
				return result, err
			}

			result.Status = models.CallStatusCompleted
			result.Content = output
			return result, nil
		},
	}
}

func invokeACPAgentCommand(ctx context.Context, cfg ACPAgentConfig, prompt, threadID string, timeout time.Duration) (string, error) {
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		return "", fmt.Errorf("acp agent command is required")
	}
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	workDir, err := ACPWorkspaceDir(threadID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("create acp workspace: %w", err)
	}

	args := expandACPArgs(cfg.Args, prompt)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, command, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), buildACPEnv(cfg, prompt)...)

	if !argsContainPromptPlaceholder(cfg.Args) {
		cmd.Stdin = strings.NewReader(prompt)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", formatACPInvocationError(command, stderr.String(), err)
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		out = "(no response)"
	}
	return out, nil
}

func ACPWorkspaceDir(threadID string) (string, error) {
	root := strings.TrimSpace(os.Getenv("DEEPAI_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "deepai-go-data")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("thread id is required for ACP workspace isolation")
	}
	return filepath.Join(root, "threads", threadID, "acp-workspace"), nil
}

func ResolveVirtualPath(ctx context.Context, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	switch {
	case strings.HasPrefix(path, "/mnt/user-data/"):
		root := threadDataRootFromThreadID(ThreadIDFromContext(ctx))
		if root == "" {
			return path
		}
		return confineToRoot(root, strings.TrimPrefix(path, "/mnt/user-data/"))
	case path == acpWorkspaceVirtualPath || strings.HasPrefix(path, acpWorkspaceVirtualPath+"/"):
		root, err := ACPWorkspaceDir(ThreadIDFromContext(ctx))
		if err != nil {
			return path
		}
		suffix := strings.TrimPrefix(path, acpWorkspaceVirtualPath)
		suffix = strings.TrimPrefix(suffix, "/")
		if suffix == "" {
			return root
		}
		return confineToRoot(root, suffix)
	default:
		return path
	}
}

func confineToRoot(root, suffix string) string {
	joined := filepath.Join(root, filepath.FromSlash(suffix))
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return root
	}
	return joined
}

func threadDataRootFromThreadID(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	root := strings.TrimSpace(os.Getenv("DEEPAI_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "deepai-go-data")
	}
	return filepath.Join(root, "threads", threadID, "user-data")
}

func acpTimeoutFromArgs(raw any) time.Duration {
	timeout := 5 * time.Minute
	switch value := raw.(type) {
	case float64:
		if value > 0 {
			timeout = time.Duration(value * float64(time.Second))
		}
	case int:
		if value > 0 {
			timeout = time.Duration(value) * time.Second
		}
	}
	return timeout
}

func expandACPArgs(args []string, prompt string) []string {
	if len(args) == 0 {
		return nil
	}
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		expanded = append(expanded, strings.ReplaceAll(arg, acpPromptPlaceholder, prompt))
	}
	return expanded
}

func argsContainPromptPlaceholder(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, acpPromptPlaceholder) {
			return true
		}
	}
	return false
}

func buildACPEnv(cfg ACPAgentConfig, prompt string) []string {
	keys := make([]string, 0, len(cfg.Env)+2)
	for key := range cfg.Env {
		keys = append(keys, key)
	}

	env := make([]string, 0, len(keys)+2)
	for _, key := range keys {
		env = append(env, key+"="+os.ExpandEnv(cfg.Env[key]))
	}
	env = append(env, "DEEPAI_ACP_PROMPT="+prompt)
	if strings.TrimSpace(cfg.Model) != "" {
		env = append(env, "DEEPAI_ACP_MODEL="+strings.TrimSpace(cfg.Model))
	}
	return env
}

func formatACPInvocationError(command, stderr string, err error) error {
	if _, ok := err.(*exec.Error); ok {
		return fmt.Errorf("acp agent command %q was not found on PATH", command)
	}
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("acp agent command %q failed: %s", command, stderr)
	}
	return fmt.Errorf("acp agent command %q failed: %w", command, err)
}

func stringSliceToAny(values []string) []any {
	if len(values) == 0 {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
