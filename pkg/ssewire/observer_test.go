package ssewire

import (
	"testing"
	"time"
)

func at(base time.Time, ms int) time.Time {
	return base.Add(time.Duration(ms) * time.Millisecond)
}

// TestObserver_LastEventTypeTracksMostRecentNonPingEvent: the wire tracer
// (pkg/llm/stream_trace.go) needs to answer "what was the stream doing right
// before it went silent" — this is that answer, and pings must not count as
// "the last thing that happened" for that purpose.
func TestObserver_LastEventTypeTracksMostRecentNonPingEvent(t *testing.T) {
	start := time.Unix(0, 0)
	o := NewObserver(start)

	o.Observe(at(start, 10), []byte("event: message_start\ndata: {}\n\n"))
	if got := o.Result().LastEventType; got != "message_start" {
		t.Fatalf("LastEventType = %q, want message_start", got)
	}

	o.Observe(at(start, 20), []byte("event: content_block_delta\ndata: {}\n\n"))
	if got := o.Result().LastEventType; got != "content_block_delta" {
		t.Fatalf("LastEventType = %q, want content_block_delta", got)
	}

	// A ping arriving after the last real event must not overwrite it —
	// otherwise a stall record taken right after a ping would misreport
	// "the stream stalled right after a ping" as "right after nothing at
	// all, an event type of ping."
	o.Observe(at(start, 30), []byte("event: ping\ndata: {}\n\n"))
	if got := o.Result().LastEventType; got != "content_block_delta" {
		t.Fatalf("LastEventType = %q, want content_block_delta (ping must not overwrite it)", got)
	}
}

// TestObserver_MaxGapOffsetLocatesTheStall: knowing the longest gap was 90s
// is much less useful than knowing it started at the 30s mark — that's what
// lets a trace record say WHERE in the stream (relative to its start) things
// went quiet.
func TestObserver_MaxGapOffsetLocatesTheStall(t *testing.T) {
	start := time.Unix(0, 0)
	o := NewObserver(start)

	o.Observe(at(start, 1000), []byte("event: message_start\ndata: {}\n\n"))
	o.Observe(at(start, 1200), []byte("event: content_block_delta\ndata: {}\n\n"))
	// The long silence begins right after the read at 1200ms.
	o.Observe(at(start, 91_200), []byte("event: content_block_delta\ndata: {}\n\n"))

	got := o.Result()
	if got.MaxGap != 90*time.Second {
		t.Fatalf("MaxGap = %v, want 90s", got.MaxGap)
	}
	if got.MaxGapOffset != 1200*time.Millisecond {
		t.Fatalf("MaxGapOffset = %v, want 1.2s (when the gap began)", got.MaxGapOffset)
	}
}
