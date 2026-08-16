package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// A batch used to run fully serially the moment ONE call was not
// ParallelSafe, so a model that emitted "4 review tasks + one bash to check
// the test entry point" in a single turn got four strictly sequential
// subagents. Claude Code partitions the same way this now does: consecutive
// concurrency-safe calls form one concurrent batch, each unsafe call stands
// alone, and relative order is preserved (see its partitionToolCalls).

// probeTracker records concurrency across the fake tools below.
type probeTracker struct {
	mu             sync.Mutex
	inFlight       int
	maxInFlight    int
	mutateSawProbe int // probes in flight when the unsafe tool ran
	mutateRan      bool
	probeStarted   bool // any probe started before the unsafe tool ran?
	gate           chan struct{}
	once           sync.Once
	releaseAt      int
}

func newProbeTracker(ctx context.Context, releaseAt int) *probeTracker {
	p := &probeTracker{gate: make(chan struct{}), releaseAt: releaseAt}
	go func() {
		<-ctx.Done()
		p.release()
	}()
	return p
}

func (p *probeTracker) release() { p.once.Do(func() { close(p.gate) }) }

// safeTool blocks until `releaseAt` of them are in flight at once (or ctx
// dies), so overlap is proven without sleeping.
func (p *probeTracker) safeTool() models.Tool {
	return models.Tool{
		Name:         "probe",
		Description:  "concurrency probe",
		ParallelSafe: true,
		InputSchema:  map[string]any{"type": "object"},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			p.mu.Lock()
			p.inFlight++
			p.probeStarted = true
			if p.inFlight > p.maxInFlight {
				p.maxInFlight = p.inFlight
			}
			reached := p.inFlight >= p.releaseAt
			p.mu.Unlock()

			if reached {
				p.release()
			}
			select {
			case <-p.gate:
			case <-ctx.Done():
			}

			p.mu.Lock()
			p.inFlight--
			p.mu.Unlock()
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "probe:" + call.ID}, nil
		},
	}
}

// unsafeTool stands in for bash/edit_file: never concurrency-safe.
func (p *probeTracker) unsafeTool() models.Tool {
	return models.Tool{
		Name:        "mutate",
		Description: "not concurrency safe",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			p.mu.Lock()
			p.mutateRan = true
			p.mutateSawProbe = p.inFlight
			p.mu.Unlock()
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "mutate"}, nil
		},
	}
}

// scriptedBatchProvider emits one assistant turn carrying the given calls,
// then ends the run.
type scriptedBatchProvider struct {
	calls []models.ToolCall
	turn  int
}

func (p *scriptedBatchProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *scriptedBatchProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
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
		ch <- llm.StreamChunk{Done: true, Stop: "stop"}
	}()
	return ch, nil
}

func probeAgent(t *testing.T, p *probeTracker, calls []models.ToolCall) *Agent {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(p.safeTool()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(p.unsafeTool()); err != nil {
		t.Fatal(err)
	}
	return New(AgentConfig{
		LLMProvider:  &scriptedBatchProvider{calls: calls},
		Tools:        reg,
		MaxToolCalls: 20,
	})
}

func TestToolBatch_SafeCallsRunConcurrentlyDespiteUnsafeCallInBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p := newProbeTracker(ctx, 2)
	calls := []models.ToolCall{
		{ID: "p1", Name: "probe", Arguments: map[string]any{}},
		{ID: "p2", Name: "probe", Arguments: map[string]any{}},
		{ID: "m1", Name: "mutate", Arguments: map[string]any{}},
	}
	a := probeAgent(t, p, calls)

	start := time.Now()
	if _, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run error after %s: %v", time.Since(start), err)
	}

	p.mu.Lock()
	maxInFlight, mutateSaw, mutateRan := p.maxInFlight, p.mutateSawProbe, p.mutateRan
	p.mu.Unlock()

	if maxInFlight != 2 {
		t.Fatalf("maxInFlight = %d, want 2 — one unsafe call in the batch must not serialize the safe ones", maxInFlight)
	}
	if !mutateRan {
		t.Fatal("the unsafe call never ran")
	}
	if mutateSaw != 0 {
		t.Fatalf("unsafe call saw %d probes in flight, want 0 — it must not overlap the concurrent segment", mutateSaw)
	}
}

