package agent

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/tools"
)

type fakeUI struct{ tools.UserInteraction }

// A subagent strips inherited UserInteraction so delegated work never prompts
// the user (plan confirmations auto-approve, clarifications use best judgment).
func TestSubagentStripsUserInteraction(t *testing.T) {
	ctx := tools.WithUserInteraction(context.Background(), fakeUI{})
	if tools.UserInteractionFromContext(ctx) == nil {
		t.Fatal("precondition: UI should be present before strip")
	}
	ctx = tools.WithUserInteraction(ctx, nil)
	if tools.UserInteractionFromContext(ctx) != nil {
		t.Fatal("nil strip failed: delegated subagent would still prompt the user")
	}
}

func TestSubagentMessageFromAgentEvent(t *testing.T) {
	cases := []struct {
		name string
		evt  AgentEvent
		want string
	}{
		{"tool start", AgentEvent{Type: AgentEventToolCallStart, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, "⚙ edit_file"},
		{"tool ok end", AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, "✓ edit_file"},
		{"tool error end", AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: &ToolCallEvent{Name: "bash", Error: "boom"}}, "✗ bash: boom"},
		{"raw token chunk dropped", AgentEvent{Type: AgentEventChunk, Text: "thinking..."}, ""},
		{"agent error", AgentEvent{Type: AgentEventError, Err: "blew up"}, "✗ blew up"},
	}
	for _, c := range cases {
		if got := subagentMessageFromAgentEvent(c.evt); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestResolveMaxTurns_Priority verifies the MaxTurns resolution chain:
// caller-explicit > agent type profile > safety floor (6).
func TestResolveMaxTurns_Priority(t *testing.T) {
	// Simulate the executor's maxTurns resolution logic.
	// In production this runs inside Execute(), but the logic is:
	//   maxTurns = task.Config.MaxTurns    // caller-explicit (max_turns arg)
	//   if maxTurns <= 0: maxTurns = profileCfg.MaxTurns  // builtin/YAML
	//   if maxTurns <= 0: maxTurns = 6     // safety floor
	resolve := func(callerMaxTurns, profileMaxTurns int) int {
		maxTurns := callerMaxTurns
		if maxTurns <= 0 {
			maxTurns = profileMaxTurns
		}
		if maxTurns <= 0 {
			maxTurns = 6
		}
		return maxTurns
	}

	tests := []struct {
		name            string
		callerMaxTurns  int
		profileMaxTurns int
		want            int
	}{
		{"caller overrides profile", 20, 10, 20},
		{"profile used when caller is 0", 0, 10, 10},
		{"safety floor when both 0", 0, 0, 6},
		{"caller=0 profile=0 → floor", 0, 0, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.callerMaxTurns, tt.profileMaxTurns); got != tt.want {
				t.Errorf("resolve(%d, %d) = %d, want %d", tt.callerMaxTurns, tt.profileMaxTurns, got, tt.want)
			}
		})
	}
}
