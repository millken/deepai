package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// --- M4-2: system-prompt prefix stabilization -------------------------------
//
// These tests guard the design in task-22-brief.md: the system prompt must
// become session-stable (no date, no memory injections baked into position
// 0), and the volatile content (date + memory) must instead ride a per-Run,
// once-computed TRAILING injection message appended as the final message of
// every request view.

// flakyMemoryService is a minimal *memory.Service wrapper helper: it saves a
// document under a key and lets the test mutate it between two Inject calls,
// so TestSystemPromptStable_AcrossMemoryChanges can prove BuildSystemPrompt no
// longer reflects those changes at all.
func newMemoryServiceWithFact(t *testing.T, sessionID, factContent string) *memory.Service {
	t.Helper()
	store, err := memory.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc := memory.NewService(slog.Default(), store, nil)
	t.Cleanup(func() { svc.Close(context.Background()) })
	now := time.Now().UTC()
	doc := memory.Document{
		SessionID: sessionID,
		Facts: []memory.Fact{{
			ID:         "f1",
			Content:    factContent,
			Confidence: 0.9,
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		UpdatedAt: now,
	}
	if err := svc.Save(context.Background(), doc); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	return svc
}

// TestSystemPromptStable_AcrossMemoryChanges is the RED test for design item
// 1: today, BuildSystemPrompt bakes the memory injection directly into the
// returned prompt, so two calls where the underlying memory document changed
// in between produce DIFFERENT prompt bytes — poisoning every OpenAI-compat
// provider's automatic prefix cache on every single turn. After the fix, the
// prompt must be byte-identical across both calls, and must contain NEITHER
// fact's content (memory now lives only in the trailing turn injection).
func TestSystemPromptStable_AcrossMemoryChanges(t *testing.T) {
	ctx := context.Background()
	sessionID := "sess-stable"
	svc := newMemoryServiceWithFact(t, sessionID, "FIRST_FACT_MARKER content")

	a := New(AgentConfig{
		SystemPrompt:  "base",
		MemoryService: svc,
	})

	first := a.BuildSystemPrompt()

	// Mutate the underlying memory document between the two calls.
	now := time.Now().UTC()
	if err := svc.Save(ctx, memory.Document{
		SessionID: sessionID,
		Facts: []memory.Fact{{
			ID:         "f2",
			Content:    "SECOND_FACT_MARKER totally different content",
			Confidence: 0.9,
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save memory (second): %v", err)
	}

	second := a.BuildSystemPrompt()

	if first != second {
		t.Fatalf("BuildSystemPrompt not stable across memory changes:\nfirst:\n%s\n\nsecond:\n%s", first, second)
	}
	if strings.Contains(first, "FIRST_FACT_MARKER") || strings.Contains(second, "SECOND_FACT_MARKER") {
		t.Fatalf("memory content leaked into system prompt:\nfirst:\n%s\n\nsecond:\n%s", first, second)
	}
}

// multiTurnCaptureProvider records every Stream request's Messages (not just
// the first), and drives a fixed number of tool-call turns before ending the
// run with a plain AI reply — enough to observe the injection across
// multiple requests within one Run.
type multiTurnCaptureProvider struct {
	mu        sync.Mutex
	requests  [][]models.Message
	toolTurns int
	turnsDone int
	// toolName is the tool called on each of the first toolTurns turns;
	// defaults to "noop_tool" when empty.
	toolName string
}

func (p *multiTurnCaptureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *multiTurnCaptureProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	snapshot := make([]models.Message, len(req.Messages))
	copy(snapshot, req.Messages)
	p.requests = append(p.requests, snapshot)
	turn := p.turnsDone
	p.turnsDone++
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if turn < p.toolTurns {
			name := p.toolName
			if name == "" {
				name = "noop_tool"
			}
			call := models.ToolCall{ID: fmt.Sprintf("call-%d", turn), Name: name, Arguments: map[string]any{"n": turn}}
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{call}}, Done: true}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *multiTurnCaptureProvider) seenRequests() [][]models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]models.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

const dateNoteMarker = "[System note: Today's date is "

// TestTurnInjection_PresenceAndPosition is the RED test for design items 1-2:
// the date/memory content must NOT be in the system prompt, but MUST appear
// as the trailing (last) message of every single request view sent to the
// provider across a multi-turn tool-call Run, and must NOT be persisted into
// RunResult.Messages.
func TestTurnInjection_PresenceAndPosition(t *testing.T) {
	sessionID := "sess-injection-pos"
	svc := newMemoryServiceWithFact(t, sessionID, "MEMFACT_PRESENCE marker text")

	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "noop_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: "ok"}, nil
		},
	})

	p := &multiTurnCaptureProvider{toolTurns: 2}
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         reg,
		Model:         "m",
		MemoryService: svc,
	})

	result, err := a.Run(context.Background(), sessionID, []models.Message{
		{Role: models.RoleHuman, Content: "please use the tool"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) != 3 {
		t.Fatalf("expected 3 requests (2 tool turns + final), got %d", len(requests))
	}
	for i, view := range requests {
		if len(view) == 0 {
			t.Fatalf("request %d: empty view", i)
		}
		last := view[len(view)-1]
		if last.Role != models.RoleHuman {
			t.Fatalf("request %d: last message role = %s, want RoleHuman (the injection)", i, last.Role)
		}
		if !strings.Contains(last.Content, dateNoteMarker) {
			t.Fatalf("request %d: last message missing date note, got: %q", i, last.Content)
		}
		if !strings.Contains(last.Content, "MEMFACT_PRESENCE") {
			t.Fatalf("request %d: last message missing memory content, got: %q", i, last.Content)
		}
	}

	// Persisted history must never contain the injection.
	for i, m := range result.Messages {
		if strings.Contains(m.Content, dateNoteMarker) {
			t.Fatalf("persisted RunResult.Messages[%d] contains the turn injection: %q", i, m.Content)
		}
	}
}

// countingStorage wraps a real memory.Storage and counts Load calls per
// session key, so tests can assert the once-per-Run computation invariant:
// memory extraction/injection must be looked up exactly once per scope per
// Run, no matter how many requests that Run makes.
type countingStorage struct {
	memory.Storage
	mu    sync.Mutex
	loads map[string]int
}

func newCountingStorage(inner memory.Storage) *countingStorage {
	return &countingStorage{Storage: inner, loads: make(map[string]int)}
}

func (c *countingStorage) Load(ctx context.Context, sessionID string) (memory.Document, error) {
	c.mu.Lock()
	c.loads[sessionID]++
	c.mu.Unlock()
	return c.Storage.Load(ctx, sessionID)
}

func (c *countingStorage) loadCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads[key]
}

