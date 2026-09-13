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

// R9: the REPL rebuilds the Agent every turn, so an agent that opens a fresh
// timestamped plan file on every construction leaves each design round
// writing to a different file. The mission's gate reads ONE file, so a
// preset PlanFile must be honored verbatim and initPlanFile skipped.
func TestPlanFile_PresetIsPinnedAcrossAgents(t *testing.T) {
	dir := t.TempDir()
	pinned := filepath.Join(dir, ".deepai", "missions", "m1", "design.md")
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true, PlanFile: pinned})
		if a.planFile != pinned {
			t.Fatalf("round %d: planFile = %q, want the pinned %q", i, a.planFile, pinned)
		}
	}
	// No timestamped plan file may have been created alongside it.
	if entries, err := os.ReadDir(filepath.Join(dir, ".deepai", "plans")); err == nil && len(entries) > 0 {
		t.Fatalf(".deepai/plans got %d file(s); a pinned plan file must not also open a timestamped one", len(entries))
	}
}

// The no-PlanFile path is every existing /plan user: unchanged.
func TestPlanFile_UnsetStillOpensATimestampedFile(t *testing.T) {
	dir := t.TempDir()
	a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true})
	if a.planFile == "" {
		t.Fatal("without a preset PlanFile, entering plan mode must still initialize one")
	}
	if !strings.HasPrefix(a.planFile, filepath.Join(dir, ".deepai", "plans")) {
		t.Fatalf("planFile = %q, want it under .deepai/plans", a.planFile)
	}
}

// R21: the implementation phase must not be able to re-enter plan mode
// mid-turn — the post-turn readback would carry plan mode into the next
// turn's config and the mission would be stuck with read-only tools.
func TestDisableEnterPlan_RemovesTheTool(t *testing.T) {
	a := New(AgentConfig{Tools: newTestRegistry(), DisableEnterPlan: true})
	if a.tools.Get("enter_plan_mode") != nil {
		t.Fatal("enter_plan_mode must not be registered when DisableEnterPlan is set")
	}
	if a.tools.Get("bash") == nil {
		t.Fatal("DisableEnterPlan must not restrict anything else")
	}
	// Default is unchanged.
	b := New(AgentConfig{Tools: newTestRegistry()})
	if b.tools.Get("enter_plan_mode") == nil {
		t.Fatal("enter_plan_mode must still be registered by default")
	}
}

// askRecorder records whether exit_plan_mode blocked on the user.
type askRecorder struct {
	asked  bool
	answer string
}

func (a *askRecorder) AskQuestion(_ context.Context, _ string, _ []string) (string, error) {
	a.asked = true
	return a.answer, nil
}

// C3/§5.2: inside a mission's design phase the human approval prompt is
// replaced by the design-review gate. exit_plan_mode must neither ask nor
// exit — the agent stays read-only until the GATE says the plan passed.
func TestExitPlanMode_DeferredToTheDesignGate(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "design.md")
	if err := os.WriteFile(planFile, []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true,
		PlanFile: planFile, DeferPlanApproval: true})

	rec := &askRecorder{answer: "Yes, proceed"}
	ctx := tools.WithUserInteraction(context.Background(), rec)
	res, err := a.tools.Get("exit_plan_mode").Handler(ctx, models.ToolCall{ID: "1", Name: "exit_plan_mode"})
	if err != nil {
		t.Fatalf("exit_plan_mode: %v", err)
	}
	if rec.asked {
		t.Error("a mission's design phase must not stop to ask the user for approval")
	}
	if !a.IsPlanMode() {
		t.Error("the agent must stay in plan mode — only the design gate ends the design phase")
	}
	if !strings.Contains(strings.ToLower(res.Content), "review") {
		t.Errorf("result should tell the model a review runs; got %q", res.Content)
	}
}

// Without the flag, /plan keeps its three-way prompt exactly as before.
func TestExitPlanMode_StillAsksOutsideAMission(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "design.md")
	if err := os.WriteFile(planFile, []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true, PlanFile: planFile})

	rec := &askRecorder{answer: "Yes, proceed"}
	ctx := tools.WithUserInteraction(context.Background(), rec)
	if _, err := a.tools.Get("exit_plan_mode").Handler(ctx, models.ToolCall{ID: "1", Name: "exit_plan_mode"}); err != nil {
		t.Fatalf("exit_plan_mode: %v", err)
	}
	if !rec.asked {
		t.Fatal("outside a mission exit_plan_mode must still ask the user")
	}
	if a.IsPlanMode() {
		t.Fatal("an approved plan must exit plan mode")
	}
}

// D7: the charter has to survive compaction, so it rides the trailing
// per-request injection rather than sitting in the message history.
func TestTurnInjection_CarriesTheMissionCharter(t *testing.T) {
	carry := NewSessionCarry()
	a := New(AgentConfig{Tools: newTestRegistry(), Session: carry})

	if got := a.buildTurnInjection(context.Background(), "s1", nil).Content; strings.Contains(got, "Mission charter") {
		t.Fatalf("no mission is active; injection must not mention a charter: %q", got)
	}

	carry.SetMissionCharter("# Mission charter (locked)\n\nstay inside scope")
	got := a.buildTurnInjection(context.Background(), "s1", nil).Content
	if !strings.Contains(got, "stay inside scope") {
		t.Fatalf("injection missing the charter: %q", got)
	}
	// Position: the charter must be in the TRAILING injection, never the
	// system prompt — a changing prefix costs the provider's prompt cache.
	if strings.Contains(a.BuildSystemPrompt(), "stay inside scope") {
		t.Fatal("the charter must not enter the system prompt")
	}

	carry.SetMissionCharter("")
	if got := a.buildTurnInjection(context.Background(), "s1", nil).Content; strings.Contains(got, "stay inside scope") {
		t.Fatal("clearing the carried charter must stop the injection (R36)")
	}
}

// The mission gate reads the plan FILE and nothing else, so an inline plan
// handed to exit_plan_mode has to land there — otherwise the model is told
// its plan was submitted while the reviewer looks at an empty file.
func TestExitPlanMode_DeferredWritesAnInlinePlanToTheFile(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "design.md")
	a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true,
		PlanFile: planFile, DeferPlanApproval: true})

	res, err := a.tools.Get("exit_plan_mode").Handler(context.Background(), models.ToolCall{
		ID: "1", Name: "exit_plan_mode",
		Arguments: map[string]any{"plan": "# inline plan\n- step one"},
	})
	if err != nil {
		t.Fatalf("exit_plan_mode: %v", err)
	}
	if res.Status != models.CallStatusCompleted {
		t.Fatalf("status = %v (%s)", res.Status, res.Error)
	}
	got, readErr := os.ReadFile(planFile)
	if readErr != nil || !strings.Contains(string(got), "step one") {
		t.Fatalf("plan file = %q, %v — the inline plan never reached the gate's file", got, readErr)
	}
}

// An existing plan file always wins: the fallback must not overwrite the
// plan write_plan already saved with a stale inline copy.
func TestExitPlanMode_DeferredKeepsTheWrittenPlan(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "design.md")
	if err := os.WriteFile(planFile, []byte("# the real plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{Tools: newTestRegistry(), WorkDir: dir, PlanMode: true,
		PlanFile: planFile, DeferPlanApproval: true})

	if _, err := a.tools.Get("exit_plan_mode").Handler(context.Background(), models.ToolCall{
		ID: "1", Name: "exit_plan_mode",
		Arguments: map[string]any{"plan": "# stale inline copy"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(planFile)
	if string(got) != "# the real plan" {
		t.Fatalf("plan file = %q, want the written plan untouched", got)
	}
}
