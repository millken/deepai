package commands

import "github.com/spf13/cobra"

func AddCommands(topLevel *cobra.Command) {
	addChat(topLevel)
	addSetup(topLevel)
	addSession(topLevel)
	addVersion(topLevel)
}
