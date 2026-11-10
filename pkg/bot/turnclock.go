package bot

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Cancel sources are reported on turn_end so the schedule Dispatcher can
// distinguish a deliberate cancellation (client / replace) from a shutdown
// or a scheduler-owned answer_timeout (docs/BOT_SCHEDULER_DESIGN.md §3.2).
type cancelSource string

const (
	cancelSourceClient    cancelSource = "client"
	cancelSourceShutdown  cancelSource = "shutdown"
	cancelSourceScheduler cancelSource = "scheduler"
)

// turnStopCause is the context cause of an interrupted turn. It carries both
// why the turn stopped (err = DeadlineExceeded for a timeout) and who stopped
// it. context.Cause(ctx) is the single source of truth for runTurn's status
// mapping, so a paused clock that expires still ends as timed_out even though
// ctx.Err() is always context.Canceled with WithCancelCause.
type turnStopCause struct {
	source string
	err    error
}

func (c turnStopCause) Error() string {
	if c.err != nil {
		return c.err.Error()
	}
	if c.source != "" {
		return "turn cancelled by " + c.source
	}
	return "turn cancelled"
}

func (c turnStopCause) Unwrap() error { return c.err }

func stopSource(ctx context.Context) string {
	var c turnStopCause
	if errors.As(context.Cause(ctx), &c) {
		return c.source
	}
	return ""
}

// turnClock is a pausable execution budget. Pause/Resume are reference
// counted so concurrent ask_clarification calls (subagents) only resume the
// clock once every question has been answered (design §7.2).
//
// remaining is the unspent budget; runningSince is when the current running
// segment began (zero while paused). The timer is armed only while the clock
// is actually running, which guarantees that waiting for an answer never
// counts against Schedule.timeout.
type turnClock struct {
	mu           sync.Mutex
	cancel       context.CancelCauseFunc
	now          func() time.Time
	limited      bool // false for --turn-timeout=0 with no per-turn budget
	remaining    time.Duration
	runningSince time.Time
	pauses       int
	timer        *time.Timer
	stopped      bool
	expired      bool
}

func newTurnClock(parent context.Context, budget time.Duration) (context.Context, *turnClock) {
	ctx, cancel := context.WithCancelCause(parent)
	c := &turnClock{cancel: cancel, now: time.Now, limited: budget > 0, remaining: budget}
	if c.limited {
		c.runningSince = c.now()
		c.timer = time.AfterFunc(budget, c.expire)
	}
	return ctx, c
}

// expire is the timer callback; tests may also call it directly after
// advancing an injected clock.
func (c *turnClock) expire() {
	c.mu.Lock()
	if c.stopped || c.expired || c.pauses > 0 {
		c.mu.Unlock()
		return
	}
	c.expired = true
	cancel := c.cancel
	c.mu.Unlock()
	cancel(turnStopCause{err: context.DeadlineExceeded})
}

// Pause freezes the remaining budget. Only the first pause stops the timer;
// later pauses just increment the reference count.
func (c *turnClock) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.expired {
		return
	}
	if c.pauses == 0 && c.limited && c.remaining > 0 {
		if !c.runningSince.IsZero() {
			c.remaining -= c.now().Sub(c.runningSince)
			if c.remaining < 0 {
				c.remaining = 0
			}
		}
		c.runningSince = time.Time{}
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
	}
	c.pauses++
}

// Resume re-arms the timer with whatever budget is left. The clock stays
// frozen until the last outstanding question has been settled.
func (c *turnClock) Resume() {
	c.mu.Lock()
	if c.pauses == 0 || c.stopped || c.expired {
		c.mu.Unlock()
		return
	}
	c.pauses--
	if c.pauses > 0 || !c.limited {
		c.mu.Unlock()
		return
	}
	if c.remaining <= 0 {
		c.expired = true
		cancel := c.cancel
		c.mu.Unlock()
		cancel(turnStopCause{err: context.DeadlineExceeded})
		return
	}
	c.runningSince = c.now()
	c.timer = time.AfterFunc(c.remaining, c.expire)
	c.mu.Unlock()
}

// Stop disarms the timer without cancelling the context (turn finished).
func (c *turnClock) Stop() {
	c.mu.Lock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// CancelWithSource stops the clock and records who cancelled the turn. The
// first cause wins: a turn that already hit its deadline stays timed_out.
func (c *turnClock) CancelWithSource(src cancelSource) {
	c.Stop()
	c.cancel(turnStopCause{source: string(src), err: context.Canceled})
}

// cancelPlain releases the context at the end of a normal turn.
func (c *turnClock) cancelPlain() {
	c.Stop()
	c.cancel(context.Canceled)
}

// turnHooks observes ask_clarification questions on a turn. Only scheduled
// turns set it; ordinary turns keep the pausable clock but never auto-answer.
type turnHooks interface {
	QuestionAsked(q Question)
	QuestionSettled(qid, answer string, auto bool)
	TurnEnded()
}
