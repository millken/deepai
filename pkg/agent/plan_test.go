package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// helper: create a registry with standard tools for plan mode tests.
func newTestRegistry() *tools.Registry {
	r := tools.NewRegistry()
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "find",
		"ask_clarification", "present_file", "bash", "write_file", "edit_file"} {
		_ = r.Register(models.Tool{
			Name: name,
			Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
				return models.ToolResult{}, nil
			},
		})
	}
	return r
}

func TestPlanMode_EnterRestrictsTools(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{Tools: registry})

	if a.tools.Get("bash") == nil {
		t.Fatal("bash should be available before plan mode")
	}
	if a.tools.Get("enter_plan_mode") == nil {
		t.Fatal("enter_plan_mode should be registered by default")
	}

	a.enterPlanMode()

	if !a.planMode.Load() {
		t.Fatal("planMode should be true")
	}
	// Write tools should be gone.
	if a.tools.Get("bash") != nil {
		t.Fatal("bash should not be available in plan mode")
	}
	if a.tools.Get("write_file") != nil {
		t.Fatal("write_file should not be available in plan mode")
	}
	// Read tools should remain.
	if a.tools.Get("read_file") == nil {
		t.Fatal("read_file should be available in plan mode")
	}
	// Plan-mode-only tools must be available.
	if a.tools.Get("exit_plan_mode") == nil {
		t.Fatal("exit_plan_mode must be available in plan mode")
	}
	if a.tools.Get("write_plan") == nil {
		t.Fatal("write_plan must be available in plan mode")
	}
}

func TestPlanMode_ExitRestoresTools(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{Tools: registry})

	a.enterPlanMode()
	if a.tools.Get("bash") != nil {
		t.Fatal("bash should not be available in plan mode")
	}

	a.exitPlanMode()

	if a.planMode.Load() {
		t.Fatal("planMode should be false after exit")
	}
	if a.tools.Get("bash") == nil {
		t.Fatal("bash should be restored after exiting plan mode")
	}
	// Plan-mode-only tools should NOT be in full tool set.
	if a.tools.Get("exit_plan_mode") != nil {
		t.Fatal("exit_plan_mode should not be in full tool set after exit")
	}
	if a.tools.Get("write_plan") != nil {
		t.Fatal("write_plan should not be in full tool set after exit")
	}
}

func TestPlanMode_DoubleEnterIsNoop(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{Tools: registry})

	a.enterPlanMode()
	fullToolsBefore := a.fullTools

	a.enterPlanMode()

	if a.fullTools != fullToolsBefore {
		t.Fatal("double enterPlanMode should not overwrite fullTools")
	}
}

func TestPlanMode_ExitWithoutEnterIsNoop(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{Tools: registry})

	a.exitPlanMode()

	if a.planMode.Load() {
		t.Fatal("planMode should remain false")
	}
	if a.tools.Get("bash") == nil {
		t.Fatal("tools should be unchanged after noop exit")
	}
}

func TestPlanMode_ConfigPlanMode(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{
		Tools:    registry,
		PlanMode: true,
	})

	if !a.planMode.Load() {
		t.Fatal("agent should start in plan mode when PlanMode=true")
	}
	if a.tools.Get("bash") != nil {
		t.Fatal("bash should not be available when PlanMode=true")
	}
	if a.tools.Get("exit_plan_mode") == nil {
		t.Fatal("exit_plan_mode must be available when PlanMode=true")
	}
}

func TestPlanMode_PlanFileCreated(t *testing.T) {
	tmpDir := t.TempDir()
	registry := newTestRegistry()
	a := New(AgentConfig{
		Tools:    registry,
		PlanMode: true,
		WorkDir:  tmpDir,
	})

	if a.planFile == "" {
		t.Fatal("planFile should be set after entering plan mode")
	}
	if !strings.HasPrefix(a.planFile, filepath.Join(tmpDir, ".deepai", "plans")) {
		t.Fatalf("planFile should be under .deepai/plans/, got %s", a.planFile)
	}
	// .deepai/plans/ directory should exist.
	if _, err := os.Stat(filepath.Dir(a.planFile)); os.IsNotExist(err) {
		t.Fatal(".deepai/plans/ directory should be created")
	}
}

func TestPlanMode_SystemPromptContainsInstructions(t *testing.T) {
	tmpDir := t.TempDir()
	registry := newTestRegistry()
	a := New(AgentConfig{
		Tools:    registry,
		PlanMode: true,
		WorkDir:  tmpDir,
	})

	prompt := a.BuildSystemPrompt()
	if !strings.Contains(prompt, "plan mode") {
		t.Fatal("system prompt should contain plan mode instructions")
	}
	if !strings.Contains(prompt, a.planFile) {
		t.Fatal("system prompt should contain the plan file path")
	}
}

func TestPlanMode_SystemPromptNoInstructionsWhenNotInPlanMode(t *testing.T) {
	registry := newTestRegistry()
	a := New(AgentConfig{Tools: registry})

	prompt := a.BuildSystemPrompt()
	if strings.Contains(prompt, "plan mode") {
		t.Fatal("system prompt should NOT contain plan mode instructions when not in plan mode")
	}
}
