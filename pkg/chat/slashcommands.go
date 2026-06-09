package chat

import "strings"

type slashCmd struct {
	Name string
	Desc string
}

var slashCommands = []slashCmd{
	{"help", "Show this help"},
	{"clear", "Clear session history"},
	{"history", "Show conversation history"},
	{"sessions", "List recent sessions"},
	{"new", "Start a new session"},
	{"title", "Set session title"},
	{"save", "Save session metadata"},
	{"undo", "Undo last turn"},
	{"compact", "Compact context now"},
	{"plan", "Enter plan mode (read-only, explore before coding)"},
	{"run", "Exit plan mode (full tool access)"},
	{"model", "Show current model (/model <name> to switch)"},
	{"exit", "Exit the REPL"},
}

// matchSlashCommands returns the commands whose name starts with prefix, in
// declaration order. An empty prefix matches all.
func matchSlashCommands(prefix string) []slashCmd {
	prefix = strings.ToLower(prefix)
	var out []slashCmd
	for _, c := range slashCommands {
		if strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func slashHelpText() string {
	lines := []string{"", "  Commands:"}
	for _, c := range slashCommands {
		lines = append(lines, "    /"+padRight(c.Name, 10)+c.Desc)
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}
