package schedule

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the round-1 review of PR #43 (H1/H2, M1–M4, L1/L2).

type scriptedLease struct {
	mu           sync.Mutex
	acquireOK    bool
	renewOK      bool
	renewErr     error
	renewCalls   int
	acquireCalls int
}

func (l *scriptedLease) AcquireLease(_ context.Context, _ string, _, _ time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquireCalls++
	return l.acquireOK, nil
}

func (l *scriptedLease) RenewLease(_ context.Context, _ string, _ time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewCalls++
	if l.renewErr != nil {
		return false, l.renewErr
	}
	return l.renewOK, nil
}

func (l *scriptedLease) ReleaseLease(context.Context, string) error { return nil }

func (l *scriptedLease) setRenew(ok bool, err error) {
	l.mu.Lock()
	l.renewOK, l.renewErr = ok, err
	l.mu.Unlock()
}

func (l *scriptedLease) renewCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renewCalls
}

type countingClock struct {
	*fakeClock
	calls atomic.Int64
}

func (c *countingClock) After(d time.Duration) <-chan time.Time {
	c.calls.Add(1)
	return c.fakeClock.After(d)
}

type observingDispatcher struct {
	fakeDispatcher
	store        *MemoryStore
	statusAtFire RunStatus
}

func (d *observingDispatcher) Fire(ctx context.Context, sch Schedule, run Run) (string, string, error) {
	if r, err := d.store.GetRun(ctx, run.ID); err == nil {
		d.statusAtFire = r.Status
	}
	return d.fakeDispatcher.Fire(ctx, sch, run)
}

type failingInsertStore struct {
	*MemoryStore
	err error
}

func (s *failingInsertStore) InsertRun(ctx context.Context, r Run) error {
	if s.err != nil {
		return s.err
	}
	return s.MemoryStore.InsertRun(ctx, r)
}

func activeIntervalSchedule(id string, next time.Time, timeout string) Schedule {
	return Schedule{
		ID: id, Name: "回归", Prompt: "检查",
		Trigger:   Trigger{Type: TriggerInterval, Every: "1h"},
		Timezone:  "UTC",
		Options:   Options{SessionMode: SessionDedicated, Overlap: OverlapSkip, Timeout: timeout},
		State:     StateActive,
		CreatedAt: testNow, UpdatedAt: testNow, NextRunAt: &next,
	}
}

// H1: a transient renewal error (SQLITE_BUSY) must not turn the lease holder
// into standby; only a confirmed takeover or an error lasting past the TTL.
func TestRenewErrorKeepsActiveUntilLeaseExpires(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)
	lease := &scriptedLease{renewOK: true}
	sched := NewScheduler(svc, &fakeDispatcher{}, SchedulerOptions{
		Clock: clock, Lease: lease, LeaseTTL: time.Minute,
	})
	sched.lastRenew = testNow

	lease.setRenew(false, errors.New("SQLITE_BUSY"))
	sched.syncLease(ctx)
	if svc.Mode() != "active" {
		t.Fatalf("mode after transient renew error = %s, want active", svc.Mode())
	}
	if lease.renewCount() != 1 {
		t.Fatalf("renew calls = %d", lease.renewCount())
	}

	lease.setRenew(true, nil)
	clock.Advance(5 * time.Second)
	sched.syncLease(ctx)
	if !sched.lastRenew.Equal(testNow.Add(5 * time.Second)) {
		t.Fatalf("lastRenew = %s after successful renew", sched.lastRenew)
	}

	lease.setRenew(false, nil) // ok == false: another owner really took over
	sched.syncLease(ctx)
	if svc.Mode() != "standby" {
		t.Fatalf("mode after confirmed takeover = %s, want standby", svc.Mode())
	}

	svc.setMode("active")
	sched.lastRenew = clock.Now().Add(-2 * time.Minute)
	lease.setRenew(false, errors.New("SQLITE_BUSY"))
	sched.syncLease(ctx)
	if svc.Mode() != "standby" {
		t.Fatalf("mode after an error lasting past the TTL = %s, want standby", svc.Mode())
	}
}

