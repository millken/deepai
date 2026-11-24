package schedule

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "bot.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMultiWorkDirLeasesAndScheduleIsolation(t *testing.T) {
	db := openTestSQLite(t)
	ctx := context.Background()

	storeA := NewSQLiteStoreForDir(db, "/repo/a")
	storeB := NewSQLiteStoreForDir(db, "/repo/b")
	storeA2 := NewSQLiteStoreForDir(db, "/repo/a")

	cfgA := DefaultConfig()
	cfgA.WorkDir = "/repo/a"
	cfgA.Timezone = "UTC"
	cfgB := DefaultConfig()
	cfgB.WorkDir = "/repo/b"
	cfgB.Timezone = "UTC"

	clk := newFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	svcA, err := NewService(storeA, cfgA, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	svcB, err := NewService(storeB, cfgB, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	svcA2, err := NewService(storeA2, cfgA, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}

	dispA := &errDispatcher{}
	dispB := &errDispatcher{}
	dispA2 := &errDispatcher{}

	schA := NewScheduler(svcA, dispA, SchedulerOptions{Clock: clk, Lease: storeA})
	schB := NewScheduler(svcB, dispB, SchedulerOptions{Clock: clk, Lease: storeB})
	schA2 := NewScheduler(svcA2, dispA2, SchedulerOptions{Clock: clk, Lease: storeA2})

	if err := schA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer schA.Stop(ctx)
	if err := schB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer schB.Stop(ctx)
	if err := schA2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer schA2.Stop(ctx)

	// Different workdirs BOTH become active; second instance on same workdir is standby.
	if svcA.Mode() != "active" {
		t.Fatalf("svcA mode = %q, want active", svcA.Mode())
	}
	if svcB.Mode() != "active" {
		t.Fatalf("svcB mode = %q, want active (independent work_dir lease)", svcB.Mode())
	}
	if svcA2.Mode() != "standby" {
		t.Fatalf("svcA2 mode = %q, want standby (same work_dir as svcA)", svcA2.Mode())
	}

	// Create schedules in each workdir + different bot IDs.
	sA, err := svcA.Create(ctx, CreateInput{
		BotID:   "reviewer",
		Name:    "A task",
		Prompt:  "check A",
		Trigger: Trigger{Type: TriggerOnce, In: "10m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sB, err := svcB.Create(ctx, CreateInput{
		BotID:   "default",
		Name:    "B task",
		Prompt:  "check B",
		Trigger: Trigger{Type: TriggerOnce, In: "10m"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross-workdir Get returns not_found, List is isolated.
	if _, err := svcA.Get(ctx, sB.ID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("svcA.Get(sB) err = %v, want not_found", err)
	}
	listA, err := svcA.List(ctx, "", 0)
	if err != nil || len(listA) != 1 || listA[0].ID != sA.ID || listA[0].BotID != "reviewer" {
		t.Fatalf("svcA.List = %+v, err=%v", listA, err)
	}
	listB, err := svcB.List(ctx, "", 0)
	if err != nil || len(listB) != 1 || listB[0].ID != sB.ID || listB[0].BotID != "default" {
		t.Fatalf("svcB.List = %+v, err=%v", listB, err)
	}

	// CountByBot & DeleteByBot work cleanly across schedules and runs,
	// and DeleteByBot only bumps schedule_metas for affected work_dirs.
	metaVer := func(wd string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, `SELECT version FROM schedule_metas WHERE work_dir = ?`, wd).Scan(&v); err != nil {
			t.Fatalf("query schedule_metas(%s): %v", wd, err)
		}
		return v
	}
	verABefore := metaVer("/repo/a")
	verBBefore := metaVer("/repo/b")
	if n, err := svcA.CountByBot(ctx, "reviewer"); err != nil || n != 1 {
		t.Fatalf("CountByBot(reviewer) = %d, %v; want 1", n, err)
	}
	if err := svcA.DeleteByBot(ctx, "reviewer"); err != nil {
		t.Fatal(err)
	}
	verAAfter := metaVer("/repo/a")
	verBAfter := metaVer("/repo/b")
	if verAAfter <= verABefore {
		t.Fatalf("expected /repo/a meta version bump: before=%d after=%d", verABefore, verAAfter)
	}
	if verBAfter != verBBefore {
		t.Fatalf("unrelated /repo/b meta version bumped: before=%d after=%d", verBBefore, verBAfter)
	}
	if n, err := svcA.CountByBot(ctx, "reviewer"); err != nil || n != 0 {
		t.Fatalf("CountByBot(reviewer) after delete = %d, %v; want 0", n, err)
	}
	// sB (different bot) remains untouched.
	if _, err := svcB.Get(ctx, sB.ID); err != nil {
		t.Fatalf("sB should survive reviewer cascade delete: %v", err)
	}
}

func TestLegacyMigrationBackfillsWorkDirFromSessions(t *testing.T) {
	db := openTestSQLite(t)
	ctx := context.Background()

	// Simulate pre-v2 bot.db: sessions table + old schedules table without bot_id/work_dir.
	_, err := db.ExecContext(ctx, `
		CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT NOT NULL);
		INSERT INTO sessions (id, cwd) VALUES ('sess_b', '/repo/b');
		CREATE TABLE schedules (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, prompt TEXT NOT NULL,
			trigger_json TEXT NOT NULL, timezone TEXT NOT NULL, options_json TEXT NOT NULL,
			session_id TEXT, state TEXT NOT NULL, created_by_json TEXT NOT NULL,
			next_run_at INTEGER, last_run_json TEXT, run_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER
		);
		INSERT INTO schedules (id, name, prompt, trigger_json, timezone, options_json, session_id, state, created_by_json, created_at, updated_at)
		VALUES
			('sch_old_b', 'Old B', 'p', '{"type":"interval","every":"1h"}', 'UTC', '{}', 'sess_b', 'active', '{"kind":"user"}', 1, 1),
			('sch_old_orphan', 'Orphan', 'p', '{"type":"interval","every":"1h"}', 'UTC', '{}', NULL, 'active', '{"kind":"user"}', 2, 2);
	`)
	if err != nil {
		t.Fatal(err)
	}

	storeA := NewSQLiteStoreForDir(db, "/repo/a")
	if err := storeA.Migrate(ctx); err != nil {
		t.Fatalf("Migrate legacy db: %v", err)
	}

	// sch_old_b is backfilled to /repo/b from sessions.cwd;
	// sch_old_orphan has no session so it stays work_dir='' (not claimed by /repo/a).
	storeB := NewSQLiteStoreForDir(db, "/repo/b")
	gotB, err := storeB.GetSchedule(ctx, "sch_old_b")
	if err != nil {
		t.Fatalf("expected sch_old_b in /repo/b: %v", err)
	}
	if gotB.BotID != DefaultBotID || gotB.WorkDir != "/repo/b" {
		t.Fatalf("gotB = %+v, want bot_id=default work_dir=/repo/b", gotB)
	}
	if _, err := storeA.GetSchedule(ctx, "sch_old_orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("storeA should not claim sessionless legacy schedule: err=%v", err)
	}
	var orphanDir string
	if err := db.QueryRowContext(ctx, `SELECT work_dir FROM schedules WHERE id = 'sch_old_orphan'`).Scan(&orphanDir); err != nil {
		t.Fatal(err)
	}
	if orphanDir != "" {
		t.Fatalf("sch_old_orphan work_dir = %q, want empty string", orphanDir)
	}
}

func TestDispatcherErrBotDisabledMarksRunSkipped(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	svc, err := NewService(NewMemoryStore(), DefaultConfig(), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetDispatcher(&errDispatcher{err: ErrBotDisabled})
	sch, err := svc.Create(ctx, CreateInput{
		BotID:   "paused_bot",
		Name:    "check",
		Prompt:  "do it",
		Trigger: Trigger{Type: TriggerOnce, In: "10m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(ctx, sch.ID, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunSkipped || got.QueuedReason != "bot_disabled" {
		t.Fatalf("run = %+v, want skipped(bot_disabled)", got)
	}
}

type errDispatcher struct{ err error }

func (d *errDispatcher) Fire(context.Context, Schedule, Run) (string, string, error) {
	return "", "", d.err
}
func (d *errDispatcher) Cancel(Run, CancelReason) error { return nil }

var _ = errors.Is
