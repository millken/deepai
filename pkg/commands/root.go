package commands

import (
	"errors"
	"log/slog"
	"os"

	"github.com/dnsoa/go/env"
	"github.com/millken/deepai/pkg/logs"
	"github.com/spf13/cobra"
)

var Root = New()

func New() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:               "deepai",
		Short:             "AI coding assistant powered by LLMs",
		SilenceUsage:      true, // Don't show usage on errors
		DisableAutoGenTag: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Load ~/.deepai/.env early so all providers can read API keys.
			if err := env.Load(EnvFile()); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					slog.Warn("failed to load .env", "path", EnvFile(), "err", err)
				} else {
					slog.Debug(".env not found, skipping", "path", EnvFile())
				}
			}

			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			cleanup, err := logs.Setup(logs.Config{
				Level:     level,
				DebugFile: "deepai-debug.log",
				ErrorFile: "deepai-error.log",
			})
			if err != nil {
				slog.Warn("failed to set up logging", "err", err)
				return
			}
			// Ensure cleanup is called on program exit to flush logs.
			cobra.OnFinalize(cleanup)
			slog.Debug("logging initialized", "verbose", verbose)

		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChat(cmd.Context(), chatFlags.Query, chatFlags.Resume, chatFlags.Continue, chatFlags.Model, chatFlags.MaxTurns)
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logs")
	root.CompletionOptions.HiddenDefaultCmd = true

	RegisterChatFlags(root)
	AddCommands(root)
	return root
}
