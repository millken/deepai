package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// --- M4-3: explicit session-state carriage across successive single-use
// Agent Runs (task-23-brief.md) ---------------------------------------------
//
// The REPL builds a fresh Agent every turn (Agent is single-use — Run's
// a.started guard). These tests guard that state which logically spans the
// whole conversation (the tool-call circuit breaker, the active skill, and
// the context-compaction anchors) survives that per-turn churn when the
// caller threads the SAME *SessionCarry through AgentConfig.Session on every
// turn, exactly as pkg/chat's ChatRepl does via its carry field.

// repeatFailProvider emits one failing, non-parallel-safe tool call
// ("sfail", fixed arguments) per turn for up to maxCalls turns, then ends the
// run cleanly with a plain text reply. Used to drive the repeat-call
// breaker's counters deterministically across two separate Agent instances
// (simulating two successive REPL turns) sharing one SessionCarry.
type repeatFailProvider struct {
	callCount int
	maxCalls  int
}

func (p *repeatFailProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *repeatFailProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > p.maxCalls {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	call := models.ToolCall{ID: fmt.Sprintf("f-%d", p.callCount), Name: "sfail", Arguments: map[string]any{"x": "y"}}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: []models.ToolCall{call}, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

func registerSFailTool(t *testing.T, reg *tools.Registry) {
	t.Helper()
	if err := reg.Register(models.Tool{
		Name: "sfail",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusFailed, Error: "boom"}, nil
		},
	}); err != nil {
		t.Fatalf("register sfail tool: %v", err)
	}
}

// TestSessionCarry_BreakerTripsAcrossRuns is the RED test for the P0-2
// cross-turn blind spot: today the repeat-call breaker (maxRepeatFails=8)
// lives entirely inside Run()'s local `breaker` variable, reset to zero every
// Run. Two consecutive Runs (fresh, single-use Agents — mirroring two REPL
// turns) sharing one SessionCarry, split 4 identical failing calls each,
// must trip the breaker on Run 2 (4+4 = 8) even though NEITHER Run alone
// reaches the threshold.
func TestSessionCarry_BreakerTripsAcrossRuns(t *testing.T) {
	session := NewSessionCarry()

	reg1 := tools.NewRegistry()
	registerSFailTool(t, reg1)
	a1 := New(AgentConfig{LLMProvider: &repeatFailProvider{maxCalls: 4}, Tools: reg1, MaxToolCalls: 10, Session: session})
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: unexpected error (4 failures alone must not trip the breaker): %v", err)
	}

	reg2 := tools.NewRegistry()
	registerSFailTool(t, reg2)
	a2 := New(AgentConfig{LLMProvider: &repeatFailProvider{maxCalls: 4}, Tools: reg2, MaxToolCalls: 10, Session: session})
	_, err := a2.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run 2: expected the repeat-loop breaker to trip using the count carried from Run 1 (4+4=8), got nil error")
	}
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("Run 2 error = %v, want a repeat-loop error", err)
	}
}

// anchorReportingProvider always reports a large provider input-token count
// (50000) on a single-turn, no-tool-calls response.
type anchorReportingProvider struct{}

func (p *anchorReportingProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *anchorReportingProvider) Stream(context.Context, llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			Message: &models.Message{Role: models.RoleAI, Content: "ok"},
			Usage:   &llm.Usage{InputTokens: 50000, OutputTokens: 10, TotalTokens: 50010},
			Done:    true,
			Stop:    "stop",
		}
	}()
	return ch, nil
}

// TestSessionCarry_AnchorCarriesAcrossRuns is the RED test for the
// compaction-anchor cross-turn blind spot: today lastInputTokens/
// lastTokenCountMsgs live only on the (single-use, per-turn) Agent struct, so
// a fresh Run's first estimate always falls back to the byte heuristic even
// though the previous Run just got a real provider-reported count. Mirrors
// TestEstimateContextTokens_PrefersProviderCount's anchor-vs-heuristic
// assertion shape (compact_test.go).
func TestSessionCarry_AnchorCarriesAcrossRuns(t *testing.T) {
	session := NewSessionCarry()

	a1 := New(AgentConfig{LLMProvider: &anchorReportingProvider{}, Tools: tools.NewRegistry(), Session: session})
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if session.lastInputTokens != 50000 {
		t.Fatalf("session.lastInputTokens = %d, want 50000 (Run 1's provider-reported count must be written back to the session)", session.lastInputTokens)
	}

	// Run 2: a fresh Agent (new single-use instance, same session) must
	// start with this anchor already primed.
	a2 := New(AgentConfig{LLMProvider: &anchorReportingProvider{}, Tools: tools.NewRegistry(), Session: session})
	view := []models.Message{{Role: models.RoleHuman, Content: "x"}}
	injection := models.Message{Role: models.RoleHuman, Content: "[System note: irrelevant]"}
	fullView := appendTurnInjection(view, injection)

	heuristic := estimateTokens(fullView, "", 0)
	got := a2.estimateContextTokens(fullView, "")
	if got < 50000 {
		t.Fatalf("Run 2's first estimate = %d (heuristic alone was %d), want >= the carried provider anchor 50000 — anchor not carried across Runs", got, heuristic)
	}
}

