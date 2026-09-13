package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
)

// sortedCopy makes two path lists comparable regardless of the order the
// producer happened to emit them in.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Scope arithmetic
// ---------------------------------------------------------------------------

func TestClassifyAgainstCharter_ScopeCompanionsExemptionsAndViolations(t *testing.T) {
	dir := t.TempDir()
	c := &Charter{ScopeFiles: []string{"pkg/chat/mission.go", "pkg/chat/review.go"}}
	abs := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	changed := []string{
		abs("pkg/chat/mission.go"),              // in scope
		abs("pkg/chat/mission_test.go"),         // test companion of an in-scope file
		abs("pkg/chat/helper_test.go"),          // *_test.go in a directory holding in-scope code
		abs("pkg/chat/testdata/fixture.golden"), // fixture beside in-scope code
		abs(".deepai/missions/m1/state.json"),   // the mission's own bookkeeping
		abs("go.sum"),                           // generated
		abs("vendor/thing.lock"),                // generated
		abs("pkg/other/helper.go"),              // OUT of scope
		abs("pkg/chat/sneaky.go"),               // new non-test file beside in-scope code: still out
	}
	scope, violations := classifyAgainstCharter(dir, c, changed)

	wantScope := []string{"pkg/chat/mission.go", "pkg/chat/mission_test.go", "pkg/chat/helper_test.go", "pkg/chat/testdata/fixture.golden"}
	var gotScope []string
	for _, abs := range scope {
		rel, _ := workdirRel(dir, abs)
		gotScope = append(gotScope, rel)
	}
	if strings.Join(sortedCopy(gotScope), ",") != strings.Join(sortedCopy(wantScope), ",") {
		t.Errorf("review scope = %v, want %v", gotScope, wantScope)
	}
	// R38: exempt generated files must NOT be handed to the reviewer.
	for _, f := range gotScope {
		if strings.HasPrefix(f, ".deepai/") || f == "go.sum" || strings.HasSuffix(f, ".lock") {
			t.Errorf("%q is exempt from the violation test but must not be reviewed", f)
		}
	}
	wantViolations := []string{"pkg/other/helper.go", "pkg/chat/sneaky.go"}
	if strings.Join(sortedCopy(violations), ",") != strings.Join(sortedCopy(wantViolations), ",") {
		t.Errorf("violations = %v, want %v", violations, wantViolations)
	}
}

func TestClassifyAgainstCharter_TestCompanionNeedsAnInScopeSibling(t *testing.T) {
	dir := t.TempDir()
	c := &Charter{ScopeFiles: []string{"pkg/chat/mission.go"}}
	_, violations := classifyAgainstCharter(dir, c,
		[]string{filepath.Join(dir, "pkg", "elsewhere", "thing_test.go")})
	if len(violations) != 1 {
		t.Fatalf("a test file in a directory with no in-scope code is still a violation; got %v", violations)
	}
}

// ---------------------------------------------------------------------------
// The mission gate
// ---------------------------------------------------------------------------

// newImplementRepl builds a repl in a REAL git repo, in the implementation
// phase with a locked charter, and returns the mission.
func newImplementRepl(t *testing.T, fake *fakeTaskTool, scopeFiles []string) (*ChatRepl, *mockUI, *mission) {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	return newImplementReplIn(t, dir, fake, scopeFiles)
}

// newImplementReplIn is the same without assuming a git worktree, so the
// non-git degradation path can be exercised on a plain directory.
func newImplementReplIn(t *testing.T, dir string, fake *fakeTaskTool, scopeFiles []string) (*ChatRepl, *mockUI, *mission) {
	t.Helper()
	r, ui := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)
	m, err := createMission(dir, "the brief")
	if err != nil {
		t.Fatalf("createMission: %v", err)
	}
	writeFileOrFatal(t, m.designPath(), "# the plan")
	if err := m.lockCharter(&Charter{ScopeFiles: scopeFiles,
		Acceptance: []string{"Given the gate, when a turn edits out of scope, then it is reverted"}}); err != nil {
		t.Fatalf("lockCharter: %v", err)
	}
	m.state.Phase = missionPhaseImplement
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	r.mission = m
	m.saveBaseline(takeWorktreeSnapshot(dir))
	return r, ui, m
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v (%s)", err, out)
		}
	}
}

