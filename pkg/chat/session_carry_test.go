package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/tools"
)

// --- M4-3: REPL/Agent lifecycle alignment (task-23-brief.md) ---------------
//
// The REPL builds a fresh, single-use Agent every turn but must carry
// certain Agent state (the circuit breaker, active skill, compaction
// anchors — see agent.SessionCarry) across that per-turn churn via the
// ChatRepl.carry field. This file's eight tests guard, at the pkg/chat
// level, everything that touches that carry and the plan-mode readback it
// sits alongside:
//
//   - plan-mode propagates back to the REPL in BOTH directions (entry via
//     enter_plan_mode mid-turn, exit via exit_plan_mode) and survives an
//     errored/interrupted turn (TestRunTurn_PlanModeEnteredMidTurnPropagatesToREPL,
//     TestRunTurn_PlanModeExitedMidTurnPropagatesToREPL,
//     TestRunTurn_PlanModeReadbackSurvivesTurnError);
//   - every history-resetting command (/clear, /new, /undo) also resets the
//     carried cross-turn state, not just the message history
//     (TestClearSession_ResetsCarriedSessionState,
//     TestStartNewSession_ResetsCarriedSessionState,
//     TestUndoLastTurn_ResetsCarriedSessionState);
//   - the carry is actually wired into every turn's AgentConfig, proven
//     through the real runTurn path rather than a hand-built AgentConfig
//     (TestRunTurn_BreakerCarriesAcrossRealTurns);
//   - the REPL's orphan path (a Run goroutine that doesn't return within
//     the wait) detaches to a fresh carry instead of handing the same one
//     to a still-live Agent (TestOrphanedTurn_DoesNotRaceWithNextTurn).