// H1: the takeover process must not declare a still-executing Run dead; it
// gets reconciled only after the original owner's grace period expires.
func TestRecoverAfterTakeoverDefersYoungRuns(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, func(c *Config) { c.DefaultTimeout = time.Minute })
	sch := activeIntervalSchedule("sch_live", testNow.Add(time.Hour), "1m")
	if err := store.InsertSchedule(ctx, sch); err != nil {
		t.Fatal(err)
	}
	started := testNow.Add(-10 * time.Second)
	r := Run{
		ID: "run_live", ScheduleID: sch.ID, Seq: 1, Trigger: FireTimer,
		ScheduledFor: testNow.Add(-time.Minute), Status: RunRunning,
		StartedAt: &started, SessionID: "sess_a", TurnID: "turn_a",
	}
	if err := store.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	leaseTTL := 30 * time.Second
	if err := svc.RecoverAfterTakeover(ctx, leaseTTL); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetRun(ctx, r.ID)
	if err != nil || got.Status != RunRunning {
		t.Fatalf("young run after takeover = %+v, %v", got, err)
	}

	clock.Advance(30 * time.Second)
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.GetRun(ctx, r.ID); got.Status != RunRunning {
		t.Fatalf("run before its deadline = %+v", got)
	}

	clock.Advance(61 * time.Second) // past StartedAt + leaseTTL + timeout
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.GetRun(ctx, r.ID); got.Status != RunFailed || got.ErrorCode != "bot_restarted" {
		t.Fatalf("run after its deadline = %+v", got)
	}
}

// H1: a late FinishRun from the original holder wins over reconciliation.
func TestReconcileRespectsLateFinishRun(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, func(c *Config) { c.DefaultTimeout = time.Minute })
	sch := activeIntervalSchedule("sch_late", testNow.Add(time.Hour), "1m")
	if err := store.InsertSchedule(ctx, sch); err != nil {
		t.Fatal(err)
	}
	started := testNow.Add(-10 * time.Second)
	r := Run{
		ID: "run_late", ScheduleID: sch.ID, Seq: 1, Trigger: FireTimer,
		ScheduledFor: testNow.Add(-time.Minute), Status: RunRunning, StartedAt: &started,
	}
	if err := store.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverAfterTakeover(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinishRun(ctx, r.ID, Outcome{Status: RunCompleted, Output: "done"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Minute)
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetRun(ctx, r.ID); got.Status != RunCompleted {
		t.Fatalf("late FinishRun was overwritten: %+v", got)
	}
}

// M1: approving an expired one-shot must not strand it active with no slot.
func TestApproveRejectsExpiredOnce(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, nil)
	past := testNow.Add(-time.Hour)
	v := Schedule{
		ID: "sch_expired", Name: "过期", Prompt: "检查",
		Trigger:  Trigger{Type: TriggerOnce, At: past.Format(time.RFC3339)},
		Timezone: "UTC", State: StatePendingApproval,
		Options:   Options{SessionMode: SessionDedicated},
		CreatedAt: testNow, UpdatedAt: testNow, NextRunAt: &past,
	}
	if err := store.InsertSchedule(ctx, v); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(ctx, v.ID); ErrorCode(err) != CodeTimeInPast {
		t.Fatalf("approve expired once err = %v", err)
	}
	if got, _ := store.GetSchedule(ctx, v.ID); got.State != StatePendingApproval {
		t.Fatalf("expired schedule state = %s", got.State)
	}

	future := testNow.Add(time.Hour)
	v2 := v
	v2.ID = "sch_future"
	v2.Trigger = Trigger{Type: TriggerOnce, At: future.Format(time.RFC3339)}
	v2.NextRunAt = &future
	if err := store.InsertSchedule(ctx, v2); err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(ctx, v2.ID)
	if err != nil || approved.State != StateActive || approved.NextRunAt == nil {
		t.Fatalf("approve future once = %+v, %v", approved, err)
	}
}

