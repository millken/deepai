package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// TestEstimateContextTokens_UsesAssembledSystemPrompt guards M3-3 item 2:
// estimateContextTokens must count whatever system prompt is passed in (the
// assembled prompt actually sent to the provider), not silently fall back to
// a.systemPrompt (the base prompt before BuildSystemPrompt's memory
// injections, file-op rule, tool recommendations, delegation prompt, and
// plan-mode text are layered on). A unit-level pin, independent of Run(),
// nails down the estimator's contract directly.
func TestEstimateContextTokens_UsesAssembledSystemPrompt(t *testing.T) {
	a := &Agent{systemPrompt: "base", tools: tools.NewRegistry()}
	msgs := []models.Message{{Role: models.RoleHuman, Content: "hi"}}

	withBase := a.estimateContextTokens(msgs, a.systemPrompt)
	assembled := "base" + strings.Repeat("X", 5000) // stands in for injected sections
	withAssembled := a.estimateContextTokens(msgs, assembled)

	if withAssembled <= withBase {
		t.Fatalf("estimateContextTokens did not grow with the assembled system prompt: base=%d assembled=%d",
			withBase, withAssembled)
	}
}

// TestEstimateContextTokens_UsesGivenView guards M3-3 item 3 at the unit
// level: the estimator must score whatever message slice ("view") it is
// given, not some other canonical copy held elsewhere. A view that is far
// smaller than a same-shaped "canonical" slice must produce a far smaller
// estimate.
func TestEstimateContextTokens_UsesGivenView(t *testing.T) {
	a := &Agent{tools: tools.NewRegistry()}
	canonical := []models.Message{
		{Role: models.RoleTool, Content: strings.Repeat("y", 20000), ToolResult: &models.ToolResult{ToolName: "read_file"}},
	}
	view := []models.Message{
		{Role: models.RoleTool, Content: "aged: truncated", ToolResult: &models.ToolResult{ToolName: "read_file"}},
	}

	canonicalEstimate := a.estimateContextTokens(canonical, "sys")
	viewEstimate := a.estimateContextTokens(view, "sys")

	if viewEstimate >= canonicalEstimate {
		t.Fatalf("estimate over the aged view (%d) was not smaller than over canonical (%d)", viewEstimate, canonicalEstimate)
	}
}

// contextMeteringProvider is a minimal Stream provider that ends the run
// immediately with a plain AI reply and no tool calls, so Run() completes
// after exactly one turn — enough to observe whether the pre-request
// compaction check fired (via the emitted AgentEventCompact).
type contextMeteringProvider struct{}

func (contextMeteringProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (contextMeteringProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

// runAndCollectEvents runs the agent to completion and drains every event
// emitted during the run. Draining concurrently (rather than after Run
// returns) matters for longer runs: the events channel has a fixed buffer
// (128) and emit() drops on overflow rather than blocking, so a many-turn
// run whose events aren't consumed as they're produced can silently lose
// some — including the AgentEventCompact events these tests count.
func runAndCollectEvents(t *testing.T, a *Agent, sessionID string, msgs []models.Message) (*RunResult, []AgentEvent) {
	t.Helper()
	var events []AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range a.Events() {
			events = append(events, evt)
		}
	}()
	result, err := a.Run(context.Background(), sessionID, msgs)
	<-done
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result, events
}

func hasCompactEvent(events []AgentEvent) bool {
	for _, e := range events {
		if e.Type == AgentEventCompact {
			return true
		}
	}
	return false
}

// smallConversation builds a message history long enough for compactMessages
// to actually reduce something (n > keepTail+2, with at least one RoleTool
// message in the middle region so compaction always changes format
// regardless of content size).
func smallConversation() []models.Message {
	return []models.Message{
		{Role: models.RoleHuman, Content: "start"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: models.RoleTool, Content: "small result", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "read_file"}},
		{Role: models.RoleAI, Content: "ok"},
		{Role: models.RoleAI, Content: "continuing"},
		{Role: models.RoleHuman, Content: "more"},
		{Role: models.RoleAI, Content: "..."},
		{Role: models.RoleTool, Content: "another", ToolResult: &models.ToolResult{CallID: "c2", ToolName: "grep"}},
		{Role: models.RoleAI, Content: "step"},
		{Role: models.RoleHuman, Content: "final question"},
		{Role: models.RoleAI, Content: "thinking"},
		{Role: models.RoleAI, Content: "final"},
	}
}

