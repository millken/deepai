package subagent

import "context"

// TaskEvent is one observation about a subagent's progress.
//
// Message is the legacy free-text rendering ("✓ read_file"). The structured
// fields below were added so a UI can show cumulative progress and a per-tool
// history instead of just "what is it doing this instant"; they are all
// optional, and a consumer that ignores them renders exactly as before.
type TaskEvent struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	RequestID   string `json:"request_id,omitempty"`
	Description string `json:"description,omitempty"`
	Message     string `json:"message,omitempty"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`

	// AgentType is the subagent's type. Carried explicitly because the TUI
	// used to recover it by scraping "[...]" out of Description, which
	// mis-parses any description that happens to contain a bracket.
	AgentType string `json:"agent_type,omitempty"`
	// Phase is what the subagent is doing at this instant: PhaseTool (a tool
	// call is in flight), PhaseThinking (the model is reasoning — no output
	// yet), or PhaseGenerating (the model is streaming its answer). Empty on
	// events that carry no phase information, in which case a consumer keeps
	// whatever phase it last saw. Added so a UI can show live activity during
	// a long model turn instead of pinning the last finished tool call on
	// screen for minutes.
	Phase string `json:"phase,omitempty"`
	// ToolName/ToolArgs/ToolStatus describe the tool this event is about.
	// ToolStatus is "running", "ok" or "error".
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolStatus string `json:"tool_status,omitempty"`
	// DurationMS is this tool call's duration; ToolCalls and Tokens are the
	// subagent's running totals, not per-event deltas.
	DurationMS int64 `json:"duration_ms,omitempty"`
	ToolCalls  int   `json:"tool_calls,omitempty"`
	Tokens     int   `json:"tokens,omitempty"`
}

// Phase values for TaskEvent.Phase.
const (
	PhaseTool       = "tool"
	PhaseThinking   = "thinking"
	PhaseGenerating = "generating"
)

type eventSinkContextKey struct{}

type EventSink func(TaskEvent)

func WithEventSink(ctx context.Context, sink EventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, eventSinkContextKey{}, sink)
}

func EmitEvent(ctx context.Context, evt TaskEvent) {
	if ctx == nil {
		return
	}
	sink, _ := ctx.Value(eventSinkContextKey{}).(EventSink)
	if sink != nil {
		sink(evt)
	}
}
