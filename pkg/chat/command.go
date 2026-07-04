package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Command is a file-based slash command. Its Body (after argument expansion) is
// injected as a user-turn prompt when the user types /Name.
type Command struct {
	Name        string // invocation name (plugin commands are "plugin:cmd")
	Description string
	Body        string
	Source      string // "user" | "project" | "plugin"
}

// PluginCommandDir pairs a plugin name (used as the command prefix) with its
// commands directory.
type PluginCommandDir struct {
	Plugin string
	Dir    string
}

// LoadCommands discovers file-commands from user (~/.deepai/commands),
// project (<workDir>/.deepai/commands), and each plugin's commands dir.
// Load order is user → project → plugin (last write wins for same name across
// user/project); plugin commands are registered as "plugin:cmd" so they never
// collide with bare names. Any name colliding with a builtin slash command is
// skipped+warned. problems holds per-file issues for the caller to surface.
func LoadCommands(workDir string, plugins []PluginCommandDir) (map[string]Command, []string) {
	builtin := make(map[string]bool, len(slashCommands))
	for _, c := range slashCommands {
		builtin[c.Name] = true
	}

	cmds := make(map[string]Command)
	var problems []string
	load := func(dir, source, prefix string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // missing dir is silent
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := prefix + strings.TrimSuffix(e.Name(), ".md")
			if err := validateCommandName(name); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s", name, err))
				continue
			}
			if builtin[name] {
				problems = append(problems, fmt.Sprintf("%s: conflicts with builtin slash command", name))
				continue
			}
			c, err := parseCommandFile(filepath.Join(dir, e.Name()), name, source)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s", name, err))
				continue
			}
			cmds[name] = c
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		load(filepath.Join(home, ".deepai", "commands"), "user", "")
	}
	if strings.TrimSpace(workDir) != "" {
		load(filepath.Join(workDir, ".deepai", "commands"), "project", "")
	}
	for _, p := range plugins {
		load(p.Dir, "plugin", p.Plugin+":")
	}
	return cmds, problems
}

func parseCommandFile(path, name, source string) (Command, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Command{}, fmt.Errorf("read: %w", err)
	}
	fm, body, err := splitCommandFrontmatter(data)
	if err != nil {
		return Command{}, fmt.Errorf("parse: %w", err)
	}
	// Parse frontmatter defensively: extract only `description`, ignore unknown
	// fields and type mismatches (e.g. argument-hint/allowed-tools in list form).
	// A single odd field must not reject the whole command.
	desc := ""
	if strings.TrimSpace(fm) != "" {
		var meta map[string]any
		if err := yaml.Unmarshal([]byte(fm), &meta); err == nil {
			if d, ok := meta["description"].(string); ok {
				desc = strings.TrimSpace(d)
			}
		}
	}
	body = strings.TrimSpace(body)
	if desc == "" {
		desc = firstNonEmptyLine(body)
	}
	return Command{Name: name, Description: desc, Body: body, Source: source}, nil
}

func validateCommandName(name string) error {
	if name == "" {
		return fmt.Errorf("empty command name")
	}
	if strings.ContainsAny(name, " \t\n/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid command name %q", name)
	}
	return nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(line, "#")); t != "" {
			return t
		}
	}
	return ""
}

func splitCommandFrontmatter(data []byte) (fm, body string, err error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return "", strings.TrimSpace(s), nil
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", fmt.Errorf("missing closing frontmatter delimiter")
	}
	fm = rest[:idx]
	body = strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	return fm, strings.TrimSpace(body), nil
}

// positionalArgRE matches $1, $2, … for positional argument substitution.
var positionalArgRE = regexp.MustCompile(`\$(\d+)`)

// Expand substitutes $ARGUMENTS (all args) and $1/$2/… (positional, 1-based,
// out-of-range → empty) in body. args is the raw text after the command name.
func Expand(body, args string) string {
	out := strings.ReplaceAll(body, "$ARGUMENTS", args)
	fields := strings.Fields(args)
	return positionalArgRE.ReplaceAllStringFunc(out, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		if n >= 1 && n <= len(fields) {
			return fields[n-1]
		}
		return ""
	})
}

// SortedCommands returns cmds sorted by name for stable /help display.
func SortedCommands(cmds map[string]Command) []Command {
	out := make([]Command, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
