package agent

import (
	"context"
	"errors"
	"time"
)

// --- M6: wall-clock graceful wrap-up ---------------------------------------
//
// Real eval runs recorded 13 wall-clock timeouts that discarded 100% of the
// work in flight (the subagent had already called tools, read files, and was
// mid-generation on its final answer — then the ctx deadline fired and
// consumeStream/Stream errored out, and Run returned with FinalOutput empty).
// Meanwhile the PRE-EXISTING tool-call-budget exhaustion path already had a
// graceful wrap-up: stop offering tools, force one more tool-less request for
// a final answer, and return THAT instead of hard-failing. This file gives
// the wall-clock deadline the same treatment, reusing wrapUp/wrapUpSystemPrompt/
// toolBudgetExhaustedNotice as-is — only the TRIGGER and the wrap-up request's
// own context are new.

// defaultWrapUpReserveFloor is the lower bound on the wall-clock reserve when
// this Run has not yet observed a turn slow enough (times the headroom
// multiplier) to justify a bigger one — in particular turn 0, before any
// observation exists at all. Real per-role per-turn latency measured 22s
// (coder) to 87s (tester); 30s sits above the fastest of those (so it doesn't
// under-reserve for a typical first turn before any observation exists) while
// staying well under the slowest (so it doesn't dominate the formula once a
// slow role's real pace has actually been observed and the multiplier branch
// below takes over). It is also always subject to the half-of-total-budget
// cap (wrapUpReserveCapFraction), so a short eval timeout is never eaten
// alive by this floor alone.
const defaultWrapUpReserveFloor = 30 * time.Second

// wrapUpReserveMultiplier is the headroom applied on top of the slowest turn
// this Run has observed so far: 20% absorbs normal turn-to-turn variance
// (tool result size, provider latency jitter) without being needlessly
// conservative — this is a reserve sized from THIS run's own observed pace,
// not a blind constant, so it doesn't need a large safety margin the way a
// one-size-fits-all guess would.
const wrapUpReserveMultiplier = 1.2

// wrapUpReserveCapFraction bounds the reserve to at most half of the run's
// total wall-clock budget (snapshotted once, at the top of Run, before the
// turn loop starts) — a hard requirement, not a tuning knob: without it, a
// short-timeout eval case would spend most or all of its budget "reserved"
// for a wrap-up that never gets meaningful work done beforehand.
const wrapUpReserveCapFraction = 0.5

// wrapUpReserveWindow bounds how far back wrapUpReserve looks when picking
// the slowest observed turn: only the most recent wrapUpReserveWindow
// COMPLETED turns are considered, not the run's entire history (F3, M6
// review). A single fixed choice trades off two failure modes:
//
//   - Too small (e.g. 1): the reserve would chase the immediately preceding
//     turn only, so it would shrink right back down the turn AFTER a slow
//     one even though the very next request might still be running long
//     (real provider slowness tends to cluster, not strictly alternate).
//   - Too large (the pre-fix behavior, effectively unbounded/"all history"):
//     one early jitter (a 429 retry storm, one slow tool call) then pins the
//     reserve at its inflated value for the rest of an arbitrarily long run,
//     even once every subsequent turn is back to a normal pace — the exact
//     defect this constant exists to fix. Measured repro: a single 250s
//     turn 0 kept the reserve capped at 5m0s through five subsequent 20s
//     turns that should have brought it back down to ~24s, burning up to
//     half the run's total budget on a stale reserve.
//
// 3 is chosen as a middle ground: large enough that a single slow turn
// doesn't cause the reserve to flap on a strict one-turn delay (a genuinely
// bumpy-but-not-jittery pace still gets counted for a couple of turns after
// each bump), small enough that a one-off spike is fully aged out of the
// window within a handful of turns rather than haunting the entire run. A
// SUSTAINED slow pace (every one of the last 3 turns genuinely slow, not
// just one) still gets its full reserve — only a spike that has actually
// passed decays away (see TestWrapUpReserve_SustainedSlowPaceStillReserved).
const wrapUpReserveWindow = 3