// TestCompactionTrigger_CountsAssembledSystemPrompt guards M3-3 item 2 at the
// Run() level: the delegation prompt (Team Delegation strategy + agent
// catalog) is assembled by BuildSystemPrompt on every request but is NOT part
// of a.systemPrompt — the base prompt configured at construction. Before the
// fix, the compaction trigger measured only the base prompt and so never saw
// this ~1.7KB addition, letting a session that is actually over the
// threshold slip past compaction. ContextWindow is picked small enough that
// the base-prompt-only estimate sits well under the 0.75 threshold, while the
// assembled (base + delegation prompt) estimate sits well over it.
func TestCompactionTrigger_CountsAssembledSystemPrompt(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})

	a := New(AgentConfig{
		LLMProvider:   contextMeteringProvider{},
		Tools:         reg,
		Model:         "m",
		SystemPrompt:  "assistant",
		ContextWindow: 700,
		AgentCatalog:  []AgentInfo{{Type: "coder", Description: "writes code"}},
	})

	_, events := runAndCollectEvents(t, a, "sess-metering-1", smallConversation())

	if !hasCompactEvent(events) {
		t.Fatal("compaction did not fire: the trigger under-counted the assembled system prompt " +
			"(delegation prompt + catalog), which BuildSystemPrompt adds on every request but " +
			"a.systemPrompt never contains")
	}
}

// TestCompactionTrigger_NoDelegationPrompt_StaysUnderThreshold is the control
// for the test above: with no AgentCatalog/task tool (so the assembled prompt
// equals the base prompt) and the same small conversation, the estimate must
// stay under threshold and compaction must NOT fire. This confirms the fix
// isn't simply "always compact" — it isolates the assembled-prompt bytes as
// the actual cause.
func TestCompactionTrigger_NoDelegationPrompt_StaysUnderThreshold(t *testing.T) {
	a := New(AgentConfig{
		LLMProvider:   contextMeteringProvider{},
		Tools:         tools.NewRegistry(),
		Model:         "m",
		SystemPrompt:  "assistant",
		ContextWindow: 700,
	})

	_, events := runAndCollectEvents(t, a, "sess-metering-2", smallConversation())

	if hasCompactEvent(events) {
		t.Fatal("compaction fired without the assembled-prompt addition; test setup no longer isolates the cause")
	}
}

// TestCompactionTrigger_ViewVsCanonical_AgingPreventsUnnecessaryCompaction
// guards M3-3 item 3 at the Run() level: with Aging enabled, the request
// actually sent to the provider is the aged VIEW, which can be far smaller
// than the canonical history once several turns have aged out. Before the
// fix, the compaction trigger measured canonical runMessages directly, so a
// canonical history that tripped the 0.75 threshold would compact (mutating
// and permanently destroying history) even though the real outgoing payload
// (the aged view) was comfortably under it. After the fix, the trigger
// measures the view, so no compaction should fire and the canonical messages
// must survive untouched.
func TestCompactionTrigger_ViewVsCanonical_AgingPreventsUnnecessaryCompaction(t *testing.T) {
	big := strings.Repeat("z", 8000)
	var history []models.Message
	// 5 aged read_file turns: with the §5.4 read_file budget (age>=3 -> 300B,
	// age 2 -> 2048B, age 1 -> 8192B) most of these get compressed hard once
	// later turns push their age up.
	for i := 0; i < 5; i++ {
		history = append(history, aiTools(""), toolMsg("read_file", big))
	}
	// Current turn: untouched by aging (age 0).
	history = append(history, aiTools(""), toolMsg("read_file", "current turn result"))

	a := New(AgentConfig{
		LLMProvider:   contextMeteringProvider{},
		Tools:         tools.NewRegistry(),
		Model:         "m",
		ContextWindow: 10000,
		Aging: &AgingConfig{
			Enabled:             true,
			MinContextPressure:  0, // gate off so aging is deterministic here
			ConversationBudgets: map[int]int{},
		},
	})

	result, events := runAndCollectEvents(t, a, "sess-metering-3", history)

	if hasCompactEvent(events) {
		t.Fatal("compaction fired even though the aged view sent to the provider was under threshold: " +
			"the trigger is still measuring canonical messages instead of the view")
	}
	// Canonical history must be untouched: the first read_file result must
	// still be the full 8000-byte string, not compactMessages' "[tool
	// result: ...]" summary.
	if len(result.Messages) == 0 || result.Messages[1].Content != big {
		t.Fatalf("canonical history was mutated by an unnecessary compaction")
	}
}