func TestMissionGate_OutOfScopeFileTakesAScopeRoundNotAReviewRound(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui, m := newImplementRepl(t, fake, []string{"in_scope.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "out_of_scope.go"), "package x")

	got := r.missionReviewGate(context.Background(), 0)

	if got.passed || got.escalate != "" {
		t.Fatalf("got %+v, want a scope-fix round", got)
	}
	if !strings.Contains(got.next, "[mission-scope round 1/2]") || !strings.Contains(got.next, "out_of_scope.go") {
		t.Fatalf("next = %q", got.next)
	}
	if fake.calls != 0 {
		t.Error("a scope violation must not spend a reviewer run")
	}
	if m.state.ImplementRound != 0 || m.state.ScopeRound != 1 {
		t.Errorf("implement_round=%d scope_round=%d — scope rounds must not spend review rounds",
			m.state.ImplementRound, m.state.ScopeRound)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "does not spend a review round") {
		t.Error("the UI must say a scope round is not a review round")
	}
}

// R25: the authority is the CURRENT delta from the baseline. Tool records
// only ever grow, so a file the model reverted on the gate's orders must
// leave the violation set — otherwise the second check violates again,
// spends the last scope round and escalates a mission that complied.
func TestMissionGate_RevertedFileLeavesTheViolationSet(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, _ := newImplementRepl(t, fake, []string{"in_scope.go"})
	stray := filepath.Join(r.cfg.WorkDir, "out_of_scope.go")
	writeFileOrFatal(t, stray, "package x")
	// The tool record survives the revert — that is the whole point.
	r.carry.RecordEditedFile(stray)

	if got := r.missionReviewGate(context.Background(), 0); got.next == "" {
		t.Fatal("first pass must report the violation")
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "in_scope.go"), "package x // real work")

	got := r.missionReviewGate(context.Background(), 0)
	if strings.Contains(got.next, "out_of_scope.go") {
		t.Fatalf("a reverted file is still reported as a violation: %q", got.next)
	}
	if got.escalate != "" {
		t.Fatalf("S3 must not fire after the model complied; got %q", got.escalate)
	}
	if fake.calls != 1 {
		t.Fatalf("the in-scope work must now be reviewed; reviewer runs = %d", fake.calls)
	}
	if !got.passed {
		t.Error("a passing verdict on in-scope work must set passed")
	}
}

// R31: an empty change set is idle, never done.
func TestMissionGate_NothingToReviewIsIdleNotDone(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui, m := newImplementRepl(t, fake, []string{"in_scope.go"})

	got := r.missionReviewGate(context.Background(), 0)
	if got.passed {
		t.Fatal("a turn that changed nothing must never be reported as a passed review")
	}
	if !strings.Contains(got.next, "[mission-idle 1/2]") {
		t.Fatalf("next = %q", got.next)
	}
	if fake.calls != 0 {
		t.Error("nothing to review must not dispatch a reviewer")
	}
	if m.state.IdleRound != 1 || m.state.ImplementRound != 0 {
		t.Errorf("idle=%d implement=%d", m.state.IdleRound, m.state.ImplementRound)
	}

	// Idle rounds are bounded, and exhausting them hands over — not done.
	r.missionReviewGate(context.Background(), 0)
	last := r.missionReviewGate(context.Background(), 0)
	if last.next != "" || last.passed {
		t.Fatalf("after %d idle rounds the gate must stop without passing: %+v", maxIdleRounds, last)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "NOT done") {
		t.Error("the hand-back must say it is not completion")
	}
}

// S1: the reviewer attributing the defect to the plan escalates on the spot.
func TestMissionGate_FaultLayerDesignEscalatesImmediately(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"in_scope.go","line":1,"message":"the charter's interface cannot express this",
		"scenario":"as planned, the gate cannot return the third outcome","fault_layer":"design"}]}`}
	r, _, _ := newImplementRepl(t, fake, []string{"in_scope.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "in_scope.go"), "package x")

	got := r.missionReviewGate(context.Background(), 0)
	if got.escalate != escalateFaultLayer {
		t.Fatalf("got %+v, want an immediate fault_layer escalation", got)
	}
}

// S2: two consecutive failures on the same file, at the point the mission
// would otherwise be handed to a human.
func TestMissionGate_RepeatedSameFileEscalatesAtTheRoundCap(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()} // issue on a.go
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
	r.reviewPrev = &agent.ReviewResult{Verdict: "fail",
		Issues: []agent.Issue{{File: "a.go", Message: "same place last round"}}}

	got := r.missionReviewGate(context.Background(), maxReviewRounds)
	if got.escalate != escalateRepeatFile {
		t.Fatalf("got %+v, want a repeat-file escalation", got)
	}
}

func TestMissionGate_DifferentFilesAtTheCapHandsOver(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()} // issue on a.go
	r, ui, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
	r.reviewPrev = &agent.ReviewResult{Verdict: "fail",
		Issues: []agent.Issue{{File: "somewhere_else.go", Message: "unrelated"}}}

	got := r.missionReviewGate(context.Background(), maxReviewRounds)
	if got.escalate != "" || got.next != "" || got.passed {
		t.Fatalf("got %+v, want a plain hand-over", got)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "STILL FAILING") {
		t.Error("the unresolved issues must be presented to the user")
	}
}

// Escalations are bounded: once spent, the same signals hand over instead.
func TestMissionGate_NoEscalationsLeftHandsOver(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"in_scope.go","line":1,"message":"m","scenario":"s","fault_layer":"design"}]}`}
	r, _, m := newImplementRepl(t, fake, []string{"in_scope.go"})
	m.state.Escalation = maxDesignEscalations
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "in_scope.go"), "package x")

	got := r.missionReviewGate(context.Background(), maxReviewRounds)
	if got.escalate != "" {
		t.Fatalf("got %q, want no escalation left", got.escalate)
	}
	if got.passed {
		t.Fatal("a failing review is never a pass")
	}
}