// wrapUpReserve sizes the wall-clock reserve carved out of ctx's deadline for
// the forced final-answer wrap-up turn, from THIS run's own RECENT observed
// turn durations (turnDurations — see the loop in Run) rather than a fixed
// constant: real measurements show per-role turn latency ranging 22s-87s, so
// any single fixed reserve is necessarily wrong (too small or too big) for at
// least half of them.
//
//   - No observations yet (turnDurations empty, i.e. still on turn 0): use
//     a.wrapUpReserveFloor — see its doc comment for why 30s specifically.
//   - Otherwise: 1.2x the slowest turn observed in the last
//     wrapUpReserveWindow completed turns (see its doc comment for why a
//     decaying window, not the run's entire history), floored at
//     a.wrapUpReserveFloor (never smaller than the conservative floor even if
//     every observed turn so far happened to be fast) and capped at
//     wrapUpReserveCapFraction of totalBudget (never more than half the
//     run's total wall-clock window).
//
// totalBudget <= 0 (ctx has no deadline at all) means the cap does not apply
// — callers only invoke this once ctx.Deadline() has already confirmed a
// deadline exists, so this is a defensive fallback, not a real code path.
func (a *Agent) wrapUpReserve(turnDurations []time.Duration, totalBudget time.Duration) time.Duration {
	window := turnDurations
	if len(window) > wrapUpReserveWindow {
		window = window[len(window)-wrapUpReserveWindow:]
	}
	var maxTurn time.Duration
	for _, d := range window {
		if d > maxTurn {
			maxTurn = d
		}
	}

	floor := a.wrapUpReserveFloor
	if floor <= 0 {
		floor = defaultWrapUpReserveFloor
	}

	reserve := time.Duration(float64(maxTurn) * wrapUpReserveMultiplier)
	if reserve < floor {
		reserve = floor
	}

	if totalBudget > 0 {
		if cap := time.Duration(float64(totalBudget) * wrapUpReserveCapFraction); reserve > cap {
			reserve = cap
		}
	}
	return reserve
}

// shouldTriggerWallClockWrapUp is the exact decision the top of Run's turn
// loop makes every iteration, pulled out as a pure function so the one
// property that matters most — a context already done for ANY reason
// (cancelled OR expired) must NEVER trigger a wrap-up — is testable without
// racing real wall-clock timing.
//
// ctxErr must be the ctx.Err() observed at the SAME instant as deadline/ok
// (i.e. call ctx.Err() and ctx.Deadline() back-to-back, then pass both here)
// so the two can't disagree about "done-ness" out from under the caller.
//
// Why gate on ctxErr instead of just checking `remaining <= reserve`: once
// ctx.Err() is non-nil, remaining/reserve arithmetic is meaningless — a
// context.Canceled (user Ctrl+C) must stop the run immediately via the
// PRE-EXISTING error paths (the next Stream() call, or the ctx.Err() checks
// already scattered through the tool-dispatch paths), never detour through
// one more forced LLM request. A context.DeadlineExceeded already-fired is
// likewise left alone here — this trigger is an EARLY warning that fires
// strictly BEFORE the deadline, not a recovery attempted after the fact.
func shouldTriggerWallClockWrapUp(ctxErr error, deadline time.Time, hasDeadline bool, remaining, reserve time.Duration) bool {
	if ctxErr != nil {
		return false
	}
	if !hasDeadline {
		return false
	}
	return remaining <= reserve
}

// buildTurnRequestCtx returns the context for one turn's LLM Stream call.
//
// Normally (wrapUp false, or wrapUp true for the tool-budget reason) it's a
// plain cancellable child of ctx: a parent deadline firing or the user
// hitting Ctrl+C aborts the in-flight request exactly as it always has.
//
// Exception: once wrap-up was triggered by the WALL-CLOCK deadline
// specifically (reason == WoundDownReasonDeadline), wrapUpBudget is the same
// reserve that was just subtracted from ctx's deadline in the trigger check
// — carved out of that deadline FOR this forced final-answer request. Using
// ctx directly here would mean the parent's deadline (now imminent by
// construction) cancels the request mid-generation, which is the exact loss
// this feature exists to prevent. So this one request instead runs on
// context.WithTimeout(context.WithoutCancel(ctx), wrapUpBudget): detached
// from the parent's cancellation but still bounded by wrapUpBudget — so a
// SINGLE such request can overshoot its wall-clock deadline by at most
// wrapUpBudget. If the wrap-up needs more than one request (e.g. a
// misbehaving provider that keeps returning tool calls, or a compaction
// retry mid wrap-up), the CALLER (react.go's turn loop) is responsible for
// passing an already-shrunk wrapUpBudget for every request after the
// first — see capCumulativeWrapUpBudget (F4, M6 review): the ENTIRE
// wrap-up phase, not each request in it, is bounded to at most one
// reserve's worth of overshoot. This function itself has no memory of
// previous requests; it only ever bounds THIS ONE call to whatever budget
// it is handed.
//
// Guarded by a fresh check of ctx.Err() specifically for context.Canceled
// (not just the wrapUp/reason state decided a turn, or a few lines, earlier):
// this keeps the window in which a real user Ctrl+C could be masked by
// WithoutCancel as small as possible. context.DeadlineExceeded is
// deliberately NOT included in that guard — by the time this runs, the
// trigger check already established the deadline was imminent, and ordinary
// processing between that check and this call (assembling the prompt,
// compaction bookkeeping, ...) can easily consume the last few milliseconds
// of a tight reserve and let ctx actually cross into DeadlineExceeded before
// the request is even built. That is the EXPECTED, planned-for case this
// whole mechanism exists to survive, not a signal to bail — bailing on it
// here would silently downgrade every tightly-timed wrap-up back into the
// exact loss (a *TimeoutError with FinalOutput empty) this feature exists to
// prevent. Only an explicit context.Canceled means a real Ctrl+C actually
// happened, which must still win.
func (a *Agent) buildTurnRequestCtx(ctx context.Context, wrapUp bool, reason WoundDownReason, wrapUpBudget time.Duration) (context.Context, context.CancelFunc) {
	if wrapUp && reason == WoundDownReasonDeadline && !errors.Is(ctx.Err(), context.Canceled) {
		return context.WithTimeout(context.WithoutCancel(ctx), wrapUpBudget)
	}
	return context.WithCancel(ctx)
}

