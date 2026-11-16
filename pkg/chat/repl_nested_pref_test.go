package chat

import (
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

// Review M2: the preference distribution records the MODEL's own tool
// choices. run_code nested child events (ParentID set) are the script's
// calls — recording them would rewrite the accumulated distribution and trip
// toolShift preference extraction for a turn where the model made exactly one
// choice (run_code).
func TestTurnToolCallNameExcludesNested(t *testing.T) {
	cases := []struct {
		name string
		evt  agent.AgentEvent
		want string
	}{
		{
			name: "top-level start recorded",
			evt: agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolEvent: &agent.ToolCallEvent{
				ID: "t1", Name: "grep", Status: models.CallStatusRunning,
			}},
			want: "grep",
		},
		{
			name: "nested start excluded",
			evt: agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolEvent: &agent.ToolCallEvent{
				ID: "rc_1.1", Name: "edit_file", ParentID: "rc_1", Status: models.CallStatusRunning,
			}},
			want: "",
		},
		{
			name: "tool_call carrier without event recorded",
			evt: agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolCall: &models.ToolCall{
				ID: "t2", Name: "bash", Status: models.CallStatusRunning,
			}},
			want: "bash",
		},
		{
			name: "end events never recorded",
			evt: agent.AgentEvent{Type: agent.AgentEventToolCallEnd, ToolEvent: &agent.ToolCallEvent{
				ID: "t1", Name: "grep", Status: models.CallStatusCompleted,
			}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnToolCallName(tc.evt); got != tc.want {
				t.Fatalf("turnToolCallName = %q, want %q", got, tc.want)
			}
		})
	}
}
