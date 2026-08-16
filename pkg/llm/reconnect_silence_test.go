package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// Both providers retry a failed stream internally (Stream's goroutine: back
// off, then re-issue NewStreaming). Both of those steps block, and neither
// used to put anything on the channel — but the channel was already handed to
// the agent, whose stream-idle watchdog (pkg/agent/streaming.go) started the
// moment Stream returned. A reconnect that takes longer than the idle window
// therefore reads as a dead stream and a perfectly healthy request is killed
// with "stream idle timeout: no data received after 2m0s".
//
// This is not hypothetical for a rate-limited fan-out: 429 is classified
// retryable (isRetryableAnthropicStreamErr), and probing the GLM endpoint
// showed it withholds response headers until it is ready to stream, so the
// re-issued request blocks for exactly as long as the request stays queued.

// stallingAnthropicServer answers the first request with a retryable status and
// then holds every later request open without writing headers, reproducing a
// queued reconnect.
func stallingAnthropicServer(t *testing.T, firstStatus int, hold time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(firstStatus)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"concurrency limit"}}`)
			return
		}
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestAnthropicStream_ReconnectDoesNotGoSilent(t *testing.T) {
	// Shrink the heartbeat so the test does not have to wait a real window.
	restore := reconnectHeartbeatInterval
	reconnectHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { reconnectHeartbeatInterval = restore })

	srv, _ := stallingAnthropicServer(t, http.StatusTooManyRequests, 3*time.Second)

	p, err := NewAnthropicProvider("test-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := p.Stream(ctx, ChatRequest{
		Model:    "test-model",
		Messages: []models.Message{{Role: models.RoleHuman, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// Stop the provider goroutine and wait for it to exit before the shared
	// reconnectHeartbeatInterval is restored — it reads that var on every retry,
	// so restoring it while the goroutine still runs is a data race. Registered
	// after the restore, so it runs before it (Cleanup is LIFO).
	t.Cleanup(func() {
		cancel()
		for range ch {
		}
	})

	// The agent's watchdog is running from here. Nothing may go quiet for longer
	// than the idle window while the provider is backing off and reconnecting.
	const idleWindow = 1500 * time.Millisecond
	deadline := time.After(4 * time.Second)
	var chunks int
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if chunks == 0 {
					t.Fatal("stream closed without ever sending a chunk")
				}
				return
			}
			chunks++
		case <-time.After(idleWindow):
			t.Fatalf("channel silent for %s while the provider was reconnecting — the agent's idle watchdog would kill this healthy request (chunks so far: %d)", idleWindow, chunks)
		case <-deadline:
			if chunks == 0 {
				t.Fatal("no chunks at all during the reconnect")
			}
			return
		}
	}
}

func TestOpenAICompatStream_ReconnectDoesNotGoSilent(t *testing.T) {
	restore := reconnectHeartbeatInterval
	reconnectHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { reconnectHeartbeatInterval = restore })

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"concurrency limit"}}`)
			return
		}
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	p, err := NewOpenAICompatProvider("openai-compat", "test-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := p.Stream(ctx, ChatRequest{
		Model:    "test-model",
		Messages: []models.Message{{Role: models.RoleHuman, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// Stop the provider goroutine and wait for it to exit before the shared
	// reconnectHeartbeatInterval is restored — it reads that var on every retry,
	// so restoring it while the goroutine still runs is a data race. Registered
	// after the restore, so it runs before it (Cleanup is LIFO).
	t.Cleanup(func() {
		cancel()
		for range ch {
		}
	})

	const idleWindow = 1500 * time.Millisecond
	deadline := time.After(4 * time.Second)
	var chunks int
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if chunks == 0 {
					t.Fatal("stream closed without ever sending a chunk")
				}
				return
			}
			chunks++
		case <-time.After(idleWindow):
			t.Fatalf("channel silent for %s while the provider was reconnecting (chunks so far: %d)", idleWindow, chunks)
		case <-deadline:
			if chunks == 0 {
				t.Fatal("no chunks at all during the reconnect")
			}
			return
		}
	}
}