// TestTurnInjection_OncePerRun is the RED test for design item 3: memory
// extraction/injection is a relevance heuristic recomputed on a 5-turn
// cadence elsewhere, so recomputing it on EVERY request within a single Run
// buys nothing and destroys prefix stability. The turn injection must be
// computed exactly once per Run, so the underlying storage.Load call count
// for each configured scope (session + user) must be exactly 1 across a
// multi-request Run, not len(requests).
func TestTurnInjection_OncePerRun(t *testing.T) {
	sessionID := "sess-once-per-run"
	uid := "user-once-per-run"

	fileStore, err := memory.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	counting := newCountingStorage(fileStore)
	svc := memory.NewService(slog.Default(), counting, nil)
	defer svc.Close(context.Background())

	now := time.Now().UTC()
	if err := svc.Save(context.Background(), memory.Document{
		SessionID: memory.UserScope(uid).Key(),
		Facts:     []memory.Fact{{ID: "u1", Content: "user scope fact", Confidence: 0.9, CreatedAt: now, UpdatedAt: now}},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save user scope memory: %v", err)
	}
	if err := svc.Save(context.Background(), memory.Document{
		SessionID: sessionID,
		Facts:     []memory.Fact{{ID: "s1", Content: "session scope fact", Confidence: 0.9, CreatedAt: now, UpdatedAt: now}},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save session scope memory: %v", err)
	}

	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "noop_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: "ok"}, nil
		},
	})

	p := &multiTurnCaptureProvider{toolTurns: 4}
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         reg,
		Model:         "m",
		MemoryService: svc,
		MemoryUserID:  uid,
		// ContextWindow left at 0 (disabled) so the compaction trigger (and
		// its own synchronous memory flush, which also calls storage.Load via
		// UpdateWith) never fires and confounds the count this test isolates.
	})

	if _, err := a.Run(context.Background(), sessionID, []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if requests := len(p.seenRequests()); requests < 3 {
		t.Fatalf("test setup: expected several requests in this Run, got %d", requests)
	}

	if got := counting.loadCount(sessionID); got != 1 {
		t.Errorf("session-scope storage.Load called %d times, want exactly 1 (once per Run)", got)
	}
	if got := counting.loadCount(memory.UserScope(uid).Key()); got != 1 {
		t.Errorf("user-scope storage.Load called %d times, want exactly 1 (once per Run)", got)
	}
}

