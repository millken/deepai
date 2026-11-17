package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
	_ "modernc.org/sqlite"
)

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type firedRun struct {
	sch Schedule
	run Run
}

type fakeDispatcher struct {
	mu        sync.Mutex
	fired     []firedRun
	cancelled []Run
	err       error
}

func (d *fakeDispatcher) Fire(_ context.Context, sch Schedule, run Run) (string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return "", "", d.err
	}
	d.fired = append(d.fired, firedRun{sch: sch, run: run})
	return "sess_" + run.ID, "turn_" + run.ID, nil
}

func (d *fakeDispatcher) Cancel(run Run, _ CancelReason) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancelled = append(d.cancelled, run)
	return nil
}

func (d *fakeDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.fired)
}

func (d *fakeDispatcher) last() firedRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fired[len(d.fired)-1]
}

func newTestService(t *testing.T, clock Clock, tweak func(*Config)) (*Service, *MemoryStore) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Timezone = "UTC"
	cfg.MinInterval = time.Second
	cfg.Confirm = "auto"
	if tweak != nil {
		tweak(&cfg)
	}
	store := NewMemoryStore()
	svc, err := NewService(store, cfg, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return svc, store
}

func TestTriggerNormalizeAndNextRuns(t *testing.T) {
	tr := Trigger{Type: TriggerOnce, In: "2h"}
	if err := tr.Normalize("Asia/Taipei", testNow, 5*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if tr.At != "2026-11-07T14:00:00Z" || tr.In != "" {
		t.Fatalf("once.in normalized to %+v", tr)
	}
	if runs := tr.NextRuns(testNow, 3); len(runs) != 1 || !runs[0].Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("once NextRuns = %v", runs)
	}

	local := Trigger{Type: TriggerOnce, At: "2026-11-07T22:00"}
	if err := local.Normalize("Asia/Taipei", testNow, 5*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if local.At != "2026-11-07T14:00:00Z" {
		t.Fatalf("local once.at = %s", local.At)
	}

	iv := Trigger{Type: TriggerInterval, Every: "6h", StartAt: "2026-11-08T09:00"}
	if err := iv.Normalize("Asia/Taipei", testNow, 5*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	runs := iv.NextRuns(testNow, 3)
	want := []string{"2026-11-08T01:00:00Z", "2026-11-08T07:00:00Z", "2026-11-08T13:00:00Z"}
	for i, w := range want {
		if i >= len(runs) || runs[i].Format(time.RFC3339) != w {
			t.Fatalf("interval NextRuns = %v, want %v", runs, want)
		}
	}

	if err := (&Trigger{Type: TriggerCron, Expr: "0 22 * * *"}).Normalize("UTC", testNow, time.Minute, false); ErrorCode(err) != CodeTriggerNotSupported {
		t.Fatalf("cron err = %v", err)
	}
	if err := (&Trigger{Type: TriggerInterval, Every: "1m"}).Normalize("UTC", testNow, 5*time.Minute, false); ErrorCode(err) != CodeIntervalTooShort {
		t.Fatalf("min interval err = %v", err)
	}
	if err := (&Trigger{Type: TriggerOnce, At: "2026-11-07T10:00"}).Normalize("UTC", testNow, time.Minute, false); ErrorCode(err) != CodeTimeInPast {
		t.Fatalf("past once err = %v", err)
	}
	if err := (&Trigger{Type: TriggerOnce, At: "2026-11-07T22:00"}).Normalize("Not/AZone", testNow, time.Minute, false); ErrorCode(err) != CodeInvalidTimezone {
		t.Fatalf("timezone err = %v", err)
	}
}

func TestServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)

	sch, err := svc.Create(ctx, CreateInput{
		Name: "每日总结", Prompt: "汇总今天的工作",
		Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sch.State != StateActive || sch.NextRunAt == nil || sch.Summary == "" || len(sch.NextRuns) != 1 {
		t.Fatalf("created = %+v", sch)
	}
	if list, err := svc.List(ctx, "", 0); err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if _, err := svc.Validate(ctx, Trigger{Type: TriggerCron, Expr: "* * * * *"}, "UTC", 3); ErrorCode(err) != CodeTriggerNotSupported {
		t.Fatalf("validate cron = %v", err)
	}

	paused, err := svc.Pause(ctx, sch.ID)
	if err != nil || paused.NextRunAt != nil || paused.State != StatePaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	resumed, err := svc.Resume(ctx, sch.ID)
	if err != nil || resumed.NextRunAt == nil {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}

	run, err := svc.RunNow(ctx, sch.ID, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunPending || run.Trigger != FireManual || run.Slot != nil {
		t.Fatalf("manual run = %+v", run)
	}
	again, err := svc.RunNow(ctx, sch.ID, "req-1")
	if err != nil || again.ID != run.ID {
		t.Fatalf("request_id dedupe = %+v, %v", again, err)
	}

	skipped, err := svc.Skip(ctx, sch.ID)
	if err != nil || skipped.Status != RunSkipped || skipped.Slot == nil {
		t.Fatalf("skip = %+v, %v", skipped, err)
	}
	runs, err := svc.Runs(ctx, sch.ID, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	if _, err := svc.Delete(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, sch.ID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("get deleted = %v", err)
	}
}

func TestSchedulerFiresOnceAndCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)
	d := &fakeDispatcher{}
	sched := NewScheduler(svc, d, SchedulerOptions{Clock: clock, SyncInterval: time.Hour})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sched.Stop(context.Background()) }()

	sch, err := svc.Create(ctx, CreateInput{
		Name: "提醒", Prompt: "检查 CI", Trigger: Trigger{Type: TriggerOnce, In: "10s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	eventually(t, func() bool { return d.count() == 1 })

	run, err := svc.GetRun(ctx, d.last().run.ID)
	if err != nil || run.Status != RunRunning || run.SessionID == "" || run.TurnID == "" {
		t.Fatalf("fired run = %+v, %v", run, err)
	}
	if _, err := svc.FinishRun(ctx, run.ID, Outcome{Status: RunCompleted, Output: "CI 通过"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.RunCount != 1 || got.LastRun == nil || got.LastRun.Status != RunCompleted {
		t.Fatalf("after finish = %+v", got)
	}
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if d.count() != 1 {
		t.Fatalf("once fired %d times", d.count())
	}
}

func TestSchedulerIntervalOverlap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overlap    string
		wantSecond RunStatus
	}{
		{"skip", OverlapSkip, RunSkipped},
		{"queue", OverlapQueue, RunPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newFakeClock(testNow)
			svc, _ := newTestService(t, clock, nil)
			d := &fakeDispatcher{}
			sched := NewScheduler(svc, d, SchedulerOptions{Clock: clock, SyncInterval: time.Hour})
			if err := sched.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sched.Stop(context.Background()) }()

			_, err := svc.Create(ctx, CreateInput{
				Name: "周期", Prompt: "每 10 秒",
				Trigger: Trigger{Type: TriggerInterval, Every: "10s", StartAt: testNow.Add(5 * time.Second).Format(time.RFC3339)},
				Options: Options{Overlap: tc.overlap},
			})
			if err != nil {
				t.Fatal(err)
			}
			clock.Advance(6 * time.Second)
			eventually(t, func() bool { return d.count() == 1 })
			first := d.last().run

			clock.Advance(10 * time.Second)
			eventually(t, func() bool {
				runs, _ := svc.Runs(ctx, first.ScheduleID, 10)
				return len(runs) == 2
			})
			runs, _ := svc.Runs(ctx, first.ScheduleID, 10)
			var second Run
			for _, r := range runs {
				if r.ID != first.ID {
					second = r
				}
			}
			if second.Status != tc.wantSecond {
				t.Fatalf("second run = %+v, want %s", second, tc.wantSecond)
			}
			if tc.overlap == OverlapSkip && second.ErrorCode != "overlap" {
				t.Fatalf("skip reason = %q", second.ErrorCode)
			}
			if _, err := svc.FinishRun(ctx, first.ID, Outcome{Status: RunCompleted}); err != nil {
				t.Fatal(err)
			}
			if tc.overlap == OverlapQueue {
				eventually(t, func() bool { return d.count() == 2 })
				started, _ := svc.GetRun(ctx, second.ID)
				if started.Status != RunRunning {
					t.Fatalf("queued run after finish = %+v", started)
				}
			}
		})
	}
}

func TestSchedulerSessionBusyRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)
	d := &fakeDispatcher{err: ErrBusy}
	sched := NewScheduler(svc, d, SchedulerOptions{Clock: clock, SyncInterval: time.Hour})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sched.Stop(context.Background()) }()

	sch, err := svc.Create(ctx, CreateInput{
		Name: "忙会话", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "5s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Second)
	eventually(t, func() bool {
		runs, _ := svc.Runs(ctx, sch.ID, 10)
		return len(runs) == 1 && runs[0].QueuedReason == "session_busy"
	})
	d.mu.Lock()
	d.err = nil
	d.mu.Unlock()
	if err := svc.TryStartPending(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := svc.Runs(ctx, sch.ID, 10)
	if runs[0].Status != RunRunning {
		t.Fatalf("run after retry = %+v", runs[0])
	}
}

func TestRecoverMarksMissedSlots(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, nil)
	past := testNow.Add(-time.Hour)
	v := Schedule{
		ID: "sch_past", Name: "错过", Prompt: "补跑检查",
		Trigger:  Trigger{Type: TriggerOnce, At: past.Format(time.RFC3339)},
		Timezone: "UTC", State: StateActive, CreatedAt: past, UpdatedAt: past,
		NextRunAt: &past, Options: Options{SessionMode: SessionDedicated, Overlap: OverlapSkip},
	}
	if err := store.InsertSchedule(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := svc.Runs(ctx, v.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != RunMissed {
		t.Fatalf("recovered runs = %v, %v", runs, err)
	}
	got, err := store.GetSchedule(ctx, v.ID)
	if err != nil || got.NextRunAt != nil {
		t.Fatalf("recovered schedule = %+v, %v", got, err)
	}
}

type fakeUI struct {
	answer string
	err    error
}

func (u fakeUI) AskQuestion(context.Context, string, []string) (string, error) {
	return u.answer, u.err
}

func TestToolActionsAndConfirmation(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, func(c *Config) { c.Confirm = "ask" })
	tool := Tool(svc)
	ctx = tools.WithThreadID(ctx, "sess_1")
	ctx = tools.WithTurnID(ctx, "turn_1")

	call := func(args map[string]any) (models.ToolResult, error) {
		return tool.Handler(ctx, models.ToolCall{ID: "call_1", Name: "schedule", Arguments: args})
	}
	res, err := call(map[string]any{
		"action": "create", "name": "提醒", "prompt": "两小时后提醒我",
		"trigger": map[string]any{"type": "once", "in": "2h"}, "timezone": "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["scheduler"] != "active" || payload["schedule"] == nil || payload["now"] == nil {
		t.Fatalf("tool payload = %v", payload)
	}

	if _, err := call(map[string]any{
		"action": "create", "name": "cron", "prompt": "x",
		"trigger": map[string]any{"type": "cron", "expr": "0 22 * * *"},
	}); ErrorCode(err) != CodeTriggerNotSupported {
		t.Fatalf("cron tool err = %v", err)
	}

	nested := tools.WithScheduleRun(ctx, "sch_parent", "run_parent")
	if _, err := tool.Handler(nested, models.ToolCall{
		ID: "call_2", Name: "schedule",
		Arguments: map[string]any{"action": "create", "name": "nested", "prompt": "x",
			"trigger": map[string]any{"type": "once", "in": "2h"}},
	}); ErrorCode(err) != CodeNotAllowedInScheduled {
		t.Fatalf("nested create err = %v", err)
	}

	periodic := map[string]any{
		"action": "create", "name": "每小时", "prompt": "检查状态",
		"trigger": map[string]any{"type": "interval", "every": "1h"}, "timezone": "UTC",
	}
	cancelCtx := tools.WithUserInteraction(ctx, fakeUI{answer: "取消"})
	if _, err := tool.Handler(cancelCtx, models.ToolCall{ID: "c3", Name: "schedule", Arguments: periodic}); ErrorCode(err) != CodeConfirmationMissing {
		t.Fatalf("cancelled confirmation err = %v", err)
	}
	okCtx := tools.WithUserInteraction(ctx, fakeUI{answer: "确认创建"})
	if _, err := tool.Handler(okCtx, models.ToolCall{ID: "c4", Name: "schedule", Arguments: periodic}); err != nil {
		t.Fatalf("confirmed create = %v", err)
	}
	list, err := svc.List(ctx, "", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list after confirm = %v, %v", list, err)
	}
}

func TestSQLiteStoreRoundTripAndLease(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewSQLiteStore(db)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := testNow
	next := now.Add(time.Hour)
	sch := Schedule{
		ID: "sch_1", Name: "任务", Prompt: "执行", Timezone: "UTC",
		Trigger: Trigger{Type: TriggerOnce, At: next.Format(time.RFC3339)},
		Options: Options{SessionMode: SessionDedicated, Overlap: OverlapSkip, Timeout: "30m"},
		State:   StateActive, CreatedBy: CreatedBy{Kind: "user"},
		NextRunAt: &next, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertSchedule(ctx, sch); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != sch.Name || got.Trigger.At != sch.Trigger.At || got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Fatalf("sqlite schedule = %+v", got)
	}
	run := Run{
		ID: "run_1", ScheduleID: sch.ID, Seq: 1, Trigger: FireTimer,
		Slot: &next, ScheduledFor: next, Status: RunPending,
	}
	if err := store.InsertRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	dup := run
	dup.ID = "run_2"
	if err := store.InsertRun(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate slot err = %v", err)
	}
	loaded, err := store.GetRun(ctx, run.ID)
	if err != nil || loaded.Status != RunPending || loaded.Slot == nil {
		t.Fatalf("sqlite run = %+v, %v", loaded, err)
	}
	loaded.Status = RunRunning
	if err := store.UpdateRun(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	active, err := store.FindRun(ctx, RunFilter{Statuses: []RunStatus{RunRunning}})
	if err != nil || len(active) != 1 {
		t.Fatalf("find active = %v, %v", active, err)
	}
	if ok, err := store.AcquireLease(ctx, "owner-a", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("acquire lease = %v, %v", ok, err)
	}
	// The second process cannot take an unexpired lease.
	if ok, err := store.AcquireLease(ctx, "owner-b", now, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("second lease = %v, %v", ok, err)
	}
	later := now.Add(2 * time.Minute)
	if ok, err := store.AcquireLease(ctx, "owner-b", later, later.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("expired lease takeover = %v, %v", ok, err)
	}
	if err := store.ReleaseLease(ctx, "owner-b"); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRunsKeepsRecentHistory(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	for i := 1; i <= 4; i++ {
		at := testNow.Add(time.Duration(i) * time.Minute)
		if err := store.InsertRun(ctx, Run{
			ID: "run_" + string(rune('a'+i)), ScheduleID: "sch_1", Seq: i,
			Trigger: FireTimer, Slot: &at, ScheduledFor: at,
			Status: RunCompleted, EndedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.InsertRun(ctx, Run{
		ID: "run_pending", ScheduleID: "sch_1", Seq: 5,
		Trigger: FireManual, ScheduledFor: testNow, Status: RunPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneRuns(ctx, "sch_1", 2); err != nil {
		t.Fatal(err)
	}
	runs, err := store.FindRun(ctx, RunFilter{ScheduleID: "sch_1"})
	if err != nil || len(runs) != 3 { // 2 newest terminal + the pending one
		t.Fatalf("after prune = %v, %v", runs, err)
	}
	for _, r := range runs {
		if r.ID == "run_b" || r.ID == "run_c" {
			t.Fatalf("old run %s survived prune", r.ID)
		}
	}
}

func TestStandbyNeverFiresSlots(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)
	sch, err := svc.Create(ctx, CreateInput{
		Name: "备用进程", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.setMode("standby")
	if err := svc.FireSlot(ctx, sch.ID, *sch.NextRunAt); err != nil {
		t.Fatal(err)
	}
	runs, _ := svc.Runs(ctx, sch.ID, 10)
	if len(runs) != 0 {
		t.Fatalf("standby created %d runs", len(runs))
	}
}

func TestRecoverInterruptedOneShotCompletes(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, nil)
	svc.SetDispatcher(&fakeDispatcher{})
	sch, err := svc.Create(ctx, CreateInput{
		Name: "一次性", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-restart")
	if err != nil {
		t.Fatal(err)
	}
	started, _ := svc.GetRun(ctx, run.ID)
	if started.Status != RunRunning {
		t.Fatalf("run = %+v", started)
	}
	// A fresh Service over the same store models a restarted bot process.
	restarted, err := NewService(store, svc.Config(), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(ctx, sch.ID)
	if err != nil || got.State != StateCompleted {
		t.Fatalf("recovered schedule = %+v, %v", got, err)
	}
	failed, _ := restarted.GetRun(ctx, run.ID)
	if failed.Status != RunFailed || failed.ErrorCode != "bot_restarted" {
		t.Fatalf("recovered run = %+v", failed)
	}
}

func TestStandbyCancelRequestIsRelayed(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)
	d := &fakeDispatcher{}
	svc.SetDispatcher(d)
	sch, err := svc.Create(ctx, CreateInput{
		Name: "跨进程取消", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-cancel")
	if err != nil {
		t.Fatal(err)
	}
	started, _ := svc.GetRun(ctx, run.ID)
	if started.Status != RunRunning {
		t.Fatalf("run = %+v", started)
	}
	svc.setMode("standby")
	cancelled, err := svc.CancelRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != RunRunning || cancelled.CancelRequestedAt == nil {
		t.Fatalf("standby cancel = %+v", cancelled)
	}
	svc.setMode("active")
	if err := svc.ProcessCancelRequests(ctx); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cancelled) != 1 || d.cancelled[0].ID != run.ID {
		t.Fatalf("relayed cancels = %v", d.cancelled)
	}
}

func TestErrorCodesAndStatus(t *testing.T) {
	if HTTPStatus(NewError(CodeNotFound, "x")) != 404 {
		t.Fatal("not_found should be 404")
	}
	if HTTPStatus(NewError(CodeRunInProgress, "x")) != 409 {
		t.Fatal("run_in_progress should be 409")
	}
	if got := ErrorCode(ErrBusy); got != CodeRunInProgress {
		t.Fatalf("ErrBusy code = %s", got)
	}
	if !strings.Contains(newID("sch_"), "sch_") {
		t.Fatal("id prefix missing")
	}
}
