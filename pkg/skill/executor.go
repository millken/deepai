package skill

import (
	"context"
	"fmt"
)

// AgentConfig holds the configuration needed to run an agent with a skill.
// This is an abstraction over the concrete agent.AgentConfig to avoid
// circular imports. The executor builds this, and the caller maps it
// to the concrete agent config.
type AgentConfig struct {
	SystemPrompt  string
	AllowedTools  []string // tools that skip permission checks
	Model         string
	MaxTurns      *int
	Temperature   *float64
	Effort        string
	AgentType     string // subagent type for context: fork
	RunInSubagent bool
}

// Executor executes skills by rendering content and building agent configs.
type Executor struct {
	registry       *Registry
	hookRunner     *HookRunner
	subagentRunner SubagentRunner
}

// NewExecutor creates a skill executor backed by the given registry.
func NewExecutor(registry *Registry) *Executor {
	return &Executor{registry: registry}
}

// WithHookRunner configures hook execution for skill lifecycle events.
func (e *Executor) WithHookRunner(hr *HookRunner) *Executor {
	e.hookRunner = hr
	return e
}

// WithSubagentRunner configures the subagent runner for context: fork skills.
func (e *Executor) WithSubagentRunner(runner SubagentRunner) *Executor {
	e.subagentRunner = runner
	return e
}

// Execute renders the skill content and returns the agent config.
// The caller is responsible for actually running the agent.
// Pre-run hooks are executed before rendering.
func (e *Executor) Execute(ctx context.Context, name string, args string) (*AgentConfig, error) {
	skill := e.registry.Get(name)
	if skill == nil {
		return nil, fmt.Errorf("skill %q not found", name)
	}

	// Lazy load body if needed
	body, err := e.registry.LoadBody(name)
	if err != nil {
		return nil, fmt.Errorf("load skill body %s: %w", name, err)
	}

	// Pre-run hooks
	sessionID := SessionIDFromContext(ctx)
	if e.hookRunner != nil {
		result := e.hookRunner.RunEvent(ctx, HookEventPreRun, skill, sessionID)
		if result.Aborted {
			return nil, fmt.Errorf("skill %q aborted by pre-run hook: %s", name, result.Error)
		}
	}

	// Render: variable replacement + dynamic injection
	rendered, err := Render(ctx, body, args, skill)
	if err != nil {
		return nil, fmt.Errorf("render skill %s: %w", name, err)
	}

	cfg := e.buildConfig(skill, rendered)

	// Fork mode requires subagent runner
	if cfg.RunInSubagent && e.subagentRunner == nil {
		return nil, fmt.Errorf("skill %q requires context:fork but no subagent runner configured", name)
	}

	return cfg, nil
}

// ExecuteFork renders the skill and runs it in a forked subagent.
// Pre-run and PostRun hooks are executed around the fork.
func (e *Executor) ExecuteFork(ctx context.Context, name string, args string) (*SubagentResult, error) {
	cfg, err := e.Execute(ctx, name, args)
	if err != nil {
		return nil, err
	}
	if !cfg.RunInSubagent {
		return nil, fmt.Errorf("skill %q is not configured for fork execution", name)
	}

	result, runErr := e.subagentRunner.RunFork(ctx, cfg, args)

	// Post-run hooks (always run, even on error)
	if e.hookRunner != nil {
		skill := e.registry.Get(name)
		sessionID := SessionIDFromContext(ctx)
		hr := e.hookRunner.RunEvent(ctx, HookEventPostRun, skill, sessionID)
		_ = hr // PostRun results are advisory; don't fail the fork
	}

	return result, runErr
}

// buildConfig constructs the AgentConfig from skill metadata and rendered content.
func (e *Executor) buildConfig(skill *Skill, rendered string) *AgentConfig {
	cfg := &AgentConfig{
		SystemPrompt:  rendered,
		AllowedTools:  []string(skill.Meta.AllowedTools),
		Model:         skill.Meta.Model,
		MaxTurns:      skill.Meta.MaxTurns,
		Temperature:   skill.Meta.Temperature,
		Effort:        skill.Meta.Effort,
		AgentType:     skill.Meta.Agent,
		RunInSubagent: skill.Meta.Context == "fork",
	}

	return cfg
}

// ExecuteAndRun renders the skill, builds config, and invokes the provided run function.
// Pre-run hooks fire before rendering; PostRun hooks fire after runFn completes (even on error).
func (e *Executor) ExecuteAndRun(ctx context.Context, name string, args string, runFn func(context.Context, *AgentConfig) error) error {
	cfg, err := e.Execute(ctx, name, args)
	if err != nil {
		return err
	}

	runErr := runFn(ctx, cfg)

	// Post-run hooks (always run, even on error)
	if e.hookRunner != nil {
		skill := e.registry.Get(name)
		sessionID := SessionIDFromContext(ctx)
		e.hookRunner.RunEvent(ctx, HookEventPostRun, skill, sessionID)
	}

	return runErr
}
