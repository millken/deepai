package bot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTurnClockPauseResumeAndDeadline(t *testing.T) {
	cur := time.Now()
	ctx, c := newTurnClock(context.Background(), time.Hour)
	defer c.cancelPlain()
	c.now = func() time.Time { return cur }
	c.runningSince = cur

	cur = cur.Add(400 * time.Millisecond)
	c.Pause()
	if c.pauses != 1 || c.timer != nil || c.remaining != time.Hour-400*time.Millisecond {
		t.Fatalf("after first pause: %+v", c)
	}
	// A second outstanding question must not spend any more budget.
	c.Pause()
	cur = cur.Add(2 * time.Hour)
	c.Resume()
	if c.pauses != 1 || c.timer != nil || c.remaining != time.Hour-400*time.Millisecond {
		t.Fatalf("still paused after one resume: %+v", c)
	}
	if ctx.Err() != nil {
		t.Fatal("execution clock fired while the turn was waiting for an answer")
	}
	c.Resume()
	if c.pauses != 0 || c.timer == nil || c.remaining != time.Hour-400*time.Millisecond {
		t.Fatalf("after last resume: %+v", c)
	}
	c.expire()
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) || ctx.Err() != context.Canceled {
		t.Fatalf("cause = %v, err = %v; want deadline exceeded", context.Cause(ctx), ctx.Err())
	}
	if stopSource(ctx) != "" {
		t.Fatalf("timeout must not carry a cancel source, got %q", stopSource(ctx))
	}
}

func TestTurnClockExhaustedBudgetOnResume(t *testing.T) {
	cur := time.Now()
	ctx, c := newTurnClock(context.Background(), 50*time.Millisecond)
	defer c.cancelPlain()
	c.now = func() time.Time { return cur }
	c.runningSince = cur
	cur = cur.Add(time.Second)
	c.Pause()
	if c.remaining != 0 {
		t.Fatalf("remaining = %v, want 0", c.remaining)
	}
	c.Resume()
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want deadline exceeded", context.Cause(ctx))
	}
}

func TestTurnClockCancelSourceFirstCauseWins(t *testing.T) {
	ctx, c := newTurnClock(context.Background(), time.Hour)
	c.CancelWithSource(cancelSourceScheduler)
	if stopSource(ctx) != string(cancelSourceScheduler) {
		t.Fatalf("source = %q", stopSource(ctx))
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
	c.CancelWithSource(cancelSourceClient)
	if stopSource(ctx) != string(cancelSourceScheduler) {
		t.Fatalf("source overwritten to %q", stopSource(ctx))
	}
}

func TestTurnClockWithoutBudget(t *testing.T) {
	ctx, c := newTurnClock(context.Background(), 0)
	defer c.cancelPlain()
	c.Pause()
	c.Resume()
	if ctx.Err() != nil || c.timer != nil {
		t.Fatalf("unlimited turn must have no timer: %+v", c)
	}
}

func TestManagerQuestionPausesExecutionClock(t *testing.T) {
	m, err := NewManager(Options{Runtime: &fakeRuntime{}, Store: NewMemoryStore(), IdleTimeout: -1, TurnTimeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	info, err := m.CreateSession(CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sub, _, _, err := m.Subscribe(info.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	turn, err := m.Send(info.ID, SendRequest{Content: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	evts := collectUntil(t, sub, EventQuestion)
	q := evts[len(evts)-1].Question
	// Three times the execution budget, but the turn is waiting for an answer.
	time.Sleep(240 * time.Millisecond)
	select {
	case <-turn.Done():
		t.Fatal("turn timed out while waiting for an answer")
	default:
	}
	if err := m.Answer(info.ID, q.ID, "a"); err != nil {
		t.Fatal(err)
	}
	sum := waitTurn(t, turn)
	if sum.Status != TurnCompleted || sum.FinalOutput != "answer=a" {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestManagerCancelSources(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		m, _, _ := newTestManager(t)
		info, _ := m.CreateSession(CreateSessionRequest{})
		turn, err := m.Send(info.ID, SendRequest{Content: "block"})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Cancel(info.ID); err != nil {
			t.Fatal(err)
		}
		sum := waitTurn(t, turn)
		if sum.Status != TurnCancelled || sum.CancelSource != string(cancelSourceClient) {
			t.Fatalf("summary = %+v", sum)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		m, err := NewManager(Options{Runtime: &fakeRuntime{}, Store: NewMemoryStore(), IdleTimeout: -1})
		if err != nil {
			t.Fatal(err)
		}
		info, _ := m.CreateSession(CreateSessionRequest{})
		turn, err := m.Send(info.ID, SendRequest{Content: "block"})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		sum := waitTurn(t, turn)
		if sum.Status != TurnCancelled || sum.CancelSource != string(cancelSourceShutdown) {
			t.Fatalf("summary = %+v", sum)
		}
	})
}
