package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// ---------------------------------------------------------------------------
// -c directory scoping (acceptance point 7, and the ContinueAny escape hatch)
// ---------------------------------------------------------------------------

// TestResolveSession_ContinueLastNoHistoryInDir_CreatesNew is acceptance
// point 7: `deepai -c` in a directory with no session of its own must NOT
// fall back to the globally-latest session (that fallback is the exact bug
// that let `-c` in repo A resume repo B's session) — it must start a new one.
func TestResolveSession_ContinueLastNoHistoryInDir_CreatesNew(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// A session exists, but in a DIFFERENT directory, and it is the global
	// latest (nothing else was ever created).
	other, err := store.Create(models.CreateOpts{CWD: "/other/dir"})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	r := &ChatRepl{
		cfg:     ReplConfig{ContinueLast: true, WorkDir: "/this/dir"},
		sessMgr: store,
	}
	if err := r.resolveSession(); err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if r.sess == nil {
		t.Fatal("resolveSession() left r.sess nil")
	}
	if r.sess.ID == other.ID {
		t.Fatalf("resolveSession() reused the OTHER directory's session %q instead of creating a new one", other.ID)
	}

	loaded, err := store.LatestInDir("/this/dir")
	if err != nil {
		t.Fatalf("LatestInDir: %v", err)
	}
	if loaded == nil || loaded.ID != r.sess.ID {
		t.Fatalf("new session was not recorded under cwd=/this/dir: LatestInDir = %+v, r.sess.ID = %q", loaded, r.sess.ID)
	}
}

// TestResolveSession_ContinueAny_UsesGlobalLatest pins the escape hatch: with
// ContinueAny set, -c must behave exactly like the old, unscoped Latest().
func TestResolveSession_ContinueAny_UsesGlobalLatest(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	other, err := store.Create(models.CreateOpts{CWD: "/other/dir"})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	r := &ChatRepl{
		cfg:     ReplConfig{ContinueLast: true, ContinueAny: true, WorkDir: "/this/dir"},
		sessMgr: store,
	}
	if err := r.resolveSession(); err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if r.sess == nil || r.sess.ID != other.ID {
		t.Fatalf("resolveSession() with ContinueAny = %+v, want the global latest session %q", r.sess, other.ID)
	}
}

// ---------------------------------------------------------------------------
// Lock conflict handling: reject / --fork / --force
// ---------------------------------------------------------------------------

// TestResolveSession_LockedContinue_RejectsByDefault is acceptance point 9,
// the regression guardrail directly reproducing the incident's shape:
// process A holds the session lock; process B's resolveSession (continue
// path) must fail rather than take over, and must not write anything to the
// session it failed to lock.
func TestResolveSession_LockedContinue_RejectsByDefault(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/repro/dir"
	sess, err := store.Create(models.CreateOpts{Model: "m", CWD: workDir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "process A's task"}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Process A: a live, fresh lock on a different host so liveness checks
	// never even come into play — this must read as unambiguously "locked".
	ownerA := models.LockOwner{PID: os.Getpid(), Host: "process-A-host"}
	if err := store.AcquireSessionLock(sess.ID, ownerA, false); err != nil {
		t.Fatalf("process A acquire: %v", err)
	}

	// Process B: a real ChatRepl.resolveSession, continuing in the same dir.
	r := &ChatRepl{
		cfg:     ReplConfig{ContinueLast: true, WorkDir: workDir},
		sessMgr: store,
	}
	err = r.resolveSession()
	if err == nil {
		t.Fatal("resolveSession() = nil error, want a lock-conflict rejection")
	}
	if !strings.Contains(err.Error(), sess.ID) {
		t.Fatalf("resolveSession() err = %q, want it to name the locked session %q", err, sess.ID)
	}
	if r.sess != nil {
		t.Fatalf("resolveSession() set r.sess = %+v after failing to lock; must leave it unset", r.sess)
	}

	// B must not have appended anything to the session it failed to lock.
	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (B must not have written anything); got %+v", len(msgs), msgs)
	}

	// A's lock must still be intact (B never should have touched it).
	if err := store.AcquireSessionLock(sess.ID, models.LockOwner{PID: 1, Host: "yet-another"}, false); err == nil {
		t.Fatal("A's lock was disturbed by B's failed resolveSession; want it to remain held")
	}
}

// TestResolveSession_LockedContinue_ForkCopiesHistoryLeavesOriginal covers
// the --fork remedy: instead of rejecting, resolveSession must land on a
// brand new session carrying a copy of the locked session's history, and
// must not touch the original session's lock or messages.
func TestResolveSession_LockedContinue_ForkCopiesHistoryLeavesOriginal(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/proj"
	sess, err := store.Create(models.CreateOpts{CWD: workDir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "hello"}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	foreign := models.LockOwner{PID: os.Getpid(), Host: "foreign-host"}
	if err := store.AcquireSessionLock(sess.ID, foreign, false); err != nil {
		t.Fatalf("foreign acquire: %v", err)
	}

	r := &ChatRepl{
		cfg:     ReplConfig{ContinueLast: true, WorkDir: workDir, ForkSession: true},
		sessMgr: store,
	}
	if err := r.resolveSession(); err != nil {
		t.Fatalf("resolveSession with --fork: %v", err)
	}
	if r.sess == nil || r.sess.ID == sess.ID {
		t.Fatalf("resolveSession() with --fork did not switch to a new session: %+v", r.sess)
	}

	msgs, err := store.LoadMessages(r.sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages(forked): %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("forked session messages = %+v, want the original history copied", msgs)
	}

	// Original session's lock must remain held by the foreign owner.
	if err := store.AcquireSessionLock(sess.ID, models.LockOwner{PID: 1, Host: "yet-another"}, false); err == nil {
		t.Fatal("original session's lock should remain held after --fork")
	}
	origMsgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages(orig): %v", err)
	}
	if len(origMsgs) != 1 {
		t.Fatalf("original session messages = %d, want 1 (untouched by fork)", len(origMsgs))
	}
}

