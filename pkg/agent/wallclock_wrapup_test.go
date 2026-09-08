package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// ---------------------------------------------------------------------------
// wrapUpReserve: the adaptive-reserve formula itself, in isolation.
// ---------------------------------------------------------------------------

// TestWrapUpReserve_NoObservationsUsesFloorCappedAtHalfBudget pins the "first
// turn, no data yet" behavior (item B in the M6 brief): with no observed turn
// durations the reserve falls back to the agent's floor, but never exceeds
// half of the total wall-clock budget — otherwise a short-timeout run would
// spend most of its budget "reserved" before doing any real work.
func TestWrapUpReserve_NoObservationsUsesFloorCappedAtHalfBudget(t *testing.T) {
	a := &Agent{wrapUpReserveFloor: 200 * time.Millisecond}

	// Plenty of total budget: floor wins outright.
	if got, want := a.wrapUpReserve(nil, 10*time.Second), 200*time.Millisecond; got != want {
		t.Fatalf("reserve = %v, want %v (the floor, budget is generous)", got, want)
	}

	// Tight total budget: the half-budget cap must win over the floor.
	if got, want := a.wrapUpReserve(nil, 300*time.Millisecond), 150*time.Millisecond; got != want {
		t.Fatalf("reserve = %v, want %v (capped to half of a 300ms budget)", got, want)
	}
}

// TestWrapUpReserve_AdaptsToObservedTurnDuration is the core "no one-size-
// fits-all reserve" property the brief demands: a run whose OWN observed
// turns are slow gets a bigger reserve than one whose turns are fast, scaled
// by the 1.2x headroom multiplier, still capped at half the total budget.
func TestWrapUpReserve_AdaptsToObservedTurnDuration(t *testing.T) {
	a := &Agent{wrapUpReserveFloor: 10 * time.Millisecond}

	fast := a.wrapUpReserve([]time.Duration{20 * time.Millisecond, 22 * time.Millisecond}, time.Hour)
	slow := a.wrapUpReserve([]time.Duration{20 * time.Millisecond, 400 * time.Millisecond}, time.Hour)

	if slow <= fast {
		t.Fatalf("slow-turn reserve (%v) must exceed fast-turn reserve (%v)", slow, fast)
	}
	// 400ms * 1.2 = 480ms, from the MAX observed turn, not an average.
	if want := 480 * time.Millisecond; slow != want {
		t.Fatalf("slow reserve = %v, want %v (1.2x the max observed turn)", slow, want)
	}

	// Same slow observation, but now the total budget is tight: the cap
	// must win over the multiplier result.
	capped := a.wrapUpReserve([]time.Duration{400 * time.Millisecond}, 200*time.Millisecond)
	if want := 100 * time.Millisecond; capped != want {
		t.Fatalf("capped reserve = %v, want %v (half of a 200ms budget, less than 480ms)", capped, want)
	}
}

// TestWrapUpReserve_DecaysAfterJitterPasses is the RED test for F3: a single
// early turn that jitters (a 429 retry storm, one slow tool call) must not
// pin the reserve at the half-budget cap for the rest of an arbitrarily long
// run. Before the fix, wrapUpReserve derives the reserve from the MAX of the
// run's ENTIRE turnDurations history, so one bad turn early on stays the
// all-time max (and thus the reserve) forever, even once every later turn is
// back to a normal pace — burning up to half the run's total budget on a
// wrap-up reserve that no longer reflects how the run is actually going.
//
// No real sleeps: turnDurations are synthetic (exactly what react.go's loop
// would have appended, had these turns really taken this long), so this is
// deterministic and immune to slow-machine flakiness.
func TestWrapUpReserve_DecaysAfterJitterPasses(t *testing.T) {
	a := &Agent{wrapUpReserveFloor: 1 * time.Millisecond}
	const totalBudget = 600 * time.Millisecond // cap = 300ms
	const jitter = 250 * time.Millisecond
	const normal = 20 * time.Millisecond

	var history []time.Duration
	history = append(history, jitter)
	if got, want := a.wrapUpReserve(history, totalBudget), 300*time.Millisecond; got != want {
		t.Fatalf("reserve right after the jitter = %v, want %v (pinned to the half-budget cap)", got, want)
	}

	for i := 0; i < 5; i++ {
		history = append(history, normal)
	}
	// After enough normal turns for the one-off jitter to age out of the
	// lookback window, the reserve must fall back to what a run that never
	// jittered at all would have: 1.2x a 20ms turn = 24ms.
	const want = 24 * time.Millisecond
	if got := a.wrapUpReserve(history, totalBudget); got != want {
		t.Fatalf("reserve after 5 normal turns following one jitter = %v, want %v (must decay)", got, want)
	}

	// Control: six 20ms turns with NO jitter at all must land on the exact
	// same reserve — proves the decayed value isn't a coincidence of this
	// particular history shape, it's the true steady-state answer.
	var control []time.Duration
	for i := 0; i < 6; i++ {
		control = append(control, normal)
	}
	if got := a.wrapUpReserve(control, totalBudget); got != want {
		t.Fatalf("control (no jitter) reserve = %v, want %v to match the decayed value", got, want)
	}
}

// TestWrapUpReserve_SustainedSlowPaceStillReserved is the counterpart
// sanity check: the decaying window must not throw away a genuinely
// SUSTAINED slow pace — only a one-off spike that has actually passed.
func TestWrapUpReserve_SustainedSlowPaceStillReserved(t *testing.T) {
	a := &Agent{wrapUpReserveFloor: 1 * time.Millisecond}
	slow := []time.Duration{80 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond}
	got := a.wrapUpReserve(slow, time.Hour)
	want := 96 * time.Millisecond // 1.2 * 80ms
	if got != want {
		t.Fatalf("reserve for a consistently-slow run = %v, want %v — a sustained slow pace must still get its full reserve, not just a recent one-off spike", got, want)
	}
}

// ---------------------------------------------------------------------------
// capCumulativeWrapUpBudget: F4's cumulative cap, in isolation.
// ---------------------------------------------------------------------------

