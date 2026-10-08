package chat

import (
	"os"
	"os/exec"
	"runtime"
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

// TestProcessStopped_RealSigstop probes processStopped against a real child
// process driven through an actual SIGSTOP/SIGCONT cycle — not a mock — so
// the darwin sysctl and linux /proc parsers are both exercised for real.
func TestProcessStopped_RealSigstop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no job control on windows")
	}
	proc := startSleepProcess(t)

	if processStopped(proc.Pid) {
		t.Fatal("running child must not report stopped")
	}
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	// Give the kernel a moment to move the process into the stopped state.
	deadline := time.Now().Add(2 * time.Second)
	for !processStopped(proc.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processStopped(proc.Pid) {
		t.Fatal("SIGSTOP'd child must report stopped")
	}
	if err := proc.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("re-SIGSTOP: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !processStopped(proc.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processStopped(proc.Pid) {
		t.Fatal("re-SIGSTOP'd child must report stopped")
	}
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
	proc := startSleepProcess(t)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !processStopped(proc.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processStopped(proc.Pid) {
		t.Fatal("precondition: child must be stopped")
	}

	me := models.LockOwner{PID: os.Getpid(), Host: "h"}
	now := time.Now()
	existing := &sessionLockRow{
		Owner:       models.LockOwner{PID: proc.Pid, Host: "h"},
		AcquiredAt:  now.Add(-2 * time.Hour),
		HeartbeatAt: now.Add(-30 * time.Second), // fresh-ish: within staleLockAfter
	}
	if canAcquireSessionLock(existing, me, false, now) {
		t.Fatal("want NOT acquirable: stopped holder, heartbeat still within staleLockAfter")
	}

	existing.HeartbeatAt = now.Add(-61 * time.Second) // past staleLockAfter
	if !canAcquireSessionLock(existing, me, false, now) {
		t.Fatal("want acquirable: stopped holder + heartbeat past staleLockAfter (old code held this behind the 10-min pid-reuse veto)")
	}

	// Sanity: the same row with a RUNNING pid keeps the full D1 veto — 61s
	// staleness must NOT reclaim a live running holder. Use a SECOND child
	// left running (os.Getpid() would match `me` and hit the reentrant branch
	// before the liveness check ever runs).
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
	proc := startSleepProcess(t)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !processStopped(proc.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processStopped(proc.Pid) {
		t.Fatal("precondition: child must be stopped")
	}

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
	// refreshed on acquire, so rewind it directly.
	if _, err := store.db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`,
		time.Now().Add(-61*time.Second).Unix(), sess.ID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	other := models.LockOwner{PID: os.Getpid(), Host: host}
	if err := store.AcquireSessionLock(sess.ID, other, false); err != nil {
		t.Fatalf("want takeover of stopped holder without force, got: %v", err)
	}
}
