package chat

import (
	"context"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// fanOutProvider emits ONE assistant turn with two parallel-safe tool calls,
// then ends the run on the next turn (which an interrupted run never reaches).
type fanOutProvider struct{ turn int }

func (p *fanOutProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *fanOutProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-quick", Name: "quick", Arguments: map[string]any{}},
					{ID: "call-blocking", Name: "blocking", Arguments: map[string]any{}},
				},
				Stop: "tool_calls",
				Done: true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestRunTurn_InterruptStillRendersPendingEvents pins the other half of the
// "interrupted fan-out looks like it is still running" bug: the event loop's
// ctx.Done branch used to break out without draining the event channel, so
// every event that had not been rendered when Ctrl+C landed was discarded.
// The clean-exit branch drains; this one did not.
//
// What that cost the user: the tool_call_end of every call in the interrupted
// batch was dropped, so the "⚙ tool…" lines committed when the batch started
// never got an outcome printed under them. The REPL was back at its prompt
// under a screen that still read as work in flight.
//
// The blocking tool here returns only after the interrupt, and then sleeps
// briefly so the event loop is already parked in its ctx.Done branch: its
// completion event can therefore only reach the UI through that branch's
// drain.
func TestRunTurn_InterruptStillRendersPendingEvents(t *testing.T) {
	r, ui := newSessionCarryTestRepl(t, &fanOutProvider{})

	quickRan := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name:         "quick",
		ParallelSafe: true,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			close(quickRan)
			return models.ToolResult{Content: "quick done"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name:         "blocking",
		ParallelSafe: true,
		Handler: func(hctx context.Context, _ models.ToolCall) (models.ToolResult, error) {
			<-hctx.Done()
			time.Sleep(100 * time.Millisecond)
			return models.ToolResult{Status: models.CallStatusFailed, Error: "interrupted"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Interrupt once the batch is demonstrably in flight, standing in for the
	// user's Ctrl+C (runTurnWithSignal's turnCancel in production).
	go func() {
		<-quickRan
		cancel()
	}()

	if err := r.runTurn(ctx, "fan out", nil, false); err == nil {
		t.Fatal("expected the interrupted turn to return an error")
	}

	var sawBlockingEnd bool
	for _, evt := range ui.events {
		if evt.Type == agent.AgentEventToolCallEnd && evt.ToolEvent != nil && evt.ToolEvent.Name == "blocking" {
			sawBlockingEnd = true
		}
	}
	if !sawBlockingEnd {
		t.Fatalf("the interrupted call's tool_call_end never reached the UI: %d events rendered, %v",
			len(ui.events), renderedEventTypes(ui.events))
	}
}

func renderedEventTypes(events []agent.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, evt := range events {
		name := string(evt.Type)
		if evt.ToolEvent != nil {
			name += ":" + evt.ToolEvent.Name
		}
		out = append(out, name)
	}
	return out
}

// TestRunTurn_InterruptDoesNotRenderCancellationError is the other half of
// draining the queue on interrupt: what must NOT be rendered. Run emits an
// AgentEventError carrying its own ctx.Err() ("context canceled") just before
// returning, and that event is still queued when the interrupt branch drains.
// Rendering it printed a red "Error: context canceled" line directly above
// the "⎿ Interrupted." notice on every single Ctrl+C — the REPL already
// reports the turn outcome once, from turnErr. Before the drain existed the
// event was discarded, so adding the drain is what made this regression
// deterministic rather than a coin flip.
func TestRunTurn_InterruptDoesNotRenderCancellationError(t *testing.T) {
	r, ui := newSessionCarryTestRepl(t, &fanOutProvider{})

	quickRan := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name:         "quick",
		ParallelSafe: true,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			close(quickRan)
			return models.ToolResult{Content: "quick done"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name:         "blocking",
		ParallelSafe: true,
		Handler: func(hctx context.Context, _ models.ToolCall) (models.ToolResult, error) {
			<-hctx.Done()
			return models.ToolResult{Status: models.CallStatusFailed, Error: "interrupted"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		<-quickRan
		cancel()
	}()

	if err := r.runTurn(ctx, "fan out", nil, false); err == nil {
		t.Fatal("expected the interrupted turn to return an error")
	}

	for _, evt := range ui.events {
		if isCancellationErrorEvent(evt) {
			t.Fatalf("the interrupt drain rendered the turn's own cancellation as an error line (%q); "+
				"the REPL reports it once at turn level: %v", evt.Err, renderedEventTypes(ui.events))
		}
	}
	// The drain must still be doing its job — this is not "render nothing".
	var sawToolEnd bool
	for _, evt := range ui.events {
		if evt.Type == agent.AgentEventToolCallEnd {
			sawToolEnd = true
		}
	}
	if !sawToolEnd {
		t.Fatalf("no tool_call_end rendered at all, so the assertion above proves nothing: %v",
			renderedEventTypes(ui.events))
	}
}