// TestCompactOnOverflow_ResetsCarriedAnchorToo is the RED test for the M4
// final-phase review's F-M4-1 (MEDIUM): compactOnOverflow
// (pkg/agent/streaming.go) invalidates the provider-anchor
// (lastInputTokens/lastTokenCountMsgs) by assigning the Agent's OWN fields
// directly instead of calling setTokenAnchor — the one direct-assignment
// site M4-3's write-back conversion missed. When a session is carried, this
// means the reset never reaches session.lastInputTokens/lastTokenCountMsgs:
// a fresh Agent on the NEXT REPL turn (New(), primed from the carried
// session) revives the stale pre-overflow anchor. Since compactMessages
// preserves message COUNT, the staleness guard in estimateContextTokens
// (lastTokenCountMsgs > len(view)-1) cannot fire either, so turn N+1
// over-estimates its very first request from a number that describes
// content already compacted away — triggering an immediate, unnecessary
// re-compaction (with its synchronous 30s memory flush) on a turn that may
// not need it at all.
func TestCompactOnOverflow_ResetsCarriedAnchorToo(t *testing.T) {
	session := NewSessionCarry()
	a := &Agent{compactionKeepTail: 6, systemPrompt: "sys", logger: slog.Default(), session: session}
	msgs := []models.Message{{Role: models.RoleHuman, Content: "start"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleAI, Content: strings.Repeat("x", 4000)})
	}

	// Simulate a prior turn's setTokenAnchor call (via a real provider
	// response) having mirrored a real anchor onto both the Agent and the
	// carried session, before THIS turn's request overflowed.
	a.lastInputTokens = 50000
	a.lastTokenCountMsgs = 12
	session.lastInputTokens = 50000
	session.lastTokenCountMsgs = 12

	if _, ok := a.compactOnOverflow(msgs, "sys", 1, "test"); !ok {
		t.Fatal("test setup: compactOnOverflow must actually compact for this fixture (see " +
			"TestCompactOnOverflow_ActuallyCompacts for the same shape)")
	}

	if a.lastInputTokens != 0 || a.lastTokenCountMsgs != 0 {
		t.Fatalf("agent anchor after compactOnOverflow = {%d, %d}, want {0, 0}", a.lastInputTokens, a.lastTokenCountMsgs)
	}
	if session.lastInputTokens != 0 || session.lastTokenCountMsgs != 0 {
		t.Fatalf("session anchor after compactOnOverflow = {%d, %d}, want {0, 0} — compactOnOverflow "+
			"invalidates the Agent's own anchor but not the carried session's, so the NEXT REPL turn's "+
			"New() would revive the stale pre-overflow anchor (review M4-final F-M4-1)",
			session.lastInputTokens, session.lastTokenCountMsgs)
	}

	// Mirrors the reviewer's exact probe: a fresh Agent primed from this
	// same session (as New() does at the top of every REPL turn) must NOT
	// see the stale anchor.
	a2 := New(AgentConfig{Tools: tools.NewRegistry(), Session: session})
	if a2.lastInputTokens != 0 || a2.lastTokenCountMsgs != 0 {
		t.Fatalf("a fresh Agent primed from the same session after compactOnOverflow = {%d, %d}, want "+
			"{0, 0} (New()'s priming revives whatever the session's fields hold)", a2.lastInputTokens, a2.lastTokenCountMsgs)
	}
}

// TestSessionCarry_CompactionStallCarriesAcrossRuns is the RED test for
// review r1 F5: the compaction-stall bookkeeping (compactionStalled/
// compactionStalledAt) is carried via New()'s priming and
// setCompactionStall's mirror write, but nothing exercised either half —
// the review proved both could be deleted with the whole suite staying
// green. Mirrors context_metering_test.go's
// TestCompactionTrigger_StalledCompactionFiresOnce fixture exactly (a
// "hopeless floor": ContextWindow=100 against 5000-byte tool results that
// can never be compacted away since they're always the protected tail) —
// that scenario is guaranteed to stall on its very first compaction
// attempt. Run 1 must leave session.compactionStalled == true afterward
// (proving the write-back mirror fired); Run 2 (a fresh Agent, same
// session, a short run whose own tiny message count is nowhere near the
// carried compactionStalledAt+compactionKeepTail unstall threshold) must
// then skip the proactive-compaction branch ENTIRELY despite still being
// far over threshold — proving New()'s priming actually seeded
// a.compactionStalled from the session, not just a.compactionStalledAt in
// isolation.
func TestSessionCarry_CompactionStallCarriesAcrossRuns(t *testing.T) {
	bigToolReg := func() *tools.Registry {
		reg := tools.NewRegistry()
		_ = reg.Register(models.Tool{
			Name: "big_tool",
			Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
				return models.ToolResult{Content: strings.Repeat("z", 5000)}, nil
			},
		})
		return reg
	}

	session := NewSessionCarry()

	// Run 1: same "hopeless floor" fixture as
	// TestCompactionTrigger_StalledCompactionFiresOnce — guaranteed to stall
	// on its first (and, per that test, only every-few-turns) compaction
	// attempt.
	provider1 := &loopingBigToolProvider{totalCalls: 10}
	a1 := New(AgentConfig{
		LLMProvider:   provider1,
		Tools:         bigToolReg(),
		Model:         "m",
		ContextWindow: 100,
		Session:       session,
	})
	if _, events := runAndCollectEvents(t, a1, "sess-stall-carry", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); !hasCompactEvent(events) {
		t.Fatal("test setup: Run 1 never compacted at all — the hopeless-floor fixture no longer trips the threshold")
	}
	if !session.compactionStalled {
		t.Fatal("session.compactionStalled = false after Run 1's hopeless-floor compaction, want true " +
			"(setCompactionStall's write-back mirror did not fire)")
	}
	stalledAt := session.compactionStalledAt
	if stalledAt == 0 {
		t.Fatal("session.compactionStalledAt = 0 after Run 1, want the canonical message count at the stall point")
	}

	// Run 2: a fresh Agent, same session, a SHORT run (its own message
	// count stays far below stalledAt+compactionKeepTail, so the unstall
	// condition in maybeCompact must not clear it) — but with the SAME
	// hopeless ContextWindow, so if the carried stall were NOT honored, the
	// proactive-compaction branch would fire again immediately.
	provider2 := &loopingBigToolProvider{totalCalls: 1}
	a2 := New(AgentConfig{
		LLMProvider:   provider2,
		Tools:         bigToolReg(),
		Model:         "m",
		ContextWindow: 100,
		Session:       session,
	})
	if !a2.compactionStalled {
		t.Fatal("a2.compactionStalled = false immediately after New(), want true — New() must prime " +
			"a.compactionStalled from the carried session, not just compactionStalledAt")
	}
	_, events2 := runAndCollectEvents(t, a2, "sess-stall-carry", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	})
	if hasCompactEvent(events2) {
		t.Fatal("Run 2 compacted despite the carried stall (compactionStalledAt+compactionKeepTail not " +
			"yet reached) — the carried stall must suppress proactive compaction until enough new " +
			"messages accumulate, exactly as it would within a single Run")
	}
}

