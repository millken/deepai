package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
)

// AttachSchedule wires a schedule.Service into a Manager: global events flow
// one way (Service → bot-wide bus) and executions flow the other way
// (Service → Dispatcher → Manager.Send on an ordinary session turn).
func AttachSchedule(m *Manager, svc *schedule.Service) *ScheduleDispatcher {
	svc.SetObserver(&scheduleObserver{m: m, last: map[string]schedule.RunStatus{}})
	d := NewScheduleDispatcher(m, svc)
	svc.SetDispatcher(d)
	return d
}

// scheduleObserver publishes schedule_* events on the global "*" bus. The
// execution process itself is deliberately NOT mirrored here: clients
// subscribe to run.session_id and see the ordinary turn events.
type scheduleObserver struct {
	m    *Manager
	mu   sync.Mutex
	last map[string]schedule.RunStatus
}

func (o *scheduleObserver) ScheduleChanged(sch schedule.Schedule, kind schedule.ChangeKind) {
	typ := EventScheduleUpdated
	switch kind {
	case schedule.ChangeCreated:
		typ = EventScheduleCreated
	case schedule.ChangeDeleted:
		typ = EventScheduleDeleted
	}
	o.m.global.publish(Event{Type: typ, Schedule: &sch, Time: time.Now().UTC()})
}

func (o *scheduleObserver) RunChanged(run schedule.Run) {
	o.mu.Lock()
	prev, seen := o.last[run.ID]
	if run.Status.Terminal() {
		delete(o.last, run.ID)
	} else {
		o.last[run.ID] = run.Status
	}
	o.mu.Unlock()
	typ := EventScheduleRunUpdated
	switch {
	case run.Status.Terminal():
		typ = EventScheduleRunFinished
	case run.Status == schedule.RunRunning && (!seen || (prev != schedule.RunRunning && prev != schedule.RunWaitingInput)):
		// Only pending → running is a start; waiting_input → running is an
		// intermediate update (design §9.1).
		typ = EventScheduleRunStarted
	}
	o.m.global.publish(Event{Type: typ, Run: &run, Time: time.Now().UTC()})
}

// ScheduleDispatcher implements schedule.Dispatcher by starting one ordinary
// turn in the task's session. Fire returns as soon as Manager.Send hands back
// the Turn handle; a goroutine waits for Done() and reports the final state
// through Service.FinishRun.
type ScheduleDispatcher struct {
	m   *Manager
	svc *schedule.Service
	log *slog.Logger

	wg sync.WaitGroup

	mu     sync.Mutex
	active map[string]*scheduledRun
}

// scheduledRun is the in-process state of one dispatched execution. The
// Dispatcher needs it to remember why a run was cancelled (replace vs
// client); answer_timeout failures are recorded on the turn's own hooks.
type scheduledRun struct {
	cancelReason schedule.CancelReason
}

// NewScheduleDispatcher builds a Dispatcher over a Manager.
func NewScheduleDispatcher(m *Manager, svc *schedule.Service) *ScheduleDispatcher {
	return &ScheduleDispatcher{m: m, svc: svc, log: slog.Default(), active: map[string]*scheduledRun{}}
}

// Fire starts a run and returns immediately. The execution budget, the
// per-run tool-call limit and the question hooks travel through SendOptions;
// the Manager owns the pausable turnClock, so the Dispatcher no longer runs
// a competing timer (design §7.2).
func (d *ScheduleDispatcher) Fire(ctx context.Context, sch schedule.Schedule, run schedule.Run) (string, string, error) {
	sessionID, err := d.resolveSession(sch, run)
	if err != nil {
		return "", "", err
	}
	timeout := runTimeout(sch.Options.Timeout)
	hooks := newScheduleQuestionHooks(d, d.svc, sch, run, sessionID)
	content := scheduledMessage(sch, run)
	req := SendRequest{Content: content, Model: sch.Options.Model}
	opts := SendOptions{
		ScheduleID:   sch.ID,
		RunID:        run.ID,
		Timeout:      timeout,
		MaxToolCalls: sch.Options.MaxToolCalls,
		Hooks:        hooks,
		Metadata: map[string]string{
			"schedule_id": sch.ID,
			"run_id":      run.ID,
			"source":      "schedule",
		},
	}
	turn, err := d.m.SendWithOptions(sessionID, req, opts)
	if err != nil {
		// A dedicated session may have been deleted by the user; recreate it
		// lazily (docs/BOT_SCHEDULER_DESIGN.md §6).
		if errors.Is(err, ErrNotFound) && sch.Options.SessionMode != schedule.SessionOrigin {
			sessionID, err = d.createSession(sch)
			if err != nil {
				return "", "", err
			}
			hooks.setSessionID(sessionID)
			turn, err = d.m.SendWithOptions(sessionID, req, opts)
		}
		if err != nil {
			if errors.Is(err, ErrBusy) || errors.Is(err, ErrClosed) {
				return "", "", schedule.ErrBusy
			}
			var locked *models.ErrSessionLocked
			if errors.As(err, &locked) {
				return "", "", schedule.ErrBusy
			}
			return "", "", err
		}
	}
	sr := &scheduledRun{}
	d.mu.Lock()
	d.active[run.ID] = sr
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.active, run.ID)
			d.mu.Unlock()
		}()
		<-turn.Done()
		sum := turn.Summary()
		out := outcomeFromSummary(sum)
		d.mu.Lock()
		reason := sr.cancelReason
		d.mu.Unlock()
		failure := hooks.failureReason()
		switch {
		case failure != "" && out.Status == schedule.RunCancelled:
			out.Status, out.ErrorCode = schedule.RunFailed, "answer_timeout"
			out.Error = failure
		case (sum != nil && sum.CancelSource == string(cancelSourceShutdown)) ||
			(d.m.Closing() && out.Status == schedule.RunCancelled):
			out.Status, out.ErrorCode = schedule.RunFailed, "bot_shutdown"
			out.Error = "bot shut down while the run was active"
		case reason == schedule.CancelReplaced && out.Status == schedule.RunCancelled:
			out.ErrorCode = "replaced"
			out.Error = "replaced by a newer run"
		}
		if out.Status == schedule.RunTimedOut && out.Error == "" {
			out.Error = "run exceeded " + timeout.String()
		}
		if _, err := d.svc.FinishRun(context.Background(), run.ID, out); err != nil {
			d.log.Warn("bot: finish schedule run", "run", run.ID, "err", err)
		}
	}()
	return sessionID, turn.ID, nil
}