// TestResolveSession_LockedContinue_ForceTakesOver covers the --force
// remedy: resolveSession must land on the SAME session, now locked to this
// process.
func TestResolveSession_LockedContinue_ForceTakesOver(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/proj"
	sess, err := store.Create(models.CreateOpts{CWD: workDir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	foreign := models.LockOwner{PID: os.Getpid(), Host: "foreign-host"}
	if err := store.AcquireSessionLock(sess.ID, foreign, false); err != nil {
		t.Fatalf("foreign acquire: %v", err)
	}

	r := &ChatRepl{
		cfg:     ReplConfig{ContinueLast: true, WorkDir: workDir, ForceSession: true},
		sessMgr: store,
	}
	if err := r.resolveSession(); err != nil {
		t.Fatalf("resolveSession with --force: %v", err)
	}
	if r.sess == nil || r.sess.ID != sess.ID {
		t.Fatalf("resolveSession() with --force = %+v, want the same locked session %q taken over", r.sess, sess.ID)
	}
}

// ---------------------------------------------------------------------------
// truncatePathHead: the picker label needs the TAIL of a cwd, not the front
// (every session usually shares the same long prefix, e.g. /Users/x/github.com/...)
// ---------------------------------------------------------------------------

func TestTruncatePathHead(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short path untouched", "/proj", 32, "/proj"},
		{"exact length untouched", "abcdefghij", 10, "abcdefghij"},
		{"long path keeps the tail", "/Users/millken/github.com/millken/deepai", 20, "...om/millken/deepai"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncatePathHead(c.in, c.n)
			if got != c.want {
				t.Fatalf("truncatePathHead(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
			if len([]rune(got)) > c.n {
				t.Fatalf("truncatePathHead(%q, %d) = %q (%d runes), exceeds the promised bound of %d", c.in, c.n, got, len([]rune(got)), c.n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D2 (session-lock review): /new must switch the session lock atomically —
// acquire the new session's lock before releasing the old one, and never
// leave r.sess and the actually-held lock disagreeing — and the heartbeat
// goroutine must never read r.sess directly (data race with /new's
// r.sess = sess).
// ---------------------------------------------------------------------------

// failOnceLockStore wraps a real *SQLiteSessionStore and can be told to fail
// exactly the next AcquireSessionLock call, to exercise the "just-created
// session's lock acquire fails" path (D7) without needing an actual lock
// conflict. It also records the ID of the last session Create(),
// ForkSession(), or ForkSessionFromMessages() made, so a test can confirm
// that session (and only that one) was cleaned up.
type failOnceLockStore struct {
	*SQLiteSessionStore
	failNextAcquire bool
	lastCreatedID   string
}

func (f *failOnceLockStore) Create(opts models.CreateOpts) (*models.Session, error) {
	sess, err := f.SQLiteSessionStore.Create(opts)
	if err == nil {
		f.lastCreatedID = sess.ID
	}
	return sess, err
}

func (f *failOnceLockStore) ForkSessionFromMessages(origID, origTitle string, origMetadata map[string]string, cwd string, msgs []models.Message) (*models.Session, error) {
	sess, err := f.SQLiteSessionStore.ForkSessionFromMessages(origID, origTitle, origMetadata, cwd, msgs)
	if err == nil {
		f.lastCreatedID = sess.ID
	}
	return sess, err
}

func (f *failOnceLockStore) AcquireSessionLock(sessionID string, owner models.LockOwner, force bool) error {
	if f.failNextAcquire {
		f.failNextAcquire = false
		return errors.New("injected: simulated lock acquire failure")
	}
	return f.SQLiteSessionStore.AcquireSessionLock(sessionID, owner, force)
}

func TestStartNewSession_SwitchesLockAtomically(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	owner := models.LockOwner{PID: 12345, Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   store,
		ui:        &mockUI{},
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)

	r.startNewSession()

	if r.sess == nil || r.sess.ID == old.ID {
		t.Fatalf("startNewSession() left r.sess = %+v, want a NEW session", r.sess)
	}
	newID := r.sess.ID

	// lockedSessionID must follow the switch — this is what Run()'s
	// heartbeat goroutine reads.
	got := r.lockedSessionID.Load()
	if got == nil || *got != newID {
		t.Fatalf("lockedSessionID = %v, want %q", got, newID)
	}

	// The new session's lock must actually be held: a foreign owner without
	// force must be rejected.
	if err := store.AcquireSessionLock(newID, models.LockOwner{PID: 1, Host: "other"}, false); err == nil {
		t.Fatal("new session is not actually locked after startNewSession()")
	}

	// The OLD session's lock must have been released.
	if err := store.AcquireSessionLock(old.ID, models.LockOwner{PID: 1, Host: "other"}, false); err != nil {
		t.Fatalf("old session's lock was not released by startNewSession(): %v", err)
	}
}

func TestStartNewSession_LockFailureKeepsOldSessionAndCleansUpOrphan(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	wrapped := &failOnceLockStore{SQLiteSessionStore: store}

	owner := models.LockOwner{PID: 12345, Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   wrapped,
		ui:        ui,
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)
	wrapped.failNextAcquire = true

	r.startNewSession()

	// Must NOT have half-switched: still the old session.
	if r.sess == nil || r.sess.ID != old.ID {
		t.Fatalf("startNewSession() with a failed Acquire left r.sess = %+v, want unchanged old session %q", r.sess, old.ID)
	}
	if got := r.lockedSessionID.Load(); got == nil || *got != old.ID {
		t.Fatalf("lockedSessionID = %v, want unchanged %q", got, old.ID)
	}
	if ui.lastInfo() == "" {
		t.Fatal("startNewSession() failure was not reported to the user via r.ui")
	}

	// The old session's lock must still be held (never released).
	if err := store.AcquireSessionLock(old.ID, models.LockOwner{PID: 1, Host: "other"}, false); err == nil {
		t.Fatal("old session's lock was released despite the failed switch")
	}

	// D7: the orphaned (unlocked, empty) session row created just before the
	// injected failure must have been deleted, not left behind as the dir's
	// updated_at-latest row.
	if wrapped.lastCreatedID == "" {
		t.Fatal("test setup: no session was created before the injected failure")
	}
	if _, err := store.Load(wrapped.lastCreatedID); err == nil {
		t.Fatalf("orphaned session %q from the failed /new was not cleaned up", wrapped.lastCreatedID)
	}
}

// TestResolveSession_NewSessionLockFailureCleansUpOrphan is D7's counterpart
// for the OTHER path that creates-then-locks a fresh session: resolveSession
// itself, when neither -c nor -r resolved to anything (plain `deepai`).
func TestResolveSession_NewSessionLockFailureCleansUpOrphan(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	wrapped := &failOnceLockStore{SQLiteSessionStore: store, failNextAcquire: true}

	r := &ChatRepl{
		cfg:     ReplConfig{WorkDir: "/fresh/dir"},
		sessMgr: wrapped,
	}
	err := r.resolveSession()
	if err == nil {
		t.Fatal("resolveSession() = nil error, want the injected Acquire failure surfaced")
	}
	if r.sess != nil {
		t.Fatalf("resolveSession() left r.sess = %+v after a failed Acquire, want nil", r.sess)
	}
	if wrapped.lastCreatedID == "" {
		t.Fatal("test setup: no session was created before the injected failure")
	}
	if _, err := store.Load(wrapped.lastCreatedID); err == nil {
		t.Fatalf("orphaned session %q from the failed fresh-session Acquire was not cleaned up", wrapped.lastCreatedID)
	}
}

// TestClearSession_DoesNotChangeLockedSession pins the "unaffected" half of
// D2: /clear (unlike /new) never switches which session is current, so it
// must never touch lockedSessionID.
func TestClearSession_DoesNotChangeLockedSession(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := &ChatRepl{
		sessMgr: store,
		sess:    sess,
		ui:      &mockUI{},
		carry:   agent.NewSessionCarry(),
	}
	r.setLockedSession(sess.ID)

	r.clearSession()

	got := r.lockedSessionID.Load()
	if got == nil || *got != sess.ID {
		t.Fatalf("clearSession() changed lockedSessionID to %v, want unchanged %q", got, sess.ID)
	}
	if r.sess.ID != sess.ID {
		t.Fatalf("clearSession() changed r.sess.ID to %q, want unchanged %q", r.sess.ID, sess.ID)
	}
}

// TestStartNewSession_NoRaceWithHeartbeat is the regression guard for the
// data race D2 flagged: the heartbeat goroutine's r.heartbeatTick() (which
// reads r.lockedSessionID) running concurrently with startNewSession's
// r.sess/r.carry/r.turn writes must be race-free under go test -race. This
// exercises the actual production method (heartbeatTick), not a
// reimplementation of it, so it stays honest if that method's field access
// ever changes.
// raceProbeStore is a bare-bones models.SessionRepository with NO internal
// synchronization of its own — every method is a plain, lock-free no-op.
// It exists only for TestStartNewSession_NoRaceWithHeartbeat: a REAL store
// (SQLite via database/sql) has its own internal locking (the connection
// pool's mutex), and using it here was observed to hide the very race this
// test exists to catch. go test -race tracks happens-before via vector
// clocks published at every mutex lock/unlock; with the heartbeat goroutine
// and the /new goroutine both constantly calling into the same *sql.DB
// (RefreshSessionLock vs. Create/AcquireSessionLock/ReleaseSessionLock/
// Save), that shared mutex traffic accidentally re-establishes ordering
// between the two goroutines on nearly every iteration — "laundering" the
// unsynchronized r.sess access so the detector never observes it as
// concurrent. Since Create is only ever called from the single "REPL"
// goroutine in this test (never from the heartbeat goroutine), its
// unsynchronized counter is safe.
type raceProbeStore struct {
	models.SessionRepository // nil embed: only the methods below are used; anything else panics loudly instead of silently doing the wrong thing
	n                        int
}

func (s *raceProbeStore) Create(opts models.CreateOpts) (*models.Session, error) {
	s.n++
	return &models.Session{ID: fmt.Sprintf("race-%d", s.n), Metadata: map[string]string{}}, nil
}
func (s *raceProbeStore) Save(sess *models.Session) error { return nil }
func (s *raceProbeStore) AcquireSessionLock(id string, owner models.LockOwner, force bool) error {
	return nil
}
func (s *raceProbeStore) RefreshSessionLock(id string, owner models.LockOwner) error { return nil }
func (s *raceProbeStore) ReleaseSessionLock(id string, owner models.LockOwner) error { return nil }

func TestStartNewSession_NoRaceWithHeartbeat(t *testing.T) {
	store := &raceProbeStore{}
	owner := models.LockOwner{PID: 555, Host: "h"}
	first, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   store,
		ui:        &mockUI{},
		sess:      first,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(first.ID)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				r.heartbeatTick() //nolint:errcheck // race coverage only
			}
		}
	}()

	const rounds = 2000
	for i := 0; i < rounds; i++ {
		r.startNewSession()
	}
	close(done)
	wg.Wait()

	if r.sess == nil {
		t.Fatal("r.sess is nil after repeated startNewSession()")
	}
}

// ---------------------------------------------------------------------------
// Minor: --continue-any/--fork/--force without -c/-r is a silent no-op —
// warn instead.
// ---------------------------------------------------------------------------

func TestResolveSession_LockFlagsWithoutContinueOrResume_Warns(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	oldStderr := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = pw
	defer func() { os.Stderr = oldStderr }()

	r := &ChatRepl{
		cfg:     ReplConfig{WorkDir: "/proj", ForceSession: true},
		sessMgr: store,
	}
	if err := r.resolveSession(); err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	pw.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, pr); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	os.Stderr = oldStderr

	if !strings.Contains(buf.String(), "--force") {
		t.Fatalf("stderr = %q, want a warning that --force is ignored without -c/-r", buf.String())
	}
}

// ---------------------------------------------------------------------------
// D5 (session-lock review): losing the session lock must be visible to the
// user (not just slog.Warn, which pkg/commands/root.go silences on stderr
// in non-verbose mode) and must stop the REPL rather than let it keep
// writing a session another process now owns.
// ---------------------------------------------------------------------------

// TestOnLockLost_SuspendsWritesAndShowsPersistentBanner pins D5's REVISED
// contract from session-lock review round 2: losing the lock must no
// longer cancel the running turn or exit the REPL (the old behavior this
// test used to assert, via a cancel callback, destroyed in-flight work to
// react to a mere notification — the review called this "取舍反了"). What
// it must do instead: suspend all further persistence, and put up a
// PERSISTENT notice (r.ui.SetLockLost(true), rendered every frame by the
// TUI — not just a one-off Info line that scrolls away).
func TestOnLockLost_SuspendsWritesAndShowsPersistentBanner(t *testing.T) {
	ui := &mockUI{}
	r := &ChatRepl{ui: ui}

	r.onLockLost()

	if !r.lockState.isSuspended() {
		t.Fatal("onLockLost() did not set writesSuspended")
	}
	if ui.lastInfo() == "" {
		t.Fatal("onLockLost() did not notify the user via r.ui.Info")
	}
	if !ui.lockLost {
		t.Fatal("onLockLost() did not raise a persistent banner via r.ui.SetLockLost(true)")
	}
}

func TestHeartbeatTick_ReportsLostWhenRefreshFails(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	r := &ChatRepl{sessMgr: store, lockOwner: owner}
	r.setLockedSession(sess.ID)

	// Never actually acquired the lock, so RefreshSessionLock must fail with
	// the DEFINITIVE sentinel (models.ErrLockNotHeld) — simulating "someone
	// else --force'd this session out from under us." A single call must
	// report lost=true immediately; this is NOT the transient case H1
	// added retry/escalation for (see TestHeartbeatTick_TransientErrors...
	// below).
	lost, err := r.heartbeatTick()
	if !lost {
		t.Fatal("heartbeatTick() lost = false, want true (lock was never held)")
	}
	if err == nil {
		t.Fatal("heartbeatTick() err = nil alongside lost = true")
	}
	if !errors.Is(err, models.ErrLockNotHeld) {
		t.Fatalf("heartbeatTick() err = %v, want errors.Is(err, models.ErrLockNotHeld)", err)
	}
}

// ---------------------------------------------------------------------------
// H-B/M-A (session-lock review round 3): the "escalate a run of consecutive
// transient RefreshSessionLock failures to lock loss" heuristic that used
// to live in heartbeatTick is GONE, not patched — see heartbeatTick's doc
// comment for the full argument (two blocking-severity defects across two
// review rounds, and RefreshSessionLock already returns a deterministic
// models.ErrLockNotHeld the moment the row's owner actually changes, so the
// heuristic bought essentially nothing). This section pins the negative
// space directly: no number of consecutive transient failures — including
// a run far longer than the OLD threshold ever was — may ever set
// lost=true or suspend writes.
// ---------------------------------------------------------------------------

// fakeLockStore is a bare models.SessionRepository whose RefreshSessionLock
// is fully scriptable, so this can be driven deterministically without
// needing to actually provoke SQLITE_BUSY out of a real database.
type fakeLockStore struct {
	models.SessionRepository // nil embed: anything not overridden below panics loudly
	refreshErr               error
	calls                    int
}

func (f *fakeLockStore) RefreshSessionLock(sessionID string, owner models.LockOwner) error {
	f.calls++
	return f.refreshErr
}

func TestHeartbeatTick_TransientErrorsNeverEscalateToLoss(t *testing.T) {
	transient := errors.New("database is locked (5)")
	fake := &fakeLockStore{refreshErr: transient}

	r := &ChatRepl{sessMgr: fake, lockOwner: models.LockOwner{PID: 1, Host: "h"}, lockHeartbeatInterval: 10 * time.Millisecond}
	r.setLockedSession("sess-1")

	// Far more consecutive failures than the OLD (deleted) escalation
	// threshold ever needed to trip (4, at a 10s interval against a 60s
	// staleLockAfter) — none of them may ever report lost=true.
	for i := 1; i <= 50; i++ {
		lost, err := r.heartbeatTick()
		if lost {
			t.Fatalf("heartbeatTick() call %d: lost = true, want false — a transient failure must NEVER escalate to loss, no matter how many times it repeats (H-B/M-A deleted that heuristic)", i)
		}
		if err == nil {
			t.Fatalf("heartbeatTick() call %d: err = nil, want the transient error surfaced (for slog.Warn)", i)
		}
		if errors.Is(err, models.ErrLockNotHeld) {
			t.Fatalf("heartbeatTick() call %d: err wraps ErrLockNotHeld for a merely transient failure", i)
		}
	}

	// And the full heartbeat reaction path — handleHeartbeatResult, which
	// is what Run()'s goroutine actually calls — must never suspend writes
	// or raise the banner for any of them either.
	ui := &mockUI{}
	r2 := &ChatRepl{ui: ui}
	for i := 0; i < 50; i++ {
		r2.handleHeartbeatResult(false, transient)
	}
	if r2.lockState.isSuspended() {
		t.Fatal("50 transient failures suspended writes via handleHeartbeatResult — the escalation heuristic must be fully removed, not just from heartbeatTick")
	}
	if ui.lockLost {
		t.Fatal("50 transient failures raised the lock-lost banner")
	}
}

// ---------------------------------------------------------------------------
// handleHeartbeatResult: the orchestration between heartbeatTick and
// onLockLost — only a DEFINITIVE loss (lost=true) may trigger onLockLost,
// and it must fire at most once per loss (the ticker keeps calling
// heartbeatTick every interval, which keeps returning lost=true, since
// RefreshSessionLock keeps failing the same way).
// ---------------------------------------------------------------------------

func TestHandleHeartbeatResult_OnlyDefinitiveLossTriggersOnLockLostOnce(t *testing.T) {
	ui := &mockUI{}
	r := &ChatRepl{ui: ui}

	r.handleHeartbeatResult(false, errors.New("transient"))
	if ui.lockLost || r.lockState.isSuspended() {
		t.Fatal("handleHeartbeatResult(lost=false, ...) must not trigger onLockLost")
	}

	r.handleHeartbeatResult(true, errors.New("definitive"))
	if !ui.lockLost {
		t.Fatal("handleHeartbeatResult(lost=true, ...) did not trigger onLockLost")
	}
	if !r.lockState.isSuspended() {
		t.Fatal("handleHeartbeatResult(lost=true, ...) did not suspend writes")
	}

	ui.infoMsgs = nil
	r.handleHeartbeatResult(true, errors.New("definitive, again"))
	if len(ui.infoMsgs) != 0 {
		t.Fatalf("handleHeartbeatResult() re-notified for an already-reported loss: %v", ui.infoMsgs)
	}
}

// ---------------------------------------------------------------------------
// M1 / D5 (session-lock review round 2): once the lock is lost, EVERY
// persistence path must actually stop writing — this is the promise the
// whole feature makes, and until now nothing enforced it: onLockLost only
// cancelled a context nothing downstream ever checked before AppendMessage.
// ---------------------------------------------------------------------------

func TestAppendMessage_SkippedWhenWritesSuspended(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := &ChatRepl{sessMgr: store, sess: sess}
	r.lockState.setLost()

	if err := r.appendMessage(models.Message{Role: models.RoleHuman, Content: "must not be saved"}); err != nil {
		t.Fatalf("appendMessage() while suspended returned an error: %v", err)
	}
	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("appendMessage() wrote a message despite writesSuspended: %+v", msgs)
	}

	r.lockState.clear()
	if err := r.appendMessage(models.Message{Role: models.RoleHuman, Content: "must be saved"}); err != nil {
		t.Fatalf("appendMessage(): %v", err)
	}
	msgs, err = store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("appendMessage() after clearing writesSuspended did not persist: %+v", msgs)
	}
}

func TestSaveSession_SkippedWhenWritesSuspended(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := &ChatRepl{sessMgr: store, sess: sess}
	r.lockState.setLost()
	r.sess.State = models.SessionStateCompleted
	r.saveSession()

	reloaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.State == models.SessionStateCompleted {
		t.Fatal("saveSession() persisted state despite writesSuspended")
	}

	r.lockState.clear()
	r.saveSession()
	reloaded, err = store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.State != models.SessionStateCompleted {
		t.Fatalf("saveSession() after clearing writesSuspended did not persist: state = %q", reloaded.State)
	}
}

// TestLockLost_FullRecoveryPath_SuspendsThenNewSessionRestores exercises the
// whole D5 path end to end against a real store: a live lock gets
// force-stolen out from under this process; the next heartbeat must detect
// it, suspend persistence, and raise the banner; every persistence call
// made in that state must be a no-op; and /new (startNewSession) must fully
// restore normal operation on the NEW session — persistence re-enabled, the
// banner cleared, and (M2) a re-armed lockLostNotified so a SECOND, later
// loss (on the new session) is still reported rather than silently
// swallowed by a one-shot guard.
func TestLockLost_FullRecoveryPath_SuspendsThenNewSessionRestores(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/proj"
	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	sess, err := store.Create(models.CreateOpts{CWD: workDir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, owner, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: workDir},
		sessMgr:   store,
		ui:        ui,
		sess:      sess,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(sess.ID)

	// Simulate the incident: another process --force takes over.
	thief := models.LockOwner{PID: os.Getpid() + 1, Host: "h"}
	if err := store.AcquireSessionLock(sess.ID, thief, true); err != nil {
		t.Fatalf("thief force-acquire: %v", err)
	}

	// Step 1: the next heartbeat must detect the loss.
	lost, hbErr := r.heartbeatTick()
	if !lost {
		t.Fatal("heartbeatTick() after a force takeover: lost = false, want true")
	}
	r.handleHeartbeatResult(lost, hbErr)

	// Step 2: persistence must now be suspended, visibly, and no writes
	// must reach the (no longer ours) session.
	if !r.lockState.isSuspended() {
		t.Fatal("writesSuspended was not set after the detected loss")
	}
	if !ui.lockLost {
		t.Fatal("the persistent lock-lost banner was not raised")
	}
	if err := r.appendMessage(models.Message{Role: models.RoleAI, Content: "orphaned turn output — must not interleave with the thief"}); err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	r.sess.State = models.SessionStateCompleted
	r.saveSession()
	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a message was persisted to the stolen session after lock loss: %+v (this IS the original interleaving-writers incident)", msgs)
	}
	reloaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.State == models.SessionStateCompleted {
		t.Fatal("this process wrote session state to a session it no longer owns")
	}

	// Step 3: /new must fully restore normal operation.
	r.startNewSession()
	newID := r.sess.ID
	if newID == sess.ID {
		t.Fatal("startNewSession() did not switch onto a new session")
	}
	if r.lockState.isSuspended() {
		t.Fatal("startNewSession() left writesSuspended set on the new session")
	}
	if ui.lockLost {
		t.Fatal("startNewSession() did not clear the persistent lock-lost banner")
	}

	// Persistence must actually work again, on the NEW session.
	if err := r.appendMessage(models.Message{Role: models.RoleHuman, Content: "back in business"}); err != nil {
		t.Fatalf("appendMessage on the new session: %v", err)
	}
	msgs, err = store.LoadMessages(newID)
	if err != nil {
		t.Fatalf("LoadMessages(new): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("appendMessage after /new did not persist to the new session: %+v", msgs)
	}

	// Step 4 (M2 callout): lockLostNotified must have been re-armed, not
	// permanently disarmed by what used to be a plain sync.Once — a later,
	// SECOND loss (this time on the new session) must still be reported.
	newThief := models.LockOwner{PID: os.Getpid() + 2, Host: "h"}
	if err := store.AcquireSessionLock(newID, newThief, true); err != nil {
		t.Fatalf("second thief force-acquire: %v", err)
	}
	lost, hbErr = r.heartbeatTick()
	if !lost {
		t.Fatal("heartbeatTick() after a SECOND force takeover: lost = false, want true")
	}
	ui.infoMsgs = nil
	r.handleHeartbeatResult(lost, hbErr)
	if !ui.lockLost || len(ui.infoMsgs) == 0 {
		t.Fatal("a second, later lock loss was not reported — lockLostNotified was not reset by /new")
	}
}

// ---------------------------------------------------------------------------
// M2 (session-lock review round 2): /new's lock-switch window must not be
// visible to a concurrently-firing heartbeat as "lock lost". The OLD order
// was ReleaseSessionLock(oldID) THEN setLockedSession(newID) — between
// those two statements, lockedSessionID still named the just-released OLD
// session, so a heartbeat landing there got RowsAffected=0 and reported
// the whole REPL as having lost its lock. TestStartNewSession_NoRaceWith-
// Heartbeat cannot see this: per its own doc comment, raceProbeStore makes
// RefreshSessionLock/ReleaseSessionLock unconditional no-ops, so nothing
// in that test can ever observe a "not held" outcome. This test uses the
// real store and injects a real heartbeatTick at EXACTLY the instant the
// old lock is released, which is the tightest possible window for the bug.
// ---------------------------------------------------------------------------

type newSessionHeartbeatProbe struct {
	*SQLiteSessionStore
	repl *ChatRepl
}

func (p *newSessionHeartbeatProbe) ReleaseSessionLock(sessionID string, owner models.LockOwner) error {
	err := p.SQLiteSessionStore.ReleaseSessionLock(sessionID, owner)
	if lost, hbErr := p.repl.heartbeatTick(); lost {
		p.repl.handleHeartbeatResult(lost, hbErr)
	}
	return err
}

func TestStartNewSession_HeartbeatDuringSwitchWindow_RealStore(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	probe := &newSessionHeartbeatProbe{SQLiteSessionStore: store}
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   probe,
		ui:        ui,
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)
	probe.repl = r

	r.startNewSession()

	if ui.lockLost {
		t.Fatal("a heartbeat landing exactly at /new's old-lock release was reported as lock lost")
	}
	for _, msg := range ui.infoMsgs {
		if strings.Contains(msg, "锁已丢失") {
			t.Fatalf("a heartbeat landing in /new's lock-switch window notified the user of lock loss: %q", msg)
		}
	}
	if r.lockState.isSuspended() {
		t.Fatal("a heartbeat landing in /new's lock-switch window suspended writes on the brand new session")
	}
}

// staleHeartbeatProbe wraps a real *SQLiteSessionStore and, on the FIRST
// RefreshSessionLock call naming staleID, synchronously runs onStale
// BEFORE delegating to the real RefreshSessionLock — simulating N2's
// scenario: heartbeatTick already captured r.lockedSessionID (staleID)
// when it started, but the UPDATE itself was queued behind /new's or
// /fork's own write transaction (busy_timeout) and only actually executes
// AFTER that switch (setLockedSession(new) + Release(staleID)) has
// completed underneath it.
type staleHeartbeatProbe struct {
	*SQLiteSessionStore
	staleID string
	onStale func()
	fired   bool
}

func (p *staleHeartbeatProbe) RefreshSessionLock(id string, owner models.LockOwner) error {
	if !p.fired && id == p.staleID {
		p.fired = true
		p.onStale()
	}
	return p.SQLiteSessionStore.RefreshSessionLock(id, owner)
}

// TestHeartbeatTick_StaleIDDuringSwitchWindow_DoesNotReportLoss is the RED
// test for N2: heartbeatTick reads r.lockedSessionID ONCE, before issuing
// RefreshSessionLock's UPDATE. If a /new or /fork switch completes in the
// window between that read and the UPDATE actually executing, the UPDATE
// legitimately finds no row for the OLD id (this process released it
// itself, on purpose) and returns models.ErrLockNotHeld — a true fact
// about an id this process no longer even claims to hold, wrongly
// escalated to a loss on whatever session is CURRENT. heartbeatTick must
// re-check r.lockedSessionID after the UPDATE and only report lost=true if
// it still names the same id this tick started for.
func TestHeartbeatTick_StaleIDDuringSwitchWindow_DoesNotReportLoss(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	probe := &staleHeartbeatProbe{SQLiteSessionStore: store, staleID: old.ID}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   probe,
		ui:        &mockUI{},
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)
	probe.onStale = func() { r.startNewSession() }

	lost, hbErr := r.heartbeatTick()

	if got := r.lockedSessionID.Load(); got == nil || *got == old.ID {
		t.Fatalf("test setup: startNewSession() inside the probe did not switch lockedSessionID away from %q: got %v", old.ID, got)
	}
	if lost {
		t.Fatalf("heartbeatTick() reported lost=true (err=%v) for a STALE id — this process released %q itself via /new during the tick and is not affected", hbErr, old.ID)
	}
}

func TestHeartbeatTick_StaleIDDuringSwitchWindow_NoBanner(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	probe := &staleHeartbeatProbe{SQLiteSessionStore: store, staleID: old.ID}
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   probe,
		ui:        ui,
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)
	probe.onStale = func() { r.startNewSession() }

	lost, hbErr := r.heartbeatTick()
	r.handleHeartbeatResult(lost, hbErr)

	if ui.lockLost {
		t.Fatal("a stale heartbeat tick for an id this process already released via /new raised the lock-lost banner on the brand new session")
	}
	if r.lockState.isSuspended() {
		t.Fatal("a stale heartbeat tick for an id this process already released via /new suspended writes on the brand new session")
	}
}