// TestSessionCarry_SkillPersistsAcrossRuns is the RED test for P1-6: a skill
// loaded in Run 1 (a fresh, single-use Agent) must still be active in Run 2
// (a DIFFERENT fresh Agent sharing the same SessionCarry) — its body present
// in the system prompt, the "Available skills" catalog STILL shown (2026-09
// fix: the catalog stays resident for the life of the session so the model
// can switch to a different skill later — see removeAppliedSkillBody's doc
// comment; only the previous skill's BODY is ever replaced), ActiveSkill()
// reporting the carried name, and (M4-2 interplay) the Run 2 turn
// injection's memory fence keyed off activeSource=="skill:<name>" from the
// very first request of Run 2 (not just mid-Run, as the existing
// TestTurnInjection_RecomputesFenceOnSkillLoadMidRun already guards within a
// single Run).
func TestSessionCarry_SkillPersistsAcrossRuns(t *testing.T) {
	sessionID := "sess-skill-carry"
	store, err := memory.NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
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

	session := NewSessionCarry()

	// Run 1: loads the "target" skill via a fake skill tool result.
	reg1 := tools.NewRegistry()
	if err := reg1.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "SKILL_BODY_TARGET",
					"skill_name":    "target",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool: %v", err)
	}
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg1, Session: session})
	if _, err := a1.Run(context.Background(), sessionID, []models.Message{
		{Role: models.RoleHuman, Content: "please continue"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Run 2: a fresh Agent, constructed the way the REPL builds every turn's
	// agent — base system prompt, THEN the "Available skills" catalog
	// appended via AppendSystemPrompt (repl.go:542-549) — sharing the same
	// session.
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	reg2 := tools.NewRegistry()
	p2 := &multiTurnCaptureProvider{toolTurns: 0}
	a2 := New(AgentConfig{
		LLMProvider:   p2,
		Tools:         reg2,
		Session:       session,
		MemoryService: svc,
	})
	a2.AppendSystemPrompt(catalogMarker + "\n- some-other-skill: does something")

	if _, err := a2.Run(context.Background(), sessionID, []models.Message{
		{Role: models.RoleHuman, Content: "hello again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	prompt := a2.BuildSystemPrompt()
	if !strings.Contains(prompt, "SKILL_BODY_TARGET") {
		t.Fatalf("Run 2 system prompt missing the carried skill body, got: %q", prompt)
	}
	if !strings.Contains(prompt, catalogMarker) {
		t.Fatalf("Run 2 system prompt lost the 'Available skills' catalog (it must stay resident even "+
			"once a skill is carried active, so the model can still switch skills — 2026-09 fix), got: %q", prompt)
	}
	if got := a2.ActiveSkill(); got != "target" {
		t.Fatalf("Run 2 ActiveSkill() = %q, want %q", got, "target")
	}

	requests := p2.seenRequests()
	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request in Run 2, got %d", len(requests))
	}
	injectionMsg := requests[0][len(requests[0])-1].Content
	if !strings.Contains(injectionMsg, "TARGET_SKILL_FACT") {
		t.Fatalf("Run 2's turn injection missing the fenced-in target-skill fact (activeSource should already be %q at Run 2 start), got: %q", "skill:target", injectionMsg)
	}
	if strings.Contains(injectionMsg, "OTHER_SKILL_FACT") {
		t.Fatalf("Run 2's turn injection should have fenced OUT the other-skill fact, got: %q", injectionMsg)
	}
}

// TestSessionCarry_SkillCarryPreservesTrailingSystemPrompt guards against two
// generations of the same class of bug around what sits BEFORE a loaded
// skill's body in the system prompt.
//
// Originally (review r1 F2) this was the RED test for
// removeSkillDescriptions truncating a.systemPrompt at the "Available
// skills" marker — a.systemPrompt[:idx] and nothing else — which silently
// discarded whatever the caller appended AFTER the catalog. The REPL appends
// the skill catalog and THEN the CLI/DEEPAI.md system prompt (repl.go's
// runTurn: two AppendSystemPrompt calls right after New()), so that trailing
// content sat exactly where the old truncation cut.
//
// 2026-09 fix: removeSkillDescriptions is gone — the catalog is never
// stripped now (it stays resident for the life of the session so the model
// can switch skills; see removeAppliedSkillBody's doc comment), so the
// specific truncation bug this test was written for is structurally
// impossible. The test is kept and repurposed to guard the analogous risk
// in the REPLACEMENT mechanism: removeAppliedSkillBody excises the
// previously-applied skill body by exact suffix match before a new body (or
// a carried one) is appended, and it must strip ONLY that suffix, leaving
// everything appended before it — base prompt, catalog, and the
// DEEPAI.md/CLI tail appended after the catalog — untouched. Checked both in
// the SAME Run the skill loads and after a carried second Run, since the
// carried-reapply path (react.go's Run()) and the mid-Run load path
// (toolexec.go's applySkillResult) both call removeAppliedSkillBody
// independently.
func TestSessionCarry_SkillCarryPreservesTrailingSystemPrompt(t *testing.T) {
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	const deepaiMD = "DEEPAI_MD_PROJECT_INSTRUCTIONS: follow the house style."

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "SKILL_BODY_X",
					"skill_name":    "x",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool: %v", err)
	}

	session := NewSessionCarry()
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	// Mirrors the REPL's real construction order: base system prompt, then
	// the skill catalog, then MORE content appended AFTER the catalog.
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg, SystemPrompt: "BASE", Session: session})
	a1.AppendSystemPrompt(catalogMarker + "\n- x: does x")
	a1.AppendSystemPrompt(deepaiMD)

	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	prompt1 := a1.BuildSystemPrompt()
	if !strings.Contains(prompt1, "SKILL_BODY_X") {
		t.Fatalf("Run 1: system prompt missing the loaded skill body, got: %q", prompt1)
	}
	if !strings.Contains(prompt1, catalogMarker) {
		t.Fatalf("Run 1: catalog missing (2026-09 fix: it must stay resident, never stripped), got: %q", prompt1)
	}
	if !strings.Contains(prompt1, deepaiMD) {
		t.Fatalf("Run 1 (review r1 F2, pre-existing single-turn variant): trailing DEEPAI.md/CLI system "+
			"prompt content lost after a skill loaded, got: %q", prompt1)
	}

	// Run 2: a fresh Agent, same session, same REPL-mirroring construction
	// order — the carried skill's reapply (react.go's Run() start) must
	// ALSO preserve the trailing content, not just the single-turn case.
	reg2 := tools.NewRegistry()
	p2 := &multiTurnCaptureProvider{toolTurns: 0}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: reg2, SystemPrompt: "BASE", Session: session})
	a2.AppendSystemPrompt(catalogMarker + "\n- x: does x")
	a2.AppendSystemPrompt(deepaiMD)

	if _, err := a2.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	prompt2 := a2.BuildSystemPrompt()
	if !strings.Contains(prompt2, "SKILL_BODY_X") {
		t.Fatalf("Run 2: carried skill body missing, got: %q", prompt2)
	}
	if !strings.Contains(prompt2, catalogMarker) {
		t.Fatalf("Run 2: catalog missing (2026-09 fix: it must stay resident, never stripped), got: %q", prompt2)
	}
	if !strings.Contains(prompt2, deepaiMD) {
		t.Fatalf("Run 2 (review r1 F2): trailing DEEPAI.md/CLI system prompt content lost across a carried "+
			"skill, got: %q", prompt2)
	}
}