// newSessionCarryTestRepl builds a minimal ChatRepl wired to actually run
// runTurn(): a real SQLite-backed session store (newTestStore, from
// session_test.go), an isolated sandbox and plan-file WorkDir (both under
// t.TempDir(), so the test never touches the real repo working directory),
// and the given provider injected as the "default" model alias (matching
// llm.NewSingleModelRegistry / ModelRegistry.DefaultName()).
func newSessionCarryTestRepl(t *testing.T, provider llm.LLMProvider) (*ChatRepl, *mockUI) {
	t.Helper()

	store, cleanup := newTestStore(t)
	t.Cleanup(cleanup)

	sess, err := store.Create(models.CreateOpts{Model: "test", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sb, err := sandbox.NewSession(t.TempDir(), sandbox.Config{})
	if err != nil {
		t.Fatalf("sandbox.NewSession: %v", err)
	}
	t.Cleanup(func() { sb.Close() })

	regModel := newMockModelRegistry(provider)
	ui := &mockUI{}

	r := &ChatRepl{
		cfg: ReplConfig{
			ModelRegistry: regModel,
			ToolRegistry:  tools.NewRegistry(),
			MaxTurns:      10,
			WorkDir:       t.TempDir(),
		},
		ui:           ui,
		sess:         sess,
		sessMgr:      store,
		sb:           sb,
		currentModel: regModel.DefaultName(),
		carry:        agent.NewSessionCarry(),
	}
	return r, ui
}

// planModeMidTurnProvider makes the FIRST Stream() call emit an
// enter_plan_mode tool call, then ends the turn on the next call with a
// plain text reply (no tool calls) — modeling a user turn where the agent
// decides mid-turn that the task needs planning first.
type planModeMidTurnProvider struct{ calls int }

func (p *planModeMidTurnProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *planModeMidTurnProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "enter_plan_mode", Arguments: map[string]any{"reason": "needs a plan"}}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "planning now"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestRunTurn_PlanModeEnteredMidTurnPropagatesToREPL is the RED test for the
// plan-mode ENTRY asymmetry (task-23-brief.md item 4): repl.go only ever
// clears r.planMode when the agent EXITS plan mode
// (`if r.planMode && !runAgent.IsPlanMode() { r.planMode = false }`) — a
// mid-turn ENTRY (enter_plan_mode tool call) is never read back, so the next
// turn starts with r.planMode still false and the agent gets full tool
// access instead of staying restricted until the user approves the plan.
func TestRunTurn_PlanModeEnteredMidTurnPropagatesToREPL(t *testing.T) {
	r, ui := newSessionCarryTestRepl(t, &planModeMidTurnProvider{})

	if r.planMode {
		t.Fatal("test precondition: planMode should start false")
	}
	if ui.statusPlan {
		t.Fatal("test precondition: mockUI's recorded status should start false (SetStatus not yet called)")
	}

	if err := r.runTurn(context.Background(), "please plan this out", nil, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if !r.planMode {
		t.Fatal("r.planMode = false after a turn where the agent called enter_plan_mode mid-turn, want true " +
			"(the REPL must read IsPlanMode() back symmetrically in both directions)")
	}
	// M4 final-phase review F-M4-4: every other write to r.planMode pairs
	// it with r.ui.SetStatus(r.currentModel, r.planMode) so the TUI footer
	// stays in sync (repl.go's Run() startup, /plan, /run, model switch);
	// the symmetric mid-turn readback above must do the same, or the
	// footer keeps showing full tool access while the agent has actually
	// restricted itself to read-only tools.
	if !ui.statusPlan {
		t.Fatal("mockUI did not record planMode=true via SetStatus after a mid-turn enter_plan_mode — " +
			"the status footer would still show non-plan-mode (review M4-final F-M4-4)")
	}
	if ui.statusModel != r.currentModel {
		t.Fatalf("mockUI recorded SetStatus model = %q, want %q", ui.statusModel, r.currentModel)
	}
}

// TestClearSession_ResetsCarriedSessionState is the RED test for the /clear
// carriage requirement (task-23-brief.md item 4): clearSession wipes the
// session's message history, but the carried cross-turn Agent state (the
// circuit breaker, active skill, compaction anchors) must be wiped alongside
// it, or a loop/skill/anchor from before the clear silently survives into
// the "fresh" conversation. SessionCarry's fields are unexported (by design —
// see its doc comment), so this asserts the externally-observable contract:
// clearSession must install a genuinely NEW SessionCarry (identity change),
// not just leave the old one in place.
func TestClearSession_ResetsCarriedSessionState(t *testing.T) {
	r, _ := newSessionCarryTestRepl(t, &planModeMidTurnProvider{})

	before := r.carry
	if before == nil {
		t.Fatal("test precondition: r.carry must be non-nil before /clear")
	}

	r.clearSession()

	if r.carry == nil {
		t.Fatal("r.carry is nil after clearSession — /clear must leave a fresh, usable SessionCarry in place")
	}
	if r.carry == before {
		t.Fatal("r.carry is unchanged after clearSession — carried cross-turn state (breaker/skill/anchors) " +
			"survives the /clear that wipes the rest of the session")
	}
}

// TestStartNewSession_ResetsCarriedSessionState is the RED test for review
// r1 F1: /new (startNewSession) resets r.sess and r.turn for the new
// conversation but, before this fix, never touched r.carry — so the
// PREVIOUS conversation's active skill, breaker counters, and compaction
// anchor silently carried into a session with a different ID and empty
// history. Same pointer-identity assertion shape as
// TestClearSession_ResetsCarriedSessionState, since SessionCarry's fields
// are intentionally unexported.
func TestStartNewSession_ResetsCarriedSessionState(t *testing.T) {
	r, _ := newSessionCarryTestRepl(t, &planModeMidTurnProvider{})

	before := r.carry
	if before == nil {
		t.Fatal("test precondition: r.carry must be non-nil before /new")
	}

	r.startNewSession()

	if r.carry == nil {
		t.Fatal("r.carry is nil after startNewSession — /new must leave a fresh, usable SessionCarry in place")
	}
	if r.carry == before {
		t.Fatal("r.carry is unchanged after startNewSession — the previous conversation's carried cross-turn " +
			"state (breaker/skill/anchors) survives into the new session (review r1 F1)")
	}
}

// TestUndoLastTurn_ResetsCarriedSessionState is the RED test for review r1
// F8: undoLastTurn shrinks r.sess.Messages (via DeleteLastUserTurn) but,
// before this fix, left r.carry untouched — so a compaction anchor
// (lastInputTokens/lastTokenCountMsgs) referring to a message count that no
// longer exists, or a skill/breaker state built up during the undone turn,
// silently survived the undo. Same pointer-identity assertion shape as the
// /clear and /new tests. A human message must be persisted first so
// DeleteLastUserTurn actually removes something (removed == 0 is a no-op
// early return in undoLastTurn that must not reset the carry — there is
// nothing to invalidate).
func TestUndoLastTurn_ResetsCarriedSessionState(t *testing.T) {
	r, _ := newSessionCarryTestRepl(t, &planModeMidTurnProvider{})

	if err := r.sessMgr.AppendMessage(r.sess.ID, models.Message{
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   "hello",
	}); err != nil {
		t.Fatalf("test setup: append human message: %v", err)
	}

	before := r.carry
	if before == nil {
		t.Fatal("test precondition: r.carry must be non-nil before /undo")
	}

	r.undoLastTurn()

	if r.carry == nil {
		t.Fatal("r.carry is nil after undoLastTurn — /undo must leave a fresh, usable SessionCarry in place")
	}
	if r.carry == before {
		t.Fatal("r.carry is unchanged after undoLastTurn — a compaction anchor pointing at now-removed " +
			"messages (or skill/breaker state from the undone turn) survives the undo (review r1 F8)")
	}
}

// exitPlanModeMidTurnProvider makes the FIRST Stream() call emit an
// exit_plan_mode tool call, then ends the turn on the next call with a
// plain text reply — modeling a user turn where the agent, already in plan
// mode, submits its plan and the user approves it.
type exitPlanModeMidTurnProvider struct{ calls int }

func (p *exitPlanModeMidTurnProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *exitPlanModeMidTurnProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "exit_plan_mode", Arguments: map[string]any{"plan": "do the thing"}}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "proceeding"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// TestRunTurn_PlanModeExitedMidTurnPropagatesToREPL is the REPL-level EXIT-
// direction counterpart to TestRunTurn_PlanModeEnteredMidTurnPropagatesToREPL
// (review r1 F6/item 6): the brief's item 5 said to "check the existing
// readback site and its test" for the exit direction, but no pkg/chat test
// exercised it — so a regression that kept ENTRY propagation working while
// silently dropping EXIT propagation (e.g. rewriting the unconditional
// assignment as `r.planMode = r.planMode || runAgent.IsPlanMode()`) would
// have gone unnoticed. r.ui here must be the mockUI returned by the test
// helper (not `_`): exit_plan_mode asks the user to confirm via
// UserInteraction.AskQuestion, and the REPL wires r.ui as that
// UserInteraction (agentCfg.UserInteraction: r.ui) — the mock must answer
// "Yes, proceed" or the agent stays in plan mode awaiting revision.
func TestRunTurn_PlanModeExitedMidTurnPropagatesToREPL(t *testing.T) {
	r, ui := newSessionCarryTestRepl(t, &exitPlanModeMidTurnProvider{})
	ui.askResult = "Yes, proceed"
	r.planMode = true

	if err := r.runTurn(context.Background(), "looks good, proceed", nil, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if r.planMode {
		t.Fatal("r.planMode = true after a turn where the agent called exit_plan_mode (user approved), want false")
	}
}

// enterPlanModeThenErrorProvider emits an enter_plan_mode tool call on the
// first Stream() call (which fully executes — a.enterPlanMode() runs
// synchronously inside the tool handler during turn 1's processing), then
// fails outright on the SECOND call, simulating a transport error partway
// through a turn that already changed plan-mode state.
type enterPlanModeThenErrorProvider struct{ calls int }

func (p *enterPlanModeThenErrorProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *enterPlanModeThenErrorProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	if p.calls == 1 {
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "enter_plan_mode", Arguments: map[string]any{"reason": "needs a plan"}}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}
	return nil, fmt.Errorf("simulated transport failure")
}

// TestRunTurn_PlanModeReadbackSurvivesTurnError is the RED test for review
// r1 item 6 (second half): the plan-mode readback must happen
// UNCONDITIONALLY, even when the turn itself ends in an error — a turn that
// called enter_plan_mode and then hit a transport failure must still leave
// r.planMode == true for the next turn (an errored/interrupted turn is not
// a signal that plan mode should be forgotten). Placing the readback AFTER
// the `if turnErr != nil { return turnErr }` early-return (the ordering
// before this fix) makes this RED.
func TestRunTurn_PlanModeReadbackSurvivesTurnError(t *testing.T) {
	r, _ := newSessionCarryTestRepl(t, &enterPlanModeThenErrorProvider{})

	if r.planMode {
		t.Fatal("test precondition: planMode should start false")
	}

	err := r.runTurn(context.Background(), "please plan this out", nil, false)
	if err == nil {
		t.Fatal("test setup: expected runTurn to return an error from the simulated transport failure")
	}

	if !r.planMode {
		t.Fatal("r.planMode = false after an ERRORED turn where the agent had already called enter_plan_mode, " +
			"want true — the plan-mode readback must run unconditionally, before the turnErr early-return")
	}
}

// repeatFailProviderChat mirrors pkg/agent's repeatFailProvider fixture
// (kept as a separate, package-local type since pkg/chat cannot reach that
// unexported pkg/agent test helper): emits one failing, fixed-argument tool
// call per turn for up to maxCalls turns, then ends the turn cleanly.
type repeatFailProviderChat struct {
	callCount int
	maxCalls  int
}

func (p *repeatFailProviderChat) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *repeatFailProviderChat) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > p.maxCalls {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	call := models.ToolCall{ID: fmt.Sprintf("f-%d", p.callCount), Name: "cfail", Arguments: map[string]any{"x": "y"}}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: []models.ToolCall{call}, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestRunTurn_BreakerCarriesAcrossRealTurns is the RED test for review r1
// F4: every other M4-3 carriage assertion drives a hand-built
// *agent.SessionCarry directly against pkg/agent's New()/Run() — none of
// them would notice if pkg/chat's runTurn simply stopped threading
// `Session: r.carry` into agentCfg (repl.go's one-line wiring of the whole
// feature). This test goes through the REAL runTurn twice (not a hand-built
// AgentConfig): two consecutive turns, 4 identical failing "cfail" tool
// calls each (maxRepeatFails=8), must trip the repeat-loop breaker on turn
// 2 — exactly like pkg/agent's TestSessionCarry_BreakerTripsAcrossRuns, but
// exercised through the actual REPL call path end to end.
func TestRunTurn_BreakerCarriesAcrossRealTurns(t *testing.T) {
	r, _ := newSessionCarryTestRepl(t, &repeatFailProviderChat{maxCalls: 4})
	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name: "cfail",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusFailed, Error: "boom"}, nil
		},
	}); err != nil {
		t.Fatalf("register cfail tool: %v", err)
	}

	if err := r.runTurn(context.Background(), "turn one", nil, false); err != nil {
		t.Fatalf("turn 1: unexpected error (4 failures alone must not trip the breaker): %v", err)
	}

	// Swap in a fresh provider instance for turn 2 (callCount is per-
	// instance) — same failing tool, same arguments, so the breaker's
	// repeat-key still matches across the two turns if (and only if)
	// AgentConfig.Session was actually threaded through.
	r.cfg.ModelRegistry = newMockModelRegistry(&repeatFailProviderChat{maxCalls: 4})

	err := r.runTurn(context.Background(), "turn two", nil, false)
	if err == nil {
		t.Fatal("turn 2: expected the repeat-loop breaker to trip using the count carried from turn 1 (4+4=8) " +
			"through the REAL REPL turn path — deleting `Session: r.carry` from runTurn's agentCfg would leave this green")
	}
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("turn 2 error = %v, want a repeat-loop error", err)
	}
}

