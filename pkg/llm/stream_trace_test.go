package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseServer starts an httptest server that streams the given lines (already
// including the trailing blank line between SSE events), flushing after each
// write and pausing pauseAfter between writes so tests can control timing.
// If holdAfter is >= 0, the handler blocks (without closing the connection)
// after writing that many lines, until the test's context is cancelled or
// release is closed — reproducing a stalled-but-open connection.
func sseServer(t *testing.T, lines []string, delay time.Duration, holdAfter int, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}
		for i, line := range lines {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
			flusher.Flush()
			if i == holdAfter-1 && release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// readTraceLines parses every line of the trace file as one JSON traceRecord.
func readTraceLines(t *testing.T, path string) []traceRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()

	var records []traceRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec traceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan trace file: %v", err)
	}
	return records
}

// Acceptance point 1: with DEEPAI_STREAM_TRACE_FILE unset, newHTTPClient must
// return exactly what it did before this feature existed — no wrapper layer,
// even a no-op one, since streaming responses must never pay for tracing
// they didn't ask for.
func TestNewHTTPClient_TraceDisabledByDefault(t *testing.T) {
	t.Setenv("DEEPAI_STREAM_TRACE_FILE", "")

	transport, ok := newHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport (no tracing wrapper) when DEEPAI_STREAM_TRACE_FILE is unset", newHTTPClient().Transport)
	}
	_ = transport
}

// Acceptance point 2: a normal SSE stream, fully consumed, leaves exactly one
// complete "done" record with the fields filled in correctly.
func TestTracingTransport_RecordsCompleteStream(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")

	lines := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	srv := sseServer(t, lines, 5*time.Millisecond, -1, nil)

	w, err := getTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("getTraceWriter: %v", err)
	}
	client := &http.Client{Transport: newTracingTransport(srv.Client().Transport, w, time.Minute)}

	resp, err := client.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	resp.Body.Close()
	if len(body) == 0 {
		t.Fatal("expected a non-empty body")
	}

	records := readTraceLines(t, tracePath)
	var done []traceRecord
	for _, r := range records {
		if r.Kind == "done" {
			done = append(done, r)
		}
	}
	if len(done) != 1 {
		t.Fatalf("got %d done records, want 1: %+v", len(done), records)
	}
	rec := done[0]
	if rec.Path != "/v1/messages" {
		t.Errorf("Path = %q, want /v1/messages", rec.Path)
	}
	if rec.Bytes == 0 {
		t.Error("Bytes = 0, want > 0")
	}
	if rec.Events != 3 {
		t.Errorf("Events = %d, want 3 (message_start + content_block_delta + message_stop)", rec.Events)
	}
	if rec.LastEventType != "message_stop" {
		t.Errorf("LastEventType = %q, want message_stop", rec.LastEventType)
	}
	if rec.EndReason != "eof" {
		t.Errorf("EndReason = %q, want eof", rec.EndReason)
	}
	if rec.TimeToFirstByte == "" {
		t.Error("TimeToFirstByte is empty")
	}
	if rec.TimeToFirstEvent == "" {
		t.Error("TimeToFirstEvent is empty")
	}
}

// Acceptance point 3: "event: ping" is exactly what the vendor SDK's
// ssestream discards before provider code ever sees it (see anthropic.go).
// The wire tracer sits below the SDK, so it must count pings correctly.
func TestTracingTransport_CountsPings(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")

	lines := []string{
		"event: message_start\ndata: {}\n\n",
		"event: ping\ndata: {}\n\n",
		"event: ping\ndata: {}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := sseServer(t, lines, 5*time.Millisecond, -1, nil)

	w, err := getTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("getTraceWriter: %v", err)
	}
	client := &http.Client{Transport: newTracingTransport(srv.Client().Transport, w, time.Minute)}

	resp, err := client.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	resp.Body.Close()

	records := readTraceLines(t, tracePath)
	var done *traceRecord
	for i := range records {
		if records[i].Kind == "done" {
			done = &records[i]
		}
	}
	if done == nil {
		t.Fatalf("no done record: %+v", records)
	}
	if done.Pings != 2 {
		t.Errorf("Pings = %d, want 2", done.Pings)
	}
	if done.Events != 2 {
		t.Errorf("Events = %d, want 2 (pings excluded)", done.Events)
	}
}

