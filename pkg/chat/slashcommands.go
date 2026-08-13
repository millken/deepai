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
	{"refine", "Refine memory now (/refine [undo|rollback <id>|list|status|on|off])"},
	{"plan", "Enter plan mode (read-only, explore before coding)"},
	{"run", "Exit plan mode (full tool access)"},
	{"model", "Show or switch model (/model <name>, /model ? for picker)"},
	{"effort", "Show or set reasoning effort (/effort [low|medium|high|disabled])"},
	{"image", "Attach image: Ctrl+V, @path, or /image <path>"},
	{"imagedetail", "Set vision detail: /imagedetail [low|high]"},
	{"doctor", "Check environment: probe models, skills, and MCP servers"},
	{"status", "Show loaded tools/plugins and tool call stats"},
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

func slashHelpText(custom []Command) string {
	lines := []string{"", "  Commands:"}
	for _, c := range slashCommands {
		lines = append(lines, "    /"+padRight(c.Name, 10)+c.Desc)
	}
	if len(custom) > 0 {
		lines = append(lines, "  Custom commands:")
		for _, c := range custom {
			lines = append(lines, "    /"+padRight(c.Name, 22)+c.Description)
		}
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
