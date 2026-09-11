package chat

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

func newTestStore(t *testing.T) (*SQLiteSessionStore, func()) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := NewSQLiteSessionStore(dbPath)
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	return store, func() { store.Close() }
}

func TestCRUD_Roundtrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model", CWD: "/tmp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("expected non-empty session ID")
	}

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != sess.ID {
		t.Fatalf("expected ID %q, got %q", sess.ID, loaded.ID)
	}

	if err := store.SetTitle(sess.ID, "My Session"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	loaded2, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load after rename: %v", err)
	}
	if loaded2.Title != "My Session" {
		t.Fatalf("expected title %q, got %q", "My Session", loaded2.Title)
	}
}

// TestClear_WipesPersistedMessages reproduces the bug where `/clear` only reset
// in-memory messages, so a later `deepai -c` (Latest + LoadMessages) replayed the
// old conversation. `/clear` deletes all persisted messages via
// DeleteMessagesAfterSeq(id, 0); the session must remain resumable but empty.
func TestClear_WipesPersistedMessages(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, role := range []models.Role{models.RoleHuman, models.RoleAI} {
		if err := store.AppendMessage(sess.ID, models.Message{Role: role, Content: "hi"}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	// Simulate `/clear`.
	if err := store.DeleteMessagesAfterSeq(sess.ID, 0); err != nil {
		t.Fatalf("DeleteMessagesAfterSeq: %v", err)
	}

	// `deepai -c` resolves the latest session and loads its messages.
	latest, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil || latest.ID != sess.ID {
		t.Fatalf("expected latest to be the cleared session %q, got %+v", sess.ID, latest)
	}
	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages after clear, got %d", len(msgs))
	}
}

func TestAppendMessage_SeqIncrement(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 3; i++ {
		msg := models.Message{
			Role:    models.RoleHuman,
			Content: "msg",
		}
		if err := store.AppendMessage(sess.ID, msg); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Verify ordering by seq via content check (all content is "msg").
	for i, m := range msgs {
		if m.SessionID != sess.ID {
			t.Fatalf("msg[%d]: wrong session_id %q", i, m.SessionID)
		}
		if m.Role != models.RoleHuman {
			t.Fatalf("msg[%d]: expected human role, got %q", i, m.Role)
		}
	}
}

func TestDeleteMessagesAfterSeq_Boundary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "msg"}); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	// Delete messages with seq > 3 (keeps seq 1, 2, 3).
	if err := store.DeleteMessagesAfterSeq(sess.ID, 3); err != nil {
		t.Fatalf("DeleteMessagesAfterSeq: %v", err)
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after delete, got %d", len(msgs))
	}

	// Append after delete should get seq 4.
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleAI, Content: "new"}); err != nil {
		t.Fatalf("AppendMessage after delete: %v", err)
	}
	msgs, err = store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages after append: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
}

func TestSearch_FTS5(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Title: "search-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Append a human message (FTS only indexes human/ai).
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "hello world test"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	results, err := store.Search("hello", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 search result for 'hello'")
	}
	found := false
	for _, r := range results {
		if r.ID == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("session not found in search results")
	}
}

func TestPrune_OnlyCompletedExpired(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create an active session (should not be pruned).
	activeSess, err := store.Create(models.CreateOpts{Title: "active-session"})
	if err != nil {
		t.Fatalf("Create active: %v", err)
	}

	// Create a completed session with old updated_at (should be pruned).
	oldSess, err := store.Create(models.CreateOpts{Title: "old-completed"})
	if err != nil {
		t.Fatalf("Create old: %v", err)
	}
	// Directly update updated_at to 120 days ago.
	_, err = store.db.Exec(`UPDATE sessions SET state = 'completed', updated_at = ? WHERE id = ?`,
		time.Now().AddDate(0, 0, -120).Unix(), oldSess.ID)
	if err != nil {
		t.Fatalf("update old session: %v", err)
	}

	count, err := store.Prune(90, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 pruned session, got %d", count)
	}

	// Active session should still exist.
	if _, err := store.Load(activeSess.ID); err != nil {
		t.Fatalf("active session should still exist: %v", err)
	}
	// Old session should be gone.
	if _, err := store.Load(oldSess.ID); err == nil {
		t.Fatal("old completed session should have been pruned")
	}
}

