package main

import (
	"context"
	"os"
	"os/signal"

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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	// cobra already prints "Error: ..." for a failing command (SilenceErrors
	// is not set on the root command), so just translate failure into a
	// non-zero exit code without printing again.
	if err := root.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}