// ---------------------------------------------------------------------------
// M4 (session-lock review round 2, half): os.Hostname() failing must fall
// back to a value unique PER PROCESS, not a fixed "unknown-host" that would
// make two different machines hitting this rare path collide onto the same
// lock "host" — letting each probe the other's meaningless pid.
// ---------------------------------------------------------------------------

func TestUnknownHostFallback_UniquePerCall(t *testing.T) {
	a := unknownHostFallback()
	b := unknownHostFallback()
	if a == b {
		t.Fatalf("unknownHostFallback() returned the same value twice (%q) — two different machines hitting this path would collide onto the same lock host", a)
	}
	if !strings.HasPrefix(a, "unknown-host-") || !strings.HasPrefix(b, "unknown-host-") {
		t.Fatalf("unknownHostFallback() = %q, %q, want both prefixed unknown-host-", a, b)
	}
}

// ---------------------------------------------------------------------------
// H-A (session-lock review round 3): /clear, /undo and /title must not
// touch a session this process no longer owns — clearSession/undoLastTurn/
// the /title command used to bypass writesSuspended entirely (only
// appendMessage/saveSession checked it), so a stale process that lost its
// lock could still zero out or rename whatever the NEW holder had written.
// Each must reject LOUDLY (a visible message via r.ui.Info), never a
// silent no-op — a /clear the user believes succeeded but didn't is its
// own data-loss bug.
// ---------------------------------------------------------------------------

