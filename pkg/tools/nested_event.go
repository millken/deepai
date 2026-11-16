package tools

import "context"

// NestedToolEvent is one run_code-internal tool call surfaced as a child of
// the outer run_code invocation (RUN_CODE_DESIGN §7.2 Phase 2). pkg/tools
// owns the shape so pkg/runcode can emit without importing pkg/agent.
type NestedToolEvent struct {
	ParentID string // the outer run_code ToolCall.ID
	Seq      int    // per-run execution order; aligns with the nested call's "<parentID>.<n>" suffix
	Tool     string
	Status   string // running | completed | failed | interrupted
	MS       int64
	Err      string
}

// NestedEventSink receives nested events as the script executes. Events fire
// from batch worker goroutines — implementations must be safe for concurrent
// use.
type NestedEventSink func(NestedToolEvent)

const nestedEventSinkContextKey contextKey = "tool_nested_event_sink"

func WithNestedEventSink(ctx context.Context, sink NestedEventSink) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, nestedEventSinkContextKey, sink)
}

func NestedEventSinkFromContext(ctx context.Context) NestedEventSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(nestedEventSinkContextKey).(NestedEventSink)
	return sink
}
