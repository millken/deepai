package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Execute: basic flows
// ---------------------------------------------------------------------------

func createSkillDirFull(t *testing.T, dir, content string) {
	t.Helper()
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_NotFound(t *testing.T) {
	reg := NewRegistry()
	exec := NewExecutor(reg)
	_, err := exec.Execute(context.Background(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for nonexistent skill")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestExecute_RenderSuccess(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "test-skill"),
		"---\nname: test-skill\ndescription: Test\n---\n\nHello $ARGUMENTS\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "test-skill", "world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cfg.SystemPrompt, "Hello world") {
		t.Errorf("prompt should contain 'Hello world', got: %s", cfg.SystemPrompt)
	}
}

func TestExecute_EnvVarsInBody(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "env-skill"),
		"---\nname: env-skill\ndescription: Env test\n---\n\nSession: ${SESSION_ID}\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	ctx := ContextWithSessionID(context.Background(), "sess-42")
	exec := NewExecutor(reg)
	cfg, err := exec.Execute(ctx, "env-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cfg.SystemPrompt, "sess-42") {
		t.Errorf("prompt should contain session ID, got: %s", cfg.SystemPrompt)
	}
}

func TestExecute_EnvVarsNoContext(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "env-skill"),
		"---\nname: env-skill\ndescription: Env test\n---\n\nSession: ${SESSION_ID}\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	// No session ID in context — should resolve to empty string
	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "env-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cfg.SystemPrompt, "Session: ") {
		t.Errorf("prompt should have Session: prefix, got: %s", cfg.SystemPrompt)
	}
	if strings.Contains(cfg.SystemPrompt, "unknown-session") {
		t.Error("should not contain stub 'unknown-session'")
	}
}

// ---------------------------------------------------------------------------
// buildConfig: frontmatter → AgentConfig mapping
// ---------------------------------------------------------------------------

func TestExecute_BuildConfig(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "cfg-skill"),
		"---\nname: cfg-skill\ndescription: Config test\nmodel: claude-3\n"+
			"effort: high\nallowed-tools:\n  - Read\n  - Grep\n"+
			"max-turns: 5\ntemperature: 0.7\n---\n\nBody.\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "cfg-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Model != "claude-3" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-3")
	}
	if cfg.Effort != "high" {
		t.Errorf("Effort = %q, want %q", cfg.Effort, "high")
	}
	if len(cfg.AllowedTools) != 2 || cfg.AllowedTools[0] != "Read" || cfg.AllowedTools[1] != "Grep" {
		t.Errorf("AllowedTools = %v, want [Read Grep]", cfg.AllowedTools)
	}
	if cfg.MaxTurns == nil || *cfg.MaxTurns != 5 {
		t.Errorf("MaxTurns = %v, want 5", cfg.MaxTurns)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", cfg.Temperature)
	}
}

// ---------------------------------------------------------------------------
// Dynamic injection in Execute
// ---------------------------------------------------------------------------

func TestExecute_DynamicInjection(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "dyn-skill"),
		"---\nname: dyn-skill\ndescription: Dynamic\n---\n\n"+
			"Files: !`echo a.txt b.txt`\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "dyn-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cfg.SystemPrompt, "a.txt") || !strings.Contains(cfg.SystemPrompt, "b.txt") {
		t.Errorf("prompt should contain command output, got: %s", cfg.SystemPrompt)
	}
	if strings.Contains(cfg.SystemPrompt, "!`") {
		t.Error("prompt should not contain raw !`command` syntax")
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestExecute_DisableModelInvocation(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "hidden-skill"),
		"---\nname: hidden-skill\ndescription: Hidden\ndisable-model-invocation: true\n---\n\nBody.\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "hidden-skill", "")
	if err != nil {
		t.Fatalf("Execute should work for disabled skills: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg should not be nil")
	}
}

func TestExecute_EmptyBody(t *testing.T) {
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "empty-skill"),
		"---\nname: empty-skill\ndescription: Empty\n---\n\n")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	exec := NewExecutor(reg)
	cfg, err := exec.Execute(context.Background(), "empty-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = cfg
}

// createSkillDirFull is defined in hooks_test.go; redeclare if needed.
// This file is in the same package so it shares helpers.

// Ensure createSkillDirFull exists (it's in hooks_test.go).
var _ = func() {
	// Compile-time check: createSkillDirFull must exist
	_ = createSkillDirFull
	_ = os.MkdirAll
	_ = filepath.Join
}
