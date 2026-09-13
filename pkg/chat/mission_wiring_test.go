package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

// R24: /clear during a mission must not leave a mission that is "active" on
// disk with its charter gone and no history to resume into.
func TestClearSession_AbortsAnActiveMission(t *testing.T) {
	dir := t.TempDir()
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	ui := &mockUI{}
	r := &ChatRepl{cfg: ReplConfig{WorkDir: dir}, ui: ui, carry: agent.NewSessionCarry(),
		sess: sess, sessMgr: store}
	r.setLockedSession(sess.ID)
	m, _ := createMission(dir, "b")
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.carry.SetMissionCharter("charter")

	r.clearSession()

	if r.mission != nil {
		t.Error("/clear must detach the mission")
	}
	if got := openOrFatal(t, dir, m.state.ID).state.Status; got != missionStatusAborted {
		t.Errorf("status = %q, want aborted", got)
	}
	if _, ok := r.sess.Metadata[sessionMissionKey]; ok {
		t.Error("/clear must clear metadata.mission_id")
	}
	if r.carry.MissionCharter() != "" {
		t.Error("the fresh carry must not carry a charter")
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "aborted by /clear") {
		t.Errorf("the user must be told the mission ended: %v", ui.infoMsgs)
	}
}

// §5.1: mission_on_plan is OFF by default, and even on, it only fires for a
// turn that actually entered plan mode.
func TestMaybeUpgradeToMission(t *testing.T) {
	newRepl := func(t *testing.T, on, planMode bool) (*ChatRepl, *mockUI) {
		r, ui := newMissionRepl(t, t.TempDir())
		r.cfg.MissionOnPlan = on
		r.planMode = planMode
		// runMission with no charter and no turn runner would loop; the
		// design phase is exercised elsewhere, so stub the turn out.
		r.missionTurn = func(context.Context, string) *turnError { return &turnError{cancelled: true} }
		return r, ui
	}

	r, _ := newRepl(t, false, true)
	r.maybeUpgradeToMission(context.Background(), "build the thing")
	if r.mission != nil {
		t.Fatal("mission_on_plan defaults off — a plan must not silently start a mission")
	}

	r, _ = newRepl(t, true, false)
	r.maybeUpgradeToMission(context.Background(), "build the thing")
	if r.mission != nil {
		t.Fatal("a turn that never entered plan mode must not be upgraded")
	}

	r, ui := newRepl(t, true, true)
	planFile := filepath.Join(t.TempDir(), "2026-09-13-101500.md")
	writeFileOrFatal(t, planFile, "# the plan the model already wrote")
	r.lastPlanFile = planFile
	r.maybeUpgradeToMission(context.Background(), "build the thing")

	if r.mission == nil {
		t.Fatal("mission_on_plan with a plan-mode turn must start a mission")
	}
	if r.mission.brief != "build the thing" {
		t.Errorf("brief = %q, want the user's own request", r.mission.brief)
	}
	// R9: the plan is COPIED to the mission's own pinned path, not adopted
	// where it sits — a revision round would never find the old path.
	if got := r.mission.readDesign(); !strings.Contains(got, "already wrote") {
		t.Errorf("design.md = %q, want the turn's plan copied in", got)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "mission_on_plan") {
		t.Error("the upgrade must be announced — the user did not ask for a mission")
	}
}

// A synthesized mission message is not the user's request. /review must not
// anchor its reviewer on one.
func TestLastUserRequest_SkipsSynthesizedMissionMessages(t *testing.T) {
	r := &ChatRepl{sess: &models.Session{Messages: []models.Message{
		{Role: models.RoleHuman, Content: "the real request"},
		{Role: models.RoleAI, Content: "ok"},
		{Role: models.RoleHuman, Content: "[mission-implement] The charter is locked. Implement it."},
		{Role: models.RoleHuman, Content: "[mission-scope round 1/2] revert these"},
	}}}
	if got := r.lastUserRequest(); got != "the real request" {
		t.Fatalf("lastUserRequest = %q", got)
	}
}

// §十一 conservation: after a mission ends, the next ordinary turn must look
// exactly like one from a session that never ran a mission.
func TestAfterMission_OrdinaryTurnsAreUnaffected(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, m := newImplementRepl(t, fake, []string{"a.go"})
	r.carry.SetMissionCharter(renderCharter(m.charter))
	r.planMode = true
	r.disableEnterPlan = true

	r.leaveMission(missionStatusDone)

	if r.carry.MissionCharter() != "" {
		t.Error("the charter must leave the turn injection")
	}
	if r.planMode || r.disableEnterPlan || r.planFile != "" || r.deferPlanApproval {
		t.Errorf("mission turn overrides survived: planMode=%v disableEnterPlan=%v planFile=%q defer=%v",
			r.planMode, r.disableEnterPlan, r.planFile, r.deferPlanApproval)
	}
	// review_after_edit is off on this repl, so the ordinary gate is silent.
	r.carry.RecordEditedFile(filepath.Join(r.cfg.WorkDir, "a.go"))
	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != (gateResult{}) {
		t.Errorf("ordinary gate = %+v, want the plain review_after_edit guard", got)
	}
}

