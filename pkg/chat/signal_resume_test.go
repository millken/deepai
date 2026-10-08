//go:build unix

package chat

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// syncMockUI wraps mockUI with a mutex for the fields the resume watcher
// goroutine writes (Info, SetLockLost) while the test goroutine reads them.
type syncMockUI struct {
	mockUI
	mu sync.Mutex
}

func (s *syncMockUI) Info(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mockUI.Info(msg)
}

func (s *syncMockUI) SetLockLost(lost bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mockUI.SetLockLost(lost)
}

func (s *syncMockUI) lockLostValue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mockUI.lockLost
}

// lostRefreshStore returns models.ErrLockNotHeld from every
// RefreshSessionLock call — the definitive "lock was taken over" answer.
type lostRefreshStore struct {
	*SQLiteSessionStore
}

func (l *lostRefreshStore) RefreshSessionLock(string, models.LockOwner) error {
	return models.ErrLockNotHeld
}

// TestWatchResumeRefresh_ContAfterTakeoverRoutesToLockLost: a SIGCONT
// arriving after the lock was taken over must route through heartbeatTick's
// definitive-loss path — onLockLost suspends writes and raises the banner —
// deterministically, without waiting for the next 15s tick. SIGCONT is safe
// to self-deliver in-process (its default action on a running process is a
// no-op; once Notify'd, the runtime just queues it).
func TestWatchResumeRefresh_ContAfterTakeoverRoutesToLockLost(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   &lostRefreshStore{SQLiteSessionStore: store},
		lockOwner: models.LockOwner{PID: os.Getpid(), Host: "h"},
		ui:        &syncMockUI{},
	}
	r.setLockedSession(sess.ID)

	done := make(chan struct{})
	go r.watchResumeRefresh(done)
	defer close(done)

	// The signal.Notify inside watchResumeRefresh races with the send below
	// by microseconds; a short sleep makes the delivery deterministic in
	// practice, and the 5s assertion window below absorbs any residual skew.
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !r.lockState.isSuspended() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !r.lockState.isSuspended() {
		t.Fatal("SIGCONT after takeover must deterministically suspend writes via onLockLost")
	}
	if !r.ui.(*syncMockUI).lockLostValue() {
		t.Fatal("lock-lost banner must be up after SIGCONT-detected loss")
	}
}

// helperHeartbeat reads the session_locks heartbeat for id, as a float unix
// timestamp (the column's storage format — see unixFrac).
func helperHeartbeat(t *testing.T, store *SQLiteSessionStore, id string) float64 {
	t.Helper()
	var hb float64
	err := store.db.QueryRow(`SELECT heartbeat_at FROM session_locks WHERE session_id = ?`, id).Scan(&hb)
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return hb
}

// TestWatchResumeRefreshHelperProcess is the re-exec half of the lifecycle
// test below. It acquires the lock, runs ONLY the resume watcher (no
// heartbeat ticker, no stop-signal interception), and sleeps until the
// parent kills it. Because it never Notifies SIGTSTP, the kernel's default
// stop action applies — this is exactly the production configuration of
// Option A, exercised end to end.
func TestWatchResumeRefreshHelperProcess(t *testing.T) {
	if os.Getenv("DEEPAI_RESUME_HELPER") != "1" {
		t.Skip("helper process only, see TestWatchResumeRefresh_SubprocessLifecycle")
	}
	store, err := NewSQLiteSessionStore(os.Getenv("DEEPAI_RESUME_DB"))
	if err != nil {
		t.Fatalf("helper store: %v", err)
	}
	defer store.Close()
	sess, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("helper create: %v", err)
	}
	owner := newLockOwner()
	if err := store.AcquireSessionLock(sess.ID, owner, false); err != nil {
		t.Fatalf("helper acquire: %v", err)
	}
	r := &ChatRepl{sessMgr: store, lockOwner: owner, ui: &mockUI{}}
	r.setLockedSession(sess.ID)
	done := make(chan struct{})
	go r.watchResumeRefresh(done)

	fmt.Printf("HELPER-READY %s\n", sess.ID)
	time.Sleep(30 * time.Second) // parent kills us when done
}

// TestWatchResumeRefresh_SubprocessLifecycle drives a REAL child through a
// kernel-default SIGTSTP stop (the child intercepts nothing — Option A) and
// a SIGCONT resume, asserting the resume-side contract of issue #18:
//
//   - stopped: the lock row does not move — there is no pre-stop refresh
//     under Option A, and no ticker exists in the helper, so any movement
//     would be a stray writer;
//   - resumed: the deterministic post-CONT refresh moves the row WITHOUT
//     any heartbeat ticker running.
//
// The stop itself is observed via processStopped, which synchronizes the
// assertions on the kernel's one-shot stop.
func TestWatchResumeRefresh_SubprocessLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "helper.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestWatchResumeRefreshHelperProcess$")
	cmd.Env = append(os.Environ(),
		"DEEPAI_RESUME_HELPER=1",
		"DEEPAI_RESUME_DB="+dbPath,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var sessionID string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "HELPER-READY ") {
			sessionID = strings.TrimPrefix(line, "HELPER-READY ")
			break
		}
	}
	if sessionID == "" {
		t.Fatal("helper never reported ready")
	}
	pid := cmd.Process.Pid

	store, err := NewSQLiteSessionStore(dbPath)
	if err != nil {
		t.Fatalf("open helper db: %v", err)
	}
	defer store.Close()

	// Space the acquire-time heartbeat from the post-CONT refresh so the
	// two are unambiguously distinguishable. 2x margin over the ~0 cost of
	// each write; well under one heartbeat interval so the frozen-row
	// assertions never race a ticker that does not exist anyway.
	aging := sessionLockHeartbeatInterval / 5
	time.Sleep(aging)

	// Kernel-default stop: the helper never registered a TSTP handler, so
	// this freezes it once, at zero cost — the baseline Option A preserves.
	if err := cmd.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatalf("SIGTSTP: %v", err)
	}
	waitProcessState(t, pid, true)

	hbAtStop := helperHeartbeat(t, store, sessionID)

	// Frozen: nothing may move the row while the child is stopped.
	time.Sleep(aging)
	if hb := helperHeartbeat(t, store, sessionID); hb != hbAtStop {
		t.Fatalf("heartbeat moved while stopped: %v -> %v", hbAtStop, hb)
	}

	// SIGCONT: the deterministic post-resume refresh must move the row
	// without any ticker goroutine existing.
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	hbAfter := hbAtStop
	for hbAfter == hbAtStop && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		hbAfter = helperHeartbeat(t, store, sessionID)
	}
	if hbAfter == hbAtStop {
		t.Fatal("post-CONT refresh never landed: heartbeat unchanged 5s after resume")
	}
	waitProcessState(t, pid, false)
}

// unixFracInverse converts a heartbeat_at column value back to time.Time.
// It pairs with unixFrac (session.go) — kept beside the tests that read
// the column directly.
func unixFracInverse(v float64) time.Time {
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9))
}