// deadlineWrapUpExpectedErr reports whether outerErr — the PARENT ctx's own
// Err(), observed right after a turn's stream finished successfully — is the
// exact, planned-for consequence of a deadline-triggered wrap-up turn that
// buildTurnRequestCtx deliberately detached from that same parent's
// cancellation: the parent's deadline elapsing (context.DeadlineExceeded)
// WHILE that detached, self-bounded request was still generating.
//
// Without this, the post-stream `if err := ctx.Err(); err != nil` check a few
// lines below Run's Stream call — which exists to catch a genuine
// cancellation/timeout the CURRENT request's own reqCtx wouldn't otherwise
// surface — would immediately convert an otherwise-successful, hard-won
// wrap-up answer back into a *TimeoutError with FinalOutput discarded: the
// parent ctx is EXPECTED to have crossed its deadline by the time a
// wrap-up request that intentionally outlives it returns. A real user
// Ctrl+C during that same window still surfaces (context.Canceled is
// deliberately excluded here) — only the anticipated deadline expiry is
// swallowed.
func deadlineWrapUpExpectedErr(wrapUp bool, reason WoundDownReason, outerErr error) bool {
	return wrapUp && reason == WoundDownReasonDeadline && errors.Is(outerErr, context.DeadlineExceeded)
}

// capCumulativeWrapUpBudget bounds ONE wrap-up request's budget so the
// ENTIRE wall-clock wrap-up phase of a Run — the initial forced request PLUS
// however many compaction retries re-enter it (react.go's `continue` paths
// can loop back through the wrap-up branch repeatedly: up to 3 distinct
// overflow-triggered retries, each handing out a full requestBudget again
// before this fix) — overshoots ctx's original deadline by at most ONE
// reserve's worth, not one reserve PER request (F4, M6 review).
//
// Real repro without this cap: parent budget 300ms, reserve ~150ms, 2
// wrap-up requests strung together via compaction retries measured a 236ms
// overshoot (1.57x the reserve); the worst case (compactOnOverflow's 3
// possible consecutive successes, so up to 4 full-budget requests in one
// Run) is unbounded by construction — at the default `--timeout 5m` eval
// setting, up to 4x150s = 600s of overshoot, dwarfing evalWaitGrace's grace
// window (see pkg/commands/agent_eval.go), which assumes (and, after this
// fix, can finally rely on) the overshoot being bounded to a SINGLE reserve.
//
// cumulativeDeadline is the absolute wall-clock instant beyond which the
// ENTIRE wrap-up phase must not run — set ONCE, when wrap-up first triggers,
// to (trigger time).Add(reserve); the zero value means "no cumulative cap
// applies" (a non-deadline wrap-up, i.e. the pre-existing tool-call-budget
// path, which this function must leave byte-for-byte unaffected).
//
// requestBudget is what the caller would otherwise hand this one request
// (react.go's wrapUpBudget, unchanged since the moment wrap-up triggered).
// The result is whichever is smaller: requestBudget itself (the common case
// — no retry has happened yet, or plenty of the cumulative allowance is
// still unspent) or however much of the cumulative allowance remains right
// now (which can be small, or even negative once the allowance is fully
// spent — buildTurnRequestCtx's context.WithTimeout treats a non-positive
// duration as "already expired," which is exactly the intended bounded
// failure mode: the request gets essentially no time and fails fast rather
// than being handed another full reserve).
func capCumulativeWrapUpBudget(requestBudget time.Duration, cumulativeDeadline time.Time, now time.Time) time.Duration {
	if cumulativeDeadline.IsZero() {
		return requestBudget
	}
	if remaining := cumulativeDeadline.Sub(now); remaining < requestBudget {
		return remaining
	}
	return requestBudget
}