// TestSessionCarry_SkillReloadSameSkillDoesNotDuplicateBody is the RED test
// for review r1 F7: reloading a skill that is ALREADY active (the model
// calls "skill" for the same name twice in one Run) used to duplicate the
// body verbatim in the system prompt — the second load just appended the
// same body again on top of the first. Still guarded post-2026-09-fix by
// applySkillResult's bodyAlreadyApplied short-circuit, which must skip the
// remove-then-reappend cycle entirely for a genuine no-op reload (not just
// avoid a NET duplicate via remove+reappend, which would needlessly churn
// the system prompt and invalidate the provider's cached prefix for no
// reason).
func TestSessionCarry_SkillReloadSameSkillDoesNotDuplicateBody(t *testing.T) {
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "SKILL_BODY_DUP",
					"skill_name":    "dup",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool: %v", err)
	}

	// toolTurns:2 with the SAME toolName drives the model calling "skill"
	// (for the same "dup" skill, per the fixed handler above) on two
	// consecutive turns of ONE Run — reloading a skill that is already
	// active by the second call.
	p := &multiTurnCaptureProvider{toolTurns: 2, toolName: "skill"}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, SystemPrompt: "BASE"})
	a.AppendSystemPrompt(catalogMarker + "\n- dup: does dup")

	if _, err := a.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prompt := a.BuildSystemPrompt()
	if got := strings.Count(prompt, "SKILL_BODY_DUP"); got != 1 {
		t.Fatalf("skill body appears %d time(s) in the system prompt after reloading the SAME already-"+
			"active skill mid-Run, want exactly 1 (review r1 F7), got prompt: %q", got, prompt)
	}
}

// TestSessionCarry_EmptySkillBodyDoesNotClearCarriedBody is the RED test for
// review r1 F11: a skill result with skill_name set but an EMPTY
// system_prompt used to overwrite session.skillPrompt unconditionally,
// wiping out a previously carried (real) body while leaving activeSkill
// pointed at the same name — the next Run would then report
// ActiveSkill()==name with no body and an un-stripped catalog.
func TestSessionCarry_EmptySkillBodyDoesNotClearCarriedBody(t *testing.T) {
	session := NewSessionCarry()

	// Run 1: loads "target" with a real, non-empty body.
	reg1 := tools.NewRegistry()
	if err := reg1.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "REAL_BODY",
					"skill_name":    "target",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool (run 1): %v", err)
	}
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg1, Session: session})
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if session.skillPrompt != "REAL_BODY" {
		t.Fatalf("test setup: session.skillPrompt = %q after Run 1, want %q", session.skillPrompt, "REAL_BODY")
	}

	// Run 2: fresh Agent, same session (carries "target" + REAL_BODY at
	// Run start) — mid-Run, the model reloads "target" again but this time
	// the skill's rendered body is genuinely EMPTY.
	reg2 := tools.NewRegistry()
	if err := reg2.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "",
					"skill_name":    "target",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool (run 2): %v", err)
	}
	p2 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: reg2, Session: session})
	if _, err := a2.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	if session.skillPrompt != "REAL_BODY" {
		t.Fatalf("session.skillPrompt = %q after an empty-body skill reload, want the previously carried "+
			"%q preserved (review r1 F11)", session.skillPrompt, "REAL_BODY")
	}
	if got := a2.ActiveSkill(); got != "target" {
		t.Fatalf("a2.ActiveSkill() = %q, want %q", got, "target")
	}
}

