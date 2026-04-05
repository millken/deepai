package skill

import (
	"context"
)

// SubagentResult captures the outcome of a forked subagent execution.
type SubagentResult struct {
	Output  string
	Error   error
	Partial bool
}

// SubagentRunner executes a skill in an isolated subagent.
// Implemented outside pkg/skill/ to avoid circular imports.
type SubagentRunner interface {
	RunFork(ctx context.Context, cfg *AgentConfig, prompt string) (*SubagentResult, error)
}
