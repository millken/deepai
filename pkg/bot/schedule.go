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

	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
)

// AttachSchedule wires a schedule.Service into a Manager: global events flow
// one way (Service → bot-wide bus) and executions flow the other way
// (Service → Dispatcher → Manager.Send on an ordinary session turn).
func AttachSchedule(m *Manager, svc *schedule.Service) *ScheduleDispatcher {
	obs := &scheduleObserver{m: m, svc: svc, last: map[string]schedule.RunStatus{}, meta: map[string]scheduleMeta{}}
	// Warm the metadata cache with existing schedules so a soft-delete
	// immediately after restart still has name/notify for cancelled run_end hooks.
	if list, err := svc.List(context.Background(), "", 0); err == nil {
		for _, sch := range list {
			obs.meta[sch.ID] = scheduleMeta{name: sch.Name, notify: sch.Options.Notify}
		}
	}
	svc.SetObserver(obs)
	d := NewScheduleDispatcher(m, svc)
	svc.SetDispatcher(d)
	go d.watchTurnEnd()
	return d
}

// watchTurnEnd wakes TryStartPending whenever ANY session's turn ends
// (design §4.1): a Run queued as session_busy behind a busy target session
// starts the moment that turn finishes, instead of waiting for the
// Scheduler's next timer sweep (up to maxWake = 1m away) — round-3 M1. The
// Manager's global bus closes on shutdown, ending this loop; it is
// deliberately NOT on d.wg because it persists nothing Wait must drain. A
// lagged subscription re-subscribes from the last seen Seq: a missed
// turn_end only delays the wake to the scheduler's periodic sweep, never
// correctness.
func (d *ScheduleDispatcher) watchTurnEnd() {
	var since int64 = -1
	for {
		sub, backlog, gap := d.m.SubscribeAll(since)
		if !gap {
			for _, e := range backlog {
				if e.Seq > since {
					since = e.Seq
				}
				if e.Type == EventTurnEnd {
					d.wakePending()
				}
			}
		}
		for e := range sub.C {
			if e.Seq > since {
				since = e.Seq
			}
			if e.Type == EventTurnEnd {
				d.wakePending()
			}
		}
		if !sub.Lagged() {
			return // bus closed: shutdown
		}
	}
}

func (d *ScheduleDispatcher) wakePending() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.svc.TryStartPending(ctx); err != nil {
		d.log.Warn("schedule: wake pending on turn_end", "err", err)
	}
	d.svc.Kick()
}

// scheduleObserver publishes schedule_* events on the global "*" bus. The
// execution process itself is deliberately NOT mirrored here: clients
// subscribe to run.session_id and see the ordinary turn events.
type scheduleObserver struct {
	m     *Manager
	svc   *schedule.Service
	mu    sync.Mutex
	last  map[string]schedule.RunStatus
	meta  map[string]scheduleMeta
}

type scheduleMeta struct {
	name   string
	notify string
}

func (o *scheduleObserver) ScheduleChanged(sch schedule.Schedule, kind schedule.ChangeKind) {
	if kind != schedule.ChangeDeleted && sch.ID != "" {
		o.mu.Lock()
		if o.meta == nil {
			o.meta = map[string]scheduleMeta{}
		}
		o.meta[sch.ID] = scheduleMeta{name: sch.Name, notify: sch.Options.Notify}
		o.mu.Unlock()
	}
	typ := EventScheduleUpdated
	switch kind {
	case schedule.ChangeCreated:
		typ = EventScheduleCreated
	case schedule.ChangeDeleted:
		typ = EventScheduleDeleted
	}
	o.m.global.publish(Event{Type: typ, Schedule: &sch, Time: time.Now().UTC()})

	if kind == schedule.ChangeCreated && sch.State == schedule.StatePendingApproval &&
		o.m.opts.Hook != nil && sch.Options.Notify != schedule.NotifyNever {
		summary := sch.Summary
		if summary == "" {
			summary = sch.Trigger.Summary(sch.Timezone)
		}
		o.m.opts.Hook.Fire(context.Background(), hook.Event{
			Kind:       hook.EventScheduleApproval,
			SessionID:  sch.CreatedBy.SessionID,
			WorkDir:    o.m.opts.WorkDir,
			ScheduleID: sch.ID,
			Message:    fmt.Sprintf("模型请求创建任务「%s」：%s", sch.Name, summary),
		})
	}
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

	if run.Status.Terminal() && o.m.opts.Hook != nil && o.svc != nil {
		// Dispatch asynchronously: RunChanged is called while Service.mu is
		// held, so we must NOT synchronously call back into Service.Get.
		go o.notifyRunEnd(run)
	}
}

