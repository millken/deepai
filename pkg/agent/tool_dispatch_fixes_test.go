package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// batchProvider emits ONE assistant turn carrying the given tool calls, then
// ends the run on the next turn.
type batchProvider struct {
	calls []models.ToolCall
	turn  int
}

func (p *batchProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *batchProvider) Stream(context.Context, llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{ToolCalls: p.calls, Stop: "tool_calls", Done: true}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Delta: "done", Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestParallelBatch_OverCapRefusalIsNotQueuedBehindAdmittedCalls pins a call
// refused by the fan-out cap to the one property that makes it a refusal:
// it is instant. The refusal does no work at all — synthesizeTaskCapResult
// builds it without dispatching anything — but the check moved inside the
// goroutine that first blocks on the concurrency semaphore, so the refusal
// queued behind every admitted call in the segment. With a long-running
// subagent holding a slot, a call that needed no slot at all rendered as
// "⚙ task…" for as long as the subagent ran, and its tool_call_end arrived
// minutes late.
//
// Concurrency is pinned to 1 and every admitted call blocks, so the two
// behaviours are mechanically distinguishable: the refusal either reports
// while the admitted calls are still parked, or it cannot report at all.
func TestParallelBatch_OverCapRefusalIsNotQueuedBehindAdmittedCalls(t *testing.T) {
	t.Setenv("DEEPAI_MAX_TOOL_CONCURRENCY", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	release := make(chan struct{})
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "task",
		ParallelSafe: true,
		Handler: func(hctx context.Context, _ models.ToolCall) (models.ToolResult, error) {
			select {
			case <-release:
				return models.ToolResult{Content: "task done"}, nil
			case <-hctx.Done():
				return models.ToolResult{}, hctx.Err()
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	// One call past maxTaskCallsPerRun: the last one is refused, the rest are
	// admitted and will all block. Distinct arguments keep the circuit
	// breaker's repeat-call detection out of the picture.
	calls := make([]models.ToolCall, 0, maxTaskCallsPerRun+1)
	for i := 0; i <= maxTaskCallsPerRun; i++ {
		calls = append(calls, models.ToolCall{
			ID:        fmt.Sprintf("call-%d", i),
			Name:      "task",
			Arguments: map[string]any{"prompt": fmt.Sprintf("job %d", i)},
		})
	}
	overCapID := calls[len(calls)-1].ID

	a := New(AgentConfig{
		LLMProvider:  &batchProvider{calls: calls},
		Tools:        reg,
		MaxToolCalls: 2 * (maxTaskCallsPerRun + 1),
	})

	refused := make(chan struct{})
	go func() {
		for evt := range a.Events() {
			if evt.Type == AgentEventToolCallEnd && evt.ToolCall != nil && evt.ToolCall.ID == overCapID {
				close(refused)
				break
			}
		}
		for range a.Events() {
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, "s1", []models.Message{
			{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "fan out"},
		})
		done <- err
	}()

	select {
	case <-refused:
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatalf("RED: the over-cap call's tool_call_end never arrived while the %d admitted calls were still blocked — "+
			"the refusal is waiting on the concurrency semaphore it never needed", maxTaskCallsPerRun)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestToolCallEnd_CarriesOffloadedContent pins the offload reference to the
// ONE event the TUI renders per finished call. tool_call_end used to be built
// from the pre-offload result: handleResult rewrote Content to the
// "[offloaded: … saved to <path>]" header afterwards, on its own copy, and
// emitted that as AgentEventToolResult — for which the TUI has no case at
// all. So a bash call returning megabytes showed a truncated head of raw
// output and never told the user where the full output had been saved.
func TestToolCallEnd_CarriesOffloadedContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	big := strings.Repeat("x\n", offloadThresholdBytes)
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "dump",
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: big}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	offloadDir := t.TempDir()
	a := New(AgentConfig{
		LLMProvider: &batchProvider{calls: []models.ToolCall{
			{ID: "call-dump", Name: "dump", Arguments: map[string]any{}},
		}},
		Tools:        reg,
		MaxToolCalls: 5,
		OffloadDir:   offloadDir,
	})

	var mu sync.Mutex
	var endContent string
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for evt := range a.Events() {
			if evt.Type == AgentEventToolCallEnd && evt.Result != nil && evt.Result.CallID == "call-dump" {
				mu.Lock()
				endContent = evt.Result.Content
				mu.Unlock()
			}
		}
	}()

	if _, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "dump it"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-drained

	mu.Lock()
	got := endContent
	mu.Unlock()

	if got == "" {
		t.Fatal("no tool_call_end for the offloaded call reached the UI")
	}
	if !strings.HasPrefix(got, "[offloaded: full output (") {
		t.Fatalf("tool_call_end carried the raw payload, not the offload reference: %.80q", got)
	}
	if !strings.Contains(got, offloadDir) {
		t.Fatalf("offload reference does not name the saved file: %.200q", got)
	}
}

// TestOffloadIfNeeded_IsIdempotent covers the seam the fix above rests on:
// the dispatch path offloads so the UI event carries the reference, and
// handleResult then runs offloadIfNeeded over the same result during its
// bookkeeping pass. The second call must neither rewrite the content again
// (an offload reference offloaded a second time) nor report "not offloaded",
// which is what the metrics record as Offloaded.
func TestOffloadIfNeeded_IsIdempotent(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &batchProvider{}, Tools: tools.NewRegistry()})
	dir := t.TempDir()

	result := models.ToolResult{
		CallID:   "call-1",
		ToolName: "dump",
		Content:  strings.Repeat("y\n", offloadThresholdBytes),
	}
	if !a.offloadIfNeeded(&result, dir) {
		t.Fatal("first offload did not happen")
	}
	first := result.Content

	if !a.offloadIfNeeded(&result, dir) {
		t.Fatal("second call reported the result as not offloaded, so metrics would under-count offloads")
	}
	if result.Content != first {
		t.Fatalf("second call rewrote the content:\n first: %.80q\nsecond: %.80q", first, result.Content)
	}
}