func TestCapCumulativeWrapUpBudget(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name               string
		requestBudget      time.Duration
		cumulativeDeadline time.Time
		want               time.Duration
	}{
		{
			"no cumulative deadline set (non-deadline wrap-up) — unaffected",
			150 * time.Millisecond, time.Time{}, 150 * time.Millisecond,
		},
		{
			"plenty of cumulative allowance left — unaffected",
			150 * time.Millisecond, now.Add(time.Hour), 150 * time.Millisecond,
		},
		{
			"cumulative allowance nearly spent — capped below the request budget",
			150 * time.Millisecond, now.Add(50 * time.Millisecond), 50 * time.Millisecond,
		},
		{
			"cumulative allowance already exhausted — capped to (at most) zero",
			150 * time.Millisecond, now.Add(-10 * time.Millisecond), -10 * time.Millisecond,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := capCumulativeWrapUpBudget(c.requestBudget, c.cumulativeDeadline, now)
			if got != c.want {
				t.Fatalf("capCumulativeWrapUpBudget(%v, deadline, now) = %v, want %v", c.requestBudget, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// F4: the ENTIRE wall-clock wrap-up phase (the initial forced request plus
// however many compaction retries re-enter it) must overshoot ctx's original
// deadline by at most the reserve decided when wrap-up first triggered — not
// a fresh full reserve handed out again to every retry.
// ---------------------------------------------------------------------------

// wrapUpCumulativeCapProvider drives exactly the shape F4 exists for: a tool
// turn slow enough to trigger the wall-clock wrap-up, whose FIRST wrap-up
// request itself spends real time before failing with a context-overflow-
// shaped error (triggering Run's pre-existing compact-and-retry path), and a
// SECOND wrap-up request that reports the ACTUAL ctx deadline it was handed
// (via ctx.Deadline(), read directly inside Stream — no wall-clock racing
// needed: this is a structural read of the budget react.go decided to grant,
// not a timing guess). The second request then succeeds immediately, so
// pass/fail here never depends on winning a real-time race — only the
// CAPTURED budget value does the asserting.
type wrapUpCumulativeCapProvider struct {
	toolTurnSleep    time.Duration
	firstWrapUpSleep time.Duration

	calls              int
	noToolCalls        int
	secondWrapUpBudget time.Duration
	sawSecondDeadline  bool
}

func (p *wrapUpCumulativeCapProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wrapUpCumulativeCapProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	if len(req.Tools) > 0 {
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			time.Sleep(p.toolTurnSleep)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("wc-%d", p.calls), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}

	p.noToolCalls++
	if p.noToolCalls == 1 {
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			time.Sleep(p.firstWrapUpSleep)
			ch <- llm.StreamChunk{Err: errors.New("context window exceeded")}
		}()
		return ch, nil
	}

	// Second (and any later) wrap-up request: record the ACTUAL budget
	// react.go granted it, then answer immediately — no sleep, so this
	// request's success never races real time.
	if dl, ok := ctx.Deadline(); ok {
		p.sawSecondDeadline = true
		p.secondWrapUpBudget = time.Until(dl)
	}
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			Message: &models.Message{Role: models.RoleAI, Content: "second wrap-up answer"},
			Done:    true,
			Stop:    "stop",
		}
	}()
	return ch, nil
}

// TestRun_WallClockWrapUp_CumulativeBudgetCappedAcrossRetries is the RED
// test for F4: parent budget 300ms, a 250ms tool turn triggers the wrap-up
// with a ~150ms reserve (half-budget cap, same setup as the other wrap-up
// tests in this file); the FIRST wrap-up request spends 100ms of that SAME
// 150ms allowance before failing with a context-overflow-shaped error,
// triggering a compaction retry. Before the fix, react.go hands the SECOND
// wrap-up request a FULL FRESH ~150ms budget (buildTurnRequestCtx is only
// ever given the constant wrapUpBudget computed once at trigger time) —
// completely ignoring that the first request already spent 100ms of what is
// supposed to be the SAME reserve. After the fix, the second request's
// budget is capped to whatever remains of the ORIGINAL reserve (~150ms minus
// the ~100ms already spent — comfortably under 100ms, with generous slack
// for scheduling overhead).
func TestRun_WallClockWrapUp_CumulativeBudgetCappedAcrossRetries(t *testing.T) {
	provider := &wrapUpCumulativeCapProvider{
		toolTurnSleep:    250 * time.Millisecond,
		firstWrapUpSleep: 100 * time.Millisecond,
	}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", paddedHistoryWithBigToolResult("s1"))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (the second wrap-up request answers immediately) — result: %+v", err, result)
	}
	if provider.noToolCalls < 2 {
		t.Fatalf("provider saw %d wrap-up request(s), want at least 2 — the compaction retry never happened, so this test never exercised the cumulative cap at all", provider.noToolCalls)
	}
	if !provider.sawSecondDeadline {
		t.Fatal("the second wrap-up request never observed a ctx deadline at all")
	}
	// The core assertion: the second request's budget must be MEANINGFULLY
	// smaller than a fresh ~150ms reserve — capped to what's left of the
	// SAME cumulative allowance the first request already spent 100ms of.
	if provider.secondWrapUpBudget >= 100*time.Millisecond {
		t.Fatalf("second wrap-up request's budget = %v, want it capped well below a fresh ~150ms reserve — the first wrap-up request already spent ~100ms of the SAME cumulative allowance, so the second request must not get a full fresh one", provider.secondWrapUpBudget)
	}
	if provider.secondWrapUpBudget <= 0 {
		t.Fatalf("second wrap-up request's budget = %v, want a small POSITIVE remainder, not zero/negative — it did go on to answer successfully", provider.secondWrapUpBudget)
	}
}

// ---------------------------------------------------------------------------
// shouldTriggerWallClockWrapUp: the gating decision in isolation — this is
// what proves a cancelled OR expired ctx can never trigger a wrap-up,
// without relying on any real-time race.
// ---------------------------------------------------------------------------