// countingExtractor implements memory.Extractor and just counts how many
// times ExtractUpdate is called, so tests can assert the synchronous
// pre-compaction memory flush runs only when compaction actually happens
// (not on every turn a trigger merely evaluates).
type countingExtractor struct {
	calls atomic.Int32
}

func (e *countingExtractor) ExtractUpdate(ctx context.Context, current memory.Document, messages []models.Message) (memory.Update, error) {
	e.calls.Add(1)
	return memory.Update{}, nil
}

// loopingBigToolProvider calls "big_tool" (with a unique call ID/arg each
// time, so the repeat-call circuit-breaker never engages) for totalCalls
// turns, then ends the run with a plain AI reply. Used to simulate a
// multi-turn session that keeps growing so the compaction trigger's ratio
// check keeps re-evaluating every turn.
type loopingBigToolProvider struct {
	totalCalls int32
	made       atomic.Int32
}

func (p *loopingBigToolProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *loopingBigToolProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	n := p.made.Add(1)
	go func() {
		defer close(ch)
		if n > p.totalCalls {
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
			return
		}
		call := models.ToolCall{
			ID:        fmt.Sprintf("call-%d", n),
			Name:      "big_tool",
			Arguments: map[string]any{"n": n},
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{call}}, Done: true}
	}()
	return ch, nil
}

// countCompactEvents counts how many AgentEventCompact events were emitted.
func countCompactEvents(events []AgentEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == AgentEventCompact {
			n++
		}
	}
	return n
}

// TestCompactionTrigger_StalledCompactionFiresOnce guards a code-review
// finding on M3-3: when the "floor" (assembled system prompt + tool schemas +
// the keepTail messages that are NEVER compacted, since they're the tail)
// already exceeds the compaction threshold by itself, no amount of
// compacting the *head* region can ever bring the ratio back under
// threshold. Before the fix, the trigger re-evaluated and re-compacted
// (including the synchronous, potentially 30s-timeout memory flush) on
// EVERY subsequent turn even though it can never help — a thrash bug, not
// just wasted work. After the fix, once one compaction attempt fails to
// drop the ratio under threshold, the agent stops retrying until enough new
// messages have accumulated to make the current tail compactable again (see
// TestCompactionTrigger_UnstallsAsHistoryGrows) — so a session stuck at or
// above a floor this hopeless (window=100 vs. 5000-byte tail messages, i.e.
// the tail alone is ~50x the window) still compacts roughly once every
// compactionKeepTail turns instead of every single turn: well below "every
// turn" (that's the thrash regression this test guards), but not "exactly
// once forever" either — the guard is a rate limit tied to how fast new
// messages accumulate, not a permanent latch.
func TestCompactionTrigger_StalledCompactionFiresOnce(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "big_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: strings.Repeat("z", 5000)}, nil
		},
	})

	store, err := memory.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	extractor := &countingExtractor{}
	memSvc := memory.NewService(slog.Default(), store, nil)

	const totalCalls = 10
	provider := &loopingBigToolProvider{totalCalls: totalCalls}

	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       reg,
		Model:       "m",
		// Small enough that even the keepTail-only "floor" (a handful of
		// 5000-byte tool results that can never be compacted away, since
		// they're always the tail) sits far above 0.75 of the window.
		ContextWindow:   100,
		MemoryService:   memSvc,
		MemoryExtractor: extractor,
	})

	_, events := runAndCollectEvents(t, a, "sess-metering-stall", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	})

	// A hopeless floor still gets re-attempted roughly once every
	// compactionKeepTail turns (the unstall condition), so exactly-once is
	// no longer the right bound — but it must stay well below "every turn"
	// (totalCalls), which is the thrash this test exists to catch.
	compactions := countCompactEvents(events)
	if compactions >= totalCalls/2 {
		t.Fatalf("compaction fired %d times across a %d-turn session stuck at its floor; want well below "+
			"totalCalls/2 (%d) — every-turn retrying is the thrash regression this test guards",
			compactions, totalCalls, totalCalls/2)
	}
	if compactions == 0 {
		t.Fatal("compaction never fired at all; test setup no longer trips the threshold")
	}
	// The memory flush runs exactly once per actual compaction (moved inside
	// didCompact), never once per turn the trigger merely evaluates.
	if got := extractor.calls.Load(); got != int32(compactions) {
		t.Fatalf("memory flush ran %d times, want exactly %d (one per actual compaction, matching the "+
			"%d AgentEventCompact events)", got, compactions, compactions)
	}
}