// TestSessionCarry_RealBodyReloadAfterEmptyBodyIsApplied is the RED test for
// review r2 F2-b: the reapply guard (`alreadyActive`, keyed on skill NAME)
// diverges from the property it needs to protect (whether the body is
// actually PRESENT in the system prompt) in exactly the durable state the
// r1 F11 fix makes reachable: Run 1 loads "target" with an EMPTY rendered
// body — carry ends up at {activeSkill: "target", skillPrompt: ""} (F11:
// activeSkill is written unconditionally, skillPrompt only when non-empty).
// Run 2 (fresh Agent, same session) starts with a.ActiveSkill()=="target"
// already (from session.activeSkill) but nothing reapplied (session.
// skillPrompt is "", so react.go's Run()-start reapply is skipped). Mid-Run,
// the model reloads "target" again, NOW with a real, non-empty body. Keying
// the reapply guard on name alone (skillName == a.ActiveSkill()) reports
// "already active" and skips the reapply entirely — the real body is never
// applied. The fix must apply the body exactly once whenever it is not
// already present, regardless of whether the NAME was already active. (The
// skill catalog's presence is no longer meaningful bug evidence here —
// 2026-09 fix: it stays resident throughout regardless of whether the body
// applied correctly; the assertion below checks it only to confirm the fix
// didn't accidentally regress that separate invariant.)
func TestSessionCarry_RealBodyReloadAfterEmptyBodyIsApplied(t *testing.T) {
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	session := NewSessionCarry()

	// Run 1: loads "target" with an EMPTY rendered body.
	reg1 := tools.NewRegistry()
	if err := reg1.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "",
					"skill_name":    "target",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool (run 1): %v", err)
	}
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg1, Session: session})
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if session.activeSkill != "target" || session.skillPrompt != "" {
		t.Fatalf("test setup: session = {activeSkill:%q skillPrompt:%q} after Run 1, want {\"target\", \"\"}",
			session.activeSkill, session.skillPrompt)
	}

	// Run 2: fresh Agent, same session (carries activeSkill="target",
	// skillPrompt="" — nothing reapplied at Run start, so the catalog is
	// still showing when Run 2's system prompt is assembled below). Mid-
	// Run, the model reloads "target" — same name, already ActiveSkill()
	// per the carry — but THIS TIME with a real, non-empty body.
	reg2 := tools.NewRegistry()
	if err := reg2.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data: map[string]any{
					"system_prompt": "REAL_BODY_LATE",
					"skill_name":    "target",
				},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool (run 2): %v", err)
	}
	p2 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: reg2, SystemPrompt: "BASE", Session: session})
	a2.AppendSystemPrompt(catalogMarker + "\n- target: t")

	if _, err := a2.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	prompt2 := a2.BuildSystemPrompt()
	if got := strings.Count(prompt2, "REAL_BODY_LATE"); got != 1 {
		t.Fatalf("REAL_BODY_LATE appears %d time(s) in Run 2's system prompt after a real-body reload "+
			"following Run 1's empty-body load, want exactly 1 (review r2 F2-b) — 0 means the body was "+
			"never applied (the bug), >1 means it duplicated, got prompt: %q", got, prompt2)
	}
	if !strings.Contains(prompt2, catalogMarker) {
		t.Fatalf("Run 2: catalog missing after the real body finally loaded (2026-09 fix: it must stay "+
			"resident, never stripped), got: %q", prompt2)
	}
}

// TestSessionCarry_SkillReloadWithDifferentBodyAppliesDespiteSubstringCollision
// is the RED test for the M4 final-phase review's F-M4-7 (theoretical, but
// cheap to fix precisely): a substring-based "already applied" check
// (strings.Contains(a.systemPrompt, loadedSkillPrompt)) can false-positive
// when a later, genuinely different skill body happens to already appear
// as a substring of the assembled prompt for an unrelated reason (shared
// vocabulary with the base prompt, in this case) — reporting "already
// applied" and silently dropping a real reload. The fix tracks the EXACT
// string this Agent itself last appended (a.appliedSkillPrompt) and
// compares for equality instead of substring containment.
func TestSessionCarry_SkillReloadWithDifferentBodyAppliesDespiteSubstringCollision(t *testing.T) {
	// The base prompt coincidentally contains, verbatim, the text of the
	// SECOND skill body below ("go for it") — unrelated to the skill
	// system, just shared vocabulary. A substring check over the assembled
	// prompt cannot tell that apart from the second body having actually
	// been appended.
	const base = "BASE PROMPT: when ready, go for it and don't look back."
	reg := tools.NewRegistry()
	calls := 0
	if err := reg.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			calls++
			if calls == 1 {
				return models.ToolResult{
					Content: "loaded",
					Data:    map[string]any{"system_prompt": "FIRST_BODY", "skill_name": "x"},
				}, nil
			}
			return models.ToolResult{
				Content: "loaded",
				// This body's text already appears in `base`, verbatim,
				// even though it has never been appended for skill "x" —
				// the exact-equality fix must not be fooled by that.
				Data: map[string]any{"system_prompt": "go for it", "skill_name": "x"},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool: %v", err)
	}

	p := &multiTurnCaptureProvider{toolTurns: 2, toolName: "skill"}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, SystemPrompt: base})

	if _, err := a.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prompt := a.BuildSystemPrompt()
	// "go for it" appears once already in `base` regardless of what
	// happened; a genuine second append makes it TWO. A substring-based
	// guard would have (wrongly) skipped the second append, leaving it at
	// one — indistinguishable, without the fix, from "already applied".
	if got := strings.Count(prompt, "go for it"); got != 2 {
		t.Fatalf("\"go for it\" appears %d time(s) in the system prompt, want 2 (1 from the base prompt's "+
			"own text + 1 from the genuinely reloaded second skill body) — got 1 means the substring-"+
			"collision false-positive skipped a real reload (review M4-final F-M4-7), got prompt: %q",
			got, prompt)
	}
}