func TestShouldTriggerWallClockWrapUp(t *testing.T) {
	future := time.Now().Add(time.Hour)

	cases := []struct {
		name        string
		ctxErr      error
		hasDeadline bool
		remaining   time.Duration
		reserve     time.Duration
		want        bool
	}{
		{"no deadline at all", nil, false, time.Hour, time.Second, false},
		{"deadline far away", nil, true, time.Hour, time.Second, false},
		{"deadline within reserve", nil, true, 500 * time.Millisecond, time.Second, true},
		{"deadline exactly at reserve", nil, true, time.Second, time.Second, true},
		{
			"ctx already cancelled, even though remaining <= reserve",
			context.Canceled, true, 100 * time.Millisecond, time.Second, false,
		},
		{
			"ctx already deadline-exceeded, even though remaining <= reserve",
			context.DeadlineExceeded, true, 0, time.Second, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldTriggerWallClockWrapUp(c.ctxErr, future, c.hasDeadline, c.remaining, c.reserve)
			if got != c.want {
				t.Fatalf("shouldTriggerWallClockWrapUp(%v, hasDeadline=%v, remaining=%v, reserve=%v) = %v, want %v",
					c.ctxErr, c.hasDeadline, c.remaining, c.reserve, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Run() end-to-end: the wall-clock trigger actually fires, offers no tools,
// and preserves the final answer instead of losing it to a cancelled stream.
// ---------------------------------------------------------------------------

// wallClockProvider models a slow-per-turn backend: every request that is
// offered tools sleeps toolTurnSleep then emits one tool call; the one
// request offered NO tools (the forced wrap-up) sleeps wrapUpSleep then
// emits the final answer. Records each request's tool count and arrival
// time so a test can assert on request shape and ordering.
type wallClockProvider struct {
	toolTurnSleep time.Duration
	wrapUpSleep   time.Duration
	calls         int
	requestTools  []int
}

func (p *wallClockProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wallClockProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	p.requestTools = append(p.requestTools, len(req.Tools))
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if len(req.Tools) > 0 {
			time.Sleep(p.toolTurnSleep)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("wc-%d", p.calls), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
			return
		}
		time.Sleep(p.wrapUpSleep)
		ch <- llm.StreamChunk{
			Message: &models.Message{Role: models.RoleAI, Content: "wound down: final answer preserved"},
			Done:    true,
			Stop:    "stop",
		}
	}()
	return ch, nil
}

func newRegistryWithEchoTool(name string) *tools.Registry {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: name,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{ToolName: name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	})
	return reg
}

// TestRun_WallClockDeadline_TriggersGracefulWrapUp is the RED test for the
// whole feature: before M6, a run whose ctx deadline arrived mid-loop was
// simply killed on its NEXT Stream() call (isContextOverflowError check
// fails, normalizeRunError wraps it as a *TimeoutError) with FinalOutput
// empty — every tool call already executed, discarded. After M6, the loop
// notices the deadline is close (using ITS OWN observed turn pace) before
// ever sending that doomed request, and instead sends one final tool-less
// request whose answer becomes FinalOutput.
func TestRun_WallClockDeadline_TriggersGracefulWrapUp(t *testing.T) {
	provider := &wallClockProvider{toolTurnSleep: 120 * time.Millisecond}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	// Shrink the floor so the test doesn't need a multi-second ctx timeout —
	// mirrors how streamIdleTimeout is set directly in tests elsewhere in
	// this package.
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want a graceful wrap-up (nil)", err)
	}
	if result.FinalOutput != "wound down: final answer preserved" {
		t.Fatalf("FinalOutput = %q, want the wrap-up answer — the prior turns' work must not be discarded", result.FinalOutput)
	}
	if !result.BudgetExhausted {
		t.Fatal("BudgetExhausted = false, want true after a wall-clock wrap-up")
	}
	if result.WoundDownReason != WoundDownReasonDeadline {
		t.Fatalf("WoundDownReason = %q, want %q", result.WoundDownReason, WoundDownReasonDeadline)
	}
	if len(provider.requestTools) < 2 {
		t.Fatalf("requests = %d, want at least 2 (a tool turn, then the wrap-up)", len(provider.requestTools))
	}
	if last := provider.requestTools[len(provider.requestTools)-1]; last != 0 {
		t.Fatalf("final request offered %d tools, want 0 (the forced wrap-up)", last)
	}
}

// TestRun_WallClockWrapUp_SurvivesParentDeadline is the RED test for item C:
// the wrap-up request's own context must be detached from ctx's cancellation
// (context.WithoutCancel) so it can actually finish even after the PARENT
// ctx's deadline has fired — bounded instead by its own reserve. Total
// parent budget is 300ms; one tool turn eats 250ms of it (leaving only 50ms,
// well under the ~150ms reserve, so wrap-up triggers); the wrap-up answer
// itself then takes 100ms — more than the 50ms that was left on the PARENT
// deadline, but comfortably inside the ~150ms reserve. Before this fix (or
// with reqCtx wired to ctx directly), the parent deadline firing at 300ms
// would cancel this in-flight request and the run would return a
// *TimeoutError with FinalOutput empty.
func TestRun_WallClockWrapUp_SurvivesParentDeadline(t *testing.T) {
	provider := &wallClockProvider{
		toolTurnSleep: 250 * time.Millisecond,
		wrapUpSleep:   100 * time.Millisecond,
	}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	started := time.Now()
	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run() error = %v, want nil — the wrap-up must survive past the parent deadline", err)
	}
	if result.FinalOutput != "wound down: final answer preserved" {
		t.Fatalf("FinalOutput = %q, want the wrap-up answer", result.FinalOutput)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("elapsed = %v, want > 300ms (the parent deadline) — this run is supposed to legitimately overshoot it", elapsed)
	}
	// Bounded overshoot: must not run drastically longer than the reserve
	// allows (generous slack for scheduler jitter in CI).
	if elapsed > 300*time.Millisecond+500*time.Millisecond {
		t.Fatalf("elapsed = %v, overshoot is not bounded as designed", elapsed)
	}
}

// TestRun_UserCancellation_BeforeRunStarts_NeverTriggersWrapUp: a ctx that is
// ALREADY cancelled when Run is called — even though it also carries a
// future-looking deadline (so ctx.Deadline() reports ok=true, the same shape
// the wall-clock trigger inspects) — must stop immediately without ever
// calling the provider. This is Run's PRE-EXISTING ctx.Err() guard (line
// ~435, unmodified by M6); pinned here because a naive read of the M6 brief
// might try to run the wall-clock check before that guard and accidentally
// race it.
func TestRun_UserCancellation_BeforeRunStarts_NeverTriggersWrapUp(t *testing.T) {
	provider := &wallClockProvider{}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want context.Canceled to propagate immediately")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if result != nil && result.WoundDownReason == WoundDownReasonDeadline {
		t.Fatal("WoundDownReason = deadline — a cancelled run must never enter the wrap-up path")
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 (Run must stop before ever dispatching a request)", provider.calls)
	}
}

