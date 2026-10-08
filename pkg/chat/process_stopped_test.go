package chat

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// startSleepProcess starts a real child (sleep) whose pid we can SIGSTOP and
// probe, then SIGCONT/kill it on cleanup. Returns the process.
func startSleepProcess(t *testing.T) *os.Process {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process
}

// waitProcessState polls the stop probe until want holds or the deadline
// passes, so a kernel that takes a moment to move the child between states
// never flakes the test.
func waitProcessState(t *testing.T, pid int, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for processStopped(pid) != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processStopped(pid) != want {
		t.Fatalf("precondition: processStopped(%d) = %v, want %v", pid, !want, want)
	}
}

// startStoppedProcess is startSleepProcess plus a real SIGSTOP, driven to a
// confirmed stopped state — the precondition three tests below share.
func startStoppedProcess(t *testing.T) *os.Process {
	t.Helper()
	proc := startSleepProcess(t)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	waitProcessState(t, proc.Pid, true)
	return proc
}

// TestProcessStopped_RealSigstop probes processStopped against a real child
// process driven through an actual SIGSTOP/SIGCONT cycle — not a mock — so
// the darwin sysctl and linux /proc parsers are both exercised for real. The
// CONT half is asserted too: recovery must report not-stopped before the
// cycle's second stop is driven, otherwise a broken (always-true) probe
// would pass the round trip vacuously.
func TestProcessStopped_RealSigstop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	proc := startSleepProcess(t)

	waitProcessState(t, proc.Pid, false)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	waitProcessState(t, proc.Pid, true)
	if err := proc.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}
	waitProcessState(t, proc.Pid, false)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("re-SIGSTOP: %v", err)
	}
	waitProcessState(t, proc.Pid, true)
}

// TestCanAcquireSessionLock_StoppedHolderShortClock is the regression test
// for the Ctrl+Z incident: a same-host holder that is job-control-stopped
// (alive, pid intact, heartbeat frozen) must be reclaimable after
// staleLockAfter — not held behind the 10-minute staleLockAfterPidReuse veto
// that D1/H2 give to RUNNING pids. A user who suspends deepai with Ctrl+Z
// and starts a new `deepai -c` was locked out for ten minutes; the stopped
// branch cuts that to the same 60s clock cross-host holders are judged on.
func TestCanAcquireSessionLock_StoppedHolderShortClock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	proc := startStoppedProcess(t)

	me := models.LockOwner{PID: os.Getpid(), Host: "h"}
	now := time.Now()
	existing := &sessionLockRow{
		Owner:       models.LockOwner{PID: proc.Pid, Host: "h"},
		AcquiredAt:  now.Add(-2 * time.Hour),
		HeartbeatAt: now.Add(-staleLockAfter + 30*time.Second), // fresh-ish: within staleLockAfter
	}
	if canAcquireSessionLock(existing, me, false, now) {
		t.Fatal("want NOT acquirable: stopped holder, heartbeat still within staleLockAfter")
	}

	existing.HeartbeatAt = now.Add(-(staleLockAfter + time.Second)) // past staleLockAfter
	if !canAcquireSessionLock(existing, me, false, now) {
		t.Fatal("want acquirable: stopped holder + heartbeat past staleLockAfter (old code held this behind the 10-min pid-reuse veto)")
	}

	// Sanity: the same row with a RUNNING pid keeps the full D1 veto —
	// staleness past staleLockAfter must NOT reclaim a live running holder.
	// Use a SECOND child left running (os.Getpid() would match `me` and hit
	// the reentrant branch before the liveness check ever runs).
	running := startSleepProcess(t)
	existing.Owner.PID = running.Pid
	if canAcquireSessionLock(existing, me, false, now) {
		t.Fatal("want NOT acquirable: RUNNING holder with merely-stale heartbeat keeps the D1 veto")
	}
}

// TestAcquireSessionLock_StoppedHolderReclaimed is the DB-level counterpart:
// through the real AcquireSessionLock path, a lock row naming a stopped
// process with a stale heartbeat is taken over without --force.
func TestAcquireSessionLock_StoppedHolderReclaimed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	proc := startStoppedProcess(t)

	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	holder := models.LockOwner{PID: proc.Pid, Host: host}
	if err := store.AcquireSessionLock(sess.ID, holder, false); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	// Age the heartbeat past staleLockAfter: the lock row's heartbeat is
	// refreshed on acquire, so rewind it directly (unixFrac — the column
	// stores fractional seconds).
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`,
		unixFrac(time.Now().Add(-(staleLockAfter + time.Second))), sess.ID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	other := models.LockOwner{PID: os.Getpid(), Host: host}
	if err := store.AcquireSessionLock(sess.ID, other, false); err != nil {
		t.Fatalf("want takeover of stopped holder without force, got: %v", err)
	}
}

// TestAcquireOrHandleLock_StoppedHolderMessageIsAccurate is the message
// contract for the stopped branch (M3 round-1, M5/M6 round-2): with a
// same-host STOPPED holder and a fresh heartbeat — a guaranteed conflict —
// the error must lead with fg (the zero-wait, zero-loss exit), offer the
// short-clock auto takeover, --fork, and --force with its /fork-rescue
// note, and keep kill LAST with an explicit irreversible-data-loss
// caveat. The generic kill -9 leftover-pid line stays banned: it is
// known-false when the holder is verifiably stopped.
func TestAcquireOrHandleLock_StoppedHolderMessageIsAccurate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	proc := startStoppedProcess(t)

	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{CWD: "/proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	holder := models.LockOwner{PID: proc.Pid, Host: host}
	// Fresh heartbeat: within staleLockAfter, so even the stopped branch
	// refuses — the exact moment a user hits this message.
	if err := store.AcquireSessionLock(sess.ID, holder, false); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	r := &ChatRepl{
		cfg:       ReplConfig{WorkDir: "/proj"},
		sessMgr:   store,
		lockOwner: models.LockOwner{PID: os.Getpid(), Host: host},
	}
	err = r.acquireOrHandleLock(sess)
	if err == nil {
		t.Fatal("acquireOrHandleLock() = nil, want the stopped-holder lock error")
	}
	msg := err.Error()
	for _, want := range []string{"停止状态", "fg", "无需任何参数", "加 --force", "/fork", "kill", "不可逆"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stopped-holder message missing %q; got:\n%s", want, msg)
		}
	}
	for _, banned := range []string{"kill -9"} {
		if strings.Contains(msg, banned) {
			t.Errorf("stopped-holder message must not contain %q (known-false when holder is stopped); got:\n%s", banned, msg)
		}
	}
}
