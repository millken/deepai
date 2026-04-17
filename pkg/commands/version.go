package commands

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is provided by govvv at compile-time
var Version string

// addVersion augments our CLI surface with version.
func addVersion(topLevel *cobra.Command) {
	topLevel.AddCommand(&cobra.Command{
		Use:   "version",
		Short: `Print ko version.`,
		Run: func(cmd *cobra.Command, args []string) {
			v := version()
			if v == "" {
				fmt.Println("could not determine build information")
			} else {
				fmt.Println(v)
			}
		},
	})
}

func version() string {
	if Version == "" {
		i, ok := debug.ReadBuildInfo()
		if !ok {
			return ""
		}
		Version = i.Main.Version
	}
	return Version
}