func TestClearSession_RejectsWhenWritesSuspended(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, c := range []string{"keep me", "and me"} {
		if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: c}); err != nil {
			t.Fatalf("AppendMessage(%q): %v", c, err)
		}
	}
	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{sessMgr: store, sess: sess, ui: ui, carry: agent.NewSessionCarry()}
	r.sess.Messages = msgs
	r.lockState.setLost()

	r.clearSession()

	if len(r.sess.Messages) != 2 {
		t.Fatalf("clearSession() while suspended changed in-memory messages: %d, want 2 unchanged", len(r.sess.Messages))
	}
	reloaded, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(reloaded) != 2 {
		t.Fatalf("clearSession() while suspended deleted persisted messages: %d remain, want 2 (this IS the round-3 H-A incident: a stale process wiping the new holder's session)", len(reloaded))
	}
	if ui.lastInfo() == "" {
		t.Fatal("clearSession() while suspended gave no rejection message — a silent no-op is its own data-loss bug")
	}
}

func TestUndoLastTurn_RejectsWhenWritesSuspended(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleAI, Content: "hi there"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{sessMgr: store, sess: sess, ui: ui, carry: agent.NewSessionCarry()}
	r.lockState.setLost()

	r.undoLastTurn()

	reloaded, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(reloaded) != 2 {
		t.Fatalf("undoLastTurn() while suspended deleted persisted messages: %d remain, want 2 (untouched)", len(reloaded))
	}
	if ui.lastInfo() == "" {
		t.Fatal("undoLastTurn() while suspended gave no rejection message")
	}
}