// TestSessionCarry_CarriedSkillThenSameSkillReloadDoesNotDuplicate is the RED
// test for the M4 final-verification review's F-V1: react.go's Run-start
// carried-reapply block (`a.appliedSkillPrompt = a.session.skillPrompt`,
// set immediately after the AppendSystemPrompt that actually applies the
// carried body) is implemented but was unguarded by any test — deleting
// that one line leaves the whole pkg/agent suite green while
// re-introducing review r1 F7's duplicate-body bug for exactly the carried
// case: a body applied via the Run-start reapply (not the mid-Run
// skill-result handling, which is the ONLY site
// TestSessionCarry_SkillReloadSameSkillDoesNotDuplicateBody exercises) must
// still be recognised as "already applied" when the model reloads the SAME
// skill with the SAME body mid-Run.
func TestSessionCarry_CarriedSkillThenSameSkillReloadDoesNotDuplicate(t *testing.T) {
	const body = "SKILL_BODY_CARRIED"
	skillTool := func() models.Tool {
		return models.Tool{
			Name: "skill",
			Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
				return models.ToolResult{
					Content: "loaded",
					Data:    map[string]any{"system_prompt": body, "skill_name": "carried"},
				}, nil
			},
		}
	}

	session := NewSessionCarry()

	// Run 1: loads "carried" — session ends up with {activeSkill:
	// "carried", skillPrompt: body}.
	reg1 := tools.NewRegistry()
	if err := reg1.Register(skillTool()); err != nil {
		t.Fatalf("register skill tool (run 1): %v", err)
	}
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg1, Session: session})
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Run 2: fresh Agent, same session — the Run-start reapply (react.go
	// :380-384) applies `body` and records it in appliedSkillPrompt. Then,
	// mid-Run, the model reloads the SAME skill with the SAME body again.
	reg2 := tools.NewRegistry()
	if err := reg2.Register(skillTool()); err != nil {
		t.Fatalf("register skill tool (run 2): %v", err)
	}
	p2 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: reg2, SystemPrompt: "BASE", Session: session})
	if _, err := a2.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	prompt := a2.BuildSystemPrompt()
	if got := strings.Count(prompt, body); got != 1 {
		t.Fatalf("carried skill body appears %d time(s) after a same-skill mid-Run reload in a CARRIED "+
			"Run, want exactly 1 (review M4-final F-V1) — the Run-start reapply's appliedSkillPrompt "+
			"write-back must make the mid-Run reload recognise the body as already applied, got prompt: %q",
			got, prompt)
	}
}

// TestApplySkillResult_SwitchingBackAndForthReplacesBody is the RED test for
// the "skill body only appended, never removed" bug: applySkillResult used
// to fold every loaded body into the system prompt via AppendSystemPrompt
// alone, with no corresponding removal — appliedSkillPrompt only remembers
// the LAST body applied, so switching A -> B -> A within a single Run/Agent
// left TWO copies of A's body (the second A load didn't match
// appliedSkillPrompt, which by then held B's body, so it re-appended A
// instead of recognising anything). The fix (removeAppliedSkillBody) must
// excise the previously-applied body before appending the new one, so at
// most one skill's body is ever present at a time — while the skill catalog
// (and anything the caller appended after it) stays untouched, since the
// catalog must remain resident for the model to be able to switch skills at
// all (2026-09 fix; see removeAppliedSkillBody's doc comment).
func TestApplySkillResult_SwitchingBackAndForthReplacesBody(t *testing.T) {
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	const tail = "DEEPAI_MD_TAIL: house style, follow it."

	a := New(AgentConfig{SystemPrompt: "BASE"})
	// Mirrors the REPL's real construction order: base prompt, then the
	// skill catalog, then more content appended after it (repl.go's
	// runTurn: catalog, then the CLI/DEEPAI.md system prompt).
	a.AppendSystemPrompt(catalogMarker + "\n- a: does a\n- b: does b")
	a.AppendSystemPrompt(tail)

	ctx := context.Background()
	loadSkill := func(name, body string) {
		a.applySkillResult(ctx, "s1", models.ToolResult{
			Content: "loaded",
			Data:    map[string]any{"system_prompt": body, "skill_name": name},
		}, nil)
	}

	// A -> B -> A, all within the same Agent (mirrors three mid-Run "skill"
	// tool calls in one Run).
	loadSkill("a", "SKILL_BODY_A")
	loadSkill("b", "SKILL_BODY_B")
	loadSkill("a", "SKILL_BODY_A")

	prompt := a.BuildSystemPrompt()
	if got := strings.Count(prompt, "SKILL_BODY_A"); got != 1 {
		t.Fatalf("SKILL_BODY_A appears %d time(s) after A -> B -> A, want exactly 1 (switching back to A "+
			"must not duplicate its body), got prompt: %q", got, prompt)
	}
	if got := strings.Count(prompt, "SKILL_BODY_B"); got != 0 {
		t.Fatalf("SKILL_BODY_B appears %d time(s) after switching away from B back to A, want 0 (B's body "+
			"must be removed when superseded, not just left behind), got prompt: %q", got, prompt)
	}
	if !strings.Contains(prompt, catalogMarker) {
		t.Fatalf("skill catalog missing after switching skills twice — it must stay resident so the model "+
			"can keep switching skills for the rest of the session, got: %q", prompt)
	}
	if !strings.Contains(prompt, tail) {
		t.Fatalf("content the caller appended after the catalog (e.g. DEEPAI.md) was lost, got: %q", prompt)
	}
	if got := a.ActiveSkill(); got != "a" {
		t.Fatalf("ActiveSkill() = %q, want %q (the last skill loaded)", got, "a")
	}
}

