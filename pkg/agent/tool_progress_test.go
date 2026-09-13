package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// fastSlowBatchProvider emits ONE assistant turn with two parallel-safe tool
// calls — one that returns at once, one that blocks — then ends the run on
// the next turn.
type fastSlowBatchProvider struct{ turn int }

func (p *fastSlowBatchProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *fastSlowBatchProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-fast", Name: "quick", Arguments: map[string]any{}},
					{ID: "call-slow", Name: "blocking", Arguments: map[string]any{}},
				},
				Stop: "tool_calls",
				Done: true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Delta: "done", Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestParallelBatch_ReportsEachCallAsItLands pins the fix for a fan-out that
// looked frozen: the parallel path's per-result bookkeeping loop only runs
// after the whole segment has drained (wg.Wait), so emitting
// AgentEventToolCallEnd from there meant a call that finished in seconds was
// still rendered as "running" until its slowest sibling returned. In the
// incident that motivated this, a 2s MCP call and a 12-minute subagent shared
// a batch with an 18-minute one: for 18 minutes the screen showed three calls
// in flight, two of which were long done.
//
// The blocking tool here waits for the FAST call's completion event to be
// observed, so the two orderings are mechanically distinguishable:
//
//	GREEN: the event is emitted as soon as the fast call lands, the blocking
//	       tool is released, and the run finishes well under the deadline.
//	RED:   the event cannot be emitted until the segment drains, which cannot
//	       happen until the blocking tool returns — a deadlock. The ctx
//	       deadline breaks it so the suite fails instead of hanging.
func TestParallelBatch_ReportsEachCallAsItLands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	fastEnded := make(chan struct{})

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "quick",
		ParallelSafe: true,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: "quick done"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(models.Tool{
		Name:         "blocking",
		ParallelSafe: true,
		Handler: func(hctx context.Context, _ models.ToolCall) (models.ToolResult, error) {
			select {
			case <-fastEnded:
				return models.ToolResult{Content: "blocking done"}, nil
			case <-hctx.Done():
				return models.ToolResult{}, hctx.Err()
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &fastSlowBatchProvider{},
		Tools:        reg,
		MaxToolCalls: 5,
	})

	go func() {
		for evt := range a.Events() {
			if evt.Type != AgentEventToolCallEnd || evt.ToolEvent == nil {
				continue
			}
			if evt.ToolEvent.Name == "quick" {
				select {
				case <-fastEnded:
				default:
					close(fastEnded)
				}
			}
		}
	}()

	start := time.Now()
	_, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run both"},
	})
	elapsed := time.Since(start)

	// Order matters: on the deadlock path Run returns a non-nil error too, so
	// the generic check below would fire first and this diagnosis — the
	// actual reason the test failed — would never print.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("RED: the fast call's tool_call_end never arrived while its sibling was still running, "+
			"so the blocking tool was only released by the %s deadline (Run returned: %v)", 3*time.Second, err)
	}
	if err != nil {
		t.Fatalf("Run: %v (elapsed=%s)", err, elapsed)
	}
}