// TestCompactionTrigger_UnstallsAsHistoryGrows guards a second code-review
// finding on M3-3: the stall guard's SET condition (afterRatio still >=
// compactionThreshold after a compaction) is normal, not a sign of a
// permanent floor — the escalating retry loop inside the compaction branch
// only tries to get the ratio under 1.0 (afterEstimated <= a.contextWindow),
// so landing in the 0.75-1.0 band after a real, productive compaction is
// routine. A sticky-forever stall (no way to reconsider) therefore
// permanently disables proactive compaction after the FIRST productive-but-
// not-quite-enough compaction, even while the session keeps growing turn
// after turn — leaving only the reactive compactOnOverflow backstop to save
// it. Across a long run with a moderate (not hopeless-floor) window, the
// stall must clear once enough new material has accumulated to make the
// previously-protected tail compactable again, so compaction fires
// repeatedly (not once) and the final session size stays within a small
// multiple of the context window instead of drifting arbitrarily high.
func TestCompactionTrigger_UnstallsAsHistoryGrows(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "big_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: strings.Repeat("z", 4000)}, nil
		},
	})

	const totalCalls = 40
	provider := &loopingBigToolProvider{totalCalls: totalCalls}

	a := New(AgentConfig{
		LLMProvider:   provider,
		Tools:         reg,
		Model:         "m",
		ContextWindow: 4000,
	})

	result, events := runAndCollectEvents(t, a, "sess-unstall", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	})

	// With a sticky-forever stall (pre-fix), this scenario measured
	// compactions=2, finalEstimate=47024 (11.8x the window) — the first
	// compaction lands in the 0.75-1.0 band, stalls forever, and the session
	// drifts unboundedly for the remaining ~38 turns. With the unstall fix,
	// it measured compactions=13, finalEstimate=9327 (2.3x the window).
	const minCompactions = 5 // comfortably above the pre-fix value (2), well below the fixed value (13)
	if compactions := countCompactEvents(events); compactions < minCompactions {
		t.Fatalf("compaction fired only %d time(s) across a %d-turn growing session (want >= %d); a "+
			"sticky-forever stall guard never reconsiders even though the session keeps growing well "+
			"past its window", compactions, totalCalls, minCompactions)
	}

	finalSystemPrompt := a.BuildSystemPrompt(context.Background(), "sess-unstall", result.Messages)
	finalView := buildPromptView(result.Messages, a.aging, a.contextWindow)
	finalEstimate := a.estimateContextTokens(finalView, finalSystemPrompt)
	if maxAllowed := a.contextWindow * 3; finalEstimate > maxAllowed {
		t.Fatalf("final estimate %d is more than 3x the context window (%d): the session drifted "+
			"unboundedly because the stall guard never recovered as history grew", finalEstimate, maxAllowed)
	}
}
