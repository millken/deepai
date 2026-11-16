package chat

import (
	"github.com/millken/deepai/pkg/agent"
)

// turnToolCallName extracts the tool name a preference-distribution start
// event should record, or "" when the event must NOT be recorded: nested
// run_code child events (ParentID set) are the SCRIPT's calls, not the
// model's own choices — recording them would rewrite the accumulated
// distribution and trip toolShift for a turn where the model made exactly
// one choice (run_code) (review M2, PR #47).
func turnToolCallName(evt agent.AgentEvent) string {
	if evt.Type != agent.AgentEventToolCallStart {
		return ""
	}
	if evt.ToolEvent != nil {
		if evt.ToolEvent.ParentID != "" {
			return ""
		}
		return evt.ToolEvent.Name
	}
	if evt.ToolCall != nil {
		return evt.ToolCall.Name
	}
	return ""
}
