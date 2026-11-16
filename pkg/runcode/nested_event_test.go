package runcode

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/tools"
)

type eventRecorder struct {
	mu     sync.Mutex
	events []tools.NestedToolEvent
	sink   tools.NestedEventSink
}

func newEventRecorder() *eventRecorder {
	r := &eventRecorder{}
	r.sink = func(e tools.NestedToolEvent) {
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
	}
	return r
}

func (r *eventRecorder) snapshot() []tools.NestedToolEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tools.NestedToolEvent(nil), r.events...)
}

// Nested events fire per lifecycle transition through the ctx-injected sink
// (§7.2 Phase 2): running at begin, completed/failed at finish.
func TestNestedEventsSuccessAndFailure(t *testing.T) {
	warm := newEchoHost()
	warm.declare("a", true, true)
	if w := runScript(t, warm, Limits{}, `return 1;`); w.Outcome != OutcomeOK {
		t.Fatalf("warmup: %s %s", w.Outcome, w.Err)
	}

	h := newEchoHost()
	h.declare("ok", true, true)
	h.declare("bad", true, true)
	h.fn = func(name string, args map[string]any) HostResult {
		if name == "bad" {
			return HostResult{Err: "boom", Tool: "bad"}
		}
		return HostResult{Status: "completed"}
	}

	rec := newEventRecorder()
	ctx := tools.WithNestedEventSink(context.Background(), rec.sink)
	out := NewEngine().Run(ctx, `
		const a = await tools.ok({});
		try { await tools.bad({}); } catch (e) {}
		return 1;
	`, h, Limits{Timeout: 10 * time.Second, MaxToolCalls: 10})
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}

	evts := rec.snapshot()
	if len(evts) != 4 {
		t.Fatalf("events = %+v, want 4 (running/completed ×2)", evts)
	}
	byTool := map[string][]string{}
	for _, e := range evts {
		byTool[e.Tool] = append(byTool[e.Tool], e.Status)
	}
	if got := byTool["ok"]; len(got) != 2 || got[0] != "running" || got[1] != "completed" {
		t.Fatalf("ok events = %v", got)
	}
	if got := byTool["bad"]; len(got) != 2 || got[0] != "running" || got[1] != "failed" {
		t.Fatalf("bad events = %v", got)
	}
	for _, e := range evts {
		if e.Seq == 0 {
			t.Fatalf("event without seq: %+v", e)
		}
	}
	// The failed event carries the rejection text.
	for _, e := range evts {
		if e.Tool == "bad" && e.Status == "failed" && !strings.Contains(e.Err, "boom") {
			t.Fatalf("failed event lost the error text: %+v", e)
		}
	}
	for i := range evts {
		if evts[i].Seq != i/2+1 {
			t.Fatalf("seq not per-call: %+v", evts)
		}
	}
}

// On a watchdog abort, still-running calls fire interrupted from the snapshot
// (the Run goroutine — the driver was abandoned).
func TestNestedEventsInterruptedOnTimeout(t *testing.T) {
	warm := newEchoHost()
	warm.declare("a", true, true)
	if w := runScript(t, warm, Limits{}, `return 1;`); w.Outcome != OutcomeOK {
		t.Fatalf("warmup: %s %s", w.Outcome, w.Err)
	}

	h := newEchoHost()
	h.declare("block", true, false)
	block := make(chan struct{})
	defer close(block)
	h.fn = func(name string, args map[string]any) HostResult {
		<-block
		return HostResult{Status: "completed"}
	}

	rec := newEventRecorder()
	ctx := tools.WithNestedEventSink(context.Background(), rec.sink)
	out := NewEngine().Run(ctx, `await tools.block({}); return 1;`, h,
		Limits{Timeout: 200 * time.Millisecond, MaxToolCalls: 10})
	if out.Outcome != OutcomeTimeout {
		t.Fatalf("outcome=%s", out.Outcome)
	}

	evts := rec.snapshot()
	if len(evts) != 2 {
		t.Fatalf("events = %+v, want running+interrupted", evts)
	}
	if evts[0].Status != "running" || evts[1].Status != "interrupted" {
		t.Fatalf("event order = %+v", evts)
	}
	if evts[1].Err == "" {
		t.Fatal("interrupted event must carry a reason")
	}
}

// Self-review defect pin: after the watchdog aborts (snapshot(true) fires
// interrupted), a late finish() from the abandoned worker must NOT emit —
// the model already saw "interrupted"; a trailing "completed" for the same
// call would contradict the journal the result carries.
func TestNestedEventsLateFinishAfterAbortIsSilent(t *testing.T) {
	rec := newEventRecorder()
	jrn := newJournal(rec.sink)

	e := jrn.begin("edit_file", map[string]any{"path": "/tmp/x/a.go"}, true)
	aborted := jrn.snapshot(true) // watchdog fires: running → interrupted + event
	if len(aborted) != 1 || aborted[0].Status != JournalInterrupted {
		t.Fatalf("snapshot = %+v", aborted)
	}
	if evts := rec.snapshot(); len(evts) != 2 || evts[1].Status != "interrupted" {
		t.Fatalf("events after abort = %+v", evts)
	}

	// The abandoned worker finishes late — must update the entry for any
	// later snapshot but must NOT emit a contradicting event.
	jrn.finish(e, JournalCompleted, 12, 100, "")
	if evts := rec.snapshot(); len(evts) != 2 {
		t.Fatalf("late finish emitted after abort: %+v", evts)
	}
}

// Review M1 pin: after the watchdog aborts (snapshot(true) ran), a LATE
// begin from the abandoned driver must NOT emit running — the outer result
// already went out without this entry; a parentless trailing ⚙ would never
// get an end line.
func TestNestedEventsBeginAfterAbortIsSilent(t *testing.T) {
	rec := newEventRecorder()
	jrn := newJournal(rec.sink)

	jrn.begin("a", nil, false)
	jrn.snapshot(true) // abort: a → interrupted + event

	jrn.begin("b", nil, false) // late arrival — must stay silent

	evts := rec.snapshot()
	if len(evts) != 2 {
		t.Fatalf("late begin emitted after abort: %+v", evts)
	}
	for _, e := range evts {
		if e.Tool == "b" {
			t.Fatalf("late begin leaked an event: %+v", evts)
		}
	}
}

// No sink in ctx: the journal stays silent and everything else works.
func TestNestedEventsAbsentSinkIsSilent(t *testing.T) {
	h := newEchoHost()
	h.declare("ok", true, true)
	out := runScript(t, h, Limits{}, `const a = await tools.ok({}); return 1;`)
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	if len(out.Calls) != 1 || out.Calls[0].Status != JournalCompleted {
		t.Fatalf("calls=%+v", out.Calls)
	}
}
