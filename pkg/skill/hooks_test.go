package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookRunner_Run_Success(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	hr := NewHookRunner()
	result := hr.Run(context.Background(), Hook{
		Event:   "PreRun",
		Command: "echo hello",
	}, skill, "sess-123")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Output != "hello" {
		t.Errorf("output = %q, want %q", result.Output, "hello")
	}
	if result.Aborted {
		t.Error("should not be aborted")
	}
}

func TestHookRunner_Run_FailureContinue(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	hr := NewHookRunner()
	result := hr.Run(context.Background(), Hook{
		Event:   "OnError",
		Command: "false", // exit 1
		OnError: HookErrorContinue,
	}, skill, "")

	if result.Error == nil {
		t.Fatal("expected error for failing command")
	}
	if result.Aborted {
		t.Error("continue policy should not abort")
	}
}

func TestHookRunner_Run_FailureAbort(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	hr := NewHookRunner()
	result := hr.Run(context.Background(), Hook{
		Event:   "OnError",
		Command: "false",
		OnError: HookErrorAbort,
	}, skill, "")

	if !result.Aborted {
		t.Error("abort policy should set Aborted=true")
	}
}

func TestHookRunner_Run_Timeout(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	hr := NewHookRunner()
	hr.timeout = 200 * time.Millisecond
	result := hr.Run(context.Background(), Hook{
		Event:   "PreRun",
		Command: "sleep 10",
		OnError: HookErrorContinue,
	}, skill, "")

	if result.Error == nil {
		t.Fatal("expected timeout error")
	}
	if result.Duration >= 5*time.Second {
		t.Errorf("should timeout quickly, took %v", result.Duration)
	}
}

func TestHookRunner_RunEvent_FiltersByEvent(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	skill.Meta.Hooks = []Hook{
		{Event: "PreRun", Command: "echo pre"},
		{Event: "PostRun", Command: "echo post"},
		{Event: "PreRun", Command: "echo pre2"},
	}

	hr := NewHookRunner()
	result := hr.RunEvent(context.Background(), HookEventPreRun, skill, "")

	// RunEvent returns the last non-aborting hook result.
	// "echo pre" runs first but its output is overwritten by "echo pre2".
	lines := stringsToSet(result.Output)
	if !lines["pre2"] {
		t.Errorf("last PreRun hook should execute, got: %v", lines)
	}
	if lines["post"] {
		t.Error("PostRun hook should NOT execute")
	}
}

func TestHookRunner_RunEvent_AbortShortCircuit(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	skill.Meta.Hooks = []Hook{
		{Event: "PreRun", Command: "false", OnError: HookErrorAbort},
		{Event: "PreRun", Command: "echo should-not-run"},
	}

	hr := NewHookRunner()
	result := hr.RunEvent(context.Background(), HookEventPreRun, skill, "")

	if !result.Aborted {
		t.Error("should abort on first failing hook")
	}
	// Second hook should not have run (output shouldn't contain "should-not-run")
	if result.Output != "" {
		t.Errorf("output should be empty (first hook failed), got: %q", result.Output)
	}
}

func TestHookRunner_SkillEnvVars(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, dir, "test", "Test skill")
	skill, _ := ParseSkill(dir)

	hr := NewHookRunner()
	result := hr.Run(context.Background(), Hook{
		Event:   "PreRun",
		Command: "echo name=$SKILL_NAME dir=$SKILL_DIR event=$SKILL_HOOK_EVENT",
	}, skill, "my-session")

	if !strings.Contains(result.Output, "name=test") {
		t.Errorf("SKILL_NAME not set, got: %q", result.Output)
	}
	if !strings.Contains(result.Output, "dir="+skill.Dir) {
		t.Errorf("SKILL_DIR not set, got: %q", result.Output)
	}
	if !strings.Contains(result.Output, "event=PreRun") {
		t.Errorf("SKILL_HOOK_EVENT not set, got: %q", result.Output)
	}
}

// ---------------------------------------------------------------------------
// Feature 6: Subagent
// ---------------------------------------------------------------------------

func TestSubagentRunner_NilRunner(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "fork-skill"),
		"---\nname: fork-skill\ndescription: Fork skill\ncontext: fork\nagent: Explore\n---\n\nBody.\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	_, err := exec.Execute(context.Background(), "fork-skill", "args")
	if err == nil {
		t.Fatal("expected error when fork skill has no subagent runner")
	}
	if !strings.Contains(err.Error(), "no subagent runner") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestExecuteFork_NotForkSkill(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "inline-skill"), "inline-skill", "Inline skill")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	_, err := exec.ExecuteFork(context.Background(), "inline-skill", "args")
	if err == nil {
		t.Fatal("expected error when non-fork skill calls ExecuteFork")
	}
	if !strings.Contains(err.Error(), "not configured for fork") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestSubagentRunner_WithSubagentRunner(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "fork-skill"),
		"---\nname: fork-skill\ndescription: Fork skill\ncontext: fork\nagent: Explore\n---\n\nBody.\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	// Should not error — runner is set (even if it's a mock stub)
	mockRunner := &mockSubagentRunner{result: &SubagentResult{Output: "done"}}
	exec := NewExecutor(reg).WithSubagentRunner(mockRunner)

	_, err := exec.Execute(context.Background(), "fork-skill", "args")
	if err != nil {
		t.Fatalf("unexpected error with subagent runner: %v", err)
	}
}

// createSkillDirFull creates a skill with custom frontmatter content.
func createSkillDirFull(t *testing.T, dir, content string) {
	t.Helper()
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// mockSubagentRunner is a test stub for SubagentRunner.
type mockSubagentRunner struct {
	result *SubagentResult
}

func (m *mockSubagentRunner) RunFork(ctx context.Context, cfg *AgentConfig, prompt string) (*SubagentResult, error) {
	return m.result, nil
}

// Helper
func stringsToSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set
}
