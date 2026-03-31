package subagent

import "context"

type TaskEvent struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	RequestID   string `json:"request_id,omitempty"`
	Description string `json:"description,omitempty"`
	Message     string `json:"message,omitempty"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
}

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
