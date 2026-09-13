package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/llm"
)

// A reviewer that runs on the same model as the implementer shares its blind
// spots: the same training, the same habits, the same wrong assumption about
// what an API guarantees. review_model points both gates at a different
// model, by alias, without touching which model the implementer uses.

func TestReviewModel_PassedToTheImplementationReviewer(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, initRepo(t), fake)
	r.cfg.ReviewModel = "reviewer-alias"
	seedEditedFile(t, r, "a.go", "package a")

	r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0)

	if got := fake.args["model"]; got != "reviewer-alias" {
		t.Fatalf("task model = %v, want the configured reviewer alias", got)
	}
}

func TestReviewModel_PassedToTheDesignReviewer(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, _, _ := newDesignRepl(t, fake, []string{"# plan"})
	r.cfg.ReviewModel = "reviewer-alias"
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	r.runDesignPhase(context.Background())

	if got := fake.args["model"]; got != "reviewer-alias" {
		t.Fatalf("task model = %v, want the configured reviewer alias", got)
	}
}

// Unset is the default and must stay invisible: no model argument at all, so
// the agent-type profile's own model: (and then the main model) still decide,
// exactly as before this knob existed.
func TestReviewModel_UnsetSendsNoModelArgument(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, initRepo(t), fake)
	seedEditedFile(t, r, "a.go", "package a")

	r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0)

	if _, ok := fake.args["model"]; ok {
		t.Fatalf("model argument present (%v) with review_model unset", fake.args["model"])
	}
}

// An alias that is not in the registry must be caught at startup and dropped.
// Left in place it would fail EVERY review — the subagent cannot resolve the
// alias, the task errors, and the gate fail-softs — which on the design side
// stops the mission outright. Falling back to the default model with a loud
// warning is the recoverable failure.
func TestReviewModel_UnknownAliasIsDroppedWithAWarning(t *testing.T) {
	ui := &mockUI{}
	r := &ChatRepl{
		cfg: ReplConfig{
			ModelRegistry: newMockModelRegistry(&mockLLMProvider{response: "x"}),
			ReviewModel:   "not-a-real-alias",
		},
		ui: ui,
	}

	r.validateReviewModel()

	if r.cfg.ReviewModel != "" {
		t.Fatalf("ReviewModel = %q, want it cleared so reviews still run", r.cfg.ReviewModel)
	}
	joined := strings.Join(ui.infoMsgs, "\n")
	if !strings.Contains(joined, "not-a-real-alias") || !strings.Contains(joined, "review_model") {
		t.Fatalf("the user must be told which alias was dropped: %q", joined)
	}
}

func TestReviewModel_KnownAliasSurvivesValidation(t *testing.T) {
	// NewSingleModelRegistry's alias is "default" ("test" is the provider).
	reg := llm.NewSingleModelRegistry("test", "test-model", "")
	reg.InjectProvider("test", "", "", &mockLLMProvider{response: "x"})
	ui := &mockUI{}
	r := &ChatRepl{cfg: ReplConfig{ModelRegistry: reg, ReviewModel: "default"}, ui: ui}

	r.validateReviewModel()

	if r.cfg.ReviewModel != "default" {
		t.Fatalf("ReviewModel = %q, want it kept", r.cfg.ReviewModel)
	}
	if len(ui.infoMsgs) != 0 {
		t.Errorf("a valid alias must not warn: %v", ui.infoMsgs)
	}
}

// /review status is where a user checks what the gate will actually do, so it
// has to say which model reviews — "same as the main model" is the answer
// that tells them they have a blind spot.
func TestReviewStatus_ReportsTheReviewerModel(t *testing.T) {
	fake := &fakeTaskTool{}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	r.currentModel = "main-alias"

	r.handleReviewCommand(context.Background(), "status")
	if !strings.Contains(ui.lastInfo(), "same model as the main agent") {
		t.Errorf("unset review_model should be reported as a shared-blind-spot state: %q", ui.lastInfo())
	}

	r.cfg.ReviewModel = "reviewer-alias"
	r.handleReviewCommand(context.Background(), "status")
	if !strings.Contains(ui.lastInfo(), "reviewer-alias") {
		t.Errorf("status = %q, want the reviewer model named", ui.lastInfo())
	}
}

// The mission's own status line reports it too: a mission spends two kinds of
// reviewer, and whether they are the implementer's own model is exactly the
// thing a reader of that output wants to know.
func TestMissionStatus_ReportsTheReviewerModel(t *testing.T) {
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	r.cfg.ReviewModel = "reviewer-alias"
	m, _ := createMission(dir, "b")
	r.mission = m

	if got := r.missionStatusText(); !strings.Contains(got, "reviewer-alias") {
		t.Fatalf("/mission status = %q, want the reviewer model named", got)
	}
}

// The knob must not leak into the implementer: fix rounds keep running on the
// user's model, only the reviewer moves.
func TestReviewModel_DoesNotChangeTheMainAgentsModel(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, initRepo(t), fake)
	r.cfg.ReviewModel = "reviewer-alias"
	r.currentModel = "main-alias"
	seedEditedFile(t, r, "a.go", "package a")

	r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0)

	if r.currentModel != "main-alias" {
		t.Fatalf("currentModel = %q — the reviewer's model must not become the session's", r.currentModel)
	}
}