// §5.4.4: the mission review runs even with review_after_edit off, and the
// ordinary episode still does not.
func TestMissionGate_IgnoresReviewAfterEditSwitch(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	r.cfg.ReviewAfterEdit = false
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")

	if got := r.missionReviewGate(context.Background(), 0); !got.passed {
		t.Fatalf("got %+v — a mission reviews regardless of the opt-in switch", got)
	}

	// With the mission gone, the same switch stops the gate again.
	r.leaveMission(missionStatusDone)
	r.carry.RecordEditedFile(filepath.Join(r.cfg.WorkDir, "a.go"))
	before := fake.calls
	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got.next != "" || got.passed {
		t.Fatalf("ordinary gate after the mission = %+v, want the review_after_edit guard", got)
	}
	if fake.calls != before {
		t.Error("the ordinary gate must not dispatch a reviewer when review_after_edit is off")
	}
}

// The ordinary gate must NEVER become the mission gate, even while a mission
// is active and in its implementation phase. runEpisode reads only the fix
// message, so an escalation, a pass, or an idle round computed on that path
// would be silently discarded — and the mission's own round counters would
// be spent by ordinary conversation.
func TestOrdinaryGate_NeverTakesTheMissionPath(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"m","scenario":"s","fault_layer":"design"}]}`}
	r, _, m := newImplementRepl(t, fake, []string{"a.go"})
	r.cfg.ReviewAfterEdit = false // the ordinary gate's own switch, off
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")

	got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0)

	if got != (gateResult{}) {
		t.Fatalf("ordinary gate during an active mission = %+v, want the plain guard", got)
	}
	if fake.calls != 0 {
		t.Error("the ordinary gate must not dispatch the mission's reviewer")
	}
	if m.state.IdleRound != 0 || m.state.ScopeRound != 0 || m.state.ImplementRound != 0 {
		t.Errorf("ordinary conversation spent mission rounds: %+v", m.state)
	}
}

// The correctness reviewer must be anchored on the charter, not on the last
// thing anyone said — and told that fault_layer=design is available.
func TestMissionGate_ReviewPromptCarriesTheCharter(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")

	r.missionReviewGate(context.Background(), 0)

	prompt, _ := fake.args["prompt"].(string)
	for _, want := range []string{"Locked charter", "the brief", "Given the gate", "# the plan", `fault_layer="design"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt missing %q:\n%s", want, prompt)
		}
	}
}

