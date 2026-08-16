package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeOnce_HeaderWaitIsBounded(t *testing.T) {
	// A server that accepts the connection and never writes response headers is
	// the queued-request shape this probe exists to describe. With the idle
	// timer armed only after client.Do returns, the probe hangs forever on it
	// and prints nothing — and the SlowHeaders verdict becomes unreachable in
	// exactly the case it names.
	// The handler must also release on an explicit signal: a cancelled client
	// request does not reliably cancel r.Context() before anything is written,
	// and srv.Close blocks on in-flight handlers.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(srv.Close)              // registered first, so it runs last
	t.Cleanup(func() { close(stop) }) // runs first, releasing the handler

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan probeResult, 1)
	go func() {
		done <- probeOnce(ctx, srv.Client(), probeConfig{
			model:     "test-model",
			baseURL:   srv.URL,
			apiKey:    "test-key",
			prompt:    "hi",
			idle:      300 * time.Millisecond,
			maxTokens: 16,
		}, 1)
	}()

	select {
	case res := <-done:
		if !res.Stalled {
			t.Fatalf("result = %+v, want Stalled", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probeOnce did not return — the wait for response headers is unbounded")
	}
}

// The probe's job is to settle one question: when N streaming requests run in
// parallel and some produce nothing for minutes, is the server withholding
// data (it accepted the request, returned headers, then went quiet) or is the
// silence ours? sseObserver is the measurement core — it timestamps raw reads
// off the wire and classifies SSE lines, including the ping keepalives the
// Anthropic SDK swallows before provider code ever sees them.

func at(base time.Time, ms int) time.Time {
	return base.Add(time.Duration(ms) * time.Millisecond)
}

func TestSSEObserver_CountsBytesEventsAndPings(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)

	body := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	o.observe(at(start, 100), []byte(body))

	got := o.result()
	if got.Bytes != len(body) {
		t.Fatalf("Bytes = %d, want %d", got.Bytes, len(body))
	}
	if got.Pings != 1 {
		t.Fatalf("Pings = %d, want 1", got.Pings)
	}
	if got.Events != 2 {
		t.Fatalf("Events = %d, want 2 (message_start + content_block_delta, ping excluded)", got.Events)
	}
}

func TestSSEObserver_FirstEventSkipsPings(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)

	// The shape that would prove "server alive but withholding output": pings
	// arrive early, the real event only much later.
	o.observe(at(start, 500), []byte("event: ping\ndata: {}\n\n"))
	o.observe(at(start, 1500), []byte("event: ping\ndata: {}\n\n"))
	o.observe(at(start, 9000), []byte("event: content_block_delta\ndata: {}\n\n"))

	got := o.result()
	if got.FirstByte != 500*time.Millisecond {
		t.Fatalf("FirstByte = %v, want 500ms (the first ping is still wire activity)", got.FirstByte)
	}
	if got.FirstEvent != 9000*time.Millisecond {
		t.Fatalf("FirstEvent = %v, want 9s (pings must not count as output)", got.FirstEvent)
	}
	if got.Pings != 2 {
		t.Fatalf("Pings = %d, want 2", got.Pings)
	}
}

func TestSSEObserver_HandlesLinesSplitAcrossReads(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)

	// TCP does not respect line boundaries.
	o.observe(at(start, 10), []byte("event: con"))
	o.observe(at(start, 20), []byte("tent_block_delta\ndata: {}\n\nev"))
	o.observe(at(start, 30), []byte("ent: ping\ndata: {}\n\n"))

	got := o.result()
	if got.Events != 1 {
		t.Fatalf("Events = %d, want 1 — a line split across reads must still be classified", got.Events)
	}
	if got.Pings != 1 {
		t.Fatalf("Pings = %d, want 1", got.Pings)
	}
	if got.FirstEvent != 20*time.Millisecond {
		t.Fatalf("FirstEvent = %v, want 20ms (the read that completed the line)", got.FirstEvent)
	}
}

func TestSSEObserver_MaxGapIncludesTheWaitBeforeFirstByte(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)

	// A 100s wait for the first byte IS the stall being investigated; measuring
	// gaps only between reads would report it as zero.
	o.observe(at(start, 100_000), []byte("event: ping\ndata: {}\n\n"))
	o.observe(at(start, 101_000), []byte("event: content_block_delta\ndata: {}\n\n"))

	got := o.result()
	if got.MaxGap != 100*time.Second {
		t.Fatalf("MaxGap = %v, want 100s (start -> first byte counts as a gap)", got.MaxGap)
	}
}

func TestSSEObserver_NoActivityReportsZeroFirstByte(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)
	got := o.result()
	if got.FirstByte != 0 || got.Bytes != 0 || got.Events != 0 {
		t.Fatalf("empty observation = %+v, want all zero", got)
	}
}

