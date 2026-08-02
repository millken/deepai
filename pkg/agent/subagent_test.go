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