// A mission that ended still answers /mission status, but must not resume.
func TestFinishedMission_DoesNotResume(t *testing.T) {
	dir := t.TempDir()
	r, ui := newMissionRepl(t, dir)
	m, _ := createMission(dir, "b")
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.leaveMission(missionStatusDone)

	r.handleMissionCommand(context.Background(), "")
	if r.mission != nil {
		t.Fatal("a finished mission must not resume on a bare /mission")
	}
	if !strings.Contains(ui.lastInfo(), "no active mission") {
		t.Fatalf("got %q", ui.lastInfo())
	}
	if !strings.Contains(r.missionStatusText(), "done") {
		t.Error("/mission status must still report the finished mission")
	}
}

func TestMissionDirLayout(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "b")
	writeFileOrFatal(t, m.designPath(), "plan")
	_ = m.lockCharter(&Charter{ScopeFiles: []string{"a.go"}, Acceptance: []string{"Given a, when b, then c"}})
	m.appendReview(missionReviewRecord{Phase: "design", Verdict: "pass"})
	m.saveBaseline(worktreeSnapshot{root: dir})

	for _, name := range []string{
		missionBriefFile, missionDesignFile, missionLockedPlanFile,
		missionCharterFile, missionStateFile, missionReviewsFile, missionBaselineFile,
	} {
		if _, err := os.Stat(filepath.Join(m.dir, name)); err != nil {
			t.Errorf("%s missing from the mission directory: %v", name, err)
		}
	}
}

func mkdirOrFatal(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// End-to-end (§十一-2): one mission from the /mission command through the
// design gate, the charter lock, the implementation and its review, with the
// synthesized messages landing in order.
func TestRunMission_DesignThroughImplementToDone(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	fake := &fakeTaskTool{}
	r, ui := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)

	var inputs []string
	r.missionTurn = func(_ context.Context, input string) *turnError {
		inputs = append(inputs, input)
		switch {
		case strings.HasPrefix(input, "[mission-design"):
			fake.content = designPassJSON()
			writeFileOrFatal(t, r.mission.designPath(), "# plan\n- edit pkg/chat/mission.go")
		case strings.HasPrefix(input, "[mission-implement"):
			fake.content = passVerdictJSON()
			mkdirOrFatal(t, filepath.Join(dir, "pkg", "chat"))
			writeFileOrFatal(t, filepath.Join(dir, "pkg", "chat", "mission.go"), "package chat // done")
		}
		return nil
	}

	r.handleMissionCommand(context.Background(), "make the loop bounded")

	if r.mission != nil {
		t.Fatal("a completed mission must detach")
	}
	id := r.sess.Metadata[sessionMissionKey]
	got := openOrFatal(t, dir, id)
	if got.state.Status != missionStatusDone || !got.state.Reviewed {
		t.Fatalf("state = %+v, want done and reviewed", got.state)
	}
	if len(inputs) != 2 ||
		!strings.HasPrefix(inputs[0], "[mission-design round 1/3]") ||
		!strings.HasPrefix(inputs[1], "[mission-implement]") {
		t.Fatalf("turn inputs = %#v", inputs)
	}
	if got.charter == nil || got.charter.Brief != "make the loop bounded" {
		t.Fatalf("charter = %+v", got.charter)
	}
	joined := strings.Join(ui.infoMsgs, "\n")
	for _, want := range []string{"design phase", "charter locked", "implement phase", "done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("UI never reported %q:\n%s", want, joined)
		}
	}
}

// The escalation edge, end to end: implementation says the plan is wrong,
// design runs again on its own round budget, and the second charter drives a
// second implementation phase.
func TestRunMission_EscalationReDesignsAndFinishes(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	fake := &fakeTaskTool{}
	r, _ := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)

	implementTurns := 0
	var inputs []string
	r.missionTurn = func(_ context.Context, input string) *turnError {
		inputs = append(inputs, input)
		switch {
		case strings.HasPrefix(input, "[mission-design"), strings.HasPrefix(input, "[mission-escalate"):
			fake.content = designPassJSON()
			writeFileOrFatal(t, r.mission.designPath(), "# plan\n- edit pkg/chat/mission.go")
		default:
			implementTurns++
			mkdirOrFatal(t, filepath.Join(dir, "pkg", "chat"))
			writeFileOrFatal(t, filepath.Join(dir, "pkg", "chat", "mission.go"), "package chat // v"+string(rune('0'+implementTurns)))
			if implementTurns == 1 {
				fake.content = `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
					"issues":[{"severity":"high","file":"pkg/chat/mission.go","line":1,
					"message":"the planned interface cannot express the third outcome",
					"scenario":"the loop has nowhere to return an escalation","fault_layer":"design"}]}`
			} else {
				fake.content = passVerdictJSON()
			}
		}
		return nil
	}

	r.handleMissionCommand(context.Background(), "make the loop bounded")

	id := r.sess.Metadata[sessionMissionKey]
	got := openOrFatal(t, dir, id)
	if got.state.Status != missionStatusDone {
		t.Fatalf("status = %q, want done after the re-design", got.state.Status)
	}
	if got.state.Escalation != 1 {
		t.Fatalf("escalation = %d, want exactly 1", got.state.Escalation)
	}
	if _, err := os.Stat(got.path("charter.v1.json")); err != nil {
		t.Errorf("the superseded charter must be archived: %v", err)
	}
	sawEscalate := false
	for _, in := range inputs {
		if strings.HasPrefix(in, "[mission-escalate") {
			sawEscalate = true
		}
	}
	if !sawEscalate {
		t.Fatalf("no escalation message was ever sent: %#v", inputs)
	}
	if got.state.EscalatedDesignRound != 1 {
		t.Errorf("escalated_design_round = %d, want 1 (its own budget)", got.state.EscalatedDesignRound)
	}
}