// Acceptance point 4: a stall must be logged BEFORE the request ends — a
// killed process must not lose it. This is the single most important record
// this feature exists to produce: it is the only way to know whether pings
// (or any bytes) were on the wire during a real 2-minute silence.
func TestTracingTransport_LogsStallBeforeRequestEnds(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")

	release := make(chan struct{})
	lines := []string{
		"event: message_start\ndata: {}\n\n",
		"event: message_stop\ndata: {}\n\n", // never reached until release closes
	}
	srv := sseServer(t, lines, 0, 1, release)
	t.Cleanup(func() { close(release) })

	w, err := getTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("getTraceWriter: %v", err)
	}
	// A short threshold so the test does not wait anywhere near 30s.
	client := &http.Client{Transport: newTracingTransport(srv.Client().Transport, w, 50*time.Millisecond)}

	resp, err := client.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	// Read only the first event; the server then holds, well past the
	// threshold, without the request ending.
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		records := readTraceLines(t, tracePath)
		for _, r := range records {
			if r.Kind == "stall" {
				if r.LastEventType != "message_start" {
					t.Errorf("stall record LastEventType = %q, want message_start", r.LastEventType)
				}
				return // found it — the request is still open (never closed here)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no stall record appeared before the request ended")
}

// Acceptance point 5: the wrapped reader must never buffer — for a streaming
// LLM response, buffering the whole body defeats token-by-token display.
// A slow, chunked server plus a reader that blocks on a channel proves bytes
// arrive at the consumer as the server sends them, not all at once at EOF.
func TestTracingTransport_DoesNotBufferResponseBody(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")

	firstSent := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		flusher.Flush()
		close(firstSent)
		<-release // hold the connection open; the second chunk never comes
		// until the test says so, well after it already observed
		// the first chunk.
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	w, err := getTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("getTraceWriter: %v", err)
	}
	client := &http.Client{Transport: newTracingTransport(srv.Client().Transport, w, time.Minute)}

	resp, err := client.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	<-firstSent // the server has flushed the first chunk and is now blocked
	// on <-release, which this test has NOT yet closed (that only happens in
	// Cleanup). If Read below returns the first chunk anyway, the reader is
	// not buffering — it handed over bytes as they arrived rather than
	// waiting for the full (never-to-arrive-yet) body.
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if n == 0 {
		t.Fatal("Read returned 0 bytes before the server sent its second chunk — response body is buffered")
	}
	if !strings.Contains(string(buf[:n]), "message_start") {
		t.Fatalf("Read returned unexpected data before message_stop was sent: %q", buf[:n])
	}
}

// Acceptance point 6: subagent fan-out means many requests trace to the same
// file concurrently. Every line must be a complete, valid JSON object — no
// interleaved writes — and every request's record must appear exactly once.
func TestTracingTransport_ConcurrentRequestsDoNotCorruptTraceFile(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")

	lines := []string{
		"event: message_start\ndata: {}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := sseServer(t, lines, time.Millisecond, -1, nil)

	w, err := getTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("getTraceWriter: %v", err)
	}
	client := &http.Client{Transport: newTracingTransport(srv.Client().Transport, w, time.Minute)}

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(srv.URL + fmt.Sprintf("/v1/messages?i=%d", i))
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Errorf("ReadAll: %v", err)
			}
		}()
	}
	wg.Wait()

	records := readTraceLines(t, tracePath) // fails the test if any line is invalid JSON
	var done int
	for _, r := range records {
		if r.Kind == "done" {
			done++
		}
	}
	if done != n {
		t.Fatalf("got %d done records, want %d (some request's record was lost or corrupted)", done, n)
	}
}