// TestTurnInjection_RecomputesFenceOnSkillLoadMidRun is the RED test for the
// review-flagged fence bug: Run() resets activeSkill to "" BEFORE the
// once-per-Run buildTurnInjection call, so the injection computed at Run
// start always has activeSource == "" — the cross-skill memory fence
// (buildInjectionWithIDs' activeSource penalty) could never fire for any
// fact retrieved via the turn injection, no matter what skill later loads
// mid-Run.
//
// Two facts are seeded, one per (fake) skill source, sized so only ONE fits
// the 2000-token injection budget: "other" has higher confidence so it wins
// when unfenced (activeSource==""), but once the "target" skill loads
// mid-Run, the fence should penalize "other" (different skill source) enough
// for "target"'s own fact to win instead. The human message content is
// deliberately neutral (no shared vocabulary with either fact) so the
// outcome is driven purely by confidence-vs-fence, not by the relevance
// context's cosine-similarity term.
func TestTurnInjection_RecomputesFenceOnSkillLoadMidRun(t *testing.T) {
	sessionID := "sess-skill-fence"
	store, err := memory.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc := memory.NewService(slog.Default(), store, nil)
	defer svc.Close(context.Background())

	targetFact := "TARGET_SKILL_FACT " + strings.Repeat("a", 5000)
	otherFact := "OTHER_SKILL_FACT " + strings.Repeat("b", 5000)
	now := time.Now().UTC()
	if err := svc.Save(context.Background(), memory.Document{
		SessionID: sessionID,
		Facts: []memory.Fact{
			{ID: "target", Content: targetFact, Confidence: 0.5, Source: "skill:target", CreatedAt: now, UpdatedAt: now},
			{ID: "other", Content: otherFact, Confidence: 0.9, Source: "skill:other", CreatedAt: now, UpdatedAt: now},
		},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "target skill body",
					"skill_name":    "target",
				},
			}, nil
		},
	})

	p := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         reg,
		Model:         "m",
		MemoryService: svc,
	})

	if _, err := a.Run(context.Background(), sessionID, []models.Message{
		{Role: models.RoleHuman, Content: "please continue"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (skill-load turn + final), got %d", len(requests))
	}

	// Turn 0 (before the skill tool result is processed): activeSource=="",
	// no fencing — the higher-confidence "other" fact wins the budget.
	first := requests[0][len(requests[0])-1].Content
	if !strings.Contains(first, "OTHER_SKILL_FACT") {
		t.Fatalf("pre-skill-load injection should contain the unfenced higher-confidence fact, got: %q", first)
	}

	// Turn 1 (after react.go processes the "skill" tool result and should
	// recompute the injection with activeSource=="skill:target"): the fence
	// penalizes "other" (different skill source) enough for "target"'s own
	// (lower-confidence) fact to win instead.
	second := requests[1][len(requests[1])-1].Content
	if !strings.Contains(second, "TARGET_SKILL_FACT") {
		t.Fatalf("post-skill-load injection was not recomputed with the new activeSource fence "+
			"(should now contain the fenced-in target-skill fact), got: %q", second)
	}
	if strings.Contains(second, "OTHER_SKILL_FACT") {
		t.Fatalf("post-skill-load injection should have fenced OUT the other-skill fact, got: %q", second)
	}
}

// TestMetering_EstimateIncludesInjectionBytes is the unit-level RED test for
// design item 3 (metering invariant): the estimate fed to the compaction
// trigger must include the turn injection's bytes, or the trigger measures a
// smaller payload than what the request actually sends and can under-compact.
func TestMetering_EstimateIncludesInjectionBytes(t *testing.T) {
	a := &Agent{tools: tools.NewRegistry()}
	view := []models.Message{{Role: models.RoleHuman, Content: "hi"}}
	sys := "system"

	withoutInjection := a.estimateContextTokens(view, sys)

	injection := models.Message{Role: models.RoleHuman, Content: strings.Repeat("m", 4000)}
	withInjection := a.estimateContextTokens(appendTurnInjection(view, injection), sys)

	if withInjection <= withoutInjection {
		t.Fatalf("estimate did not grow with the injection: without=%d with=%d", withoutInjection, withInjection)
	}
}

// TestEstimateContextTokens_AnchorDoesNotDoubleCountInjection is the RED test
// for the review-flagged anchor bug: react.go sets a.lastInputTokens to the
// PROVIDER's real input-token count for a request whose VIEW already had
// a.turnInjection appended (maybeCompact does this on every request), while
// a.lastTokenCountMsgs anchors only the CANONICAL message count at that
// moment (react.go:477's `len(runMessages)`, which never includes the
// injection — it isn't part of canonical history). The next turn's delta
// (view[lastTokenCountMsgs:]) therefore includes the FRESH re-appended
// injection at the view's tail — a second copy of bytes already priced into
// lastInputTokens via the first copy — inflating the estimate by roughly one
// injection's worth of tokens every single turn and tripping compaction
// early.
//
// This reproduces the exact anchor-setting react.go:477 performs (rather
// than exercising a bare Agent that only hits the heuristic branch, which is
// why the existing metering tests didn't catch this).
func TestEstimateContextTokens_AnchorDoesNotDoubleCountInjection(t *testing.T) {
	a := &Agent{tools: tools.NewRegistry()}
	sys := "sys"
	injection := models.Message{Role: models.RoleHuman, Content: strings.Repeat("i", 4000)}
	a.turnInjection = injection

	oldRunMessages := []models.Message{{Role: models.RoleHuman, Content: "start"}}
	oldView := appendTurnInjection(oldRunMessages, injection)
	// Simulate react.go:471-477 exactly: lastInputTokens is what the
	// provider reported for a request sent with oldView (injection
	// included); lastTokenCountMsgs is the CANONICAL length at that moment.
	a.lastInputTokens = estimateTokens(oldView, sys, 0)
	a.lastTokenCountMsgs = len(oldRunMessages)

	// Next turn: canonical grew by two messages (assistant + tool result);
	// the view sent this turn re-derives from the new canonical set and
	// re-appends the SAME injection content at the tail.
	newRunMessages := append(append([]models.Message{}, oldRunMessages...),
		models.Message{Role: models.RoleAI, Content: "reply"},
		models.Message{Role: models.RoleTool, Content: "result"},
	)
	newView := appendTurnInjection(newRunMessages, injection)

	got := a.estimateContextTokens(newView, sys)

	// The true single-copy total: the whole current view (all canonical
	// messages plus exactly ONE injection) measured directly.
	want := estimateTokens(newView, sys, 0)

	// A generous rounding allowance (per-message overhead splits differently
	// whether accumulated in one pass or via anchor+delta), but nowhere near
	// a full extra injection's worth of tokens (~1200 for 4000 bytes at the
	// package's calibrated 3.3 bytes/token).
	const roundingSlack = 50
	if got > want+roundingSlack {
		t.Fatalf("anchor-based estimate double-counts the trailing injection: got=%d, want<=%d (true single-copy estimate=%d)",
			got, want+roundingSlack, want)
	}
}

// bigToolThenDoneProvider streams a fixed number of big-tool-result turns
// (each with a unique call ID so the repeat-call breaker never engages),
// then ends the run with a plain AI reply. Modeled on
// context_metering_test.go's loopingBigToolProvider but also captures every
// request's Messages, so compaction-interplay assertions can inspect the
// exact view sent on the turn compaction fires.
type bigToolThenDoneProvider struct {
	mu         sync.Mutex
	requests   [][]models.Message
	totalCalls int
	made       int
}

func (p *bigToolThenDoneProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *bigToolThenDoneProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	snapshot := make([]models.Message, len(req.Messages))
	copy(snapshot, req.Messages)
	p.requests = append(p.requests, snapshot)
	p.made++
	n := p.made
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if n > p.totalCalls {
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
			return
		}
		call := models.ToolCall{ID: fmt.Sprintf("call-%d", n), Name: "big_tool", Arguments: map[string]any{"n": n}}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{call}}, Done: true}
	}()
	return ch, nil
}

