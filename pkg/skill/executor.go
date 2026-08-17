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
	SystemPrompt string
	AllowedTools []string // tools that skip permission checks
	Model        string
	MaxTurns     *int
	Temperature  *float64
	Effort       string
}

// Executor executes skills by rendering content and building agent configs.
type Executor struct {
	registry *Registry
}

// NewExecutor creates a skill executor backed by the given registry.
func NewExecutor(registry *Registry) *Executor {
	return &Executor{registry: registry}
}

// Execute renders the skill content and returns the agent config.
// The caller is responsible for actually running the agent.
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

	// Render: variable replacement + dynamic injection
	rendered, err := Render(ctx, body, args, skill)
	if err != nil {
		return nil, fmt.Errorf("render skill %s: %w", name, err)
	}

	return e.buildConfig(skill, rendered), nil
}

// buildConfig constructs the AgentConfig from skill metadata and rendered content.
func (e *Executor) buildConfig(skill *Skill, rendered string) *AgentConfig {
	return &AgentConfig{
		SystemPrompt: rendered,
		AllowedTools: []string(skill.Meta.AllowedTools),
		Model:        skill.Meta.Model,
		MaxTurns:     skill.Meta.MaxTurns,
		Temperature:  skill.Meta.Temperature,
		Effort:       skill.Meta.Effort,
	}
}
