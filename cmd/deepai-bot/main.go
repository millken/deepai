// Command deepai-bot runs the deepai agent as a long-lived background
// service. Clients (the bundled web console, a desktop GUI, a mobile app, a
// chat-platform adapter) drive it over HTTP, Server-Sent Events, NDJSON or
// WebSocket. See cmd/deepai-bot/README.md and docs/BOT_DESIGN.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run())
}

// run is separate from main so every deferred cleanup (session persistence,
// MCP shutdown, state-file removal) completes before os.Exit.
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}