// M2: a standby process with an already-due slot must not spin at 0 delay.
func TestStandbyDoesNotSpinOnDueSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &countingClock{fakeClock: newFakeClock(testNow)}
	svc, store := newTestService(t, clock, nil)
	past := testNow.Add(-time.Minute)
	if err := store.InsertSchedule(ctx, activeIntervalSchedule("sch_due", past, "")); err != nil {
		t.Fatal(err)
	}
	lease := &scriptedLease{} // acquireOK=false → Start leaves us in standby
	sched := NewScheduler(svc, &fakeDispatcher{}, SchedulerOptions{
		Clock: clock, Lease: lease, SyncInterval: time.Hour, MaxWake: time.Hour,
	})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.Mode() != "standby" {
		t.Fatalf("mode = %s, want standby", svc.Mode())
	}
	time.Sleep(80 * time.Millisecond)
	if calls := clock.calls.Load(); calls > 6 {
		t.Fatalf("standby loop rebuilt %d timers in 80ms (hot spin)", calls)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := sched.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

// M3: Fire must see a persisted running Run, and a crash afterwards must
// never replay the same Run as pending.
func TestFirePersistsRunningBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, nil)
	d := &observingDispatcher{store: store}
	svc.SetDispatcher(d)
	sch, err := svc.Create(ctx, CreateInput{
		Name: "一次性", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-m3")
	if err != nil {
		t.Fatal(err)
	}
	if d.statusAtFire != RunRunning {
		t.Fatalf("run status inside Fire = %s, want running", d.statusAtFire)
	}

	// A fresh Service over the same store models a crash + restart.
	restarted, err := NewService(store, svc.Config(), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	fresh := &fakeDispatcher{}
	restarted.SetDispatcher(fresh)
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.TryStartPending(ctx); err != nil {
		t.Fatal(err)
	}
	if fresh.count() != 0 {
		t.Fatalf("restart replayed %d run(s)", fresh.count())
	}
	if got, _ := restarted.GetRun(ctx, run.ID); got.Status != RunFailed || got.ErrorCode != "bot_restarted" {
		t.Fatalf("interrupted run = %+v", got)
	}
}

// L1: only UNIQUE conflicts are swallowed; other store errors surface.
func TestFireSlotSurfacesStoreErrors(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	cfg := DefaultConfig()
	cfg.Timezone, cfg.MinInterval, cfg.Confirm = "UTC", time.Second, "auto"
	store := &failingInsertStore{MemoryStore: NewMemoryStore()}
	svc, err := NewService(store, cfg, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	sch, err := svc.Create(ctx, CreateInput{
		Name: "落库检查", Prompt: "检查", Trigger: Trigger{Type: TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("disk full")
	if err := svc.FireSlot(ctx, sch.ID, *sch.NextRunAt); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("FireSlot swallowed store error: %v", err)
	}
	store.err = ErrConflict
	if err := svc.FireSlot(ctx, sch.ID, *sch.NextRunAt); err != nil {
		t.Fatalf("FireSlot should ignore a UNIQUE conflict: %v", err)
	}
}

// H2 (Service side): a dedicated session recreated by the Dispatcher is
// re-pinned even when the schedule already had an older session ID.
func TestDedicatedSessionIsRepinned(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, store := newTestService(t, clock, nil)
	svc.SetDispatcher(&fakeDispatcher{})
	sch, err := svc.Create(ctx, CreateInput{
		Name: "专属会话", Prompt: "检查",
		Trigger: Trigger{Type: TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.SessionID = "sess_deleted"
	if err := store.UpdateSchedule(ctx, got); err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-h2")
	if err != nil {
		t.Fatal(err)
	}
	after, err := svc.Get(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "sess_" + run.ID; after.SessionID != want {
		t.Fatalf("session after recreate = %q, want %q", after.SessionID, want)
	}
}

// L2: substring "no" must not turn a colloquial confirmation into a cancel.
func TestClassifyConfirmation(t *testing.T) {
	cases := []struct {
		answer    string
		confirmed bool
		denied    bool
	}{
		{"确认创建", true, false},
		{"yes, please", true, false},
		{"no problem, go ahead", true, false},
		{"取消", false, true},
		{"no", false, true},
		{"cancel it", false, true},
		{"note", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		ok, denied := classifyConfirmation(c.answer)
		if ok != c.confirmed || denied != c.denied {
			t.Errorf("classifyConfirmation(%q) = (%v,%v), want (%v,%v)",
				c.answer, ok, denied, c.confirmed, c.denied)
		}
	}
}