// TestRun_UserCancellation_MidRun_NeverTriggersWrapUp is the more realistic
// Ctrl+C shape: the run is already under way (one tool call has executed),
// the user cancels, and the NEXT thing that must happen is immediate
// termination — never a forced extra "give your final answer" request. The
// tool handler cancels ctx itself (standing in for a user keypress landing
// exactly between two turns); ctx also carries a deadline far in the future,
// so a wrap-up bug that checked only "is the deadline near" and ignored
// ctx.Err() entirely would correctly stay quiet here too — the real risk
// this guards is a future refactor that starts checking remaining time
// without first checking ctx.Err().
func TestRun_UserCancellation_MidRun_NeverTriggersWrapUp(t *testing.T) {
	provider := &wallClockProvider{}
	var cancel context.CancelFunc
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "wecho",
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			cancel() // simulate Ctrl+C landing right as this tool result comes back
			return models.ToolResult{ToolName: "wecho", Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	})
	a := New(AgentConfig{LLMProvider: provider, Tools: reg})

	var ctx context.Context
	ctx, cancel = context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want context.Canceled to propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if result != nil && result.WoundDownReason == WoundDownReasonDeadline {
		t.Fatal("WoundDownReason = deadline — cancellation mid-run must never be treated as a wrap-up")
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (turn 0 only — no wrap-up retry after cancellation)", provider.calls)
	}
}

// ---------------------------------------------------------------------------
// F2: the wrap-up notice/error text a deadline-triggered wrap-up produces
// must describe the REAL constraint (time), never the tool-call-budget
// wording — that wording is a lie on the wall-clock path (doubly so when
// a.maxToolCalls is 0, the eval harness's actual configuration, where the
// notice reads "... (0 calls)" and the tool-call trigger below can never
// fire at all, making wall-clock wrap-up the ONLY wrap-up path in practice).
// ---------------------------------------------------------------------------

// wallClockNoticeProvider is wallClockProvider plus the ability to script
// exactly what the WRAP-UP (tools-empty) request itself produces — a normal
// answer, empty content (to hit the "wrap-up produced no output" error), or
// tool calls despite being offered none (to hit the "provider still returned
// tool calls" refusal+error path) — and it records every no-tools request's
// full message list so a test can inspect the injected notice directly.
type wallClockNoticeProvider struct {
	toolTurnSleep  time.Duration
	wrapUpBehavior string // "normal" (default), "empty", "toolcalls"

	calls              int
	lastWrapUpMessages []models.Message
}

func (p *wallClockNoticeProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wallClockNoticeProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	ch := make(chan llm.StreamChunk, 1)
	if len(req.Tools) > 0 {
		go func() {
			defer close(ch)
			time.Sleep(p.toolTurnSleep)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("wc-%d", p.calls), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}

	p.lastWrapUpMessages = append([]models.Message(nil), req.Messages...)
	go func() {
		defer close(ch)
		switch p.wrapUpBehavior {
		case "empty":
			ch <- llm.StreamChunk{Delta: "", Stop: "stop", Done: true}
		case "toolcalls":
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "misbehaving-1", Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		default:
			ch <- llm.StreamChunk{
				Message: &models.Message{Role: models.RoleAI, Content: "wound down: final answer preserved"},
				Done:    true,
				Stop:    "stop",
			}
		}
	}()
	return ch, nil
}

// lastNoticeMessage returns the trailing injected RoleHuman notice from the
// last wrap-up request's message list (the actual prompt VIEW content sent
// to the provider), or fails the test if none is found.
func (p *wallClockNoticeProvider) lastNoticeMessage(t *testing.T) models.Message {
	t.Helper()
	for i := len(p.lastWrapUpMessages) - 1; i >= 0; i-- {
		if p.lastWrapUpMessages[i].Role == models.RoleHuman {
			return p.lastWrapUpMessages[i]
		}
	}
	t.Fatal("no RoleHuman notice message found in the wrap-up request")
	return models.Message{}
}

// TestRun_WallClockWrapUp_NoticeDescribesTimeNotToolBudget is the RED test
// for F2's prompt-notice half: react.go unconditionally injects
// toolBudgetExhaustedNotice on ANY wrap-up, including a wall-clock one, so
// the model is told "You have used your tool call limit for this task (0
// calls)" (a.maxToolCalls is 0 — unlimited — in this test, mirroring the
// eval harness's real configuration for every role) even though the actual
// constraint that forced the wrap-up was time, not tool calls, and no tool
// call limit exists at all.
func TestRun_WallClockWrapUp_NoticeDescribesTimeNotToolBudget(t *testing.T) {
	provider := &wallClockNoticeProvider{toolTurnSleep: 120 * time.Millisecond}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
		// MaxToolCalls left at 0 (unlimited) — the eval harness's real
		// per-role configuration, and exactly what makes the lie sharpest:
		// "(0 calls)" reads as if the budget were already exhausted at zero.
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.WoundDownReason != WoundDownReasonDeadline {
		t.Fatalf("WoundDownReason = %q, want %q — test setup didn't reach the wall-clock path", result.WoundDownReason, WoundDownReasonDeadline)
	}

	notice := provider.lastNoticeMessage(t)
	if strings.Contains(notice.Content, "tool call limit") || strings.Contains(notice.Content, "(0 calls)") {
		t.Fatalf("notice = %q, must NOT use tool-call-budget wording on the wall-clock path", notice.Content)
	}
	if !strings.Contains(strings.ToLower(notice.Content), "time") {
		t.Fatalf("notice = %q, want it to describe the real constraint (time)", notice.Content)
	}
	if !strings.Contains(notice.Content, "Do not attempt any further tool calls") {
		t.Fatalf("notice = %q, want it to still tell the model to stop calling tools", notice.Content)
	}
	const schemaSentence = "if a JSON schema or structured format was required, your final answer MUST still follow it"
	if !strings.Contains(notice.Content, schemaSentence) {
		t.Fatalf("notice = %q, want it to still preserve the schema-compliance sentence from the tool-budget wording", notice.Content)
	}
}

// TestRun_WallClockWrapUp_EmptyOutputErrorMentionsDeadlineNotToolBudget is
// the RED test for F2's "wrap-up produced no output" error message
// (react.go, the len(toolCalls)==0 branch): the tool-budget-worded error
// ("agent exceeded tool call budget (0) and the wrap-up turn produced no
// output") is actively misleading on the wall-clock path — there IS no tool
// call budget (a.maxToolCalls is 0/unlimited here).
func TestRun_WallClockWrapUp_EmptyOutputErrorMentionsDeadlineNotToolBudget(t *testing.T) {
	provider := &wallClockNoticeProvider{toolTurnSleep: 120 * time.Millisecond, wrapUpBehavior: "empty"}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an error (the wrap-up turn produced no output)")
	}
	if strings.Contains(err.Error(), "tool call budget") {
		t.Fatalf("err = %q, must NOT mention a tool call budget on the wall-clock path", err.Error())
	}
	if !strings.Contains(strings.ToLower(err.Error()), "deadline") && !strings.Contains(strings.ToLower(err.Error()), "wall-clock") && !strings.Contains(strings.ToLower(err.Error()), "time") {
		t.Fatalf("err = %q, want it to mention the real constraint (deadline/wall-clock/time)", err.Error())
	}
}

