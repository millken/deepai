package tools

import (
	"context"
	"testing"
)

func TestNestedEventSinkRoundtrip(t *testing.T) {
	if sink := NestedEventSinkFromContext(context.Background()); sink != nil {
		t.Fatalf("plain ctx: sink = %v, want nil", sink)
	}

	var got []NestedToolEvent
	sink := NestedEventSink(func(e NestedToolEvent) { got = append(got, e) })
	ctx := WithNestedEventSink(context.Background(), sink)

	out := NestedEventSinkFromContext(ctx)
	if out == nil {
		t.Fatal("WithNestedEventSink ctx: sink not found")
	}
	out(NestedToolEvent{ParentID: "rc_1", Seq: 1, Tool: "grep", Status: "completed"})

	if len(got) != 1 || got[0].Tool != "grep" || got[0].ParentID != "rc_1" {
		t.Fatalf("sink received %+v", got)
	}
}

func TestWithNestedEventSinkNil(t *testing.T) {
	ctx := WithNestedEventSink(context.Background(), nil)
	if _, ok := ctx.Value(nestedEventSinkContextKey).(NestedEventSink); ok {
		t.Fatal("nil sink must not be stored")
	}
}
