package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/ssewire"
)

// EnvStreamTraceFile is the environment variable that turns on wire-level
// tracing of every streaming LLM response (see the doc comment on
// tracingTransport below for what it exists to answer). Empty/unset (the
// default) means "off, zero overhead" — newHTTPClient returns the exact
// same *http.Transport it always has, not even a no-op wrapper. Named
// consistently with DEEPAI_DEBUG_FILE / DEEPAI_ERROR_FILE (pkg/logs/config.go).
const EnvStreamTraceFile = "DEEPAI_STREAM_TRACE_FILE"

// defaultStallLogThreshold is how long a response body can go silent before
// a "stall" record is written for it, independent of whether the request
// ever finishes. 30s is comfortably above any legitimate inter-chunk gap
// this codebase has measured (probe-stream saw sub-11s first-event latency
// even against a 400KB/100k-token prompt, and periodic ping cadence is on
// the order of seconds — see HANDOFF.md's stream-idle section), so it will
// not fire on ordinary "the model is thinking" pauses, but it fires many
// times over before the agent's own 2-minute idle watchdog would kill the
// request — which is the whole point: by the time the watchdog acts, this
// file already has several stall records describing exactly what the wire
// was doing.
const defaultStallLogThreshold = 30 * time.Second

// traceRecord is one line of a stream trace file: either the final outcome
// of a request ("done") or a snapshot taken because the body has been
// silent past the stall threshold while the request is still open
// ("stall"). Fields are plain strings for durations (not nanoseconds) so the
// file is readable without tooling — this is meant to be grepped by a human
// chasing a live incident, not parsed by a dashboard.
type traceRecord struct {
	Seq              int64  `json:"seq"`
	Kind             string `json:"kind"` // "done" | "stall"
	StartedAt        string `json:"started_at"`
	Path             string `json:"path"`
	TimeToHeaders    string `json:"time_to_headers,omitempty"`
	TimeToFirstByte  string `json:"time_to_first_byte,omitempty"`
	TimeToFirstEvent string `json:"time_to_first_event,omitempty"`
	Bytes            int    `json:"bytes"`
	Events           int    `json:"events"`
	Pings            int    `json:"pings"`
	MaxGap           string `json:"max_gap,omitempty"`
	MaxGapOffset     string `json:"max_gap_offset,omitempty"`
	LastEventType    string `json:"last_event_type,omitempty"`
	EndReason        string `json:"end_reason,omitempty"`      // "done" records only
	SinceLastByte    string `json:"since_last_byte,omitempty"` // "stall" records only
}

// traceSeq is a process-wide monotonic request counter, shared by every
// tracingTransport instance (there is one per provider's *http.Client, but
// they typically share one trace file — see getTraceWriter). A single
// counter keeps "seq" meaningfully ordered across providers in that file.
var traceSeq int64

// traceWriter serializes appends to one trace file across goroutines (and
// across every tracingTransport pointed at the same path) so concurrent
// requests — the normal shape of a subagent fan-out — never interleave two
// records into one corrupt line.
type traceWriter struct {
	mu   sync.Mutex
	file *os.File
}

// traceWriters caches one traceWriter per file path so every tracingTransport
// built against the same DEEPAI_STREAM_TRACE_FILE value shares a single
// writer (and a single *os.File) instead of racing independent ones.
var (
	traceWritersMu sync.Mutex
	traceWriters   = map[string]*traceWriter{}
)