func TestResolveAll_Priority(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create sessions with specific titles.
	s1, _ := store.Create(models.CreateOpts{Title: "Alpha"})
	s2, _ := store.Create(models.CreateOpts{Title: "Alpha Beta"})
	_, _ = store.Create(models.CreateOpts{Title: "Beta Alpha"})

	// Priority 1: ID exact match.
	metas, err := store.ResolveAll(s1.ID)
	if err != nil {
		t.Fatalf("ResolveAll by ID: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s1.ID {
		t.Fatalf("expected single ID match, got %v", metas)
	}

	// Priority 2: Title exact match.
	metas, err = store.ResolveAll("Alpha Beta")
	if err != nil {
		t.Fatalf("ResolveAll exact title: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s2.ID {
		t.Fatalf("expected exact title match for 'Alpha Beta', got %v", metas)
	}

	// Priority 3: Title prefix match.
	metas, err = store.ResolveAll("Alpha B")
	if err != nil {
		t.Fatalf("ResolveAll prefix title: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s2.ID {
		t.Fatalf("expected prefix match for 'Alpha B', got %v", metas)
	}

	// Priority 4: Fuzzy match (case-insensitive contains).
	metas, err = store.ResolveAll("lpha")
	if err != nil {
		t.Fatalf("ResolveAll fuzzy title: %v", err)
	}
	if len(metas) < 2 {
		t.Fatalf("expected at least 2 fuzzy matches for 'lpha', got %d: %v", len(metas), metas)
	}

	// No match.
	_, err = store.ResolveAll("nonexistent_title_xyz")
	if err == nil {
		t.Fatal("expected error for no match")
	}
}

func TestPrune_DryRun(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Title: "dryrun-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Set as completed and old.
	_, err = store.db.Exec(`UPDATE sessions SET state = 'completed', updated_at = ? WHERE id = ?`,
		time.Now().AddDate(0, 0, -120).Unix(), sess.ID)
	if err != nil {
		t.Fatalf("update session: %v", err)
	}

	count, err := store.Prune(90, true)
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected dry-run count 1, got %d", count)
	}

	// Session should still exist after dry-run.
	if _, err := store.Load(sess.ID); err != nil {
		t.Fatal("session should still exist after dry-run")
	}
}

func TestDeleteLastUserTurn(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulates a session whose history diverges from a filtered in-memory view:
	// an orphan assistant (unresolved tool call) sits before a resolved one.
	seqRoles := []struct {
		role    models.Role
		content string
	}{
		{models.RoleHuman, "task A"},       // seq 1
		{models.RoleAI, "orphan tool_use"}, // seq 2
		{models.RoleAI, "resolved work B"}, // seq 3
		{models.RoleHuman, "task C"},       // seq 4 (last human)
		{models.RoleAI, "work D"},          // seq 5
	}
	for _, m := range seqRoles {
		if err := store.AppendMessage(sess.ID, models.Message{Role: m.role, Content: m.content}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	removed, err := store.DeleteLastUserTurn(sess.ID)
	if err != nil {
		t.Fatalf("DeleteLastUserTurn: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (task C + work D)", removed)
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("remaining = %d, want 3 (task A, orphan, resolved work B)", len(msgs))
	}
	// The resolved work B (before the last human) must survive — the old
	// index==seq undo would have wrongly deleted it.
	if msgs[2].Content != "resolved work B" {
		t.Fatalf("msgs[2] = %q, want 'resolved work B' (must not be deleted)", msgs[2].Content)
	}
}

func TestDeleteLastUserTurn_NothingToUndo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, _ := store.Create(models.CreateOpts{})
	// Only assistant messages, no human turn.
	_ = store.AppendMessage(sess.ID, models.Message{Role: models.RoleAI, Content: "hi"})

	removed, err := store.DeleteLastUserTurn(sess.ID)
	if err != nil {
		t.Fatalf("DeleteLastUserTurn: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (no human turn)", removed)
	}
}

func TestLatest_DeterministicOnTiedUpdatedAt(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	a, _ := store.Create(models.CreateOpts{})
	b, _ := store.Create(models.CreateOpts{})

	// Force both sessions to the SAME updated_at (a same-second tie). Without a
	// deterministic tiebreaker, Latest() could return either nondeterministically.
	const tied = 1700000000.0
	if _, err := store.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id IN (?, ?)`, tied, a.ID, b.ID); err != nil {
		t.Fatalf("force tie: %v", err)
	}

	expected := a.ID
	if b.ID > a.ID {
		expected = b.ID
	}
	for i := 0; i < 5; i++ {
		sess, err := store.Latest()
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if sess.ID != expected {
			t.Fatalf("Latest() = %q, want deterministic %q (id DESC tiebreaker)", sess.ID, expected)
		}
	}
}

// ---------------------------------------------------------------------------
// busy_timeout PRAGMA (§二.2 of the milestone brief) — this only pins that
// the PRAGMA is actually wired into the DSN; it does NOT by itself prove a
// second writer waits instead of erroring. Whether busy_timeout can rescue a
// conflicting writer at all depends on the DSN's _txlock: under the default
// _txlock=deferred a write/write conflict surfaces as SQLITE_BUSY_SNAPSHOT at
// COMMIT time, which busy_timeout does NOT retry (verified experimentally —
// this is exactly the D3 finding in the session-lock review, see the
// _txlock=immediate comment on NewSQLiteSessionStore). The actual
// "second writer waits, doesn't error" behavior this store relies on is
// exercised by TestAcquireSessionLock_ConcurrentAcquireYieldsTypedError
// below, against the real DSN including _txlock=immediate.
// ---------------------------------------------------------------------------

func TestNewSQLiteSessionStore_SetsBusyTimeout(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	var timeoutMS int
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeoutMS); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeoutMS != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeoutMS)
	}
}

// ---------------------------------------------------------------------------
// LatestInDir — acceptance point 6
// ---------------------------------------------------------------------------

func TestLatestInDir_ScopesByCWD(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	a, err := store.Create(models.CreateOpts{CWD: "/proj/a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := store.Create(models.CreateOpts{CWD: "/proj/b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	// b is updated more recently than a, but lives in a DIFFERENT directory —
	// LatestInDir("/proj/a") must still return a, not silently fall through
	// to whatever is globally newest (that fallthrough is the bug being fixed).
	future := time.Now().Add(time.Hour).Unix()
	if _, err := store.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, future, b.ID); err != nil {
		t.Fatalf("bump b.updated_at: %v", err)
	}

	got, err := store.LatestInDir("/proj/a")
	if err != nil {
		t.Fatalf("LatestInDir: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Fatalf("LatestInDir(/proj/a) = %+v, want session %q", got, a.ID)
	}

	// No session at all in an unrelated directory: (nil, nil), same
	// empty-result convention as Latest().
	none, err := store.LatestInDir("/proj/nonexistent")
	if err != nil {
		t.Fatalf("LatestInDir nonexistent: %v", err)
	}
	if none != nil {
		t.Fatalf("LatestInDir(/proj/nonexistent) = %+v, want nil", none)
	}
}

// ---------------------------------------------------------------------------
// SessionMeta.CWD wiring (feeds the picker's per-line label)
// ---------------------------------------------------------------------------

func TestListRecent_IncludesCWD(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := store.Create(models.CreateOpts{CWD: "/proj/x", Title: "t"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	metas, err := store.ListRecent(10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(metas) != 1 || metas[0].CWD != "/proj/x" {
		t.Fatalf("ListRecent() = %+v, want one meta with CWD=/proj/x", metas)
	}
}

// ---------------------------------------------------------------------------
// Session locking — acceptance points 1-5
// ---------------------------------------------------------------------------

// spawnAndReapPID runs and waits out a trivial child process, returning its
// pid. Once Run() returns, the process has been reaped, so the pid names no
// live process — a "necessarily nonexistent pid" for the dead-pid test,
// without hardcoding a number that could collide with something real.
func spawnAndReapPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn short-lived process: %v", err)
	}
	return cmd.Process.Pid
}

// TestAcquireSessionLock_SecondOwnerGetsErrSessionLocked is acceptance point 1.
func TestAcquireSessionLock_SecondOwnerGetsErrSessionLocked(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := models.LockOwner{PID: 111, Host: "host-a"}
	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}

	err = store.AcquireSessionLock(sess.ID, b, false)
	if err == nil {
		t.Fatal("owner B acquire: want ErrSessionLocked, got nil")
	}
	var lockErr *models.ErrSessionLocked
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v (%T), want errors.As to find *models.ErrSessionLocked", err, err)
	}
	if lockErr.Owner != a {
		t.Fatalf("lockErr.Owner = %+v, want holder %+v", lockErr.Owner, a)
	}
	if lockErr.SessionID != sess.ID {
		t.Fatalf("lockErr.SessionID = %q, want %q", lockErr.SessionID, sess.ID)
	}
}

// TestAcquireSessionLock_StaleHeartbeatIsAcquirable is acceptance point 2.
func TestAcquireSessionLock_StaleHeartbeatIsAcquirable(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := models.LockOwner{PID: 111, Host: "host-a"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	stale := time.Now().Add(-90 * time.Second)
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`, unixFrac(stale), sess.ID); err != nil {
		t.Fatalf("force stale heartbeat: %v", err)
	}

	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, b, false); err != nil {
		t.Fatalf("owner B acquire over a >60s-stale lock: %v", err)
	}
}

// TestAcquireSessionLock_DeadPidSameHostIsAcquirable is acceptance point 3.
func TestAcquireSessionLock_DeadPidSameHostIsAcquirable(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		host = "test-host"
	}
	deadPID := spawnAndReapPID(t)
	a := models.LockOwner{PID: deadPID, Host: host}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A (dead pid) acquire: %v", err)
	}

	b := models.LockOwner{PID: os.Getpid(), Host: host}
	if err := store.AcquireSessionLock(sess.ID, b, false); err != nil {
		t.Fatalf("owner B acquire over a same-host dead-pid lock: %v", err)
	}
}