func TestToolBatch_UnsafeCallBeforeSafeRunPreservesOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p := newProbeTracker(ctx, 2)
	// Unsafe call FIRST: it must complete before the probes start, or the
	// partition has reordered the batch.
	calls := []models.ToolCall{
		{ID: "m1", Name: "mutate", Arguments: map[string]any{}},
		{ID: "p1", Name: "probe", Arguments: map[string]any{}},
		{ID: "p2", Name: "probe", Arguments: map[string]any{}},
	}
	a := probeAgent(t, p, calls)

	if _, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	p.mu.Lock()
	maxInFlight, mutateSaw := p.maxInFlight, p.mutateSawProbe
	p.mu.Unlock()

	if mutateSaw != 0 {
		t.Fatalf("unsafe call ran with %d probes in flight — order not preserved", mutateSaw)
	}
	if maxInFlight != 2 {
		t.Fatalf("maxInFlight = %d, want 2 — the trailing safe run must still be concurrent", maxInFlight)
	}
}

func TestToolBatch_ConcurrencyCapBoundsFanOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// releaseAt is unreachable (6 > cap), so probes rely on the short hold
	// below; the assertion is an upper bound, which no timing can falsify.
	p := newProbeTracker(ctx, 6)
	var calls []models.ToolCall
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		calls = append(calls, models.ToolCall{ID: id, Name: "probe", Arguments: map[string]any{}})
	}
	a := probeAgent(t, p, calls)
	a.maxToolConcurrency = 2

	// Release the gate shortly after the run starts so probes finish.
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.release()
	}()

	res, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	p.mu.Lock()
	maxInFlight := p.maxInFlight
	p.mu.Unlock()
	if maxInFlight > 2 {
		t.Fatalf("maxInFlight = %d, want <= 2 — the concurrency cap was not applied", maxInFlight)
	}

	// Every call still has to produce a result: the cap bounds parallelism,
	// it must never drop work.
	var toolResults int
	for _, msg := range res.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResults++
		}
	}
	if toolResults != len(calls) {
		t.Fatalf("got %d tool results, want %d", toolResults, len(calls))
	}
}

func TestPartitionToolCalls_GroupsConsecutiveSafeRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newProbeTracker(ctx, 99)
	a := probeAgent(t, p, nil)

	calls := []models.ToolCall{
		{ID: "1", Name: "probe"},
		{ID: "2", Name: "probe"},
		{ID: "3", Name: "mutate"},
		{ID: "4", Name: "probe"},
		{ID: "5", Name: "mutate"},
		{ID: "6", Name: "mutate"},
	}
	segs := a.partitionToolCalls(calls)

	want := []toolSegment{
		{start: 0, end: 2, parallel: true},
		{start: 2, end: 3, parallel: false},
		{start: 3, end: 4, parallel: true},
		{start: 4, end: 5, parallel: false},
		{start: 5, end: 6, parallel: false},
	}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments %+v, want %d %+v", len(segs), segs, len(want), want)
	}
	for i := range want {
		if segs[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v (full: %+v)", i, segs[i], want[i], segs)
		}
	}
}

func TestPartitionToolCalls_UnknownToolIsNotConcurrencySafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newProbeTracker(ctx, 99)
	a := probeAgent(t, p, nil)

	// Fail closed: a name the registry does not know must never be assumed
	// safe to run alongside anything else.
	segs := a.partitionToolCalls([]models.ToolCall{
		{ID: "1", Name: "probe"},
		{ID: "2", Name: "nonexistent"},
	})
	if len(segs) != 2 {
		t.Fatalf("got %+v, want the unknown tool split into its own segment", segs)
	}
	if segs[1].parallel {
		t.Fatalf("unknown tool marked parallel-safe: %+v", segs[1])
	}
}
