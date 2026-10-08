//go:build unix

package chat

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// stopHeartbeatBudget bounds how long a stop-signal handler may spend
// refreshing the heartbeat before stopping the process. RefreshSessionLock
// can queue behind a concurrent write for its full busy_timeout (5s); a user
// pressing Ctrl+Z must not wait that long for the process to actually stop.
// On timeout the refresh goroutine may still land its write before the
// process freezes; either way the post-CONT refresh makes the heartbeat
// deterministic again.
const stopHeartbeatBudget = 500 * time.Millisecond

// watchStopResume owns this REPL's stop/resume signal lifecycle (issue #18).
//
// Why it exists: between turns the TUI restores cooked mode, and a
// backgrounded deepai that touches the tty gets SIGTTIN — both paths stop
// the process mid-heartbeat-cycle, freezing the lock row's heartbeat while
// its holder sits stopped, the exact combination the processStopped short
// clock in canAcquireSessionLock was added to reclaim. Both real incidents
// behind PR #17 ended in zsh's "suspended (tty input)" — SIGTTIN, not
// Ctrl+Z — so all three tty stop signals are handled identically here.
// This watcher makes the freeze well-behaved at the source:
//
//   - before stopping: one best-effort bounded heartbeat refresh, so the row
//     enters the freeze fresh and the 60s short clock counts from the real
//     stop moment, not from a heartbeat up to 15s older. The result is
//     discarded: a loss detected here would render into a terminal the user
//     has already handed back to their shell; the post-CONT refresh reports
//     it when it can actually be seen.
//   - after resuming: one synchronous RefreshSessionLock, turning "fg back
//     and discover the lock was taken over" from a ticker-timing accident
//     into a deterministic first move. Failure semantics are exactly
//     heartbeatTick's: only models.ErrLockNotHeld is definitive (and routes
//     to onLockLost); transient errors are logged and retried by the next
//     ordinary tick.
//
// The stop itself is a self-delivered SIGSTOP, NOT a Reset+re-raise of the
// stop signal. Re-raising cannot work in a pure Go program: signal.Reset
// only stops os/signal's user-channel delivery — runtime sigdisable
// uninstalls the Go handler solely when sigInstallGoHandler(sig) is false,
// which for the tty stop signals it never is — so a re-raised SIGTSTP
// lands in sighandler, finds no consumer, and returns without stopping the
// process (verified empirically on darwin/Go 1.26: Reset+Kill(self, TSTP)
// leaves the process runnable; the pre-Notify default stop only works
// because no handler was installed in the first place). SIGSTOP is
// uncatchable at the kernel level, stops the process just the same, and
// lands in the same stopped state PR #17's probes read (darwin P_stat
// SSTOP, linux /proc stat T), so the shell's WUNTRACED/fg flow and
// canAcquireSessionLock's short clock are unaffected. For SIGTTIN there is
// a follow-on: after a CONT that leaves the process backgrounded, the
// pending tty read retries, re-faults TTIN, and this handler stops it
// again — each cycle carries a fresh heartbeat, and progress requires an
// external CONT, so it cannot spin.
//
// done follows the heartbeat goroutine's lifecycle: close → signal.Stop →
// return. A handler blocked inside the self-SIGSTOP cannot hold up exit
// (the whole process is frozen then); once resumed it observes done and
// returns, so the Run() WaitGroup join stays bounded.
func (r *ChatRepl) watchStopResume(done <-chan struct{}) {
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU, syscall.SIGCONT)
	defer signal.Stop(sig)
	for {
		select {
		case <-done:
			return
		case s := <-sig:
			if s == syscall.SIGCONT {
				lost, err := r.heartbeatTick()
				r.handleHeartbeatResult(lost, err)
				continue
			}
			r.refreshHeartbeatBounded()
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
			// Resumed by a CONT from fg or the shell. That same CONT is
			// queued on this channel too, so the next loop iteration runs
			// the deterministic post-resume refresh.
		}
	}
}

// refreshHeartbeatBounded runs one heartbeatTick under stopHeartbeatBudget.
func (r *ChatRepl) refreshHeartbeatBounded() {
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		_, _ = r.heartbeatTick()
	}()
	select {
	case <-fin:
	case <-time.After(stopHeartbeatBudget):
	}
}
