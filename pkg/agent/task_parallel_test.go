package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// barrierTaskPool is a fake subagent pool that observes how many task
// tool calls are "in flight" at once. It satisfies pkg/tools' unexported
// taskPool interface (StartTask + Wait) structurally.
//
// The barrier releases every blocked Wait call when EITHER:
//   - 2 StartTask calls have been observed (the concurrent/GREEN case), or
//   - the caller's ctx is done (the serial/RED case, so the test can never
//     hang — see TestTaskTool_ParallelBatch_OverlapsSubagents for why this
//     matters).
//
// maxInFlight records the high-water mark of concurrently-started tasks
// that haven't yet returned from Wait — the signal the test cares about.
type barrierTaskPool struct {
	mu          sync.Mutex
	started     int
	inFlight    int
	maxInFlight int
	barrierCh   chan struct{}
	closeOnce   sync.Once
}

func newBarrierTaskPool(ctx context.Context) *barrierTaskPool {
	p := &barrierTaskPool{barrierCh: make(chan struct{})}
	go func() {
		<-ctx.Done()
		p.release()
	}()
	return p
}

func (p *barrierTaskPool) release() {
	p.closeOnce.Do(func() { close(p.barrierCh) })
}

func (p *barrierTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	p.mu.Lock()
	p.started++
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	started := p.started
	p.mu.Unlock()

	if started >= 2 {
		p.release()
	}
	return &subagent.Task{ID: fmt.Sprintf("barrier-task-%d", started)}, nil
}

func (p *barrierTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	select {
	case <-p.barrierCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return &subagent.Task{
		ID:     taskID,
		Status: subagent.TaskStatusCompleted,
		Result: "ok:" + taskID,
	}, nil
}

func (p *barrierTaskPool) observedMax() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

// twoTaskCallProvider emits ONE assistant turn with 2 distinct task tool
// calls, then (on the next turn, whenever the loop asks for it) ends the
// run with no tool calls.
type twoTaskCallProvider struct {
	turn int
}

func (p *twoTaskCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *twoTaskCallProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	if p.turn == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{
					{ID: "call-a", Name: "task", Arguments: map[string]any{"description": "sub A", "prompt": "do A"}},
					{ID: "call-b", Name: "task", Arguments: map[string]any{"description": "sub B", "prompt": "do B"}},
				},
				Stop: "tool_calls",
				Done: true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestTaskTool_ParallelBatch_OverlapsSubagents is the concurrency
// integration test for M2-1b. It registers the REAL tools.TaskTool against
// a fake pool whose Wait() blocks on a barrier that only opens once 2
// StartTask calls have been observed.
//
// A single assistant turn issues 2 distinct task calls. If the ReAct loop
// runs them one at a time (today's behavior, because the task tool does
// not declare ParallelSafe), the first call's Wait blocks forever waiting
// for a second StartTask that can never arrive (the loop hasn't gotten to
// the second call yet) — a genuine deadlock. To keep the test from hanging
// the suite while that's true, the barrier ALSO releases when the run's
// ctx hits its deadline (2s here), and Wait returns ctx.Err() in that case.
//
// RED signature (serial path, today): the run consumes the full 2s
// deadline, Agent.Run returns a deadline-exceeded error right after the
// first tool call (the ctx.Err() check between sequential tool calls -
// see react.go), and the second call's StartTask never happens — so
// observedMax() == 1, not 2.
//
// GREEN signature (after ParallelSafe: true on the task tool): both task
// calls run in goroutines, both StartTask calls land before either Wait
// unblocks, the barrier closes almost immediately, and the run finishes
// well under the deadline with observedMax() == 2.
func TestTaskTool_ParallelBatch_OverlapsSubagents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool := newBarrierTaskPool(ctx)

	reg := tools.NewRegistry()
	if err := reg.Register(tools.TaskTool(pool, nil)); err != nil {
		t.Fatalf("register task tool: %v", err)
	}

	a := New(AgentConfig{
		LLMProvider: &twoTaskCallProvider{},
		Tools:       reg,
		MaxTurns:    5,
	})

	start := time.Now()
	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "fan out to two sub-agents"},
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run returned error (RED signature: serial path deadlocked until the %s ctx deadline, elapsed=%s): %v", 2*time.Second, elapsed, err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult")
	}
	if got := pool.observedMax(); got != 2 {
		t.Fatalf("observedMax() = %d, want 2 (both task calls should overlap); elapsed=%s, ctx err=%v", got, elapsed, ctx.Err())
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("run took %s — did not finish before the barrier's ctx-deadline fallback, meaning the calls did not overlap", elapsed)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx expired during the run (RED signature)")
	}

	// Guard against dropped/swapped batch results: the fake pool's
	// distinguishable per-task payload (task ID reflects StartTask arrival
	// order) must show up exactly once, paired with the correct CallID, for
	// BOTH call-a and call-b. A dropped result would leave a call ID
	// missing; a swapped/duplicated result would leave both entries with
	// identical content.
	toolResultsByCall := make(map[string]models.ToolResult)
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResultsByCall[msg.ToolResult.CallID] = *msg.ToolResult
		}
	}
	resA, okA := toolResultsByCall["call-a"]
	if !okA {
		t.Fatalf("missing tool_result for call-a; got call IDs %v", toolResultKeys(toolResultsByCall))
	}
	resB, okB := toolResultsByCall["call-b"]
	if !okB {
		t.Fatalf("missing tool_result for call-b; got call IDs %v", toolResultKeys(toolResultsByCall))
	}
	if resA.Content == "" || resB.Content == "" {
		t.Fatalf("expected non-empty distinguishable content for both calls, got call-a=%q call-b=%q", resA.Content, resB.Content)
	}
	if resA.Content == resB.Content {
		t.Fatalf("call-a and call-b tool results have identical content %q — batch results may have been dropped/swapped", resA.Content)
	}
}

func toolResultKeys(m map[string]models.ToolResult) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