// TestRemoveAppliedSkillBody_TailInvariantViolationLogsInsteadOfSilentlyDegrading
// is the RED test for the final-review gap: removeAppliedSkillBody's whole
// design rests on the invariant (documented on its own doc comment) that a
// skill body, once appended, is always the LAST thing in a.systemPrompt —
// but that invariant was only ever documented, never asserted. The original
// fix cleared a.appliedSkillPrompt unconditionally even when the suffix
// match failed, which would silently and permanently drop the only handle
// pointing at the stale body still sitting in the prompt, with zero signal
// anything went wrong — a quieter recurrence of exactly the "body left
// behind forever" bug this whole fix targets. Exercises removeAppliedSkillBody
// directly (constructing the Agent literal, same package, mirroring
// compact_test.go's own log-capture pattern) so the invariant-violated case
// can be driven precisely, independent of what either real call site does
// with a.appliedSkillPrompt right afterward.
func TestRemoveAppliedSkillBody_TailInvariantViolationLogsInsteadOfSilentlyDegrading(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	const oldBody = "SKILL_BODY_STALE"
	const interloper = "SOMETHING_APPENDED_AFTER_THE_BODY"

	a := &Agent{
		logger: logger,
		// Simulates the tail invariant already broken: something appended
		// AFTER the recorded skill body, so it's no longer the trailing
		// suffix removeAppliedSkillBody expects.
		systemPrompt:       "BASE\n\n" + oldBody + "\n\n" + interloper,
		appliedSkillPrompt: oldBody,
	}

	a.removeAppliedSkillBody()

	if !strings.Contains(logBuf.String(), "level=WARN") {
		t.Fatalf("expected a Warn log when the skill body isn't found at the system prompt tail, got log: %q",
			logBuf.String())
	}
	if got := strings.Count(a.systemPrompt, oldBody); got != 1 {
		t.Fatalf("stale body count = %d after a failed removal, want exactly 1 — must not be guessed-removed "+
			"from the wrong place, and must not silently vanish, got prompt: %q", got, a.systemPrompt)
	}
	if !strings.Contains(a.systemPrompt, interloper) {
		t.Fatalf("content after the stale body was lost — a failed match must not touch a.systemPrompt at "+
			"all, got: %q", a.systemPrompt)
	}
	if a.appliedSkillPrompt != oldBody {
		t.Fatalf("appliedSkillPrompt = %q after a failed removal, want unchanged %q — clearing it here would "+
			"claim nothing is applied even though the stale body is still sitting in the prompt", a.appliedSkillPrompt, oldBody)
	}
}

// TestApplySkillResult_TailInvariantViolationWarnsAndDoesNotSilentlyDuplicate
// drives the same invariant violation through the real call path
// (applySkillResult), constructed the way the final review suggested: load
// a skill normally, then manually AppendSystemPrompt more content —
// something no real code path in this package does mid-Run (see
// removeAppliedSkillBody's doc comment), simulating the invariant breaking
// out from under it — and confirm loading a DIFFERENT skill afterward warns
// rather than silently corrupting the prompt (duplicating or dropping
// bodies).
func TestApplySkillResult_TailInvariantViolationWarnsAndDoesNotSilentlyDuplicate(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	a := New(AgentConfig{SystemPrompt: "BASE"})
	a.logger = logger
	a.AppendSystemPrompt("Available skills (use the matching skill when the user request fits):\n- a: does a\n- b: does b")

	ctx := context.Background()
	loadSkill := func(name, body string) {
		a.applySkillResult(ctx, "s1", models.ToolResult{
			Content: "loaded",
			Data:    map[string]any{"system_prompt": body, "skill_name": name},
		}, nil)
	}

	loadSkill("a", "SKILL_BODY_A")

	// Break the invariant: something appends after the applied body.
	a.AppendSystemPrompt("INTERLOPER_TEXT_AFTER_BODY")

	loadSkill("b", "SKILL_BODY_B")

	if !strings.Contains(logBuf.String(), "level=WARN") {
		t.Fatalf("expected a Warn log once the tail invariant is violated, got log: %q", logBuf.String())
	}
	prompt := a.BuildSystemPrompt()
	if got := strings.Count(prompt, "SKILL_BODY_A"); got != 1 {
		t.Fatalf("SKILL_BODY_A appears %d time(s) after the invariant broke, want exactly 1 (left in place, "+
			"neither duplicated nor silently dropped), got prompt: %q", got, prompt)
	}
	if !strings.Contains(prompt, "INTERLOPER_TEXT_AFTER_BODY") {
		t.Fatalf("interloper content was lost, got: %q", prompt)
	}
	if got := strings.Count(prompt, "SKILL_BODY_B"); got != 1 {
		t.Fatalf("SKILL_BODY_B appears %d time(s), want exactly 1, got prompt: %q", got, prompt)
	}
}

// TestSessionCarry_CarriedSkillBodyStaysSingleCopyWithCatalogResident is the
// RED test for the cross-Run half of the same fix: a skill loaded in Run 1
// must still be active in Run 2 (a different, single-use Agent sharing the
// same SessionCarry) with its body present exactly ONCE and the skill
// catalog still resident — not just "not duplicated" (already covered,
// within a single Run, by TestSessionCarry_SkillReloadSameSkillDoesNotDuplicateBody
// and, across Runs with a mid-Run reload, by
// TestSessionCarry_CarriedSkillThenSameSkillReloadDoesNotDuplicate) but also
// "still switchable", which the old removeSkillDescriptions-based design
// broke permanently the moment any skill first loaded.
func TestSessionCarry_CarriedSkillBodyStaysSingleCopyWithCatalogResident(t *testing.T) {
	const catalogMarker = "Available skills (use the matching skill when the user request fits):"
	const body = "SKILL_BODY_CARRIED_RESIDENT"

	session := NewSessionCarry()

	// Run 1: loads skill "a".
	reg1 := tools.NewRegistry()
	if err := reg1.Register(models.Tool{
		Name: "skill",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				Content: "loaded",
				Data:    map[string]any{"system_prompt": body, "skill_name": "a"},
			}, nil
		},
	}); err != nil {
		t.Fatalf("register skill tool (run 1): %v", err)
	}
	p1 := &multiTurnCaptureProvider{toolTurns: 1, toolName: "skill"}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: reg1, SystemPrompt: "BASE", Session: session})
	a1.AppendSystemPrompt(catalogMarker + "\n- a: does a\n- b: does b")
	if _, err := a1.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Run 2: fresh Agent, same session, same REPL-mirroring construction
	// order, NO mid-Run tool calls — this exercises ONLY the Run-start
	// carried reapply (react.go's Run()), not the mid-Run skill-result path.
	reg2 := tools.NewRegistry()
	p2 := &multiTurnCaptureProvider{toolTurns: 0}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: reg2, SystemPrompt: "BASE", Session: session})
	a2.AppendSystemPrompt(catalogMarker + "\n- a: does a\n- b: does b")
	if _, err := a2.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "go again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	prompt := a2.BuildSystemPrompt()
	if got := strings.Count(prompt, body); got != 1 {
		t.Fatalf("carried skill body appears %d time(s) in Run 2, want exactly 1, got prompt: %q", got, prompt)
	}
	if !strings.Contains(prompt, catalogMarker) {
		t.Fatalf("Run 2: skill catalog missing — it must stay resident across a carried skill so the "+
			"model can switch to a different skill in this Run too, got: %q", prompt)
	}
	if got := a2.ActiveSkill(); got != "a" {
		t.Fatalf("Run 2 ActiveSkill() = %q, want %q", got, "a")
	}
}