// TestRun_WallClockWrapUp_ProviderStillReturnsToolCalls_RefusalAndErrorMentionDeadline
// is the RED test for F2's third and fourth spots: a misbehaving provider
// that returns tool calls despite being offered none during a wall-clock
// wrap-up gets both a synthesized-refusal reason string and (when there's no
// text alongside the calls) a hard error — both currently hardcode "tool
// call budget (%d) exhausted"/"agent exceeded tool call budget (%d)".
func TestRun_WallClockWrapUp_ProviderStillReturnsToolCalls_RefusalAndErrorMentionDeadline(t *testing.T) {
	provider := &wallClockNoticeProvider{toolTurnSleep: 120 * time.Millisecond, wrapUpBehavior: "toolcalls"}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an error (no text alongside the refused tool calls)")
	}
	if strings.Contains(err.Error(), "tool call budget") {
		t.Fatalf("err = %q, must NOT mention a tool call budget on the wall-clock path", err.Error())
	}

	var refusalErr string
	for _, m := range result.Messages {
		if m.Role == models.RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "misbehaving-1" {
			refusalErr = m.ToolResult.Error
		}
	}
	if refusalErr == "" {
		t.Fatal("no synthesized refusal found for the misbehaving tool call")
	}
	if strings.Contains(refusalErr, "tool call budget") {
		t.Fatalf("refusal reason = %q, must NOT mention a tool call budget on the wall-clock path", refusalErr)
	}
}

// ---------------------------------------------------------------------------
// F1: a provider failure DURING the wrap-up request must surface as itself,
// never be masked into a generic *TimeoutError just because the PARENT ctx
// (whose deadline triggered the wrap-up in the first place) has, by design,
// already expired by the time the wrap-up request's own error is observed.
// ---------------------------------------------------------------------------

// wrapUpProviderFailureProvider models the real repro: a tool turn slow
// enough to push the PARENT ctx's deadline within the reserve (triggering the
// wall-clock wrap-up), followed by a wrap-up request that itself fails —
// either via a chunk carrying Err (a stream that broke mid-generation, e.g. a
// provider 503) or via Stream() itself returning an error (a connection
// failure) — deliberately AFTER sleeping long enough that the parent ctx's
// deadline has for-real elapsed by the time the failure is observed, exactly
// mirroring "wall-clock wrap-up is the common case, not the edge case" from
// the M6 brief.
type wrapUpProviderFailureProvider struct {
	toolTurnSleep    time.Duration
	wrapUpSleep      time.Duration
	failStreamCall   bool // fail Stream() itself instead of via a chunk
	failureMsg       string
	calls            int
	wrapUpCallsCount int
}

func (p *wrapUpProviderFailureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wrapUpProviderFailureProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	if len(req.Tools) > 0 {
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			time.Sleep(p.toolTurnSleep)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("wc-%d", p.calls), Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}

	p.wrapUpCallsCount++
	if p.failStreamCall {
		time.Sleep(p.wrapUpSleep)
		return nil, errors.New(p.failureMsg)
	}
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		time.Sleep(p.wrapUpSleep)
		ch <- llm.StreamChunk{Err: errors.New(p.failureMsg), Done: true}
	}()
	return ch, nil
}

// TestRun_WallClockWrapUp_ChunkErrorSurvivesParentDeadlineExpiry is the RED
// test for F1 (chunk.Err shape): parent ctx budget 300ms, a 250ms tool turn
// triggers the wrap-up (reserve ~150ms), and the wrap-up request's own chunk
// carries a provider error after 80ms — by which point real elapsed time
// (250ms+80ms=330ms) is safely past the parent's 300ms deadline, so
// ctx.Err() == DeadlineExceeded at the moment react.go checks it. Before the
// fix, react.go:750 calls normalizeRunError(ctx, ...) with that PARENT ctx,
// which unconditionally rewrites ANY error into a generic *TimeoutError once
// ctx.Err() is DeadlineExceeded — discarding the real provider failure. After
// the fix (using reqCtx, the wrap-up request's OWN detached-but-self-bounded
// ctx, which is not yet passed its own wrapUpBudget at the 80ms mark), the
// real error must survive.
func TestRun_WallClockWrapUp_ChunkErrorSurvivesParentDeadlineExpiry(t *testing.T) {
	provider := &wrapUpProviderFailureProvider{
		toolTurnSleep: 250 * time.Millisecond,
		wrapUpSleep:   80 * time.Millisecond,
		failureMsg:    "provider 503: stream broke mid-generation",
	}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatalf("Run() error = nil, want the provider failure to surface — result: %+v", result)
	}
	if !strings.Contains(err.Error(), "provider 503") {
		t.Fatalf("Run() error = %q, want it to contain the real provider failure (\"provider 503: ...\"), not a generic masked timeout", err.Error())
	}
	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) && timeoutErr.Message == "agent request timed out" {
		t.Fatalf("Run() error = %v, is the generic masked *TimeoutError — the real provider failure during wrap-up must never be discarded just because the PARENT ctx's deadline (the reason wrap-up triggered) has since elapsed", err)
	}
	if provider.wrapUpCallsCount < 1 {
		t.Fatal("provider never received a wrap-up (tool-less) request — test setup didn't exercise the wrap-up path at all")
	}
}