func runTimeout(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
}

// Cancel only emits the cancellation; the terminal state still comes from
// the turn_end callback above. The source is recorded on turn_end so the Run
// can distinguish a user cancellation from a shutdown or an overlap replace.
func (d *ScheduleDispatcher) Cancel(run schedule.Run, reason schedule.CancelReason) error {
	if run.SessionID == "" {
		return nil
	}
	d.mu.Lock()
	if sr := d.active[run.ID]; sr != nil {
		sr.cancelReason = reason
	}
	d.mu.Unlock()
	src := cancelSourceClient
	switch reason {
	case schedule.CancelReplaced, schedule.CancelScheduler:
		src = cancelSourceScheduler
	case schedule.CancelShutdown:
		src = cancelSourceShutdown
	}
	err := d.m.cancelWithSource(run.SessionID, src)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	return err
}

// Wait lets shutdown drain the turn_end callbacks before the database is
// closed. It never waits longer than ctx.
func (d *ScheduleDispatcher) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *ScheduleDispatcher) resolveSession(sch schedule.Schedule, run schedule.Run) (string, error) {
	switch sch.Options.SessionMode {
	case schedule.SessionNew:
		return d.createSession(sch)
	case schedule.SessionOrigin:
		if sch.CreatedBy.SessionID != "" {
			return sch.CreatedBy.SessionID, nil
		}
		// A task created over REST has no origin session; fall back to a
		// dedicated one instead of failing the run.
		if sch.SessionID != "" {
			return sch.SessionID, nil
		}
		return d.createSession(sch)
	default: // dedicated
		if sch.SessionID != "" {
			return sch.SessionID, nil
		}
		return d.createSession(sch)
	}
}

func (d *ScheduleDispatcher) createSession(sch schedule.Schedule) (string, error) {
	info, err := d.m.CreateSession(CreateSessionRequest{Title: "⏰ " + sch.Name})
	if err != nil {
		return "", fmt.Errorf("create schedule session: %w", err)
	}
	return info.ID, nil
}

func scheduledMessage(sch schedule.Schedule, run schedule.Run) string {
	loc, err := schedule.Location(sch.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := run.ScheduledFor.In(loc).Format("2006-01-02 15:04")
	header := fmt.Sprintf("[定时任务] %s · 第 %d 次 · 计划时间 %s (%s)",
		sch.Name, run.Seq, local, sch.Timezone)
	return header + "\n\n" + strings.TrimSpace(sch.Prompt)
}

// outcomeFromSummary maps a turn_end status onto a Run terminal state.
func outcomeFromSummary(sum *TurnSummary) schedule.Outcome {
	if sum == nil {
		return schedule.Outcome{Status: schedule.RunFailed, Error: "turn ended without a summary", ErrorCode: "missing_summary"}
	}
	out := schedule.Outcome{
		Output: sum.FinalOutput, Error: sum.Error, ToolCalls: sum.ToolCalls,
	}
	switch sum.Status {
	case TurnCompleted:
		out.Status = schedule.RunCompleted
	case TurnCancelled:
		out.Status, out.ErrorCode = schedule.RunCancelled, "cancelled"
	case TurnTimedOut:
		out.Status, out.ErrorCode = schedule.RunTimedOut, "timed_out"
	default:
		out.Status, out.ErrorCode = schedule.RunFailed, "turn_failed"
	}
	if sum.Usage != nil {
		if raw, err := json.Marshal(sum.Usage); err == nil {
			var usage map[string]any
			if json.Unmarshal(raw, &usage) == nil {
				out.Usage = usage
			}
		}
	}
	return out
}