func (p *bigToolThenDoneProvider) seenRequests() [][]models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]models.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

// TestCompactionInterplay_SentViewEndsWithExactlyOneInjection is the RED test
// for design item 3's compaction-rebuild path: after maybeCompact rebuilds
// promptView from the compacted canonical messages, the injection must be
// re-appended exactly once — never zero times (which would mean the rebuilt
// request lost the volatile content) and never twice (which would mean a
// stale copy survived the rebuild alongside a fresh one).
func TestCompactionInterplay_SentViewEndsWithExactlyOneInjection(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "big_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: strings.Repeat("z", 5000)}, nil
		},
	})

	const totalCalls = 10
	p := &bigToolThenDoneProvider{totalCalls: totalCalls}

	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         reg,
		Model:         "m",
		ContextWindow: 4000,
	})

	if _, err := a.Run(context.Background(), "sess-compact-injection", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) == 0 {
		t.Fatal("test setup: no requests captured")
	}
	for i, view := range requests {
		count := 0
		for _, m := range view {
			if strings.Contains(m.Content, dateNoteMarker) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("request %d: view contains %d injection message(s), want exactly 1", i, count)
		}
		if last := view[len(view)-1]; !strings.Contains(last.Content, dateNoteMarker) {
			t.Fatalf("request %d: last message is not the injection: %q", i, last.Content)
		}
	}
}
