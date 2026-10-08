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
// lock was taken over" from a ticker-timing accident into a discovery that
// deterministically happens within milliseconds of resume (the watcher
// goroutine and the REPL goroutine resume concurrently — the contract is
// not "before any write" but "immediately", and in practice the user's
// fg is followed by input and a model round-trip before any appendMessage),
// without touching the stop side at all.
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
// one-shot stop needs no help from us; only the resume side did. A
// corollary that narrows the stopped-holder scenario further: the kernel
// DROPS job-control stop signals (TSTP/TTIN/TTOU) entirely for members of
// an orphaned process group — only SIGSTOP always applies — so a deepai
// orphaned by its shell cannot even enter the stopped state via the tty
// signals (PR #19 round-2 review, verified with a Setpgid harness). The
// original interception design would have diverged here too: it
// self-delivered SIGSTOP precisely in those cases where the kernel meant
// to drop the signal, creating a stop the shell never learns about.
//
// The pre-stop heartbeat refresh from the original design is likewise
// gone. Its only purchase was making the 60s stopped-holder short clock
// count from the real stop moment; without it the frozen row may be up to
// one heartbeat interval (15s) stale, so reclaim lands at stop + 45–60s
// (the heartbeat cannot be NEWER than the stop) — inside the "稍等约 1
// 分钟" the stopped-holder message promises. If a guaranteed floor of 60s
// is ever wanted, extend the stopped-branch threshold by
// sessionLockHeartbeatInterval (→ stop + 60–75s) — pure arithmetic, no
// signal handling; exact 60s-from-stop would require knowing the frozen
// row's unknowable 0–15s age.
//
// signal.Stop(SIGCONT) on the way out is harmless, but NOT because the
// disposition reverts: sigdisable leaves the Go handler installed for
// SIGCONT too (sigInstallGoHandler is true for it, same as for the stop
// signals — SigCgt keeps the bit after Stop). The correct mechanism: the
// kernel performs SIGCONT's "resume" action unconditionally, independent
// of any disposition — a handler (or its absence) can only add user-space
// observation on top, never swallow the resume. So after Stop, a CONT
// still resumes a stopped process and is simply unseen by us — nothing
// behaviorally swallowed. Exit bound: a CONT
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
