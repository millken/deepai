package chat

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCmd(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadCommands_SourcesPriorityAndPluginPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCmd(t, filepath.Join(home, ".deepai", "commands", "u1.md"), "---\ndescription: user one\n---\nuser body")
	writeCmd(t, filepath.Join(home, ".deepai", "commands", "shared.md"), "---\ndescription: from user\n---\nuser shared")

	proj := t.TempDir()
	writeCmd(t, filepath.Join(proj, ".deepai", "commands", "p1.md"), "---\ndescription: proj one\n---\nproj body")
	writeCmd(t, filepath.Join(proj, ".deepai", "commands", "shared.md"), "---\ndescription: from project\n---\nproj shared")

	plug := t.TempDir()
	writeCmd(t, filepath.Join(plug, "greet.md"), "---\ndescription: greet\n---\nhi $ARGUMENTS")

	cmds, problems := LoadCommands(proj, []PluginCommandDir{{Plugin: "demo", Dir: plug}})
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if cmds["u1"].Source != "user" {
		t.Fatalf("u1 source = %q", cmds["u1"].Source)
	}
	if cmds["p1"].Source != "project" {
		t.Fatalf("p1 source = %q", cmds["p1"].Source)
	}
	if cmds["shared"].Description != "from project" {
		t.Fatalf("project should win over user for shared; got %q", cmds["shared"].Description)
	}
	if cmds["demo:greet"].Body != "hi $ARGUMENTS" {
		t.Fatalf("plugin command must be prefixed: %+v", cmds["demo:greet"])
	}
	if cmds["demo:greet"].Source != "plugin" {
		t.Fatalf("plugin source: %q", cmds["demo:greet"].Source)
	}
}

func TestLoadCommands_BuiltinCollisionSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCmd(t, filepath.Join(home, ".deepai", "commands", "help.md"), "---\ndescription: x\n---\nbody")
	cmds, problems := LoadCommands("", nil)
	if _, ok := cmds["help"]; ok {
		t.Fatal("a file command must not override the builtin /help")
	}
	if len(problems) != 1 {
		t.Fatalf("expected 1 problem for builtin collision, got %v", problems)
	}
}

func TestLoadCommands_DescriptionFallbackToFirstLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No frontmatter; description falls back to the first non-empty body line.
	writeCmd(t, filepath.Join(home, ".deepai", "commands", "no-fm.md"), "Review the code carefully.\nMore detail.")
	cmds, _ := LoadCommands("", nil)
	if cmds["no-fm"].Description != "Review the code carefully." {
		t.Fatalf("description fallback = %q", cmds["no-fm"].Description)
	}
}

func TestLoadCommands_FrontmatterToleratesOddFields(t *testing.T) {
	// argument-hint as a YAML sequence (!seq) must NOT reject the whole command;
	// description is still extracted, body kept.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCmd(t, filepath.Join(home, ".deepai", "commands", "odd.md"),
		"---\ndescription: odd command\nargument-hint: [words]\nallowed-tools: Bash(git:*)\n---\nbody text")
	cmds, problems := LoadCommands("", nil)
	if len(problems) != 0 {
		t.Fatalf("defensive parse should yield no problems: %v", problems)
	}
	if cmds["odd"].Description != "odd command" {
		t.Fatalf("description = %q", cmds["odd"].Description)
	}
	if cmds["odd"].Body != "body text" {
		t.Fatalf("body = %q", cmds["odd"].Body)
	}
}

func TestExpand_ArgumentsAndPositional(t *testing.T) {
	cases := []struct{ body, args, want string }{
		{"all: $ARGUMENTS", "a b c", "all: a b c"},
		{"$1 and $2 and $3", "x y", "x and y and "},        // $3 out of range → empty
		{"fix #$ARGUMENTS now", "123", "fix #123 now"},     // adjacent
		{"no args [$ARGUMENTS] end", "", "no args [] end"}, // empty args
		{"first $1 only", "alpha", "first alpha only"},
	}
	for _, c := range cases {
		if got := Expand(c.body, c.args); got != c.want {
			t.Errorf("Expand(%q, %q) = %q, want %q", c.body, c.args, got, c.want)
		}
	}
}