// ---------------------------------------------------------------------------
// The phase loop
// ---------------------------------------------------------------------------

func TestImplementPhase_PassEndsTheMissionAsDone(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui, m := newImplementRepl(t, fake, []string{"a.go"})
	var inputs []string
	r.missionTurn = func(_ context.Context, input string) *turnError {
		inputs = append(inputs, input)
		writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
		return nil
	}

	if r.runImplementPhase(context.Background()) {
		t.Fatal("a done mission does not continue in another phase")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID); got.state.Status != missionStatusDone || !got.state.Reviewed {
		t.Fatalf("state = %+v, want done+reviewed", got.state)
	}
	if len(inputs) != 1 || !strings.HasPrefix(inputs[0], "[mission-implement]") {
		t.Fatalf("first implement input = %v", inputs)
	}
	if !strings.Contains(inputs[0], "Given the gate") {
		t.Error("the implement message must carry the acceptance criteria")
	}
	if r.carry.MissionCharter() != "" {
		t.Error("the charter injection must stop when the mission ends")
	}
	_ = ui
}

// R21: no implementation turn may run in plan mode, and enter_plan_mode is
// not even registered for them.
func TestImplementPhase_ForcesWritableToolsEveryTurn(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	r.planMode = true // as if a design turn's readback had left it on
	var sawPlan []bool
	var sawDisable []bool
	r.missionTurn = func(_ context.Context, _ string) *turnError {
		sawPlan = append(sawPlan, r.planMode)
		sawDisable = append(sawDisable, r.disableEnterPlan)
		writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
		r.planMode = true // the agent re-enters plan mode mid-turn
		return nil
	}
	r.runImplementPhase(context.Background())
	if len(sawPlan) == 0 || sawPlan[0] || !sawDisable[0] {
		t.Fatalf("implement turn ran with planMode=%v disableEnterPlan=%v", sawPlan, sawDisable)
	}
}