func (o *scheduleObserver) notifyRunEnd(run schedule.Run) {
	var name, notify string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if sch, err := o.svc.Get(ctx, run.ScheduleID); err == nil {
		name, notify = sch.Name, sch.Options.Notify
		o.mu.Lock()
		if o.meta == nil {
			o.meta = map[string]scheduleMeta{}
		}
		o.meta[run.ScheduleID] = scheduleMeta{name: name, notify: notify}
		o.mu.Unlock()
	} else {
		// Fall back to cached metadata: Delete soft-deletes the schedule before
		// this async goroutine runs, causing Get to return not_found.
		o.mu.Lock()
		cached, ok := o.meta[run.ScheduleID]
		o.mu.Unlock()
		if !ok {
			o.m.log.Warn("bot: lookup schedule for run_end notification failed", "schedule", run.ScheduleID, "run", run.ID, "err", err)
			return
		}
		name, notify = cached.name, cached.notify
	}
	if !shouldNotifyRunEnd(notify, run.Status) {
		return
	}
	msg := formatScheduleRunMessage(name, run)
	o.m.opts.Hook.Fire(context.Background(), hook.Event{
		Kind:       hook.EventScheduleRunEnd,
		SessionID:  run.SessionID,
		WorkDir:    o.m.opts.WorkDir,
		ScheduleID: run.ScheduleID,
		RunID:      run.ID,
		Message:    msg,
	})
}

// shouldNotifyRunEnd implements the notify policy matrix in design §10:
//   - skipped: never notifies
//   - never: never notifies
//   - on_failure: failed, timed_out, missed
//   - always (default): completed, failed, timed_out, missed, cancelled
func shouldNotifyRunEnd(notify string, status schedule.RunStatus) bool {
	if status == schedule.RunSkipped || notify == schedule.NotifyNever {
		return false
	}
	switch notify {
	case schedule.NotifyOnFailure:
		return status == schedule.RunFailed || status == schedule.RunTimedOut || status == schedule.RunMissed
	default: // NotifyAlways or empty
		return status == schedule.RunCompleted || status == schedule.RunFailed ||
			status == schedule.RunTimedOut || status == schedule.RunMissed || status == schedule.RunCancelled
	}
}

func formatScheduleRunMessage(name string, run schedule.Run) string {
	switch run.Status {
	case schedule.RunCompleted:
		if out := strings.TrimSpace(run.Output); out != "" {
			return fmt.Sprintf("「%s」完成：%s", name, truncateRunes(out, 500))
		}
		return fmt.Sprintf("「%s」完成", name)
	case schedule.RunFailed:
		return fmt.Sprintf("「%s」失败：%s", name, truncateRunes(firstNonEmpty(run.Error, run.ErrorCode, "unknown error"), 500))
	case schedule.RunTimedOut:
		return fmt.Sprintf("「%s」超时：%s", name, truncateRunes(firstNonEmpty(run.Error, "timed_out"), 500))
	case schedule.RunMissed:
		return fmt.Sprintf("「%s」错过执行（%s）", name, firstNonEmpty(run.ErrorCode, "missed"))
	case schedule.RunCancelled:
		return fmt.Sprintf("「%s」已取消", name)
	default:
		return fmt.Sprintf("「%s」%s", name, run.Status)
	}
}

func hookSummary(prefix, output, errText string) string {
	body := firstNonEmpty(strings.TrimSpace(errText), strings.TrimSpace(output))
	if body == "" {
		return prefix
	}
	return prefix + "：" + truncateRunes(body, 500)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
		out = finalizeOutcome(out, sum, d.m.Closing(), hooks.failureReason(), reason, timeout)
		if _, err := d.svc.FinishRun(context.Background(), run.ID, out); err != nil {
			d.log.Warn("bot: finish schedule run", "run", run.ID, "err", err)
		}
	}()
	return sessionID, turn.ID, nil
}

// finalizeOutcome attributes a cancelled turn to its first cause. A genuine
// shutdown cancellation carries cancel_source=shutdown; only an unattributed
// cancellation that happens while the Manager is closing falls back to
// bot_shutdown, so a client cancellation started before the shutdown keeps
// its cancelled state (review L4).
func finalizeOutcome(out schedule.Outcome, sum *TurnSummary, closing bool, failure string, reason schedule.CancelReason, timeout time.Duration) schedule.Outcome {
	switch {
	case failure != "" && out.Status == schedule.RunCancelled:
		out.Status, out.ErrorCode, out.Error = schedule.RunFailed, "answer_timeout", failure
	case sum != nil && sum.CancelSource == string(cancelSourceShutdown):
		out.Status, out.ErrorCode = schedule.RunFailed, "bot_shutdown"
		out.Error = "bot shut down while the run was active"
	case sum != nil && sum.CancelSource == "" && closing && out.Status == schedule.RunCancelled:
		out.Status, out.ErrorCode = schedule.RunFailed, "bot_shutdown"
		out.Error = "bot shut down while the run was active"
	case reason == schedule.CancelReplaced && out.Status == schedule.RunCancelled:
		out.ErrorCode, out.Error = "replaced", "replaced by a newer run"
	}
	if out.Status == schedule.RunTimedOut && out.Error == "" {
		out.Error = "run exceeded " + timeout.String()
	}
	return out
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