// --- review r1 F3: SessionCarry's single-goroutine access contract --------
//
// probeSlowToolProvider/probeQuickToolProvider + a slow, ctx-ignoring tool
// handler mirror the reviewer's own probe shape: two Agents, run via raw
// goroutines joined by a WaitGroup (no channel-mediated event/outcome
// plumbing in between, unlike pkg/chat's runTurn — see the doc comment on
// TestSessionCarry_DetachedCarriesDoNotRaceConcurrently for why that
// plumbing matters), one of which is still busy in a slow tool call while
// the other runs concurrently.

type probeSlowToolProvider struct{ calls int }

func (p *probeSlowToolProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *probeSlowToolProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "s1", Name: "slow_probe", Arguments: map[string]any{}}},
				Stop:      "tool_calls",
				Done:      true,
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

type probeQuickToolProvider struct{ calls int }

func (p *probeQuickToolProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *probeQuickToolProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "q1", Name: "quick_probe", Arguments: map[string]any{}}},
				Stop:      "tool_calls",
				Done:      true,
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

func registerProbeRaceTools(t *testing.T, reg *tools.Registry) {
	t.Helper()
	if err := reg.Register(models.Tool{
		Name: "slow_probe",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			// Ignores ctx on purpose, mirroring a stuck/misbehaving tool.
			time.Sleep(20 * time.Millisecond)
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted}, nil
		},
	}); err != nil {
		t.Fatalf("register slow_probe: %v", err)
	}
	if err := reg.Register(models.Tool{
		Name: "quick_probe",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted}, nil
		},
	}); err != nil {
		t.Fatalf("register quick_probe: %v", err)
	}
}

// TestSessionCarry_SharedAcrossConcurrentRunsRaces is a demonstration, NOT a
// guard: it proves the hazard review r1 F3 identified is real by
// deliberately violating SessionCarry's single-goroutine access contract —
// ONE SessionCarry handed to TWO Agents whose Run() calls execute
// concurrently on raw goroutines (exactly what would happen if repl.go's
// orphan path did NOT detach). This is EXPECTED to fail under `go test
// -race` (that is the point), so it is SKIPPED by default — run it
// explicitly with `go test -race ./pkg/agent/ -run
// TestSessionCarry_SharedAcrossConcurrentRunsRaces -v` to see the exact
// warning the review's own probe reported. It must never run as part of the
// normal suite (it would make the -race gate fail on correct, expected
// behavior — SessionCarry documents that this usage is unsupported, not
// that it's race-free).
func TestSessionCarry_SharedAcrossConcurrentRunsRaces(t *testing.T) {
	t.Skip("demonstration only — deliberately violates SessionCarry's single-goroutine contract; " +
		"see TestSessionCarry_DetachedCarriesDoNotRaceConcurrently for the guard that must actually pass")

	reg := tools.NewRegistry()
	registerProbeRaceTools(t, reg)

	session := NewSessionCarry()
	a1 := New(AgentConfig{LLMProvider: &probeSlowToolProvider{}, Tools: reg, Session: session})
	a2 := New(AgentConfig{LLMProvider: &probeQuickToolProvider{}, Tools: reg, Session: session})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = a1.Run(context.Background(), "s1", []models.Message{{Role: models.RoleHuman, Content: "go"}})
	}()
	go func() {
		defer wg.Done()
		_, _ = a2.Run(context.Background(), "s2", []models.Message{{Role: models.RoleHuman, Content: "go"}})
	}()
	wg.Wait()
}

// TestSessionCarry_DetachedCarriesDoNotRaceConcurrently is the review r1 F3
// guard that MUST pass under `go test -race`: it mirrors
// TestSessionCarry_SharedAcrossConcurrentRunsRaces's exact shape (same
// providers, same slow/quick tool handlers, same raw-goroutine +
// WaitGroup concurrency — no channel-mediated event/outcome plumbing to
// obscure the result, unlike pkg/chat's runTurn) but gives the two Agents
// TWO SEPARATE SessionCarry instances instead of one shared one — exactly
// what pkg/chat/repl.go's orphan-path fix (runTurn's `case
// <-time.After(r.orphanWaitOrDefault())` branch) produces: a fresh carry for
// the next turn instead of reusing the one a still-live, abandoned Run
// goroutine might still be mutating. Distinct instances can never
// conflict, so this is expected to be race-free — the value of this test
// (paired with the sibling above) is documenting, executably, that the
// FIX's mechanism (detach = never share) is what actually eliminates the
// hazard, not some other property of the code.
func TestSessionCarry_DetachedCarriesDoNotRaceConcurrently(t *testing.T) {
	reg := tools.NewRegistry()
	registerProbeRaceTools(t, reg)

	// Two SEPARATE sessions — this is the only difference from the skipped
	// sibling test above.
	session1 := NewSessionCarry()
	session2 := NewSessionCarry()
	a1 := New(AgentConfig{LLMProvider: &probeSlowToolProvider{}, Tools: reg, Session: session1})
	a2 := New(AgentConfig{LLMProvider: &probeQuickToolProvider{}, Tools: reg, Session: session2})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = a1.Run(context.Background(), "s1", []models.Message{{Role: models.RoleHuman, Content: "go"}})
	}()
	go func() {
		defer wg.Done()
		_, _ = a2.Run(context.Background(), "s2", []models.Message{{Role: models.RoleHuman, Content: "go"}})
	}()
	wg.Wait()
}
