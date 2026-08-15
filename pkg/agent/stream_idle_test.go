package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// idleHangProvider emits exactly one chunk and then blocks forever on the
// stream's ctx: it never sends anything else and never closes the channel
// on its own — the channel only closes (via the deferred close) once ctx is
// cancelled out from under it. This models a genuinely hung LLM request: the
// underlying HTTP transport deliberately has no per-request timeout
// (pkg/llm/http.go), so nothing EXCEPT an idle watchdog that cancels the
// per-request ctx will ever make this provider give up.
type idleHangProvider struct{}

func (idleHangProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (idleHangProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Delta: "hello"}
		<-ctx.Done()
	}()
	return ch, nil
}

// TestStreamIdleWatchdog_FiresOnHungStream is the RED test for the stream
// idle watchdog (plan #8): a stream that emits one chunk and then goes
// silent forever must not be allowed to hang Agent.Run indefinitely.
//
// RED signature (today): Run passes the bare outer ctx (context.Background()
// here, no deadline) straight to a.llm.Stream and consumes the channel with
// a plain `for chunk := range stream`, with nothing that ever times out an
// individual chunk wait — idleHangProvider's goroutine blocks on
// `<-ctx.Done()` which never fires, so the channel never closes and Run
// hangs forever. A test-level timeout guard (the select below) turns that
// hang into an observable, clean test failure instead of hanging the whole
// test binary.
func TestStreamIdleWatchdog_FiresOnHungStream(t *testing.T) {
	a := New(AgentConfig{LLMProvider: idleHangProvider{}, MaxToolCalls: 5})
	a.streamIdleTimeout = 50 * time.Millisecond

	type outcome struct {
		result *RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := a.Run(context.Background(), "s1", []models.Message{
			{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"},
		})
		done <- outcome{result, err}
	}()

	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("Run() error = nil, want an idle-timeout error")
		}
		var timeoutErr *TimeoutError
		if !errors.As(out.err, &timeoutErr) {
			t.Fatalf("Run() error = %v (%T), want *TimeoutError (errors.As must catch it — see pkg/agent/subagent.go's retry logic, which relies on this)", out.err, out.err)
		}
		if !strings.Contains(strings.ToLower(out.err.Error()), "idle") {
			t.Fatalf("Run() error = %q, want it to mention the idle timeout", out.err.Error())
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return within 1s of a 50ms idle timeout — stream idle watchdog did not fire (RED signature: Run hangs forever on a silent stream)")
	}
}

// idleGapProvider streams several chunks with small gaps between them (all
// well under the configured idle timeout), then finishes normally. Control
// case for the idle watchdog: normal cadence must not be mistaken for a hang.
type idleGapProvider struct {
	turn int
}

func (idleGapProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *idleGapProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		for i := 0; i < 3; i++ {
			time.Sleep(10 * time.Millisecond)
			ch <- llm.StreamChunk{Delta: fmt.Sprintf("chunk%d ", i)}
		}
		time.Sleep(10 * time.Millisecond)
		ch <- llm.StreamChunk{Done: true, Stop: "stop"}
	}()
	return ch, nil
}

func TestStreamIdleWatchdog_NormalGapsComplete(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &idleGapProvider{}, MaxToolCalls: 5})
	a.streamIdleTimeout = 200 * time.Millisecond // gaps of 10ms are well under this

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (gaps stay well under the idle timeout)", err)
	}
	if result == nil || strings.TrimSpace(result.FinalOutput) == "" {
		t.Fatalf("expected non-empty FinalOutput, got %+v", result)
	}
}

// deadlineAwareChattyProvider streams chunks forever at a fast, steady
// cadence (never idle) but respects ctx cancellation, closing its channel
// promptly once ctx.Done() fires — the well-behaved-provider contract that
// requestTimeout's ctx-deadline handling has always relied on (unrelated to,
// and pre-dating, the idle watchdog).
type deadlineAwareChattyProvider struct{}

func (deadlineAwareChattyProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (deadlineAwareChattyProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				ch <- llm.StreamChunk{Delta: "x"}
			}
		}
	}()
	return ch, nil
}

// TestStreamIdleWatchdog_ComposesWithRequestTimeout is the interaction check
// for brief item B4: the idle watchdog and the pre-existing requestTimeout
// (total-duration) wrap must compose — whichever fires first wins. Here
// chunks arrive every 10ms (never idle), so the generous idle timeout must
// NOT be what ends the run; only the much shorter requestTimeout should.
func TestStreamIdleWatchdog_ComposesWithRequestTimeout(t *testing.T) {
	a := New(AgentConfig{
		LLMProvider:    deadlineAwareChattyProvider{},
		MaxToolCalls:   5,
		RequestTimeout: 100 * time.Millisecond,
	})
	// Leave a.streamIdleTimeout at its (generous, 2-minute) default — chunks
	// every 10ms never come close to it, isolating the requestTimeout path.

	start := time.Now()
	_, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() error = nil, want a request-timeout error")
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Run() error = %v (%T), want *TimeoutError", err, err)
	}
	if timeoutErr.Duration != 100*time.Millisecond {
		t.Fatalf("TimeoutError.Duration = %s, want 100ms (the requestTimeout) — a different value would mean the idle watchdog fired instead of the deadline", timeoutErr.Duration)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run() took %s, want well under the 2-minute idle timeout (proves requestTimeout — not the idle watchdog — ended the run)", elapsed)
	}
}

// errThenHangProvider emits one chunk carrying an error and then blocks
// forever WITHOUT closing the channel and WITHOUT respecting ctx at all —
// modeling a provider goroutine that has already reported a failure but then
// wedges (e.g. on a downstream write) instead of unwinding as expected.
type errThenHangProvider struct{}

func (errThenHangProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (errThenHangProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		// Deliberately no `defer close(ch)`: this goroutine never unwinds,
		// modeling the pathological case the fix must not depend on the
		// provider avoiding.
		ch <- llm.StreamChunk{Err: errors.New("boom")}
		select {} // block forever, ignoring ctx entirely
	}()
	return ch, nil
}

// TestStreamIdleWatchdog_ChunkErrorDrainDoesNotBlockRun is the RED test for
// review finding #1: consumeStream's chunk.Err branch drains the remaining
// stream SYNCHRONOUSLY (`for range stream {}` in the same goroutine that is
// trying to return from Run). That assumed the provider would unwind and
// close its channel shortly after surfacing an error — true for realistic
// providers, but not guaranteed, and a provider that emits an error chunk
// and then wedges without closing hangs Run forever, exactly like the idle-
// timeout case the watchdog was built to catch. The fix must apply the same
// background-drain-goroutine treatment already used for the idle-timer exit:
// cancel first, drain in the background, return immediately.
//
// RED signature (today): Run blocks forever inside the synchronous drain,
// so the test-level 1s guard below fires instead of observing a real return.
func TestStreamIdleWatchdog_ChunkErrorDrainDoesNotBlockRun(t *testing.T) {
	a := New(AgentConfig{LLMProvider: errThenHangProvider{}, MaxToolCalls: 5})

	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), "s1", []models.Message{
			{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() error = nil, want the provider's error to surface")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("Run() error = %q, want it to mention the provider's error (\"boom\")", err.Error())
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return within 1s after a provider error chunk — the chunk.Err drain blocked Run (RED signature: synchronous drain on a channel that never closes)")
	}
}
