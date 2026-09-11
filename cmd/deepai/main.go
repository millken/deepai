package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/millken/deepai/pkg/commands"
)

func main() {
	os.Exit(run())
}

// run executes the root command and returns the process exit code. Exit
// logic lives here, separate from main, so that every deferred cleanup in
// this function (context cancellation, and anything cobra/the TUI defers
// during ExecuteContext) runs to completion before main calls os.Exit —
// os.Exit terminates the process immediately and does not run deferred
// calls, so it must never be called from a function that still has defers
// pending.
func run() int {
	root := commands.New()

	// SIGTERM (plain `kill <pid>`, an IDE's stop button) and SIGHUP (a
	// closed terminal — NOT SIGTERM; a terminal that goes away sends its
	// foreground process group SIGHUP, per POSIX job control, and that is
	// what actually fires when a user closes the window mid-session), as
	// well as SIGINT (Ctrl+C): without trapping these, only Ctrl+C ran the
	// deferred session-lock release (pkg/chat/repl.go's Run()), and a
	// killed or SIGHUP'd process left its lock to expire on staleLockAfter
	// (60s, pkg/chat/session.go) instead of being released immediately.
	// (L2, session-lock review round 2: the comment used to misattribute
	// "closed terminal" to SIGTERM, and SIGHUP was not trapped at all —
	// fixed here alongside the comment, since it is a one-line addition
	// that actually closes the gap the comment already claimed was closed.)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	// cobra already prints "Error: ..." for a failing command (SilenceErrors
	// is not set on the root command), so just translate failure into a
	// non-zero exit code without printing again.
	if err := root.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}
