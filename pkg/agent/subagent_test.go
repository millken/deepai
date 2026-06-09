package agent

import "testing"

func TestSubagentMessageFromAgentEvent(t *testing.T) {
	cases := []struct {
		name string
		evt  AgentEvent
		want string
	}{
		{"tool start", AgentEvent{Type: AgentEventToolCallStart, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, "⚙ edit_file"},
		{"tool ok end is silent", AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, ""},
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
