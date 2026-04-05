package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// HookEvent identifies a lifecycle event for skill hooks.
type HookEvent string

const (
	HookEventPreRun      HookEvent = "PreRun"
	HookEventPostRun     HookEvent = "PostRun"
	HookEventPreToolUse  HookEvent = "PreToolUse"
	HookEventPostToolUse HookEvent = "PostToolUse"
	HookEventOnError     HookEvent = "OnError"
)

// HookResult captures the outcome of a hook execution.
type HookResult struct {
	Event    HookEvent
	Output   string
	Error    error
	Aborted  bool
	Duration time.Duration
}

// HookRunner executes skill-scoped hooks.
type HookRunner struct {
	shell   string        // default: "bash"
	timeout time.Duration // default: 30s
}

// NewHookRunner creates a HookRunner with defaults.
func NewHookRunner() *HookRunner {
	return &HookRunner{
		shell:   defaultShell,
		timeout: injectTimeout,
	}
}

// Run executes a single skill hook.
func (hr *HookRunner) Run(ctx context.Context, hook Hook, skill *Skill, sessionID string) HookResult {
	shell := hr.shell
	if skill != nil && skill.Meta.Shell != "" {
		shell = skill.Meta.Shell
	}

	timeout := hr.timeout
	if hook.Timeout > 0 {
		timeout = hook.Timeout
	}

	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(hctx, shell, "-c", hook.Command)

	// Inherit parent env + skill context
	env := os.Environ()
	env = append(env, fmt.Sprintf("SKILL_NAME=%s", skill.Meta.Name))
	env = append(env, fmt.Sprintf("SKILL_DIR=%s", skill.Dir))
	env = append(env, fmt.Sprintf("SESSION_ID=%s", sessionID))
	env = append(env, fmt.Sprintf("SKILL_HOOK_EVENT=%s", hook.Event))
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	result := HookResult{
		Event:    HookEvent(hook.Event),
		Output:   strings.TrimSpace(string(output)),
		Duration: duration,
	}

	if err != nil {
		result.Error = fmt.Errorf("%s: %w", result.Output, err)
		if hook.OnError == HookErrorAbort {
			result.Aborted = true
		}
	}

	return result
}

// RunEvent executes all hooks in a skill that match the given event.
// Short-circuits on first abort.
func (hr *HookRunner) RunEvent(ctx context.Context, event HookEvent, skill *Skill, sessionID string) HookResult {
	if skill == nil || len(skill.Meta.Hooks) == 0 {
		return HookResult{}
	}

	var last HookResult
	for _, hook := range skill.Meta.Hooks {
		if hook.Event != string(event) {
			continue
		}
		result := hr.Run(ctx, hook, skill, sessionID)
		if result.Aborted {
			return result
		}
		last = result
	}

	return last
}