// TestAcquireSessionLock_ForceOverridesLiveLock is acceptance point 4.
func TestAcquireSessionLock_ForceOverridesLiveLock(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Same pid as the test process (so it's alive) but a different host, so
	// the same-host dead-pid check never fires and this is a genuinely live,
	// fresh, foreign lock.
	a := models.LockOwner{PID: os.Getpid(), Host: "host-a"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}

	b := models.LockOwner{PID: os.Getpid(), Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, b, false); err == nil {
		t.Fatal("owner B acquire without force: want ErrSessionLocked, got nil")
	}
	if err := store.AcquireSessionLock(sess.ID, b, true); err != nil {
		t.Fatalf("owner B acquire with force: %v", err)
	}
}

// TestReleaseSessionLock_OnlyOwnerCanRelease is acceptance point 5.
func TestReleaseSessionLock_OnlyOwnerCanRelease(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := models.LockOwner{PID: 111, Host: "host-a"}
	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}

	// B releasing A's lock must be a silent no-op — A's lock stays intact.
	if err := store.ReleaseSessionLock(sess.ID, b); err != nil {
		t.Fatalf("ReleaseSessionLock by non-owner returned an error, want silent no-op: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, b, false); err == nil {
		t.Fatal("A's lock was released by non-owner B; want it to remain held")
	}

	// A releasing its own lock actually frees it.
	if err := store.ReleaseSessionLock(sess.ID, a); err != nil {
		t.Fatalf("ReleaseSessionLock by owner: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, b, false); err != nil {
		t.Fatalf("acquire after legitimate release: %v", err)
	}
}

func TestRefreshSessionLock_KeepsLockFromGoingStale(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := models.LockOwner{PID: 111, Host: "host-a"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	// Back-date the heartbeat to just under the stale threshold.
	near := time.Now().Add(-55 * time.Second)
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`, unixFrac(near), sess.ID); err != nil {
		t.Fatalf("force near-stale heartbeat: %v", err)
	}
	if err := store.RefreshSessionLock(sess.ID, a); err != nil {
		t.Fatalf("RefreshSessionLock: %v", err)
	}

	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, b, false); err == nil {
		t.Fatal("lock should still be held after a fresh refresh, want ErrSessionLocked")
	}
}

func TestRefreshSessionLock_NonOwnerErrors(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := models.LockOwner{PID: 111, Host: "host-a"}
	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	if err := store.RefreshSessionLock(sess.ID, b); err == nil {
		t.Fatal("RefreshSessionLock by non-owner: want error, got nil")
	}
}

func TestCanAcquireSessionLock_Rules(t *testing.T) {
	me := models.LockOwner{PID: os.Getpid(), Host: "h"}
	now := time.Now()

	t.Run("no existing lock", func(t *testing.T) {
		if !canAcquireSessionLock(nil, me, false, now) {
			t.Fatal("want acquirable when no lock exists")
		}
	})
	t.Run("reentrant same owner", func(t *testing.T) {
		existing := &sessionLockRow{Owner: me, AcquiredAt: now, HeartbeatAt: now}
		if !canAcquireSessionLock(existing, me, false, now) {
			t.Fatal("want acquirable by the same owner (reentrant)")
		}
	})
	t.Run("stale heartbeat", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: 1, Host: "elsewhere"},
			AcquiredAt:  now.Add(-2 * time.Hour),
			HeartbeatAt: now.Add(-61 * time.Second),
		}
		if !canAcquireSessionLock(existing, me, false, now) {
			t.Fatal("want acquirable when heartbeat is older than staleLockAfter")
		}
	})
	t.Run("fresh foreign lock without force is not acquirable", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: 1, Host: "elsewhere"},
			AcquiredAt:  now,
			HeartbeatAt: now,
		}
		if canAcquireSessionLock(existing, me, false, now) {
			t.Fatal("want NOT acquirable: fresh heartbeat, foreign host, no force")
		}
	})
	t.Run("force overrides a live lock", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: 1, Host: "elsewhere"},
			AcquiredAt:  now,
			HeartbeatAt: now,
		}
		if !canAcquireSessionLock(existing, me, true, now) {
			t.Fatal("want acquirable with force=true")
		}
	})
	// D1 (session-lock review): same host, holder pid still ALIVE, but its
	// heartbeat looks stale (>60s old — e.g. a suspended laptop, SIGSTOP, a
	// debugger breakpoint, or a long GC/IO pause froze the 15s ticker
	// without the process actually dying). The old code checked staleness
	// BEFORE same-host liveness, so this combination fell through the
	// staleness branch and returned true — a live process getting its lock
	// stolen without --force. Same-host liveness must veto regardless of
	// how old the heartbeat looks. Existing
	// TestAcquireSessionLock_StaleHeartbeatIsAcquirable uses host-a/host-b
	// (deliberately cross-host, see its doc comment) and so never exercised
	// this branch at all — this is the coverage gap the review flagged.
	t.Run("same host, alive pid, stale heartbeat is NOT acquirable", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: os.Getpid(), Host: "h"}, // "h" == me's host; PID == this test process, unambiguously alive
			AcquiredAt:  now.Add(-2 * time.Hour),
			HeartbeatAt: now.Add(-90 * time.Second), // well past staleLockAfter (60s)
		}
		other := models.LockOwner{PID: 999999, Host: "h"} // same host as existing, different pid, NOT the reentrant case
		if canAcquireSessionLock(existing, other, false, now) {
			t.Fatal("want NOT acquirable: same host, holder pid is alive, no force — heartbeat staleness must not override a live holder")
		}
	})
}

// TestAcquireSessionLock_SameHostAlivePidStaleHeartbeatRejected is the
// DB-level counterpart of the canAcquireSessionLock subtest above: the same
// scenario, but through the real AcquireSessionLock path (which is what
// resolveSession actually calls), confirming the fix holds end to end and
// not just in the pure decision function.
func TestAcquireSessionLock_SameHostAlivePidStaleHeartbeatRejected(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		host = "test-host"
	}
	a := models.LockOwner{PID: os.Getpid(), Host: host} // this test process: unambiguously alive
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	stale := time.Now().Add(-90 * time.Second)
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`, unixFrac(stale), sess.ID); err != nil {
		t.Fatalf("force stale heartbeat: %v", err)
	}

	b := models.LockOwner{PID: os.Getpid() + 1, Host: host} // same host, different pid, still not A
	err = store.AcquireSessionLock(sess.ID, b, false)
	if err == nil {
		t.Fatal("owner B acquired over a same-host, ALIVE, merely-heartbeat-stale holder — want ErrSessionLocked")
	}
	var lockErr *models.ErrSessionLocked
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v (%T), want *models.ErrSessionLocked", err, err)
	}
}

// ---------------------------------------------------------------------------
// ForkSession — acceptance point 8
// ---------------------------------------------------------------------------

func TestForkSession_CopiesMessagesAndPreservesOriginal(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	orig, err := store.Create(models.CreateOpts{Model: "m", CWD: "/proj", Title: "T"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	contents := []string{"one", "two", "three"}
	for _, c := range contents {
		if err := store.AppendMessage(orig.ID, models.Message{Role: models.RoleHuman, Content: c}); err != nil {
			t.Fatalf("AppendMessage(%q): %v", c, err)
		}
	}
	origMsgsBefore, err := store.LoadMessages(orig.ID)
	if err != nil {
		t.Fatalf("LoadMessages before fork: %v", err)
	}

	forked, err := store.ForkSession(orig.ID, "/proj")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if forked.ID == orig.ID {
		t.Fatal("forked session has the same ID as the original")
	}
	if forked.Metadata["cwd"] != "/proj" || forked.Metadata["model"] != "m" {
		t.Fatalf("forked metadata = %+v, want cwd=/proj model=m", forked.Metadata)
	}
	if !strings.Contains(forked.Title, orig.ID) {
		t.Fatalf("forked.Title = %q, want it to name the session it was forked from (%q)", forked.Title, orig.ID)
	}

	forkedMsgs, err := store.LoadMessages(forked.ID)
	if err != nil {
		t.Fatalf("LoadMessages(forked): %v", err)
	}
	if len(forkedMsgs) != len(contents) {
		t.Fatalf("forked messages = %d, want %d", len(forkedMsgs), len(contents))
	}
	for i, c := range contents {
		if forkedMsgs[i].Content != c {
			t.Fatalf("forkedMsgs[%d].Content = %q, want %q (order must match original)", i, forkedMsgs[i].Content, c)
		}
	}

	// The original must be byte-for-byte untouched: same messages, same IDs,
	// same seqs, same count.
	origMsgsAfter, err := store.LoadMessages(orig.ID)
	if err != nil {
		t.Fatalf("LoadMessages after fork: %v", err)
	}
	if len(origMsgsAfter) != len(origMsgsBefore) {
		t.Fatalf("original message count changed: before=%d after=%d", len(origMsgsBefore), len(origMsgsAfter))
	}
	for i := range origMsgsBefore {
		if origMsgsAfter[i].ID != origMsgsBefore[i].ID || origMsgsAfter[i].Content != origMsgsBefore[i].Content {
			t.Fatalf("original message[%d] mutated: before=%+v after=%+v", i, origMsgsBefore[i], origMsgsAfter[i])
		}
	}
}

// TestForkSessionFromMessages_ZeroCreatedAtGetsFallback is the RED test for
// N4's first half: ForkSessionFromMessages (the /fork command's in-memory
// path — runTurn's userMsg never sets CreatedAt, see repl.go) must not
// persist a zero-value time.Time verbatim. AppendMessage already defaults
// a zero CreatedAt to time.Now() (see its doc comment); forkInto's copy
// loop must do the same, or unixFrac(m.CreatedAt) writes a large NEGATIVE
// timestamp (time.Time's zero value is year 1, long before the Unix
// epoch) that reads back as 1754-08-31 (the closest float64 rounding of
// that negative epoch allows) instead of "now."
func TestForkSessionFromMessages_ZeroCreatedAtGetsFallback(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	before := time.Now().Add(-time.Minute)
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "hello"}, // CreatedAt deliberately zero-value
	}

	forked, err := store.ForkSessionFromMessages("orig-id", "orig title", nil, "/proj", msgs)
	if err != nil {
		t.Fatalf("ForkSessionFromMessages: %v", err)
	}

	got, err := store.LoadMessages(forked.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("forked messages = %d, want 1", len(got))
	}
	if got[0].CreatedAt.Before(before) {
		t.Fatalf("forked message CreatedAt = %v, want a fallback near now() (a zero-value CreatedAt must not be persisted verbatim — see AppendMessage's zero-CreatedAt fallback)", got[0].CreatedAt)
	}
}

// TestForkSessionFromMessages_ImagesGetPlaceholder is the RED test for
// N4's second half: forkInto must apply the SAME "images are not
// persisted, replace with a text placeholder" rule AppendMessage already
// applies (see AppendMessage's doc comment) — otherwise a message with
// only Images and no Content forks into an empty, contentless row instead
// of the placeholder a reload of the ORIGINAL session would have shown.
func TestForkSessionFromMessages_ImagesGetPlaceholder(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	msgs := []models.Message{
		{
			Role:    models.RoleHuman,
			Content: "look at this",
			Images:  []models.MessageImage{{MimeType: "image/png", Base64: "AAAA"}},
		},
	}

	forked, err := store.ForkSessionFromMessages("orig-id", "orig title", nil, "/proj", msgs)
	if err != nil {
		t.Fatalf("ForkSessionFromMessages: %v", err)
	}

	got, err := store.LoadMessages(forked.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("forked messages = %d, want 1", len(got))
	}
	want := "look at this\n[1 image(s) attached — not persisted]"
	if got[0].Content != want {
		t.Fatalf("forked message content = %q, want %q (forkInto must apply AppendMessage's image placeholder logic, not skip it)", got[0].Content, want)
	}
}

// ---------------------------------------------------------------------------
// D3 (session-lock review): BEGIN IMMEDIATE via _txlock=immediate — without
// it, a real write/write race on AcquireSessionLock could surface as a bare
// "database is locked" (SQLITE_BUSY_SNAPSHOT, which busy_timeout does not
// retry under the default BEGIN DEFERRED) instead of *models.ErrSessionLocked,
// which silently breaks the --fork/--force remedies in
// pkg/chat/repl.go's acquireOrHandleLock (they key off errors.As finding
// that type). This uses two REAL *sql.DB connections against the same file
// (mirroring two real deepai processes), not two goroutines sharing one
// connection pool, since the DSN-level fix only matters across connections.
// ---------------------------------------------------------------------------

func TestAcquireSessionLock_ConcurrentAcquireYieldsTypedError(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	storeA, err := NewSQLiteSessionStore(dbPath)
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteSessionStore(dbPath)
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer storeB.Close()

	const rounds = 30
	bothSucceeded := 0
	for i := 0; i < rounds; i++ {
		// A fresh session per round so each round starts from a clean,
		// unlocked row rather than needing to release between rounds.
		sess, err := storeA.Create(models.CreateOpts{})
		if err != nil {
			t.Fatalf("round %d: Create: %v", i, err)
		}
		ownerA := models.LockOwner{PID: 10000 + i, Host: "a"}
		ownerB := models.LockOwner{PID: 20000 + i, Host: "b"}

		var wg sync.WaitGroup
		var errA, errB error
		wg.Add(2)
		go func() { defer wg.Done(); errA = storeA.AcquireSessionLock(sess.ID, ownerA, false) }()
		go func() { defer wg.Done(); errB = storeB.AcquireSessionLock(sess.ID, ownerB, false) }()
		wg.Wait()

		if errA == nil && errB == nil {
			bothSucceeded++
			continue // mutual exclusion broken; assert below after the loop with a full count
		}
		var lockErr *models.ErrSessionLocked
		if errA != nil && !errors.As(errA, &lockErr) {
			t.Fatalf("round %d: A's error is not *models.ErrSessionLocked (bare BUSY?): %v", i, errA)
		}
		if errB != nil && !errors.As(errB, &lockErr) {
			t.Fatalf("round %d: B's error is not *models.ErrSessionLocked (bare BUSY?): %v", i, errB)
		}
	}
	if bothSucceeded > 0 {
		t.Fatalf("%d/%d rounds: both A and B acquired the same session's lock — mutual exclusion broken", bothSucceeded, rounds)
	}
}

// ---------------------------------------------------------------------------
// D4 (session-lock review): LatestInDir / Create cwd normalization —
// os.Getwd() (WorkDir's source) is not a stable spelling of a directory:
// $PWD wins over the resolved path, symlinks aren't resolved, and a
// trailing slash passes through untouched. An exact cwd = ? match silently
// misses same-directory sessions spelled differently.
// ---------------------------------------------------------------------------

func TestLatestInDir_TrailingSlashMatchesUnnormalizedRow(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Simulate a row written before this fix (or by any path that stores
	// cwd unnormalized): no trailing slash, inserted directly so Create's
	// own normalization can't be the thing making this pass.
	legacyCWD := "/legacy/proj"
	now := time.Now()
	_, err := store.db.Exec(`
		INSERT INTO sessions (id, title, model, cwd, source, state, created_at, updated_at, metadata)
		VALUES ('legacy1', '', '', ?, 'cli', 'active', ?, ?, '{}')
	`, legacyCWD, unixFrac(now), unixFrac(now))
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Today's $PWD carries a trailing slash — a real, observed os.Getwd()
	// quirk (see normalizeCWD's doc comment).
	got, err := store.LatestInDir(legacyCWD + "/")
	if err != nil {
		t.Fatalf("LatestInDir: %v", err)
	}
	if got == nil || got.ID != "legacy1" {
		t.Fatalf("LatestInDir(%q) = %+v, want it to match the legacy row at %q despite the trailing slash", legacyCWD+"/", got, legacyCWD)
	}
}

func TestLatestInDir_SymlinkedCWDMatchesAcrossSpellings(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// t.TempDir() on macOS lives under a symlink (/var -> /private/var), so
	// this exercises EvalSymlinks for real without hardcoding /tmp.
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if resolved == dir {
		t.Skip("temp dir is not behind a symlink on this system; nothing to exercise")
	}

	sess, err := store.Create(models.CreateOpts{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Query with the OTHER spelling of the same directory than the one
	// passed to Create.
	got, err := store.LatestInDir(resolved)
	if err != nil {
		t.Fatalf("LatestInDir(%q): %v", resolved, err)
	}
	if got == nil || got.ID != sess.ID {
		t.Fatalf("LatestInDir(resolved form) = %+v, want session %q created with the unresolved form %q", got, sess.ID, dir)
	}
}

func TestCreate_NormalizesCWDColumn(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if resolved == dir {
		t.Skip("temp dir is not behind a symlink on this system; nothing to exercise")
	}

	sess, err := store.Create(models.CreateOpts{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var storedCWD string
	if err := store.db.QueryRow(`SELECT cwd FROM sessions WHERE id = ?`, sess.ID).Scan(&storedCWD); err != nil {
		t.Fatalf("query stored cwd: %v", err)
	}
	if storedCWD != resolved {
		t.Fatalf("stored cwd = %q, want the normalized/resolved form %q (Create must normalize at write time)", storedCWD, resolved)
	}
}

// TestForkSession_UsesCallerCWDNotOriginalMetadata pins the "顺带" fix noted
// alongside D4: ForkSession must use the CALLER's current cwd for the new
// session, not orig.Metadata["cwd"] — otherwise a fork inherits a
// potentially stale/different directory and becomes invisible to
// LatestInDir in the directory the fork is actually continuing in.
func TestForkSession_UsesCallerCWDNotOriginalMetadata(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	orig, err := store.Create(models.CreateOpts{CWD: "/old/dir", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	forked, err := store.ForkSession(orig.ID, "/new/dir")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if forked.Metadata["cwd"] != "/new/dir" {
		t.Fatalf("forked.Metadata[cwd] = %q, want the CALLER's cwd %q, not the original's %q", forked.Metadata["cwd"], "/new/dir", "/old/dir")
	}

	got, err := store.LatestInDir("/new/dir")
	if err != nil {
		t.Fatalf("LatestInDir: %v", err)
	}
	if got == nil || got.ID != forked.ID {
		t.Fatalf("LatestInDir(/new/dir) = %+v, want the forked session %q to be findable there", got, forked.ID)
	}
}

// ---------------------------------------------------------------------------
// D8 (session-lock review): forked-session title — must not duplicate an
// empty original's id, and must not stack "(forked from ...)" without bound
// across repeated forks.
// ---------------------------------------------------------------------------

func TestForkedTitle(t *testing.T) {
	cases := []struct {
		name      string
		origTitle string
		origID    string
		want      string
	}{
		{"titled original", "My Session", "id1", "My Session (forked from id1)"},
		{
			"untitled original must not duplicate the id",
			"", "20260831_235721_55e9",
			"(forked from 20260831_235721_55e9)",
		},
		{
			"forking an already-forked title does not stack",
			"T (forked from id1)", "id2",
			"T (forked from id2)",
		},
		{
			"forking a chain of forks still only names the immediate parent",
			"T (forked from id1) ", "id3", // trailing space, as a title field might round-trip
			"T (forked from id3)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := forkedTitle(c.origTitle, c.origID)
			if got != c.want {
				t.Fatalf("forkedTitle(%q, %q) = %q, want %q", c.origTitle, c.origID, got, c.want)
			}
		})
	}
}

func TestForkSession_UntitledOriginalTitleDoesNotDuplicateID(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	orig, err := store.Create(models.CreateOpts{}) // no title
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	forked, err := store.ForkSession(orig.ID, "")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	want := "(forked from " + orig.ID + ")"
	if forked.Title != want {
		t.Fatalf("forked.Title = %q, want %q (the id must not appear twice)", forked.Title, want)
	}
}

func TestForkSession_RepeatedForkDoesNotStackSuffixes(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	orig, err := store.Create(models.CreateOpts{Title: "Original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := store.ForkSession(orig.ID, "")
	if err != nil {
		t.Fatalf("ForkSession(1): %v", err)
	}
	second, err := store.ForkSession(first.ID, "")
	if err != nil {
		t.Fatalf("ForkSession(2): %v", err)
	}
	if strings.Count(second.Title, "forked from") != 1 {
		t.Fatalf("second.Title = %q, want exactly one \"forked from\" (no stacking)", second.Title)
	}
	want := "Original (forked from " + first.ID + ")"
	if second.Title != want {
		t.Fatalf("second.Title = %q, want %q", second.Title, want)
	}
}

// ---------------------------------------------------------------------------
// H1 (session-lock review round 2): RefreshSessionLock must let a caller
// distinguish "we definitely no longer hold this lock" (RowsAffected == 0)
// from any other, transient failure (e.g. SQLITE_BUSY from a concurrent
// writer holding a transaction open elsewhere on the same DB). Only the
// former is models.ErrLockNotHeld.
// ---------------------------------------------------------------------------

func TestRefreshSessionLock_NotHeldIsErrLockNotHeld(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := models.LockOwner{PID: 111, Host: "host-a"}
	b := models.LockOwner{PID: 222, Host: "host-b"}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}

	// B never held this lock — RefreshSessionLock must report the
	// DEFINITIVE sentinel, not just any error.
	err = store.RefreshSessionLock(sess.ID, b)
	if err == nil {
		t.Fatal("RefreshSessionLock by non-owner: want error, got nil")
	}
	if !errors.Is(err, models.ErrLockNotHeld) {
		t.Fatalf("RefreshSessionLock by non-owner: err = %v, want errors.Is(err, models.ErrLockNotHeld)", err)
	}
}

// ---------------------------------------------------------------------------
// H2 (session-lock review round 2): same-host liveness must still veto
// takeover for a merely-stale heartbeat (D1's guarantee, re-pinned here so a
// future change to H2 cannot silently regress it) — but an EXTREMELY stale
// heartbeat (staleLockAfterPidReuse) must be reclaimable even with a "live"
// pid, because past that distance the pid is far more likely to have been
// reused by an unrelated process than to still be the same live deepai.
// ---------------------------------------------------------------------------

func TestCanAcquireSessionLock_H2_PidReuseWindow(t *testing.T) {
	now := time.Now()

	t.Run("same host, alive pid, heartbeat just past staleLockAfter (D1) is NOT acquirable", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: os.Getpid(), Host: "h"},
			AcquiredAt:  now.Add(-2 * time.Hour),
			HeartbeatAt: now.Add(-90 * time.Second), // > staleLockAfter (60s), well under staleLockAfterPidReuse (10m)
		}
		other := models.LockOwner{PID: 999999, Host: "h"}
		if canAcquireSessionLock(existing, other, false, now) {
			t.Fatal("want NOT acquirable: merely-stale heartbeat must not override a live same-host pid (D1)")
		}
	})

	t.Run("same host, alive pid, heartbeat past staleLockAfterPidReuse IS acquirable", func(t *testing.T) {
		existing := &sessionLockRow{
			Owner:       models.LockOwner{PID: os.Getpid(), Host: "h"},
			AcquiredAt:  now.Add(-2 * time.Hour),
			HeartbeatAt: now.Add(-staleLockAfterPidReuse - time.Second),
		}
		other := models.LockOwner{PID: 999999, Host: "h"}
		if !canAcquireSessionLock(existing, other, false, now) {
			t.Fatal("want acquirable: a live-looking pid whose heartbeat is EXTREMELY stale is presumed pid-reuse (H2), not a live holder")
		}
	})
}

// TestAcquireSessionLock_ExtremelyStaleSameHostAlivePidIsAcquirable is the
// DB-level counterpart of the H2 subtest above, through the real
// AcquireSessionLock path.
func TestAcquireSessionLock_ExtremelyStaleSameHostAlivePidIsAcquirable(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "test-host"
	}
	a := models.LockOwner{PID: os.Getpid(), Host: host}
	if err := store.AcquireSessionLock(sess.ID, a, false); err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	ancient := time.Now().Add(-staleLockAfterPidReuse - time.Minute)
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`, unixFrac(ancient), sess.ID); err != nil {
		t.Fatalf("force ancient heartbeat: %v", err)
	}

	b := models.LockOwner{PID: os.Getpid() + 1, Host: host}
	if err := store.AcquireSessionLock(sess.ID, b, false); err != nil {
		t.Fatalf("owner B acquire over a same-host, alive-pid, but 10x-stale lock: %v (want success — pid reuse presumed)", err)
	}
}
