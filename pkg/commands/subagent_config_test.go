package commands

import (
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/tools"
)

// TestResolveSubagentTimeout pins the default: absent and negative both mean
// no deadline. Only an explicit positive value starts the wall clock.
func TestResolveSubagentTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{"absent means no deadline", 0, 0},
		{"negative means no deadline", -1, 0},
		{"positive is minutes", 7, 7 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSubagentTimeout(tc.configured); got != tc.want {
				t.Fatalf("resolveSubagentTimeout(%d) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// TestRegisterChatTools_DefaultSubagentPoolHasNoDeadline: the interactive
// REPL used to hand every dispatched subagent a 10-minute deadline. That
// clock forces a tool-less wrap-up and reports the run as completed, which
// cuts a coder off mid-change. Absent config must leave the pool unbounded.
func TestRegisterChatTools_DefaultSubagentPoolHasNoDeadline(t *testing.T) {
	registry := tools.NewRegistry()
	modelRegistry := llm.NewSingleModelRegistry("test", "test-model", "")
	pool := registerChatTools(registry, modelRegistry, stubProvider{}, false, t.TempDir(), 0, nil, nil, nil, nil, "", resolveSubagentTimeout(0))

	if pool == nil {
		t.Fatal("registerChatTools returned a nil pool")
	}
	if got := pool.DefaultTimeout(); got != 0 {
		t.Fatalf("subagent pool DefaultTimeout() = %v, want 0 (no deadline) when subagent_timeout is absent", got)
	}
}

// A positive subagent_timeout is still the switch that turns the wall clock on.
func TestRegisterChatTools_PositiveSubagentTimeoutCarriesADeadline(t *testing.T) {
	registry := tools.NewRegistry()
	modelRegistry := llm.NewSingleModelRegistry("test", "test-model", "")
	pool := registerChatTools(registry, modelRegistry, stubProvider{}, false, t.TempDir(), 0, nil, nil, nil, nil, "", resolveSubagentTimeout(7))

	if got := pool.DefaultTimeout(); got != 7*time.Minute {
		t.Fatalf("subagent pool DefaultTimeout() = %v, want 7m", got)
	}
}
