// Package ssewire classifies raw bytes read off an SSE (Server-Sent Events)
// HTTP response body: it timestamps reads and tracks how much time elapsed
// between them, and separates real events from "event: ping" keepalives.
//
// This started as a one-off inside pkg/commands/probe.go (the stream-idle
// diagnostic CLI). It is extracted here so pkg/llm's wire-level tracing
// transport (pkg/llm/stream_trace.go) can share the exact same classifier
// instead of a second, inevitably-drifting copy — this repository has
// stepped on that failure mode more than once (see HANDOFF.md).
package ssewire

import (
	"bytes"
	"strings"
	"time"
)

// Observation is what one response body did on the wire.
type Observation struct {
	FirstByte  time.Duration // start -> first byte of body (any byte, including a ping)
	FirstEvent time.Duration // start -> first non-ping SSE event
	MaxGap     time.Duration // longest silence, counting the wait before the first byte
	// MaxGapOffset is start -> the moment the longest gap BEGAN (i.e. the
	// last read before it), so a stall can be located in the stream rather
	// than only measured.
	MaxGapOffset time.Duration
	Bytes        int
	Events       int
	Pings        int
	// LastEventType is the "event:" name of the most recently classified
	// non-ping event (e.g. "content_block_delta"), or "" if none yet. It
	// answers "what was the stream doing right before it went silent" —
	// before the first event, mid-text, or mid-tool-call-arguments.
	LastEventType string
}

// Observer timestamps raw reads and classifies SSE lines as they stream
// past. Classification tolerates reads that split lines, since TCP does not
// respect line boundaries.
type Observer struct {
	start    time.Time
	lastRead time.Time
	obs      Observation
	partial  []byte
}

// NewObserver creates an Observer whose durations are measured from start.
func NewObserver(start time.Time) *Observer {
	return &Observer{start: start, lastRead: start}
}

// Observe records one non-empty read of the body at time at.
func (o *Observer) Observe(at time.Time, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if gap := at.Sub(o.lastRead); gap > o.obs.MaxGap {
		o.obs.MaxGap = gap
		o.obs.MaxGapOffset = o.lastRead.Sub(o.start)
	}
	o.lastRead = at
	if o.obs.Bytes == 0 {
		o.obs.FirstByte = at.Sub(o.start)
	}
	o.obs.Bytes += len(chunk)

	data := append(o.partial, chunk...)
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		o.classify(at, strings.TrimSpace(string(data[:idx])))
		data = data[idx+1:]
	}
	o.partial = append(o.partial[:0], data...)
}

func (o *Observer) classify(at time.Time, line string) {
	name, ok := strings.CutPrefix(line, "event:")
	if !ok {
		return
	}
	name = strings.TrimSpace(name)
	if name == "ping" {
		o.obs.Pings++
		return
	}
	o.obs.Events++
	o.obs.LastEventType = name
	if o.obs.FirstEvent == 0 {
		o.obs.FirstEvent = at.Sub(o.start)
	}
}

// Finish folds in the silence between the last read and the end of the
// request. Without it the stall a STALL row exists to describe — minutes of
// nothing after the final byte — is never measured at all.
func (o *Observer) Finish(at time.Time) {
	if gap := at.Sub(o.lastRead); gap > o.obs.MaxGap {
		o.obs.MaxGap = gap
		o.obs.MaxGapOffset = o.lastRead.Sub(o.start)
	}
}

// Result returns the observation accumulated so far. Safe to call
// concurrently with itself (read-only), but callers that also call Observe
// or Finish from another goroutine must serialize with their own lock —
// Observer has none of its own.
func (o *Observer) Result() Observation { return o.obs }