func TestImplementPhase_EscalationReturnsToDesign(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"the charter's interface cannot express this",
		"scenario":"as planned the loop has nowhere to return","fault_layer":"design"}]}`}
	r, ui, m := newImplementRepl(t, fake, []string{"a.go"})
	r.carry.SetMissionCharter(renderCharter(m.charter))
	r.missionTurn = func(_ context.Context, _ string) *turnError {
		writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
		return nil
	}

	if !r.runImplementPhase(context.Background()) {
		t.Fatal("an escalation continues the mission in the design phase")
	}
	if m.state.Phase != missionPhaseDesign || m.state.Escalation != 1 {
		t.Fatalf("state = %+v", m.state)
	}
	// R28: the archived charter must stop binding the author immediately.
	if r.carry.MissionCharter() != "" {
		t.Error("the escalated charter must come off the injection")
	}
	if m.charter != nil {
		t.Error("the in-memory charter must be detached")
	}
	if _, err := os.Stat(m.path("charter.v1.json")); err != nil {
		t.Errorf("charter.v1.json missing: %v", err)
	}
	if !strings.HasPrefix(r.missionPendingInput, "[mission-escalate 1/1]") {
		t.Fatalf("pending design input = %q", r.missionPendingInput)
	}
	if !strings.Contains(r.missionPendingInput, "cannot express this") {
		t.Error("the escalation message must say what could not be built")
	}
	if r.missionEscalationNote == "" {
		t.Error("the design reviewer needs the escalation reason too")
	}
	// R4: the escalated design phase counts from zero.
	if m.state.EscalatedDesignRound != 0 || m.state.ImplementRound != 0 {
		t.Errorf("counters not reset: %+v", m.state)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "escalating to design") {
		t.Error("the escalation must be visible to the user")
	}
	// The baseline survives, so the edits made under the archived charter
	// stay attributed to the mission instead of vanishing into a new
	// baseline and being reported as "done" unreviewed later.
	if _, err := os.Stat(m.path(missionBaselineFile)); err != nil {
		t.Errorf("implement.baseline must survive an escalation: %v", err)
	}
}

func TestImplementPhase_InterruptedTurnLeavesTheMissionActive(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui, m := newImplementRepl(t, fake, []string{"a.go"})
	r.missionTurn = func(_ context.Context, _ string) *turnError { return &turnError{cancelled: true} }

	if r.runImplementPhase(context.Background()) {
		t.Fatal("an interrupted turn does not change phase")
	}
	if r.mission == nil {
		t.Fatal("Ctrl+C ends the turn, not the mission")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusActive {
		t.Fatalf("status = %q", got)
	}
	if fake.calls != 0 {
		t.Error("incomplete edits must not be reviewed")
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "UNREVIEWED") {
		t.Error("the user must be told the changes are unreviewed")
	}
}

// A purely conversational implementation phase must never end as done.
func TestImplementPhase_TalkingOnlyHandsOverNotDone(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, m := newImplementRepl(t, fake, []string{"a.go"})
	turns := 0
	r.missionTurn = func(_ context.Context, _ string) *turnError { turns++; return nil }

	r.runImplementPhase(context.Background())

	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusHandedOver {
		t.Fatalf("status = %q, want handed_over", got)
	}
	if turns != maxIdleRounds+1 {
		t.Fatalf("ran %d turns, want %d (the first plus the idle nudges)", turns, maxIdleRounds+1)
	}
	if fake.calls != 0 {
		t.Error("nothing was ever changed, so nothing should have been reviewed")
	}
}

// S2 must compare THIS round's failure with the PREVIOUS round's, never with
// itself: a first-round failure that also carries fault_layer=design (S1)
// would otherwise look like a repeat once the escalation budget is spent.
func TestMissionGate_RepeatFileNeedsTwoDistinctRounds(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()} // issue on a.go
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")
	r.reviewPrev = nil // no previous round

	got := r.missionReviewGate(context.Background(), maxReviewRounds)
	if got.escalate != "" {
		t.Fatalf("got %q — one failing round is not a repeat", got.escalate)
	}
}

// R30: outside a git worktree the HARD parts degrade — the scope check and
// its escalation — but the review itself must keep running off the tool
// records, exactly as the ordinary gate does. Deriving the review scope from
// an empty snapshot delta instead would silently review nothing at all.
func TestMissionGate_NonGitKeepsReviewingAndDropsTheScopeCheck(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	dir := t.TempDir() // deliberately NOT a git worktree
	r, ui, m := newImplementReplIn(t, dir, fake, []string{"in_scope.go"})
	if m.baseline.root != "" {
		t.Skip("temp dir is inside a git worktree; this case needs a non-repo directory")
	}
	// An edit the charter does NOT cover: with no snapshot there is no
	// trustworthy current-state view, so it must not be called a violation.
	stray := filepath.Join(dir, "out_of_scope.go")
	writeFileOrFatal(t, stray, "package x")
	r.carry.RecordEditedFile(stray)

	got := r.missionReviewGate(context.Background(), 0)

	if !got.passed {
		t.Fatalf("got %+v — the review must still run without git", got)
	}
	if fake.calls != 1 {
		t.Fatalf("reviewer runs = %d, want 1 (scope falls back to the tool records)", fake.calls)
	}
	if m.state.ScopeRound != 0 || got.escalate != "" {
		t.Errorf("the hard scope check and S3 must be OFF without a baseline: scope=%d escalate=%q",
			m.state.ScopeRound, got.escalate)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "not a git worktree") {
		t.Error("the degradation must be stated once")
	}
}

// R25 with the scope budget actually spent: after two revert rounds the
// mission is one violation away from S3. Complying must take it OFF that
// edge — the accumulated tool records, which still name the reverted file,
// must not be what the check reads.
func TestMissionGate_S3DoesNotFireAfterTheModelComplied(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, m := newImplementRepl(t, fake, []string{"in_scope.go"})
	stray := filepath.Join(r.cfg.WorkDir, "out_of_scope.go")
	writeFileOrFatal(t, stray, "package x")
	r.carry.RecordEditedFile(stray) // survives the revert, by design

	for i := 1; i <= maxScopeFixRounds; i++ {
		got := r.missionReviewGate(context.Background(), 0)
		if got.escalate != "" || got.next == "" {
			t.Fatalf("round %d: got %+v, want a scope-fix round", i, got)
		}
	}
	if m.state.ScopeRound != maxScopeFixRounds {
		t.Fatalf("scope_round = %d, want the budget fully spent", m.state.ScopeRound)
	}

	// The model complies: the file leaves git's porcelain output entirely.
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "in_scope.go"), "package x // the real work")

	got := r.missionReviewGate(context.Background(), 0)
	if got.escalate != "" {
		t.Fatalf("S3 fired after the model did exactly what it was told: %+v", got)
	}
	if !got.passed {
		t.Fatalf("got %+v, want the in-scope work reviewed and passed", got)
	}
}

// R31, the case the idle CAP test hides: reverting the out-of-scope files is
// compliance, not completion. With nothing in scope changed, the gate must
// report idle — never a pass, and never a mission that ends as done.
func TestMissionGate_RevertingEverythingIsNotDone(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, m := newImplementRepl(t, fake, []string{"in_scope.go"})
	stray := filepath.Join(r.cfg.WorkDir, "out_of_scope.go")
	writeFileOrFatal(t, stray, "package x")

	if got := r.missionReviewGate(context.Background(), 0); got.next == "" {
		t.Fatal("the violation must be reported first")
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}

	got := r.missionReviewGate(context.Background(), 0)
	if got.passed {
		t.Fatal("reverting the only change is not a reviewed pass")
	}
	if !strings.Contains(got.next, "[mission-idle") {
		t.Fatalf("next = %q, want an idle nudge", got.next)
	}
	if fake.calls != 0 {
		t.Error("there is nothing in scope to review, so no reviewer should run")
	}
	if m.state.Reviewed {
		t.Error("nothing was ever reviewed")
	}
}

// One talking turn must leave the mission running — the idle counter bounds
// the loop, it does not end it on the first quiet turn.
func TestMissionGate_OneIdleTurnLeavesTheMissionActive(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _, m := newImplementRepl(t, fake, []string{"in_scope.go"})

	got := r.missionReviewGate(context.Background(), 0)

	if got.next == "" || got.passed {
		t.Fatalf("got %+v, want an idle nudge that keeps the phase going", got)
	}
	if r.mission == nil {
		t.Fatal("one quiet turn must not end the mission")
	}
	if st := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state; st.Status != missionStatusActive || st.IdleRound != 1 {
		t.Fatalf("persisted state = %+v, want active with one idle round", st)
	}
}

// S1 with the escalation budget spent must hand the mission to a human, not
// spend another fix round: the reviewer has just said the code is not where
// the problem is.
func TestMissionGate_FaultLayerWithoutBudgetHandsOverInsteadOfFixing(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"the charter cannot express this",
		"scenario":"as planned there is nowhere to return","fault_layer":"design"}]}`}
	r, ui, m := newImplementRepl(t, fake, []string{"a.go"})
	m.state.Escalation = maxDesignEscalations
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")

	got := r.missionReviewGate(context.Background(), 0) // round 0: fix rounds still available

	if got.next != "" {
		t.Fatalf("next = %q — another fix round spends the budget on the wrong layer", got.next)
	}
	if got.escalate != "" || got.passed {
		t.Fatalf("got %+v, want a plain hand-over", got)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "blames the PLAN") {
		t.Error("the user must be told why the mission stopped")
	}
}

// M5: a reviewer that fails a change but returns an empty issues array — a
// perfectly valid shape under the Strict schema — must not be recorded as a
// reviewed pass. Only an explicit "pass" verdict ends a mission as done.
func TestMissionGate_FailWithNoIssuesIsNotAPass(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"correctness-reviewer","verdict":"fail","summary":"broken","issues":[]}`}
	r, _, m := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x")

	got := r.missionReviewGate(context.Background(), 0)

	if got.passed {
		t.Fatal("a fail verdict must never end the mission as done, however empty its issue list")
	}
	if m.state.Reviewed {
		t.Error("Reviewed must stay false")
	}
}