// oneSlowToolProvider emits exactly one tool call (fixed name/args) on its
// first Stream() call, then (defensively, in case it is ever called again)
// ends with plain text. The slowness that keeps the orphaned Run alive past
// ctx cancellation lives in the TOOL HANDLER (see slowRaceTool below), not
// here — react.go calls a.runOneTool with no ctx.Err() check beforehand, so
// a handler that ignores ctx and blocks is exactly the "a tool ignoring ctx
// could delay Run's return" scenario the orphan path exists for.
type oneSlowToolProvider struct{ calls int }

func (p *oneSlowToolProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *oneSlowToolProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "slow1", Name: "slow_race", Arguments: map[string]any{}}},
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

// quickTurnProvider emits exactly one ordinary tool call, then ends.
type quickTurnProvider struct{ calls int }

func (p *quickTurnProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *quickTurnProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if p.calls == 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "c2", Name: "noop_race", Arguments: map[string]any{}}},
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

// TestOrphanedTurn_DoesNotRaceWithNextTurn is the functional regression test
// for review r1 F3's fix: runTurn's orphan path (ctx cancelled, the Run
// goroutine hasn't returned within the wait — here shortened via the
// test-only r.orphanWait seam, see orphanWaitOrDefault) must DETACH
// r.carry — install a fresh *agent.SessionCarry — rather than handing the
// SAME instance to both the still-live orphaned Agent's Run goroutine and
// the next turn's fresh Agent. This drives the REAL runTurn code path (a
// slow, ctx-ignoring tool handler on turn 1 to force a genuine orphan, then
// a second, ordinary turn) and asserts the identity change deterministically
// — reverting the fix (see TestSessionCarry_DetachedCarriesDoNotRaceConcurrently's
// doc comment in pkg/agent for the full story) makes this fail immediately
// at the `r.carry == before` check, every time, with no timing sensitivity.
//
// This test does NOT itself attempt to trigger a `go test -race` failure by
// reverting the fix: runTurn's own event/outcome channel plumbing turned out
// to establish incidental happens-before edges between the orphaned Agent's
// early state (including its breaker's very creation) and the second turn's
// Agent, even when the carry is (wrongly) shared — so a race here would only
// show up for specific, hard-to-pin-down interleavings, not reliably. The
// actual "must pass under -race, and provably WOULD fail if carries were
// shared" evidence lives in pkg/agent/session_carry_test.go's
// TestSessionCarry_DetachedCarriesDoNotRaceConcurrently, which reproduces the
// hazard with raw goroutines (no channel-mediated ordering in the way) —
// mirroring the reviewer's own probe shape directly, at the level where the
// race is unambiguous. Together the two tests cover both properties: this
// one proves repl.go's orphan branch actually performs the detach; that one
// proves detaching two carries (instead of sharing one) is what makes
// concurrent Agents race-free.
func TestOrphanedTurn_DoesNotRaceWithNextTurn(t *testing.T) {
	toolDone := make(chan struct{})
	r, _ := newSessionCarryTestRepl(t, &oneSlowToolProvider{})
	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name: "slow_race",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			defer close(toolDone)
			// Ignores ctx on purpose — this models a tool call that keeps
			// running regardless of cancellation (network call, subprocess,
			// etc.), which is the exact scenario the orphan branch exists
			// to survive.
			time.Sleep(200 * time.Millisecond)
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("register slow_race tool: %v", err)
	}
	if err := r.cfg.ToolRegistry.Register(models.Tool{
		Name: "noop_race",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("register noop_race tool: %v", err)
	}
	r.orphanWait = 10 * time.Millisecond

	// A plain timeout — NOT gated on any signal from the tool handler — so
	// there is no happens-before edge between the handler's eventual write
	// and anything that follows here. Generous relative to the 200ms tool
	// sleep, leaving headroom for goroutine-scheduling latency under -race
	// (which is otherwise easy to underestimate: if ctx were already
	// expired by the time the Run goroutine is actually scheduled, react.go
	// would bail at its very first ctx.Err() check, WITHOUT ever reaching
	// the slow tool call — collapsing this into an ordinary fast-returning
	// turn instead of a genuine orphan).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	before := r.carry
	if err := r.runTurn(ctx, "go", nil, false); err == nil {
		t.Fatal("expected runTurn to return an error (ctx cancelled, Run orphaned past the wait)")
	}
	if r.carry == before {
		t.Fatal("test setup: orphan path did not detach r.carry — the race this test exercises would be a real " +
			"bug, not a regression guard")
	}

	// Drive a SECOND, ordinary turn using the (now fresh) r.carry — the
	// orphaned Run goroutine from above is still alive, asleep inside
	// slow_race's handler for another ~150ms. go test -race must find
	// nothing, because the two Agents now use different SessionCarry
	// instances.
	r.cfg.ModelRegistry = newMockModelRegistry(&quickTurnProvider{})
	if err := r.runTurn(context.Background(), "go again", nil, false); err != nil {
		t.Fatalf("second runTurn: %v", err)
	}

	// Let the orphan's handler finish (and perform its eventual, genuinely
	// unsynchronized write) before the test — and the process, if this is
	// the last test — exits, so the race detector actually observes it.
	select {
	case <-toolDone:
	case <-time.After(2 * time.Second):
		t.Fatal("test setup: orphaned slow_race handler never completed")
	}
}