func TestSSEObserver_FinishFoldsInTheTerminalSilence(t *testing.T) {
	// The stall a STALL row exists to describe is the silence AFTER the last
	// read. Folding gaps only on reads reports the quietest possible number for
	// the loudest possible failure.
	start := time.Unix(0, 0)
	o := newSSEObserver(start)
	o.observe(at(start, 1000), []byte("event: ping\ndata: {}\n\n"))
	o.finish(at(start, 121_000)) // idle timer cancelled the read here

	got := o.result()
	if got.MaxGap != 120*time.Second {
		t.Fatalf("MaxGap = %v, want 120s (silence from the last read to the end)", got.MaxGap)
	}
}

func TestSSEObserver_FinishNeverShrinksMaxGap(t *testing.T) {
	start := time.Unix(0, 0)
	o := newSSEObserver(start)
	o.observe(at(start, 50_000), []byte("event: content_block_delta\ndata: {}\n\n"))
	o.finish(at(start, 50_100))

	if got := o.result().MaxGap; got != 50*time.Second {
		t.Fatalf("MaxGap = %v, want 50s", got)
	}
}

func TestProbeVerdict_NonOKStatusIsNotWithholding(t *testing.T) {
	// A fast 429 has headers, no SSE events, and a body read in milliseconds.
	// Calling that "accepted the request and withheld output" is the exact
	// opposite of what it means — and the wrong conclusion for a concurrency
	// investigation, which is what this probe is for.
	results := []probeResult{
		{Index: 1, Status: 200, HeadersAt: 400 * time.Millisecond, Obs: observation{Events: 40}},
		{Index: 2, Status: 429, HeadersAt: 300 * time.Millisecond, Stalled: true},
	}
	v := probeVerdict(results)
	if v.ServerWithheld {
		t.Fatalf("verdict = %+v, want ServerWithheld=false for a 429", v)
	}
	if !v.Rejected {
		t.Fatalf("verdict = %+v, want Rejected", v)
	}
}

func TestProbeVerdict_ServerWithheldData(t *testing.T) {
	// Headers came back fast for every request, but only one produced events:
	// the server accepted all of them and then went quiet on the rest.
	results := []probeResult{
		{Index: 1, Status: 200, HeadersAt: 400 * time.Millisecond, Obs: observation{Events: 40, FirstEvent: time.Second}},
		{Index: 2, Status: 200, HeadersAt: 420 * time.Millisecond, Stalled: true},
		{Index: 3, Status: 200, HeadersAt: 410 * time.Millisecond, Stalled: true},
	}
	v := probeVerdict(results)
	if !v.ServerWithheld {
		t.Fatalf("verdict = %+v, want ServerWithheld (headers fast, no events)", v)
	}
	if v.KeepalivesSeen {
		t.Fatal("no pings were recorded, KeepalivesSeen must be false")
	}
}

func TestProbeVerdict_KeepalivesDuringStall(t *testing.T) {
	// The stalled requests were receiving pings the whole time — the connection
	// was demonstrably alive and the idle watchdog is killing a healthy stream
	// because the SDK drops pings.
	results := []probeResult{
		{Index: 1, Status: 200, HeadersAt: 400 * time.Millisecond, Obs: observation{Events: 40}},
		{Index: 2, Status: 200, HeadersAt: 420 * time.Millisecond, Stalled: true, Obs: observation{Pings: 12, Bytes: 300}},
	}
	v := probeVerdict(results)
	if !v.KeepalivesSeen {
		t.Fatalf("verdict = %+v, want KeepalivesSeen", v)
	}
}

func TestProbeVerdict_NoStallReproduced(t *testing.T) {
	results := []probeResult{
		{Index: 1, HeadersAt: 400 * time.Millisecond, Obs: observation{Events: 40}},
		{Index: 2, HeadersAt: 410 * time.Millisecond, Obs: observation{Events: 38}},
	}
	v := probeVerdict(results)
	if v.ServerWithheld || v.KeepalivesSeen {
		t.Fatalf("verdict = %+v, want a clean run", v)
	}
	if v.Stalled != 0 {
		t.Fatalf("Stalled = %d, want 0", v.Stalled)
	}
}

func TestProbeVerdict_SlowHeadersIsNotWithholding(t *testing.T) {
	// If the stall is before response headers, the agent's idle watchdog never
	// even starts (NewStreaming blocks on headers) — a different failure, and
	// the verdict must not conflate the two.
	results := []probeResult{
		{Index: 1, Status: 200, HeadersAt: 400 * time.Millisecond, Obs: observation{Events: 40}},
		{Index: 2, Status: 200, HeadersAt: 150 * time.Second, Stalled: true},
	}
	v := probeVerdict(results)
	if v.ServerWithheld {
		t.Fatalf("verdict = %+v, want ServerWithheld=false — that request stalled before headers", v)
	}
	if !v.SlowHeaders {
		t.Fatalf("verdict = %+v, want SlowHeaders", v)
	}
}
