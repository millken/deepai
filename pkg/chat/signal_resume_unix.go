//go:build unix

package chat

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResumeRefresh delivers the resume half of issue #18: on every
// SIGCONT — fg, shell job control, anything that resumes this process —
// run one synchronous RefreshSessionLock through the existing
// heartbeatTick/handleHeartbeatResult semantics (only models.ErrLockNotHeld
// is definitive and reaches onLockLost; transient errors are logged and
// retried by the next ordinary tick). This turns "fg back and discover the
// lock was taken over" from a ticker-timing accident into a deterministic
// first move, without touching the stop side at all.
//
// Why ONLY SIGCONT, and no interception of SIGTSTP/SIGTTIN/SIGTTOU (the
// design record, per this repo's comments-are-the-spec rule): intercepting
// the tty stop signals makes things strictly worse on the path that
// actually produced the two PR #17 incidents (zsh "suspended (tty input)"
// = SIGTTIN). tty_check_change returns -ERESTARTSYS to a background tty
// read; with no handler the kernel stops the process once, at zero cost.
// Installing a Go handler (SA_RESTART) means the handler returns, the read
// is restarted, re-faults SIGTTIN, and the loop spins hot until the
// self-SIGSTOP lands — measured in a pty/setsid harness (PR #19 round-1
// review, linux/amd64): 379,987 SIGTTIN deliveries and ~4.2 CPU-seconds in
// a 1.5s window; ~1.1 CPU-seconds burned per stop even with the refresh
// budget at its 500ms cap. Worse, the queued stop signals outlive the stop
// itself: after a legitimate fg the job re-suspends several more times
// draining the backlog, and the SIGCONT that resumed it is dropped by the
// full signal channel — the deterministic refresh this watcher exists for
// would never fire on exactly the path that matters. The kernel's default
// one-shot stop needs no help from us; only the resume side did.
//
// The pre-stop heartbeat refresh from the original design is likewise
// gone. Its only purchase was making the 60s stopped-holder short clock
// count from the real stop moment; without it the frozen row may be up to
// one heartbeat interval (15s) stale, so reclaim lands at 45–75s — still
// inside the "稍等约 1 分钟" the stopped-holder message promises. If exact
// 60s-from-stop is ever wanted, extend the stopped-branch threshold by
// sessionLockHeartbeatInterval — pure arithmetic, no signal handling.
//
// signal.Stop(SIGCONT) on the way out is harmless, unlike stopping
// notification for a STOP signal: sigdisable restores the pre-Notify
// disposition (typically SIG_DFL), and SIG_DFL for SIGCONT is "resume",
// a no-op on a running process — nothing is swallowed. Exit bound: a CONT
// landing just before done closes may run one in-flight refresh (bounded
// by RefreshSessionLock's busy_timeout) before the watcher returns, so the
// Run() WaitGroup join stays bounded by that.
func (r *ChatRepl) watchResumeRefresh(done <-chan struct{}) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCONT)
	defer signal.Stop(sig)
	for {
		select {
		case <-done:
			return
		case <-sig:
			// A CONT racing with shutdown must not kick off a refresh
			// after done has closed — select picks ready cases at random,
			// so re-check done non-blockingly before doing any work.
			select {
			case <-done:
				return
			default:
			}
			lost, err := r.heartbeatTick()
			r.handleHeartbeatResult(lost, err)
		}
	}
}
