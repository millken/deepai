//go:build unix

package chat

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// syncMockUI wraps mockUI with a mutex for the fields the stop/resume
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

// blockingRefreshStore parks inside RefreshSessionLock until block closes,
// so refreshHeartbeatBounded's timeout path can be exercised in-process.
type blockingRefreshStore struct {
	*SQLiteSessionStore
	block chan struct{}
}

func (b *blockingRefreshStore) RefreshSessionLock(string, models.LockOwner) error {
	<-b.block
	return nil
}

// TestWatchStopResume_ContAfterTakeoverRoutesToLockLost: a SIGCONT arriving
// after the lock was taken over must route through heartbeatTick's
// definitive-loss path — onLockLost suspends writes and raises the banner —
// deterministically, without waiting for the next 15s tick. SIGCONT is safe
// to self-deliver in-process (its default action on a running process is a
// no-op; once Notify'd, the runtime just queues it).
func TestWatchStopResume_ContAfterTakeoverRoutesToLockLost(t *testing.T) {
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
	go r.watchStopResume(done)
	defer close(done)

	// The signal.Notify inside watchStopResume races with the send below by
	// microseconds; a short sleep makes the delivery deterministic in
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

// TestRefreshHeartbeatBounded_Timeout pins the pre-stop refresh budget: a
// RefreshSessionLock stuck behind a write (busy_timeout is 5s in the real
// store) must not delay the actual stop beyond stopHeartbeatBudget.
func TestRefreshHeartbeatBounded_Timeout(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the parked goroutine
	r := &ChatRepl{
		sessMgr:   &blockingRefreshStore{SQLiteSessionStore: store, block: block},
		lockOwner: models.LockOwner{PID: os.Getpid(), Host: "h"},
	}

	start := time.Now()
	r.refreshHeartbeatBounded()
	if elapsed := time.Since(start); elapsed > 2*stopHeartbeatBudget {
		t.Fatalf("refreshHeartbeatBounded took %v, want bounded by ~%v", elapsed, stopHeartbeatBudget)
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

// TestWatchStopResumeHelperProcess is the re-exec half of the lifecycle
// test below. It builds a real REPL-side store, acquires the lock, runs
// watchStopResume (no ticker goroutine — the heartbeat row then moves ONLY
// on acquire / pre-stop refresh / post-CONT refresh, which is exactly what
// the parent process asserts), and sleeps until the parent kills it.
func TestWatchStopResumeHelperProcess(t *testing.T) {
	if os.Getenv("DEEPAI_STOPRESUME_HELPER") != "1" {
		t.Skip("helper process only, see TestWatchStopResume_SubprocessLifecycle")
	}
	store, err := NewSQLiteSessionStore(os.Getenv("DEEPAI_STOPRESUME_DB"))
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
	go r.watchStopResume(done)

	fmt.Printf("HELPER-READY %s\n", sess.ID)
	time.Sleep(30 * time.Second) // parent kills us when done
}

// TestWatchStopResume_SubprocessLifecycle drives a REAL child process
// through SIGTSTP/SIGCONT and asserts the two heartbeat contracts of issue
// #18 against the child's lock row:
//
//   - entering the stop: the pre-stop refresh landed, so the row froze FRESH
//     (heartbeat age ~0s at stop time, not the up-to-15s staleness a frozen
//     ticker would leave behind);
//   - frozen: the row does not move while the child is stopped;
//   - resuming: the post-CONT refresh is deterministic (row moves without
//     any ticker running).
//
// The stop itself is observed via processStopped, which synchronizes the
// assertions: the child only becomes stopped AFTER its handler ran the
// pre-stop refresh and re-raised SIGTSTP.
func TestWatchStopResume_SubprocessLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	dbPath := filepath.Join(t.TempDir(), "helper.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestWatchStopResumeHelperProcess$")
	cmd.Env = append(os.Environ(),
		"DEEPAI_STOPRESUME_HELPER=1",
		"DEEPAI_STOPRESUME_DB="+dbPath,
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

	// Let the acquire-time heartbeat age ~1.5s so a MISSING pre-stop
	// refresh is distinguishable (row age ~1.5-2s) from a landed one (~0s).
	time.Sleep(1500 * time.Millisecond)

	// SIGTSTP: once the child is observably stopped, its handler has
	// already run the pre-stop refresh (refresh → Reset → self-raise →
	// kernel stop, in that order).
	if err := cmd.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatalf("SIGTSTP: %v", err)
	}
	waitProcessState(t, pid, true)

	hbAtStop := helperHeartbeat(t, store, sessionID)
	if age := time.Since(unixFracInverse(hbAtStop)); age > time.Second {
		t.Fatalf("pre-stop refresh did not land: heartbeat age %v at stop, want < 1s (frozen-ticker staleness would be ~1.5s+)", age)
	}

	// Frozen: nothing may move the row while the child is stopped (this
	// helper runs no ticker; a moving row would mean a stray writer).
	time.Sleep(time.Second)
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
func unixFracInverse(v float64) time.Time {
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9))
}