// TestRun_WallClockWrapUp_StreamCallErrorSurvivesParentDeadlineExpiry is the
// RED test for F1's OTHER call site (react.go:727, the Stream() call itself
// returning an error rather than a mid-stream chunk — e.g. a connection
// failure establishing the wrap-up request).
func TestRun_WallClockWrapUp_StreamCallErrorSurvivesParentDeadlineExpiry(t *testing.T) {
	provider := &wrapUpProviderFailureProvider{
		toolTurnSleep:  250 * time.Millisecond,
		wrapUpSleep:    80 * time.Millisecond,
		failStreamCall: true,
		failureMsg:     "dial tcp: connection refused",
	}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	a.wrapUpReserveFloor = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatalf("Run() error = nil, want the connection failure to surface — result: %+v", result)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Run() error = %q, want it to contain the real connection failure, not a generic masked timeout", err.Error())
	}
	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) && timeoutErr.Message == "agent request timed out" {
		t.Fatalf("Run() error = %v, is the generic masked *TimeoutError — the real connection failure establishing the wrap-up request must never be discarded just because the PARENT ctx's deadline has since elapsed", err)
	}
	if provider.wrapUpCallsCount < 1 {
		t.Fatal("provider never received a wrap-up (tool-less) request — test setup didn't exercise the wrap-up path at all")
	}
}

// ---------------------------------------------------------------------------
// buildTurnRequestCtx: the guard itself, in isolation — this is what proves
// a user Ctrl+C landing right before a deadline-triggered wrap-up request is
// built can never be masked by context.WithoutCancel. Deleting
// "&& !errors.Is(ctx.Err(), context.Canceled)" from buildTurnRequestCtx makes
// go vet/build still pass and every OTHER existing test in this package still
// pass (the two cancellation Run() tests never get wrapUp to true — see
// their own doc comments) — these direct tests are the only thing that
// catches that mutation.
// ---------------------------------------------------------------------------

// TestBuildTurnRequestCtx_NonDeadlineWrapUp_PlainCancellableChild covers both
// shapes that must NOT get the detached treatment: not wrapping up at all,
// and wrapping up for the (pre-existing) tool-call budget rather than the
// wall-clock deadline. Both must stay a real, cancellable child of ctx —
// unbounded by wrapUpBudget and immediately cancelled alongside the parent.
func TestBuildTurnRequestCtx_NonDeadlineWrapUp_PlainCancellableChild(t *testing.T) {
	a := &Agent{}
	cases := []struct {
		name   string
		wrapUp bool
		reason WoundDownReason
	}{
		{"not wrapping up at all", false, ""},
		{"wrapping up, but for the tool-call budget, not the deadline", true, WoundDownReasonToolBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()

			reqCtx, cancel := a.buildTurnRequestCtx(parent, c.wrapUp, c.reason, 20*time.Millisecond)
			defer cancel()

			if reqCtx.Err() != nil {
				t.Fatalf("reqCtx already done before parent cancellation: %v", reqCtx.Err())
			}
			if _, ok := reqCtx.Deadline(); ok {
				t.Fatal("reqCtx has a deadline — the plain WithCancel(ctx) path must not be bounded by wrapUpBudget")
			}

			parentCancel()
			select {
			case <-reqCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("reqCtx never observed parent cancellation — the plain path must stay a REAL child of ctx")
			}
			if !errors.Is(reqCtx.Err(), context.Canceled) {
				t.Fatalf("reqCtx.Err() = %v, want context.Canceled", reqCtx.Err())
			}
		})
	}
}

// TestBuildTurnRequestCtx_DeadlineWrapUp_NormalCtx_DetachedButSelfBounded
// pins the intended, non-exceptional behavior of the ONE special case: a
// deadline-triggered wrap-up whose ctx is not (yet) done at all. The
// returned ctx must survive the parent being cancelled LATER (that's the
// entire point of context.WithoutCancel — the forced final-answer request
// must not be killed by the imminent parent deadline this feature exists to
// beat) but must still expire on its own, bounded by wrapUpBudget.
func TestBuildTurnRequestCtx_DeadlineWrapUp_NormalCtx_DetachedButSelfBounded(t *testing.T) {
	a := &Agent{}
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	const budget = 30 * time.Millisecond
	reqCtx, cancel := a.buildTurnRequestCtx(parent, true, WoundDownReasonDeadline, budget)
	defer cancel()

	if reqCtx.Err() != nil {
		t.Fatalf("reqCtx already done: %v", reqCtx.Err())
	}
	if dl, ok := reqCtx.Deadline(); !ok {
		t.Fatal("reqCtx has no deadline — must be bounded by wrapUpBudget")
	} else if remaining := time.Until(dl); remaining <= 0 || remaining > budget {
		t.Fatalf("reqCtx deadline is %v from now, want roughly %v (wrapUpBudget)", remaining, budget)
	}

	// Cancelling the PARENT (simulating the parent's deadline arriving, or a
	// Ctrl+C the guard did not need to react to because it hadn't happened
	// yet at buildTurnRequestCtx-call time) must NOT reach this detached ctx.
	parentCancel()
	time.Sleep(5 * time.Millisecond)
	if reqCtx.Err() != nil {
		t.Fatalf("reqCtx.Err() = %v after parent cancellation, want nil — context.WithoutCancel must have detached it", reqCtx.Err())
	}

	select {
	case <-reqCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("reqCtx never expired on its own — WithTimeout(wrapUpBudget) must still bound it")
	}
	if !errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("reqCtx.Err() = %v, want context.DeadlineExceeded (its OWN wrapUpBudget timeout, not the parent's)", reqCtx.Err())
	}
}

// TestBuildTurnRequestCtx_DeadlineWrapUp_CtxAlreadyCanceled_MustNotDetach is
// THE mutation-killing test: delete
// "&& !errors.Is(ctx.Err(), context.Canceled)" from buildTurnRequestCtx and
// this is the only test in the package that turns red. wrapUpBudget is set
// to an HOUR specifically so there is no ambiguity — under the mutant, a ctx
// already cancelled by the time this is called would still get the
// detached, WithoutCancel treatment, and reqCtx.Err() would read nil (a real
// Ctrl+C silently ignored for up to an hour) instead of propagating the
// cancellation immediately.
func TestBuildTurnRequestCtx_DeadlineWrapUp_CtxAlreadyCanceled_MustNotDetach(t *testing.T) {
	a := &Agent{}
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel() // the user's Ctrl+C already landed before this call

	reqCtx, cancel := a.buildTurnRequestCtx(parent, true, WoundDownReasonDeadline, time.Hour)
	defer cancel()

	if reqCtx.Err() == nil {
		t.Fatal("reqCtx.Err() = nil, want context.Canceled immediately — a ctx already cancelled by the time buildTurnRequestCtx is called must not be masked by WithoutCancel, or a real Ctrl+C would be ignored for up to wrapUpBudget (an hour, here)")
	}
	if !errors.Is(reqCtx.Err(), context.Canceled) {
		t.Fatalf("reqCtx.Err() = %v, want context.Canceled", reqCtx.Err())
	}
	select {
	case <-reqCtx.Done():
	default:
		t.Fatal("reqCtx.Done() is not closed even though reqCtx.Err() != nil — should be impossible")
	}
}