func TestTitleCommand_RejectsWhenWritesSuspended(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Title: "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{sessMgr: store, sess: sess, ui: ui, cfg: ReplConfig{}}
	r.lockState.setLost()

	r.handleSlashCommand(context.Background(), SlashCommand{Name: "title", Args: "stolen title"})

	if r.sess.Title == "stolen title" {
		t.Fatal("handleSlashCommand(/title) while suspended changed r.sess.Title in memory")
	}
	reloaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Title == "stolen title" {
		t.Fatal("handleSlashCommand(/title) while suspended renamed the persisted session — this could be renaming another process's live session")
	}
	if ui.lastInfo() == "" {
		t.Fatal("/title while suspended gave no rejection message")
	}
}

// TestGenerateTitle_RechecksLockBeforePersisting is the RED test for the
// "async goroutine re-check" half of H-A: generateTitle runs on its own
// goroutine and its LLM call can take up to 30s, during which the lock can
// be lost — checking r.lockState only once, at goroutine start, would leave
// that whole window open. mockLLMProvider.onChat fires synchronously INSIDE
// the (mocked) LLM call, simulating a loss detected mid-flight; generateTitle
// must still refuse to call SetTitle once it returns.
func TestGenerateTitle_RechecksLockBeforePersisting(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := &ChatRepl{currentModel: "default", sessMgr: store}
	mock := &mockLLMProvider{response: "Stolen Title"}
	mock.onChat = func() { r.lockState.setLost() }
	r.cfg = ReplConfig{ModelRegistry: newMockModelRegistry(mock)}

	r.generateTitle(sess.ID, "hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Title != "" {
		t.Fatalf("generateTitle() persisted title %q after the lock was lost MID-CALL — it must re-check r.lockState right before SetTitle, not just once at goroutine start", loaded.Title)
	}
}

