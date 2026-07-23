package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/millken/deepai/pkg/commands"
)

func main() {
	root := commands.New()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		// Exit with error code, but without os.Exit to allow defer cleanup
		// (including TUI terminal state restoration)
		return
	}
}
