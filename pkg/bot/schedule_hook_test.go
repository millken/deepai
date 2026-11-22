package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/schedule"
)

type recordingEmitter struct {
	mu     sync.Mutex
	events []hook.Event
}

func (r *recordingEmitter) Fire(_ context.Context, evt hook.Event) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
}

func (r *recordingEmitter) waitFor(t *testing.T, kind hook.Kind) hook.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.events {
			if e.Kind == kind {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("timed out waiting for hook event %q; got %+v", kind, r.events)
	return hook.Event{}
}

func (r *recordingEmitter) count(kind hook.Kind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestScheduleNotifyPolicyMatrix(t *testing.T) {
	cases := []struct {
		notify string
		status schedule.RunStatus
		want   bool
	}{
		{schedule.NotifyAlways, schedule.RunCompleted, true},
		{schedule.NotifyAlways, schedule.RunFailed, true},
		{schedule.NotifyAlways, schedule.RunTimedOut, true},
		{schedule.NotifyAlways, schedule.RunMissed, true},
		{schedule.NotifyAlways, schedule.RunCancelled, true},
		{schedule.NotifyAlways, schedule.RunSkipped, false},

		{schedule.NotifyOnFailure, schedule.RunCompleted, false},
		{schedule.NotifyOnFailure, schedule.RunCancelled, false},
		{schedule.NotifyOnFailure, schedule.RunSkipped, false},
		{schedule.NotifyOnFailure, schedule.RunFailed, true},
		{schedule.NotifyOnFailure, schedule.RunTimedOut, true},
		{schedule.NotifyOnFailure, schedule.RunMissed, true},

		{schedule.NotifyNever, schedule.RunCompleted, false},
		{schedule.NotifyNever, schedule.RunFailed, false},
		{schedule.NotifyNever, schedule.RunTimedOut, false},
		{schedule.NotifyNever, schedule.RunMissed, false},
	}
	for _, tc := range cases {
		if got := shouldNotifyRunEnd(tc.notify, tc.status); got != tc.want {
			t.Errorf("shouldNotifyRunEnd(%q, %q) = %v, want %v", tc.notify, tc.status, got, tc.want)
		}
	}
}

func TestScheduleHookEvents(t *testing.T) {
	ctx := context.Background()
	emitter := &recordingEmitter{}
	m, err := NewManager(Options{
		Runtime: &questionRuntime{fakeRuntime: &fakeRuntime{}, questions: 1},
		Store:   NewMemoryStore(),
		WorkDir: "/tmp/proj",
		Hook:    emitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Close(cctx)
	})

	cfg := schedule.DefaultConfig()
	cfg.Confirm = "auto"
	cfg.Timezone = "UTC"
	svc, err := schedule.NewService(schedule.NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	AttachSchedule(m, svc)

	// 1. schedule_approval on pending_approval creation
	pending, err := svc.Create(ctx, schedule.CreateInput{
		Name: "嵌套日报", Prompt: "总结",
		Trigger:  schedule.Trigger{Type: schedule.TriggerCron, Expr: "0 22 * * *"},
		Timezone: "UTC",
		State:    schedule.StatePendingApproval,
		Options:  schedule.Options{Notify: schedule.NotifyAlways},
	})
	if err != nil {
		t.Fatal(err)
	}
	evApproval := emitter.waitFor(t, hook.EventScheduleApproval)
	if evApproval.ScheduleID != pending.ID || !strings.Contains(evApproval.Message, "嵌套日报") {
		t.Fatalf("approval event = %+v", evApproval)
	}

	// 2. schedule_ask + schedule_run_end via ask + auto-answer
	sch, err := svc.Create(ctx, schedule.CreateInput{
		Name: "值守巡检", Prompt: "ask",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC",
		Options: schedule.Options{
			Notify:          schedule.NotifyAlways,
			AnswerTimeout:   "40ms",
			OnAnswerTimeout: schedule.OnAnswerTimeoutAssume,
			AutoAnswers:     1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-hook-1")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, run.ID, schedule.RunCompleted)

	evAsk := emitter.waitFor(t, hook.EventScheduleAsk)
	if evAsk.ScheduleID != sch.ID || evAsk.RunID != run.ID || !strings.Contains(evAsk.Message, "值守巡检") {
		t.Fatalf("ask event = %+v", evAsk)
	}
	evEnd := emitter.waitFor(t, hook.EventScheduleRunEnd)
	if evEnd.ScheduleID != sch.ID || evEnd.RunID != run.ID || !strings.Contains(evEnd.Message, "完成") {
		t.Fatalf("run_end event = %+v", evEnd)
	}

	// 3. notify=never emits no run_end
	quiet, err := svc.Create(ctx, schedule.CreateInput{
		Name: "静默任务", Prompt: "hello",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC",
		Options: schedule.Options{
			Notify:          schedule.NotifyNever,
			AnswerTimeout:   "20ms",
			OnAnswerTimeout: schedule.OnAnswerTimeoutAssume,
			AutoAnswers:     1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeCount := emitter.count(hook.EventScheduleRunEnd)
	qrun, err := svc.RunNow(ctx, quiet.ID, "req-quiet")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, qrun.ID, schedule.RunCompleted)
	time.Sleep(50 * time.Millisecond)
	if after := emitter.count(hook.EventScheduleRunEnd); after != beforeCount {
		t.Fatalf("notify=never emitted run_end: before=%d after=%d", beforeCount, after)
	}

	// 4. deleting a schedule while a run is active still delivers cancelled run_end
	// via cached metadata even after the schedule is soft-deleted (PR #49 L4).
	delSch, err := svc.Create(ctx, schedule.CreateInput{
		Name: "删前取消", Prompt: "ask",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC",
		Options:  schedule.Options{Notify: schedule.NotifyAlways, AnswerTimeout: "10s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	delRun, err := svc.RunNow(ctx, delSch.ID, "req-del-cancel")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, delRun.ID, schedule.RunWaitingInput)
	beforeDel := emitter.count(hook.EventScheduleRunEnd)
	if _, err := svc.Delete(ctx, delSch.ID, true); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, delRun.ID, schedule.RunCancelled)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && emitter.count(hook.EventScheduleRunEnd) == beforeDel {
		time.Sleep(15 * time.Millisecond)
	}
	if emitter.count(hook.EventScheduleRunEnd) <= beforeDel {
		t.Fatalf("deleting active schedule lost cancelled schedule_run_end notification")
	}
}

// PR #49 round-2 L4: after a restart (fresh Manager + AttachSchedule over an
// existing Store), deleting a pre-existing schedule while a run is active must
// still deliver the cancelled schedule_run_end notification even though the
// new process never saw ScheduleChanged for that schedule.
func TestDeletePreExistingScheduleAfterRestartNotifiesRunEnd(t *testing.T) {
	ctx := context.Background()
	sharedStore := schedule.NewMemoryStore()
	cfg := schedule.DefaultConfig()
	cfg.Confirm = "auto"
	cfg.Timezone = "UTC"

	// Process 1 creates the schedule and exits.
	svc1, err := schedule.NewService(sharedStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := svc1.Create(ctx, schedule.CreateInput{
		Name: "重启前任务", Prompt: "ask",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC",
		Options:  schedule.Options{Notify: schedule.NotifyAlways, AnswerTimeout: "10s"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Process 2 starts fresh over the same store and attaches observer.
	emitter := &recordingEmitter{}
	m2, err := NewManager(Options{
		Runtime: &questionRuntime{fakeRuntime: &fakeRuntime{}, questions: 1},
		Store:   NewMemoryStore(),
		WorkDir: "/tmp/restart",
		Hook:    emitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m2.Close(cctx)
	})

	svc2, err := schedule.NewService(sharedStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	AttachSchedule(m2, svc2)

	run, err := svc2.RunNow(ctx, pre.ID, "req-restart-del")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc2, run.ID, schedule.RunWaitingInput)

	if _, err := svc2.Delete(ctx, pre.ID, true); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc2, run.ID, schedule.RunCancelled)

	ev := emitter.waitFor(t, hook.EventScheduleRunEnd)
	if ev.ScheduleID != pre.ID || ev.RunID != run.ID || !strings.Contains(ev.Message, "重启前任务") || !strings.Contains(ev.Message, "已取消") {
		t.Fatalf("unexpected restart-delete run_end event = %+v", ev)
	}
}

// End-to-end test: cron schedule created over REST -> real Scheduler with fake
// clock fires when cron slot arrives -> fakeRuntime asks a question ->
// auto-answer completes the run -> real hook.Dispatcher delivers webhook JSON.
func TestScheduleP2EndToEndCronAndWebhook(t *testing.T) {
	var (
		mu       sync.Mutex
		received []hook.Event
	)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var evt hook.Event
		if err := json.NewDecoder(r.Body).Decode(&evt); err == nil {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	hookDisp := hook.NewDispatcher([]hook.NotificationConfig{{
		Events:     []string{string(hook.EventScheduleAsk), string(hook.EventScheduleRunEnd)},
		WebhookURL: webhook.URL,
	}})

	m, err := NewManager(Options{
		Runtime: &questionRuntime{fakeRuntime: &fakeRuntime{}, questions: 1},
		Store:   NewMemoryStore(),
		WorkDir: "/tmp/e2e",
		Hook:    hookDisp,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Close(cctx)
	})

	start := time.Date(2026, 11, 7, 21, 59, 30, 0, time.UTC)
	clk := &e2eClock{now: start}
	cfg := schedule.DefaultConfig()
	cfg.Confirm = "auto"
	cfg.Timezone = "UTC"
	svc, err := schedule.NewService(schedule.NewMemoryStore(), cfg, schedule.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	disp := AttachSchedule(m, svc)
	sched := schedule.NewScheduler(svc, disp, schedule.SchedulerOptions{Clock: clk})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, sc := context.WithTimeout(context.Background(), 3*time.Second)
		defer sc()
		_ = sched.Stop(sctx)
	}()

	srv := NewServer(ServerOptions{Manager: m, Schedules: svc})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Create a cron task via REST for 22:00 UTC (30s from fake clock start).
	body := `{"name":"晚间汇总","prompt":"ask","trigger":{"type":"cron","expr":"0 22 * * *"},"timezone":"UTC","answer_timeout":"30ms","on_answer_timeout":"assume","notify":"always"}`
	resp, err := http.Post(ts.URL+"/v1/schedules", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/schedules status = %d", resp.StatusCode)
	}
	var created struct {
		Schedule schedule.Schedule `json:"schedule"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Schedule.NextRunAt == nil || created.Schedule.NextRunAt.Format(time.RFC3339) != "2026-11-07T22:00:00Z" {
		t.Fatalf("cron next_run_at = %v, want 2026-11-07T22:00:00Z", created.Schedule.NextRunAt)
	}

	// Advance fake clock past 22:00:00Z and let Scheduler fire.
	clk.advance(35 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	var doneRun schedule.Run
	for time.Now().Before(deadline) {
		runs, _ := svc.Runs(context.Background(), created.Schedule.ID, 5)
		if len(runs) > 0 && runs[0].Status == schedule.RunCompleted {
			doneRun = runs[0]
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if doneRun.Status != schedule.RunCompleted || doneRun.AutoAnswers != 1 {
		t.Fatalf("cron e2e run = %+v, want completed with auto_answers=1", doneRun)
	}

	// Poll until both webhook deliveries arrive (notifyRunEnd runs asynchronously
	// after RunChanged releases Service.mu).
	var sawAsk, sawEnd bool
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		hookDisp.WaitIdle(50 * time.Millisecond)
		mu.Lock()
		sawAsk, sawEnd = false, false
		for _, ev := range received {
			if ev.Kind == hook.EventScheduleAsk && ev.RunID == doneRun.ID {
				sawAsk = true
			}
			if ev.Kind == hook.EventScheduleRunEnd && ev.RunID == doneRun.ID {
				sawEnd = true
			}
		}
		mu.Unlock()
		if sawAsk && sawEnd {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !sawAsk || !sawEnd {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("webhook events = %+v, want both schedule_ask and schedule_run_end", received)
	}
}

type e2eClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []e2eWaiter
}

type e2eWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *e2eClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *e2eClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	target := c.now.Add(d)
	if !target.After(c.now) {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, e2eWaiter{at: target, ch: ch})
	return ch
}

func (c *e2eClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var keep []e2eWaiter
	for _, w := range c.waiters {
		if !w.at.After(now) {
			w.ch <- now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
}
