package commands

import "github.com/spf13/cobra"

func AddCommands(topLevel *cobra.Command) {
	addVersion(topLevel)
}