// TestBuildTurnRequestCtx_DeadlineWrapUp_CtxAlreadyDeadlineExceeded_StillDetaches
// pins the one deliberate, documented EXCEPTION to the guard above: a parent
// that has already crossed ITS OWN deadline (not a real Ctrl+C) must still
// get the detached, self-bounded treatment. By the time buildTurnRequestCtx
// runs, the trigger check already established the deadline was imminent;
// ordinary per-turn bookkeeping between that check and this call can easily
// let ctx actually cross into DeadlineExceeded first. That is the EXPECTED
// case this whole mechanism exists to survive — a future "fix" that also
// bails out on DeadlineExceeded here would silently downgrade every
// tightly-timed wrap-up back into the exact loss (a *TimeoutError with
// FinalOutput empty) M6 exists to prevent.
func TestBuildTurnRequestCtx_DeadlineWrapUp_CtxAlreadyDeadlineExceeded_StillDetaches(t *testing.T) {
	a := &Agent{}
	parent, parentCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer parentCancel()
	<-parent.Done() // let the parent's OWN deadline actually elapse
	if !errors.Is(parent.Err(), context.DeadlineExceeded) {
		t.Fatalf("test setup: parent.Err() = %v, want context.DeadlineExceeded", parent.Err())
	}

	const budget = 30 * time.Millisecond
	reqCtx, cancel := a.buildTurnRequestCtx(parent, true, WoundDownReasonDeadline, budget)
	defer cancel()

	if reqCtx.Err() != nil {
		t.Fatalf("reqCtx.Err() = %v, want nil — a parent that hit its OWN deadline (not a real Ctrl+C) must still get the detached, self-bounded path (the documented exception in buildTurnRequestCtx's doc comment)", reqCtx.Err())
	}
	if _, ok := reqCtx.Deadline(); !ok {
		t.Fatal("reqCtx has no deadline — must still be bounded by wrapUpBudget even though the parent's own deadline already fired")
	}

	select {
	case <-reqCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("reqCtx never expired on its own")
	}
	if !errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("reqCtx.Err() = %v, want context.DeadlineExceeded (from its OWN wrapUpBudget, not the parent's)", reqCtx.Err())
	}
}

// ---------------------------------------------------------------------------
// deadlineWrapUpExpectedErr: a TARGETED test of every branch, not the
// incidental coverage TestStreamIdleWatchdog_ComposesWithRequestTimeout
// happens to provide (that test never has wrapUp true when this function
// runs, so it only ever exercises — and only by accident — the "always
// false because wrapUp is false" branch; a mutant that made this function
// return an unconditional true still fails that test, but for a reason that
// has nothing to do with what the function is actually supposed to decide
// once wrapUp IS true). This table pins every combination explicitly.
// ---------------------------------------------------------------------------