// getTraceWriter returns the shared traceWriter for path, opening the file
// (append, create) the first time path is seen.
func getTraceWriter(path string) (*traceWriter, error) {
	traceWritersMu.Lock()
	defer traceWritersMu.Unlock()
	if w, ok := traceWriters[path]; ok {
		return w, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &traceWriter{file: f}
	traceWriters[path] = w
	return w, nil
}

// writeRecord appends one JSON line. Errors are logged, not returned:
// tracing must never be able to break (or even slow down, beyond the lock)
// the actual streaming request it is observing.
func (w *traceWriter) writeRecord(rec traceRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("stream trace: could not marshal record", "err", err)
		return
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.file.Write(line); err != nil {
		slog.Warn("stream trace: write failed", "err", err)
	}
}

// tracingTransport wraps an http.RoundTripper to answer one question a live
// incident could not answer any other way: while a streaming response is
// silent, is there anything at all on the wire (a ping, or anything else)?
// The vendor SDK's ssestream discards "event: ping" before provider code
// ever sees it, so pkg/llm has never had a way to observe this — see
// anthropic.go's payloadMiddleware, which only ever looks at the request.
//
// This is measurement only. It does not retry, does not change what the
// idle watchdog does, and does not alter the response in any way — it just
// counts and times bytes as they pass through, and appends structured
// records to a file.
type tracingTransport struct {
	next           http.RoundTripper
	w              *traceWriter
	stallThreshold time.Duration
}

// newTracingTransport builds a tracingTransport. stallThreshold is a
// parameter (rather than always reading defaultStallLogThreshold) purely so
// tests can shrink it — production code (newHTTPClient) always passes
// defaultStallLogThreshold.
func newTracingTransport(next http.RoundTripper, w *traceWriter, stallThreshold time.Duration) *tracingTransport {
	return &tracingTransport{next: next, w: w, stallThreshold: stallThreshold}
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	seq := atomic.AddInt64(&traceSeq, 1)
	path := req.URL.Path

	resp, err := t.next.RoundTrip(req)
	timeToHeaders := time.Since(start)
	if err != nil {
		t.w.writeRecord(traceRecord{
			Seq:           seq,
			Kind:          "done",
			StartedAt:     start.Format(time.RFC3339Nano),
			Path:          path,
			TimeToHeaders: timeToHeaders.String(),
			EndReason:     "transport error: " + err.Error(),
		})
		return resp, err
	}
	if resp.Body == nil {
		return resp, err
	}

	tb := &tracedBody{
		rc:            resp.Body,
		obs:           ssewire.NewObserver(start),
		w:             t.w,
		seq:           seq,
		start:         start,
		path:          path,
		timeToHeaders: timeToHeaders,
		threshold:     t.stallThreshold,
		lastByteAt:    start,
		stallDone:     make(chan struct{}),
	}
	go tb.watchStalls()
	resp.Body = tb
	return resp, nil
}

// tracedBody wraps a response body to observe it byte-for-byte as the
// caller reads it — it never reads ahead and never holds a copy of the
// content, only running counts (via ssewire.Observer) — while a background
// goroutine watches for silence exceeding the stall threshold so a stall is
// recorded WHILE the request is still open, not only once it ends (see
// watchStalls).
type tracedBody struct {
	rc            io.ReadCloser
	w             *traceWriter
	seq           int64
	start         time.Time
	path          string
	timeToHeaders time.Duration
	threshold     time.Duration

	mu          sync.Mutex
	obs         *ssewire.Observer
	lastByteAt  time.Time
	lastAlertAt time.Time

	stallDone chan struct{}
	finished  atomic.Bool
}

func (b *tracedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	now := time.Now()
	if n > 0 {
		b.mu.Lock()
		b.obs.Observe(now, p[:n])
		b.lastByteAt = now
		b.lastAlertAt = time.Time{}
		b.mu.Unlock()
	}
	if err != nil {
		b.finish(now, classifyEndReason(err))
	}
	return n, err
}

func (b *tracedBody) Close() error {
	b.finish(time.Now(), "closed")
	return b.rc.Close()
}

// finish writes the terminal "done" record exactly once (Read hitting EOF
// or an error, and Close, race to call this) and stops watchStalls.
func (b *tracedBody) finish(at time.Time, reason string) {
	if !b.finished.CompareAndSwap(false, true) {
		return
	}
	close(b.stallDone)

	b.mu.Lock()
	b.obs.Finish(at)
	result := b.obs.Result()
	b.mu.Unlock()

	b.w.writeRecord(traceRecord{
		Seq:              b.seq,
		Kind:             "done",
		StartedAt:        b.start.Format(time.RFC3339Nano),
		Path:             b.path,
		TimeToHeaders:    b.timeToHeaders.String(),
		TimeToFirstByte:  result.FirstByte.String(),
		TimeToFirstEvent: result.FirstEvent.String(),
		Bytes:            result.Bytes,
		Events:           result.Events,
		Pings:            result.Pings,
		MaxGap:           result.MaxGap.String(),
		MaxGapOffset:     result.MaxGapOffset.String(),
		LastEventType:    result.LastEventType,
		EndReason:        reason,
	})
}

// watchStalls polls at a fine grain (threshold/10, floored at 1ms) so a
// stall is caught promptly regardless of how large threshold is, but only
// WRITES a record once per threshold-length window of continued silence —
// otherwise a real multi-minute stall would spam one record per poll tick.
// lastAlertAt is reset to zero the moment a byte arrives (see Read), so a
// fresh gap always logs promptly again.
func (b *tracedBody) watchStalls() {
	interval := b.threshold / 10
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stallDone:
			return
		case now := <-ticker.C:
			b.mu.Lock()
			silence := now.Sub(b.lastByteAt)
			shouldAlert := silence >= b.threshold &&
				(b.lastAlertAt.IsZero() || now.Sub(b.lastAlertAt) >= b.threshold)
			var bytesSoFar, eventsSoFar, pingsSoFar int
			var lastEvt string
			if shouldAlert {
				b.lastAlertAt = now
				res := b.obs.Result()
				bytesSoFar, eventsSoFar, pingsSoFar, lastEvt = res.Bytes, res.Events, res.Pings, res.LastEventType
			}
			b.mu.Unlock()
			if !shouldAlert {
				continue
			}
			b.w.writeRecord(traceRecord{
				Seq:           b.seq,
				Kind:          "stall",
				StartedAt:     b.start.Format(time.RFC3339Nano),
				Path:          b.path,
				TimeToHeaders: b.timeToHeaders.String(),
				Bytes:         bytesSoFar,
				Events:        eventsSoFar,
				Pings:         pingsSoFar,
				LastEventType: lastEvt,
				SinceLastByte: silence.String(),
			})
		}
	}
}

// classifyEndReason turns a Read error into a short, greppable category:
// the normal end of stream, the two shapes a cancelled context takes, or
// anything else verbatim.
func classifyEndReason(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return "eof"
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	return "read error: " + err.Error()
}

// newTracingHTTPTransport wraps transport in a tracingTransport when
// EnvStreamTraceFile is set to a non-empty path, logging (never failing) if
// the file cannot be opened. It returns transport UNCHANGED when the env
// var is unset — no wrapper of any kind — which is what
// TestNewHTTPClient_TraceDisabledByDefault pins down.
func newTracingHTTPTransport(transport http.RoundTripper) http.RoundTripper {
	path := os.Getenv(EnvStreamTraceFile)
	if path == "" {
		return transport
	}
	w, err := getTraceWriter(path)
	if err != nil {
		slog.Warn("stream trace: could not open trace file, tracing disabled",
			"env", EnvStreamTraceFile, "path", path, "err", err)
		return transport
	}
	return newTracingTransport(transport, w, defaultStallLogThreshold)
}
