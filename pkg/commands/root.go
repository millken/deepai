package commands

import (
	"log/slog"
	"os"

	"github.com/millken/deepai/pkg/logs"
	"github.com/spf13/cobra"
)

var Root = New()

func New() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:               "deepai",
		Short:             "deepai tools",
		SilenceUsage:      true, // Don't show usage on errors
		DisableAutoGenTag: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if verbose {
				slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
			}
			cleanup, err := logs.Setup(logs.Config{
				Level:     slog.LevelInfo,
				DebugFile: "deepai-debug.log",
			})
			if err != nil {
				slog.Warn("failed to set up logging", "err", err)
				return
			}
			// Ensure cleanup is called on program exit to flush logs.
			cobra.OnFinalize(cleanup)
			slog.Debug("logging initialized", "verbose", verbose)

		},
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logs")

	AddCommands(root)
	return root
}