// TestGenerateTitle_RechecksSessionIdentityBeforePersisting is the RED test
// for N1: generateTitle's rechecks used to compare r.lockState.isSuspended()
// — a GLOBAL flag — instead of whether the goroutine's captured sessionID
// is still the one this process holds. /new and /fork both call
// r.lockState.clear() as part of a successful switch, which flips
// isSuspended() back to false even though r.lockedSessionID no longer names
// the OLD session this generateTitle call was started for. Simulate exactly
// that: the mocked LLM call's onChat hook runs startNewSession() (as if the
// user typed /new while the title LLM call — up to 30s — was in flight),
// and generateTitle must not persist the title onto the abandoned old
// session, even though writes are (correctly) NOT suspended on the new one.
func TestGenerateTitle_RechecksSessionIdentityBeforePersisting(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/proj"
	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	sess, err := store.Create(models.CreateOpts{CWD: workDir, Model: "test-model"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, owner, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:          ReplConfig{WorkDir: workDir},
		sessMgr:      store,
		ui:           ui,
		sess:         sess,
		lockOwner:    owner,
		currentModel: "default",
		carry:        agent.NewSessionCarry(),
	}
	r.setLockedSession(sess.ID)

	mock := &mockLLMProvider{response: "Stolen Title"}
	mock.onChat = func() { r.startNewSession() }
	r.cfg.ModelRegistry = newMockModelRegistry(mock)

	oldID := sess.ID
	r.generateTitle(oldID, "hello, this is a test message")

	if r.sess == nil || r.sess.ID == oldID {
		t.Fatalf("test setup: startNewSession() inside onChat did not switch sessions: r.sess=%+v", r.sess)
	}

	loaded, err := store.Load(oldID)
	if err != nil {
		t.Fatalf("Load(old): %v", err)
	}
	if loaded.Title != "" {
		t.Fatalf("generateTitle() persisted title %q onto the OLD session %q after a /new switch mid-call — it must recheck session identity (r.lockedSessionID), not just r.lockState.isSuspended(), which /new's lockState.clear() resets to false", loaded.Title, oldID)
	}
}

// ---------------------------------------------------------------------------
// M-B (session-lock review round 3): /fork rescues the REPL's in-memory
// transcript — including a turn that never reached storage because
// appendMessage silently dropped it after a lock loss — into a brand-new,
// freshly locked session, and clears writesSuspended/the banner. /new, by
// contrast, discards that content on purpose; /fork exists so the user has
// a way NOT to lose it.
// ---------------------------------------------------------------------------

func TestForkCommand_PreservesInMemoryTranscriptAfterLockLoss(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workDir := "/proj"
	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	sess, err := store.Create(models.CreateOpts{CWD: workDir, Title: "orig"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, owner, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "persisted turn"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: workDir},
		sessMgr:   store,
		ui:        ui,
		sess:      sess,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(sess.ID)
	// The in-memory transcript has an ADDITIONAL orphaned turn that never
	// reached the DB — exactly what /fork exists to rescue.
	r.sess.Messages = []models.Message{
		{Role: models.RoleHuman, Content: "persisted turn"},
		{Role: models.RoleAI, Content: "orphaned reply, never saved"},
	}

	// Simulate the incident: another process --force takes over, and the
	// heartbeat detects it.
	thief := models.LockOwner{PID: os.Getpid() + 1, Host: "h"}
	if err := store.AcquireSessionLock(sess.ID, thief, true); err != nil {
		t.Fatalf("thief force-acquire: %v", err)
	}
	lost, hbErr := r.heartbeatTick()
	r.handleHeartbeatResult(lost, hbErr)
	if !r.lockState.isSuspended() {
		t.Fatal("test setup: lock loss was not detected")
	}

	r.forkCurrentSession()

	if r.sess == nil || r.sess.ID == sess.ID {
		t.Fatalf("forkCurrentSession() did not switch to a new session: %+v", r.sess)
	}
	if len(r.sess.Messages) != 2 {
		t.Fatalf("forked session in-memory messages = %d, want 2", len(r.sess.Messages))
	}

	persistedNew, err := store.LoadMessages(r.sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages(new): %v", err)
	}
	if len(persistedNew) != 2 {
		t.Fatalf("forked session persisted messages = %d, want 2 — the orphaned turn must be saved by /fork", len(persistedNew))
	}
	if persistedNew[1].Content != "orphaned reply, never saved" {
		t.Fatalf("forked session missing the orphaned turn: %+v", persistedNew)
	}

	origMsgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages(orig): %v", err)
	}
	if len(origMsgs) != 1 {
		t.Fatalf("original session messages = %d, want 1 (untouched by /fork)", len(origMsgs))
	}

	if r.lockState.isSuspended() {
		t.Fatal("forkCurrentSession() left writes suspended on the new session")
	}
	if ui.lockLost {
		t.Fatal("forkCurrentSession() did not clear the lock-lost banner")
	}
}

// ---------------------------------------------------------------------------
// N3 (session-lock review, mechanical-cleanup pass): forkCurrentSession
// (/fork) and acquireOrHandleLock's --fork startup branch both create the
// forked session FIRST and only then try to lock it — exactly the same
// two-step shape createLockedSession's D7 cleanup already guards. Neither
// used to clean up the just-forked row when that second step failed,
// leaving an unlocked, empty-of-nothing (it already carries the full
// copied transcript) orphan behind as the dir's updated_at-latest row for
// a later `deepai -c`/`--continue-any` to silently resume instead of
// whatever the user actually meant to keep using.
// ---------------------------------------------------------------------------

func TestForkCommand_LockFailureCleansUpOrphanedSession(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	wrapped := &failOnceLockStore{SQLiteSessionStore: store}

	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	sess, err := store.Create(models.CreateOpts{CWD: "/proj", Title: "orig"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(sess.ID, owner, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   wrapped,
		ui:        ui,
		sess:      sess,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(sess.ID)
	wrapped.failNextAcquire = true

	r.forkCurrentSession()

	// Must NOT have switched — the original session stays current.
	if r.sess == nil || r.sess.ID != sess.ID {
		t.Fatalf("forkCurrentSession() with a failed Acquire left r.sess = %+v, want unchanged original session %q", r.sess, sess.ID)
	}
	if ui.lastInfo() == "" {
		t.Fatal("forkCurrentSession() failure was not reported to the user via r.ui")
	}
	if wrapped.lastCreatedID == "" {
		t.Fatal("test setup: no forked session was created before the injected failure")
	}
	if _, err := store.Load(wrapped.lastCreatedID); err == nil {
		t.Fatalf("orphaned forked session %q from the failed /fork was not cleaned up", wrapped.lastCreatedID)
	}
}

// failForkAcquireStore wraps a real *SQLiteSessionStore and fails
// AcquireSessionLock for every session id EXCEPT protectedID — used to let
// the FIRST AcquireSessionLock call in acquireOrHandleLock's --fork branch
// (against the already-contested, real session) fail with a genuine
// *models.ErrSessionLocked from the underlying store, while the SECOND
// call (against the freshly forked session, whose ID isn't known ahead of
// time) fails with an injected error — reproducing "the fork's own lock
// acquire fails" without needing two real, timing-dependent processes.
type failForkAcquireStore struct {
	*SQLiteSessionStore
	protectedID  string
	lastForkedID string
}

func (f *failForkAcquireStore) ForkSession(id, cwd string) (*models.Session, error) {
	sess, err := f.SQLiteSessionStore.ForkSession(id, cwd)
	if err == nil {
		f.lastForkedID = sess.ID
	}
	return sess, err
}

func (f *failForkAcquireStore) AcquireSessionLock(sessionID string, owner models.LockOwner, force bool) error {
	if sessionID != f.protectedID {
		return errors.New("injected: simulated forked-session lock acquire failure")
	}
	return f.SQLiteSessionStore.AcquireSessionLock(sessionID, owner, force)
}

func TestAcquireOrHandleLock_ForkStartupLockFailureCleansUpOrphanedSession(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Same PID as this test process (guaranteed alive) but a DIFFERENT
	// host, so canAcquireSessionLock's cross-host branch applies: a
	// just-acquired heartbeat is nowhere near staleLockAfter, so this is a
	// genuine, unresolvable-without-force conflict (mirrors
	// TestResolveSession_LockedContinue_ForkCopiesHistoryLeavesOriginal).
	foreign := models.LockOwner{PID: os.Getpid(), Host: "foreign-host"}
	if err := store.AcquireSessionLock(sess.ID, foreign, false); err != nil {
		t.Fatalf("foreign acquire: %v", err)
	}

	wrapped := &failForkAcquireStore{SQLiteSessionStore: store, protectedID: sess.ID}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj", ForkSession: true},
		sessMgr:   wrapped,
		lockOwner: models.LockOwner{PID: os.Getpid(), Host: "h"},
	}

	err = r.acquireOrHandleLock(sess)
	if err == nil {
		t.Fatal("acquireOrHandleLock() = nil error, want the injected forked-session Acquire failure surfaced")
	}
	if wrapped.lastForkedID == "" {
		t.Fatal("test setup: no forked session was created before the injected failure")
	}
	if _, lerr := store.Load(wrapped.lastForkedID); lerr == nil {
		t.Fatalf("orphaned forked session %q from the failed --fork startup path was not cleaned up", wrapped.lastForkedID)
	}
}

// ---------------------------------------------------------------------------
// M-B item 1: /new must say PLAINLY that unsaved content is not carried
// into the new session — the old banner implied "continue" without
// clarifying that a turn's un-persisted output is abandoned.
// ---------------------------------------------------------------------------

func TestStartNewSession_WarnsWhenDiscardingUnsavedContent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	owner := models.LockOwner{PID: os.Getpid(), Host: "h"}
	old, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AcquireSessionLock(old.ID, owner, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ui := &mockUI{}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   store,
		ui:        ui,
		sess:      old,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(old.ID)
	r.sess.Messages = []models.Message{{Role: models.RoleAI, Content: "unsaved output"}}
	r.lockState.setLost()

	r.startNewSession()

	last := ui.lastInfo()
	if !strings.Contains(last, "不会带入") && !strings.Contains(last, "/fork") {
		t.Fatalf("startNewSession() while discarding unsaved content did not say so plainly: %q", last)
	}
}

// ---------------------------------------------------------------------------
// Race coverage point 3 (session-lock review round 3): the EXISTING
// TestStartNewSession_NoRaceWithHeartbeat uses raceProbeStore, whose
// RefreshSessionLock always returns nil — heartbeatTick there can NEVER
// observe lost=true, so onLockLost (and r.lockState.setLost/clear racing
// against startNewSession's own r.lockState.clear) has zero -race coverage.
// raceLossProbeStore closes that gap: RefreshSessionLock always returns
// models.ErrLockNotHeld, so the heartbeat goroutine calls onLockLost on
// essentially every tick, concurrently with startNewSession's tight loop
// (which itself calls r.lockState.clear() on every iteration).
//
// mockUI is NOT safe for concurrent use (a plain, unsynchronized slice
// append) — using it here would test mockUI's own bugs, not ChatRepl's.
// raceSafeLockUI is a minimal, genuinely concurrency-safe stand-in, mirroring
// the real *TUI's documented safety for concurrent Info/SetLockLost calls
// (see onLockLost's doc comment).
// ---------------------------------------------------------------------------

type raceSafeLockUI struct {
	mu       sync.Mutex
	infoMsgs []string
	lockLost atomic.Bool
}

func (u *raceSafeLockUI) Info(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.infoMsgs = append(u.infoMsgs, msg)
}
func (u *raceSafeLockUI) SetStatus(string, bool) {}
func (u *raceSafeLockUI) SetLockLost(lost bool)  { u.lockLost.Store(lost) }
func (u *raceSafeLockUI) Banner(BannerInfo)      {}
func (u *raceSafeLockUI) AskQuestion(context.Context, string, []string) (string, error) {
	return "", nil
}
func (u *raceSafeLockUI) ReadPrompt(context.Context) (string, []models.MessageImage, error) {
	return "", nil, nil
}
func (u *raceSafeLockUI) TurnStart(int, string)                  {}
func (u *raceSafeLockUI) TurnEnd(*agent.Usage)                   {}
func (u *raceSafeLockUI) RenderEvent(agent.AgentEvent)           {}
func (u *raceSafeLockUI) RenderSubagentEvent(subagent.TaskEvent) {}
func (u *raceSafeLockUI) RenderInterrupted()                     {}
func (u *raceSafeLockUI) InterruptCh() <-chan struct{}           { return nil }
func (u *raceSafeLockUI) CancelTaskCh() <-chan string            { return nil }
func (u *raceSafeLockUI) LoadHistory(string)                     {}
func (u *raceSafeLockUI) SaveHistory()                           {}
func (u *raceSafeLockUI) Close()                                 {}

type raceLossProbeStore struct {
	models.SessionRepository // nil embed: only the methods below are used
	n                        int
}

func (s *raceLossProbeStore) Create(opts models.CreateOpts) (*models.Session, error) {
	s.n++
	return &models.Session{ID: fmt.Sprintf("race-loss-%d", s.n), Metadata: map[string]string{}}, nil
}
func (s *raceLossProbeStore) Save(sess *models.Session) error { return nil }
func (s *raceLossProbeStore) AcquireSessionLock(id string, owner models.LockOwner, force bool) error {
	return nil
}
func (s *raceLossProbeStore) RefreshSessionLock(id string, owner models.LockOwner) error {
	return models.ErrLockNotHeld
}
func (s *raceLossProbeStore) ReleaseSessionLock(id string, owner models.LockOwner) error { return nil }

func TestOnLockLost_NoRaceWithStartNewSession(t *testing.T) {
	store := &raceLossProbeStore{}
	owner := models.LockOwner{PID: 777, Host: "h"}
	first, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   store,
		ui:        &raceSafeLockUI{},
		sess:      first,
		lockOwner: owner,
		carry:     agent.NewSessionCarry(),
	}
	r.setLockedSession(first.ID)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				lost, hbErr := r.heartbeatTick()
				r.handleHeartbeatResult(lost, hbErr)
			}
		}
	}()

	const rounds = 2000
	for i := 0; i < rounds; i++ {
		r.startNewSession()
	}
	close(done)
	wg.Wait()

	if r.sess == nil {
		t.Fatal("r.sess is nil after repeated startNewSession()")
	}
}
