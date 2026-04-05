package skill

import (
	"strings"
	"testing"
)

func TestReplaceVariables_Arguments(t *testing.T) {
	skill := &Skill{Dir: "/tmp/test-skill"}

	tests := []struct {
		name string
		body string
		args string
		want string
	}{
		{
			name: "standalone $ARGUMENTS",
			body: "Fix issue $ARGUMENTS now.",
			args: "123",
			want: "Fix issue 123 now.",
		},
		{
			name: "$ARGUMENTS not present — append",
			body: "Fix the issue.",
			args: "123",
			want: "Fix the issue.\n\nARGUMENTS: 123",
		},
		{
			name: "no args — no append",
			body: "Fix the issue.",
			args: "",
			want: "Fix the issue.",
		},
		{
			name: "$ARGUMENTS[0] actual value",
			body: "View issue $ARGUMENTS[0] and $ARGUMENTS[1]",
			args: "123 456",
			want: "View issue 123 and 456",
		},
		{
			name: "$0 $1 shorthand actual values",
			body: "Migrate $0 from $1 to $2",
			args: "SearchBar React Vue",
			want: "Migrate SearchBar from React to Vue",
		},
		{
			name: "${SKILL_DIR}",
			body: "Run ${SKILL_DIR}/scripts/test.sh",
			args: "",
			want: "Run /tmp/test-skill/scripts/test.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceVariables(tt.body, tt.args, skill, "test-session")
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestReplaceVariables_CommandInjection(t *testing.T) {
	skill := &Skill{Dir: "/tmp/test-skill"}

	// $ARGUMENTS in !`command` must be replaced with env var ref, NOT actual value
	got := replaceVariables("Info: !`gh issue view $ARGUMENTS`", "123; rm -rf /", skill, "test-session")
	// The command block must contain env var ref, not raw args
	expected := "!`gh issue view " + `"$SKILL_ARGS"` + "`"
	if !strings.Contains(got, expected) {
		t.Errorf("command block should use env var ref, got: %s", got)
	}
	// Raw args must NOT appear inside the command block
	if strings.Contains(got, "!`gh issue view 123; rm -rf /`") {
		t.Errorf("command injection: raw args leaked into command block, got: %s", got)
	}
}

func TestReplaceVariables_CommandArgsEnvRef(t *testing.T) {
	skill := &Skill{Dir: "/tmp/test-skill"}

	tests := []struct {
		name string
		body string
		args string
		want string
	}{
		{
			name: "$ARGUMENTS in command → env ref",
			body: "Run: !`echo $ARGUMENTS`",
			args: "hello world",
			want: "Run: !`echo \"$SKILL_ARGS\"`\n\nARGUMENTS: hello world",
		},
		{
			name: "$0 in command → env ref",
			body: "Run: !`echo $0`",
			args: "hello world",
			want: "Run: !`echo \"$SKILL_ARG_0\"`\n\nARGUMENTS: hello world",
		},
		{
			name: "$ARGUMENTS[0] in command → env ref",
			body: "Run: !`echo $ARGUMENTS[0]`",
			args: "hello world",
			want: "Run: !`echo \"$SKILL_ARG_0\"`\n\nARGUMENTS: hello world",
		},
		{
			name: "no args in command — append args",
			body: "Run: !`echo hello`",
			args: "world",
			want: "Run: !`echo hello`\n\nARGUMENTS: world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceVariables(tt.body, tt.args, skill, "test-session")
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestReplaceVariables_MixedCommandAndText(t *testing.T) {
	skill := &Skill{Dir: "/skills/deploy"}
	body := "Deploy $ARGUMENTS to production.\nRun ${SKILL_DIR}/scripts/build.sh\nThen test $0 component.\nOutput: !`echo $0`"
	args := "v1.2.3 frontend"

	got := replaceVariables(body, args, skill, "test-session")

	if !strings.Contains(got, "Deploy v1.2.3 frontend to production.") {
		t.Error("$ARGUMENTS not replaced with actual value")
	}
	if !strings.Contains(got, "/skills/deploy/scripts/build.sh") {
		t.Error("${SKILL_DIR} not replaced")
	}
	// $0 in text → actual value
	if !strings.Contains(got, "test v1.2.3 component") {
		t.Error("$0 in text not replaced with actual value")
	}
	// $0 in !`command` → env var ref (security)
	if !strings.Contains(got, `echo "$SKILL_ARG_0"`) {
		t.Error("$0 in command not replaced with env var ref")
	}
}

func TestReplaceVariables_AppendArgsOnlyNonCommand(t *testing.T) {
	skill := &Skill{Dir: "/tmp/test"}
	// $ARGUMENTS only in command block, not in text → should still append to non-command text
	body := "Do something.\nCheck: !`echo $ARGUMENTS`"
	args := "test-arg"

	got := replaceVariables(body, args, skill, "test-session")
	if !strings.Contains(got, "ARGUMENTS: test-arg") {
		t.Errorf("expected args appended for non-command text, got: %s", got)
	}
}

func TestParseDynamicInjections(t *testing.T) {
	content := "\n## PR Info\n" +
		"- Diff: !`gh pr diff`\n" +
		"- Comments: !`gh pr view --comments`\n" +
		"- Issue: !`gh issue view $ARGUMENTS[0]`\n"

	injections := ParseDynamicInjections(content)
	if len(injections) != 3 {
		t.Fatalf("count = %d, want 3", len(injections))
	}

	if injections[0].Command != "gh pr diff" {
		t.Errorf("[0] Command = %q", injections[0].Command)
	}
	if injections[0].HasArgs {
		t.Error("[0] HasArgs = true, want false")
	}

	if injections[2].Command != "gh issue view $ARGUMENTS[0]" {
		t.Errorf("[2] Command = %q", injections[2].Command)
	}
	if !injections[2].HasArgs {
		t.Error("[2] HasArgs = false, want true")
	}
}

func TestParseDynamicInjections_None(t *testing.T) {
	content := "No dynamic injections here."
	if injections := ParseDynamicInjections(content); len(injections) != 0 {
		t.Errorf("got %d injections, want 0", len(injections))
	}
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"123", []string{"123"}},
		{"SearchBar React Vue", []string{"SearchBar", "React", "Vue"}},
		{"  spaced  out  ", []string{"spaced", "out"}},
	}

	for _, tt := range tests {
		got := splitArgs(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitArgs(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestReplaceTextArgs_OutOfBounds(t *testing.T) {
	// Index beyond arg count → leave as-is
	got, found := replaceTextArgs("View $ARGUMENTS[5] and $9", "a b", strings.Fields("a b"))
	if !found {
		t.Error("expected found=true")
	}
	if !strings.Contains(got, "$ARGUMENTS[5]") {
		t.Error("out-of-bounds $ARGUMENTS[5] should be left as-is")
	}
	if !strings.Contains(got, "$9") {
		t.Error("out-of-bounds $9 should be left as-is")
	}
}

func TestReplaceCommandArgs_OutOfBounds(t *testing.T) {
	// Even out-of-bounds → env var ref (shell will handle empty var)
	got := replaceCommandArgs("echo $ARGUMENTS[5] and $9")
	if !strings.Contains(got, `$SKILL_ARG_5`) {
		t.Error("expected env var ref for out-of-bounds index")
	}
	if !strings.Contains(got, `$SKILL_ARG_9`) {
		t.Error("expected env var ref for out-of-bounds $9")
	}
}
