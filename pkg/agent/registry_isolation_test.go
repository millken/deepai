package agent

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// P0-1 regression: New() must never mutate the caller's registry. The REPL
// hands the same process-wide registry to a fresh agent every turn; if New()
// registers plan tools into it, turn 1's closure (bound to a dead agent)
// shadows every later turn, and subagents built from the parent's List()
// inherit enter_plan_mode.
func TestNew_DoesNotMutateSharedRegistry(t *testing.T) {
	shared := newTestRegistry()
	before := len(shared.List())

	_ = New(AgentConfig{LLMProvider: &modelCaptureProvider{}, Tools: shared, Model: "m", WorkDir: t.TempDir()})

	if shared.Get("enter_plan_mode") != nil {
		t.Fatal("New() registered enter_plan_mode into the caller's shared registry")
	}
	if got := len(shared.List()); got != before {
		t.Fatalf("shared registry tool count changed: %d -> %d", before, got)
	}
}

// P0-1 regression: with a shared registry, each agent's enter_plan_mode must
// operate on that agent — not on whichever agent registered first.
func TestPlanTools_BoundToOwnAgentAcrossSharedRegistry(t *testing.T) {
	shared := newTestRegistry()
	dir := t.TempDir()

	a1 := New(AgentConfig{LLMProvider: &modelCaptureProvider{}, Tools: shared, Model: "m", WorkDir: dir})
	a2 := New(AgentConfig{LLMProvider: &modelCaptureProvider{}, Tools: shared, Model: "m", WorkDir: dir})

	tool := a2.tools.Get("enter_plan_mode")
	if tool == nil {
		t.Fatal("second agent from shared registry has no enter_plan_mode")
	}
	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID: "c1", Name: "enter_plan_mode",
		Arguments: map[string]any{"reason": "test"},
	}); err != nil {
		t.Fatalf("enter_plan_mode handler: %v", err)
	}

	if !a2.IsPlanMode() {
		t.Error("agent 2's enter_plan_mode did not put agent 2 into plan mode")
	}
	if a1.IsPlanMode() {
		t.Error("agent 2's enter_plan_mode leaked into agent 1")
	}
}
