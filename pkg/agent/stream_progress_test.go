package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// toolArgProgressProvider models the real-world failure this file guards
// against: a model emitting one very large tool-call argument (e.g.
// write_file's markdown body) as many llm.StreamChunk{Progress: true}
// fragments — the provider-layer signal added so pkg/agent's stream idle
// watchdog sees that accumulation as activity instead of silence. Each
// fragment arrives `gap` apart; turn 2 (and beyond) finishes the run cleanly
// with no tool calls so a successful Run has a deterministic, text-bearing
// FinalOutput.
type toolArgProgressProvider struct {
	turn         int
	fragments    int
	gap          time.Duration
	toolName     string
	toolCallID   string
	sawTextDelta bool // set if this provider is ever asked to also emit a text delta (it never should be here)
}

func (*toolArgProgressProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *toolArgProgressProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	turn := p.turn
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if turn == 1 {
			for i := 0; i < p.fragments; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(p.gap):
				}
				ch <- llm.StreamChunk{Progress: true}
			}
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: p.toolCallID, Name: p.toolName, Arguments: map[string]any{"path": "design.md"}}},
			}
			ch <- llm.StreamChunk{Done: true, Stop: "tool_calls"}
			return
		}
		// Any later turn (after the tool result comes back) ends the run
		// cleanly with plain text, no tool calls.
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

func newRegistryWithNoOpTool(name string) *tools.Registry {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: name,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{ToolName: name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	})
	return reg
}

// TestStreamIdleWatchdog_ToolArgumentProgressKeepsStreamAlive is the RED test
// for the fix: a stream that never sends text, but sends many
// llm.StreamChunk{Progress: true} fragments (modeling a provider mid-flight
// on a large tool-call argument), with every inter-fragment gap under the
// idle window but their TOTAL span well over it, must not trip the idle
// watchdog. Before the fix, providers sent nothing at all during this span
// (input_json_delta/tool_calls deltas were accumulated into a builder and
// never forwarded), so the fragments/gap below would starve the idle timer
// and Run would return a *TimeoutError instead of completing.
func TestStreamIdleWatchdog_ToolArgumentProgressKeepsStreamAlive(t *testing.T) {
	const gap = 30 * time.Millisecond
	const fragments = 10 // total span 300ms, well over the 80ms idle window below
	idleWindow := 80 * time.Millisecond
	if gap >= idleWindow {
		t.Fatalf("test setup: gap (%s) must be under idleWindow (%s)", gap, idleWindow)
	}
	if time.Duration(fragments)*gap <= idleWindow {
		t.Fatalf("test setup: total span (%s) must exceed idleWindow (%s)", time.Duration(fragments)*gap, idleWindow)
	}

	provider := &toolArgProgressProvider{fragments: fragments, gap: gap, toolName: "write_file", toolCallID: "call_1"}
	a := New(AgentConfig{LLMProvider: provider, Tools: newRegistryWithNoOpTool("write_file"), MaxToolCalls: 5})
	a.streamIdleTimeout = idleWindow

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "write a long design doc"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — a steady stream of tool-argument progress chunks must not starve the idle watchdog", err)
	}
	if result == nil || strings.TrimSpace(result.FinalOutput) != "done" {
		t.Fatalf("Run() result = %+v, want FinalOutput %q", result, "done")
	}
}

// TestStreamIdleWatchdog_ToolArgumentOnlyStreamNoStrayText covers the other
// required shape: a stream that emits nothing but Progress chunks and a
// final ToolCalls chunk must produce EXACTLY the tool call and no stray
// text — no AgentEventTextChunk, and the assistant message recorded for that
// turn has empty Content.
func TestStreamIdleWatchdog_ToolArgumentOnlyStreamNoStrayText(t *testing.T) {
	provider := &toolArgProgressProvider{fragments: 5, gap: 5 * time.Millisecond, toolName: "write_file", toolCallID: "call_1"}
	a := New(AgentConfig{LLMProvider: provider, Tools: newRegistryWithNoOpTool("write_file"), MaxToolCalls: 5})
	a.streamIdleTimeout = 2 * time.Second

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "write a long design doc"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	var sawText bool
	for evt := range a.Events() {
		if evt.Type == AgentEventTextChunk {
			sawText = true
			t.Errorf("unexpected AgentEventTextChunk with text %q — a Progress-only stream must never surface a text event", evt.Text)
		}
	}
	if sawText {
		t.Fatal("saw at least one stray text chunk event (see above)")
	}

	// The turn-1 assistant message (tool call, no text) must be present with
	// empty Content and exactly the one tool call.
	var found bool
	for _, m := range result.Messages {
		if m.Role != models.RoleAI || len(m.ToolCalls) == 0 {
			continue
		}
		found = true
		if m.Content != "" {
			t.Errorf("assistant tool-call message Content = %q, want empty (no text was ever streamed)", m.Content)
		}
		if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name != "write_file" {
			t.Errorf("assistant tool calls = %+v, want exactly one write_file call", m.ToolCalls)
		}
	}
	if !found {
		t.Fatal("no assistant tool-call message found in result.Messages")
	}
}

// TestStreamIdleWatchdog_SilentAfterProgressStillTimesOut is the control
// case: the fix must not disable the watchdog. A stream that sends a few
// genuine progress chunks and then goes silent for longer than the idle
// window must still fail with a *TimeoutError, exactly as before.
func TestStreamIdleWatchdog_SilentAfterProgressStillTimesOut(t *testing.T) {
	idleWindow := 50 * time.Millisecond
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Progress: true}
		ch <- llm.StreamChunk{Progress: true}
		// Then genuine silence, well past idleWindow, forever (until ctx is
		// cancelled by the watchdog).
		select {}
	}()
	provider := progressThenHangProvider{ch: ch}
	a := New(AgentConfig{LLMProvider: provider, MaxToolCalls: 5})
	a.streamIdleTimeout = idleWindow

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
			t.Fatalf("Run() error = %v (%T), want *TimeoutError", out.err, out.err)
		}
		if !strings.Contains(strings.ToLower(out.err.Error()), "idle") {
			t.Fatalf("Run() error = %q, want it to mention the idle timeout", out.err.Error())
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return within 1s of a 50ms idle timeout after progress chunks stopped — the watchdog must still fire on genuine silence")
	}
}

// progressThenHangProvider wraps a pre-built channel so the goroutine that
// feeds it (started once, in the test above) is reused unmodified as the
// Stream() result.
type progressThenHangProvider struct {
	ch <-chan llm.StreamChunk
}

func (progressThenHangProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p progressThenHangProvider) Stream(context.Context, llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	return p.ch, nil
}
