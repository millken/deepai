package commands

import (
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/tools"
)

// TestResolveSubagentTimeout pins the same "0 = default, negative = unlimited"
// contract review_timeout already uses, so config.yaml reads consistently.
func TestResolveSubagentTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{"absent uses the default", 0, defaultSubagentTimeout},
		{"negative means unlimited", -1, 0},
		{"positive is minutes", 7, 7 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSubagentTimeout(tc.configured); got != tc.want {
				t.Fatalf("resolveSubagentTimeout(%d) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// TestRegisterChatTools_SubagentPoolCarriesADeadline is the wiring guard for
// the fix: the interactive REPL used to build its pool with NewSubagentPool(
// subExecutor, 0), so every dispatched subagent ran under a deadline-free ctx.
// pkg/agent's graceful wall-clock wind-down (react.go) reads ctx.Deadline()
// and nothing else, so that 0 silently disabled it — a review subagent ran
// until the parent turn ended rather than wrapping up with a verdict.
func TestRegisterChatTools_SubagentPoolCarriesADeadline(t *testing.T) {
	registry := tools.NewRegistry()
	modelRegistry := llm.NewSingleModelRegistry("test", "test-model", "")
	pool := registerChatTools(registry, modelRegistry, stubProvider{}, false, t.TempDir(), 0, nil, nil, nil, nil, defaultSubagentTimeout)

	if pool == nil {
		t.Fatal("registerChatTools returned a nil pool")
	}
	if got := pool.DefaultTimeout(); got != defaultSubagentTimeout {
		t.Fatalf("subagent pool DefaultTimeout() = %v, want %v — a 0 here disables the graceful wall-clock wind-down", got, defaultSubagentTimeout)
	}
}