func TestDeadlineWrapUpExpectedErr(t *testing.T) {
	cases := []struct {
		name   string
		wrapUp bool
		reason WoundDownReason
		err    error
		want   bool
	}{
		{"not wrapping up at all", false, WoundDownReasonDeadline, context.DeadlineExceeded, false},
		{"wrapping up for the tool budget, not the deadline", true, WoundDownReasonToolBudget, context.DeadlineExceeded, false},
		{"deadline wrap-up, but the outer ctx isn't done at all", true, WoundDownReasonDeadline, nil, false},
		{"deadline wrap-up, outer ctx cancelled (a real Ctrl+C) — must not be swallowed", true, WoundDownReasonDeadline, context.Canceled, false},
		{"deadline wrap-up, some unrelated error — must not be swallowed", true, WoundDownReasonDeadline, errors.New("boom"), false},
		{"deadline wrap-up, the outer ctx's own deadline fired — the one expected case", true, WoundDownReasonDeadline, context.DeadlineExceeded, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deadlineWrapUpExpectedErr(c.wrapUp, c.reason, c.err)
			if got != c.want {
				t.Fatalf("deadlineWrapUpExpectedErr(wrapUp=%v, reason=%q, err=%v) = %v, want %v",
					c.wrapUp, c.reason, c.err, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Run() end-to-end: a real Ctrl+C landing AFTER a wall-clock wrap-up has
// already triggered must stop the run immediately — not be masked by
// context.WithoutCancel for up to a full wrapUpBudget.
// ---------------------------------------------------------------------------

// wrapUpCancelRaceProvider drives the one real-world shape
// buildTurnRequestCtx's Canceled guard exists for: a wall-clock wrap-up has
// already triggered (wrapUp true, reason deadline — both sticky for the rest
// of the Run), and only THEN does the user's Ctrl+C land. Since wrapUp/reason
// never reset once set, EVERY later buildTurnRequestCtx call in this same Run
// must still re-check ctx.Err() — not just the first one. This provider
// forces a second wrap-up request via a context-overflow-shaped chunk error
// (Run's pre-existing compact-and-retry path, unrelated to cancellation)
// specifically so there IS a second buildTurnRequestCtx call, built AFTER
// the parent ctx is already cancelled, to observe.
type wrapUpCancelRaceProvider struct {
	toolTurnSleep time.Duration
	blockedSleep  time.Duration
	cancelParent  context.CancelFunc

	calls       int
	noToolCalls int
}

func (p *wrapUpCancelRaceProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *wrapUpCancelRaceProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.calls++
	if len(req.Tools) > 0 {
		// The ordinary tool turn: real wall-clock sleep is what lets
		// "remaining" fall under the reserve by the NEXT turn's trigger
		// check (see wrapUpReserve — the reserve is capped at half the
		// total budget, so it can never fire on turn 0 of a real deadline).
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			time.Sleep(p.toolTurnSleep)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "wc-1", Name: "wecho"}},
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}

	p.noToolCalls++
	ch := make(chan llm.StreamChunk, 1)
	if p.noToolCalls == 1 {
		// The forced wrap-up request goes out: simulate the user's Ctrl+C
		// landing at that exact moment, then surface a context-overflow-
		// shaped error so Run's PRE-EXISTING compact-and-retry path (nothing
		// to do with cancellation) sends a second wrap-up request — the one
		// buildTurnRequestCtx's guard must protect, since by the time it
		// runs the parent is already cancelled.
		p.cancelParent()
		ch <- llm.StreamChunk{Err: errors.New("context window exceeded")}
		close(ch)
		return ch, nil
	}

	// The second wrap-up request: the parent is ALREADY cancelled by now.
	// Race this request's OWN ctx (whatever buildTurnRequestCtx just decided
	// to hand it) against a sleep several times longer than any reasonable
	// wrap-up reserve. A correctly-guarded call hands this request an
	// already-done, plain ctx, so ctx.Done() below fires essentially
	// instantly; an unguarded mutant instead hands it a
	// context.WithoutCancel(parent) — immune to the cancellation, bounded
	// only by its own fresh wrapUpBudget timeout — so this select still
	// eventually resolves via ctx.Done(), just far later (bounded by
	// wrapUpBudget, never by blockedSleep, which is what proves this isn't
	// merely a slow test).
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			ch <- llm.StreamChunk{Err: ctx.Err()}
		case <-time.After(p.blockedSleep):
			ch <- llm.StreamChunk{
				Message: &models.Message{Role: models.RoleAI, Content: "should never arrive"},
				Done:    true,
				Stop:    "stop",
			}
		}
	}()
	return ch, nil
}

// paddedHistoryWithBigToolResult returns a message history engineered so a
// later context-overflow-shaped provider error can actually be compacted
// away: compactOnOverflow only retries when its before/after token estimate
// shows a real reduction (see its doc comment), and compactMessages only
// touches the middle region between the protected head (system + first
// human message) and the protected tail (compactionKeepTail messages, 6 by
// default). This big (~5KB) tool-result message sits squarely in that
// middle region, so summarizing it away (compactToolMessage) is a huge,
// guaranteed size reduction regardless of estimator quirks.
func paddedHistoryWithBigToolResult(sessionID string) []models.Message {
	big := strings.Repeat("x", 5000)
	return []models.Message{
		{ID: "h0", SessionID: sessionID, Role: models.RoleHuman, Content: "go"},
		{ID: "a1", SessionID: sessionID, Role: models.RoleAI, Content: "ack"},
		{ID: "t1", SessionID: sessionID, Role: models.RoleTool, Content: big, ToolResult: &models.ToolResult{
			CallID: "pad-1", ToolName: "pad", Status: models.CallStatusCompleted, Content: big,
		}},
		{ID: "a2", SessionID: sessionID, Role: models.RoleAI, Content: "noted"},
		{ID: "h1", SessionID: sessionID, Role: models.RoleHuman, Content: "continue"},
		{ID: "a3", SessionID: sessionID, Role: models.RoleAI, Content: "ok"},
		{ID: "h2", SessionID: sessionID, Role: models.RoleHuman, Content: "more"},
		{ID: "a4", SessionID: sessionID, Role: models.RoleAI, Content: "sure"},
	}
}

// TestRun_UserCancellation_AfterWrapUpTriggered_ReturnsImmediately is the
// end-to-end RED test for the same property the unit tests above pin in
// isolation: once a wall-clock wrap-up has triggered, a user Ctrl+C that
// lands afterward must stop Run() immediately, not be masked by
// context.WithoutCancel for up to a full wrapUpBudget.
//
// Not flaky by construction, not by generous timing margins: wrapUpBudget is
// forced to a deterministic 400ms (half of the 800ms ctx timeout — see
// wrapUpReserveCapFraction) by setting wrapUpReserveFloor absurdly high, so
// the cap always wins regardless of how long the tool turn's own sleep
// actually measures as on a slow/loaded machine. The decisive assertion is
// the error TYPE (context.Canceled vs. context.DeadlineExceeded), which is
// binary, not timing-sensitive: an unguarded buildTurnRequestCtx hands the
// second wrap-up request a context.WithoutCancel'd ctx that is immune to the
// cancellation, so it can only ever surface ITS OWN wrapUpBudget timeout
// (context.DeadlineExceeded) — never context.Canceled — no matter how the
// scheduler jitters. The elapsed-time assertion is a secondary check with a
// wide (700ms vs. an actual ~500ms/~900ms split) margin, and the 2-second
// blockedSleep the provider races against (never reached either way) proves
// the fast return isn't just a fast provider.
func TestRun_UserCancellation_AfterWrapUpTriggered_ReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	provider := &wrapUpCancelRaceProvider{
		toolTurnSleep: 500 * time.Millisecond,
		blockedSleep:  2 * time.Second,
		cancelParent:  cancel,
	}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       newRegistryWithEchoTool("wecho"),
	})
	// Force the half-of-total-budget CAP (not the multiplier) to decide the
	// reserve: an absurdly high floor guarantees a deterministic 400ms
	// reserve/wrapUpBudget regardless of exactly how long the tool turn's
	// own sleep measures as.
	a.wrapUpReserveFloor = 10 * time.Second

	start := time.Now()
	result, err := a.Run(ctx, "s1", paddedHistoryWithBigToolResult("s1"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run() error = nil, want context.Canceled — result: %+v", result)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled — an unguarded buildTurnRequestCtx lets the SECOND forced wrap-up request run on a context.WithoutCancel'd ctx immune to the user's Ctrl+C, so it surfaces its own wrapUpBudget timeout (context.DeadlineExceeded) instead of the real cancellation", err)
	}
	if elapsed > 700*time.Millisecond {
		t.Fatalf("Run() took %s to return after cancellation, want well under 700ms (~500ms tool turn + a near-instant guarded stop) — an unguarded buildTurnRequestCtx lets the second wrap-up request run for its full ~400ms wrapUpBudget instead of stopping immediately, and the 2s blockedSleep the provider raced against (never reached either way) proves this isn't just a fast provider", elapsed)
	}
	if provider.noToolCalls < 2 {
		t.Fatalf("provider saw %d tool-less request(s), want at least 2 (the initial wrap-up request, then the compaction retry that re-enters buildTurnRequestCtx after cancellation) — otherwise this test never exercised the guarded path at all", provider.noToolCalls)
	}
}
