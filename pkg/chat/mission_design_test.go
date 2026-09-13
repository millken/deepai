package chat

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
)

func designPassJSON() string {
	return `{"agent":"design-reviewer","verdict":"pass","summary":"answers the brief",
		"issues":[],"scope_files":["pkg/chat/mission.go","pkg/chat/mission_test.go"],
		"acceptance":["Given an active mission, when a turn edits an out-of-scope file, then the gate orders it reverted"]}`
}

func designFailJSON() string {
	return `{"agent":"design-reviewer","verdict":"fail","summary":"scope is wrong",
		"issues":[{"severity":"high","file":"plan","line":0,"area":"scope",
		"message":"the plan never names the review gate it has to change",
		"scenario":"implemented as written, the gate still returns a bare string and the escalation has nowhere to go"}],
		"scope_files":[],"acceptance":[]}`
}

// newDesignRepl wires a mission repl whose turns are faked: the fake writes
// whatever plan the test wants and records the inputs the loop synthesized.
func newDesignRepl(t *testing.T, fake *fakeTaskTool, plans []string) (*ChatRepl, *mockUI, *[]string) {
	t.Helper()
	dir := t.TempDir()
	r, ui := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)
	inputs := &[]string{}
	turn := 0
	r.missionTurn = func(_ context.Context, input string) *turnError {
		*inputs = append(*inputs, input)
		if turn < len(plans) && r.mission != nil {
			if plans[turn] == "" {
				_ = os.Remove(r.mission.designPath())
			} else {
				writeFileOrFatal(t, r.mission.designPath(), plans[turn])
			}
		}
		turn++
		return nil
	}
	return r, ui, inputs
}

func TestDesignPhase_PassLocksTheCharterAndMovesToImplement(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, ui, inputs := newDesignRepl(t, fake, []string{"# plan\n- do the thing"})
	m, _ := createMission(r.cfg.WorkDir, "make the gate structured")
	r.mission = m

	if !r.runDesignPhase(context.Background()) {
		t.Fatal("a passing design must continue into the implementation phase")
	}
	if m.state.Phase != missionPhaseImplement {
		t.Fatalf("phase = %q, want implement", m.state.Phase)
	}
	if m.charter == nil || len(m.charter.ScopeFiles) != 2 {
		t.Fatalf("charter = %+v", m.charter)
	}
	if m.charter.Brief != "make the gate structured" {
		t.Errorf("charter brief = %q, want the mission's own brief", m.charter.Brief)
	}
	if r.carry.MissionCharter() == "" {
		t.Error("the locked charter must be put on the turn injection")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID); got.charter == nil {
		t.Error("the charter must be persisted, not only held in memory")
	}
	// R20: the first design turn is the synthesized message, not the raw brief.
	if len(*inputs) != 1 || !strings.HasPrefix((*inputs)[0], "[mission-design round 1/3]") {
		t.Fatalf("first design input = %q", (*inputs)[0])
	}
	if !strings.Contains((*inputs)[0], "make the gate structured") {
		t.Error("the first design message must carry the brief verbatim")
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "charter locked") {
		t.Error("locking the charter must be reported")
	}
	// The reviewer never sees the session, only brief + plan.
	prompt, _ := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "make the gate structured") || !strings.Contains(prompt, "do the thing") {
		t.Errorf("design review prompt = %q", prompt)
	}
	if got := fake.args["agent_type"]; got != "design-reviewer" {
		t.Errorf("agent_type = %v, want design-reviewer", got)
	}
	if got := fake.args["max_tool_calls"]; got != reviewMaxToolCalls {
		t.Errorf("max_tool_calls = %v, want the gate's %d", got, reviewMaxToolCalls)
	}
}

func TestDesignPhase_FailFeedsIssuesBackAndRetries(t *testing.T) {
	fake := &fakeTaskTool{content: designFailJSON()}
	r, _, inputs := newDesignRepl(t, fake, []string{"# plan a", "# plan b", "# plan c"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	if r.runDesignPhase(context.Background()) {
		t.Fatal("three failing rounds must not reach the implementation phase")
	}
	if r.mission != nil {
		t.Fatal("the mission must have ended")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusDesignFailed {
		t.Fatalf("status = %q, want design_failed", got)
	}
	if len(*inputs) != maxDesignRounds {
		t.Fatalf("ran %d design turns, want %d", len(*inputs), maxDesignRounds)
	}
	if !strings.HasPrefix((*inputs)[1], "[mission-design-review round 2/3]") {
		t.Fatalf("second input = %q", (*inputs)[1])
	}
	if !strings.Contains((*inputs)[1], "the plan never names the review gate") {
		t.Error("the revision message must carry the reviewer's issues")
	}
	if !strings.Contains((*inputs)[1], "write_plan") {
		t.Error("the revision message must say the whole plan has to be rewritten (write_plan replaces the file)")
	}
	// Every gate decision is auditable.
	data, err := os.ReadFile(m.path(missionReviewsFile))
	if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != maxDesignRounds {
		t.Fatalf("reviews.jsonl = %q, %v", data, err)
	}
}

// C4: a "pass" that did not fill the charter is a FAIL. Locking an empty
// scope would make the implementation phase's hard check meaningless.
func TestDesignPhase_PassWithoutCharterFieldsIsAFail(t *testing.T) {
	fake := &fakeTaskTool{content: `{"agent":"design-reviewer","verdict":"pass","summary":"looks fine",
		"issues":[],"scope_files":[],"acceptance":[]}`}
	r, _, inputs := newDesignRepl(t, fake, []string{"# plan", "# plan2", "# plan3"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	if r.runDesignPhase(context.Background()) {
		t.Fatal("an empty charter must never pass the gate")
	}
	if !strings.Contains((*inputs)[1], "could not fill the charter") {
		t.Errorf("the author must be told WHY: %q", (*inputs)[1])
	}
}

func TestDesignOutcome(t *testing.T) {
	dir := t.TempDir()
	good := &agent.DesignReviewResult{Verdict: "pass",
		ScopeFiles: []string{"a.go"}, Acceptance: []string{"Given a, when b, then c"}}
	if _, ok := designOutcome(dir, good); !ok {
		t.Error("a filled pass must pass")
	}
	cases := map[string]*agent.DesignReviewResult{
		"nil":                nil,
		"fail verdict":       {Verdict: "fail", Issues: []agent.Issue{{Message: "m"}}, ScopeFiles: []string{"a.go"}, Acceptance: []string{"Given a, when b, then c"}},
		"no scope":           {Verdict: "pass", Acceptance: []string{"Given a, when b, then c"}},
		"no acceptance":      {Verdict: "pass", ScopeFiles: []string{"a.go"}},
		"hedged acceptance":  {Verdict: "pass", ScopeFiles: []string{"a.go"}, Acceptance: []string{"works"}},
		"scope outside tree": {Verdict: "pass", ScopeFiles: []string{"/etc/passwd"}, Acceptance: []string{"Given a, when b, then c"}},
	}
	for name, v := range cases {
		if _, ok := designOutcome(dir, v); ok {
			t.Errorf("%s: must not pass", name)
		}
	}
	// A failing verdict with an EMPTY issue list — a shape the Strict schema
	// permits — must not lock a charter. This is the design-gate half of the
	// same hole isMissionPassVerdict closes on the done path.
	if _, ok := designOutcome(dir, &agent.DesignReviewResult{Verdict: "fail", ScopeFiles: []string{"a.go"},
		Acceptance: []string{"Given a, when b, then c"}}); ok {
		t.Error("a fail verdict must not pass just because it listed no issues")
	}
	if _, ok := designOutcome(dir, &agent.DesignReviewResult{Verdict: "", ScopeFiles: []string{"a.go"},
		Acceptance: []string{"Given a, when b, then c"}}); ok {
		t.Error("an empty verdict is not a pass — both reviewer prompts promise the literal word")
	}
}

// §六-1: design-side fail-soft is the opposite of the implementation gate's.
// Nothing has been written yet, so an unavailable review must STOP the
// mission rather than let an unreviewed plan through to implementation.
func TestDesignPhase_ReviewFailSoftDoesNotImplement(t *testing.T) {
	fake := &fakeTaskTool{content: "not json at all"}
	r, ui, _ := newDesignRepl(t, fake, []string{"# plan"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	if r.runDesignPhase(context.Background()) {
		t.Fatal("an unreviewed plan must never reach implementation")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusHandedOver {
		t.Fatalf("status = %q, want handed_over", got)
	}
	joined := strings.Join(ui.infoMsgs, "\n")
	if !strings.Contains(joined, "unparseable") || !strings.Contains(joined, "NOT implementing") {
		t.Errorf("the user must be told the plan is unreviewed and nothing was built: %q", joined)
	}
}

func TestDesignPhase_EmptyPlanCostsARoundAndNeverDispatches(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, _, inputs := newDesignRepl(t, fake, []string{"", "# plan now"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	if !r.runDesignPhase(context.Background()) {
		t.Fatal("the second round wrote a plan that passes; the mission must continue")
	}
	if fake.calls != 1 {
		t.Fatalf("dispatched %d reviews, want 1 — an empty plan must not cost a reviewer run", fake.calls)
	}
	if !strings.Contains((*inputs)[1], "is still empty") {
		t.Errorf("second input = %q", (*inputs)[1])
	}
	if m.state.DesignRound != 2 {
		t.Fatalf("design_round = %d, want 2 — an empty round is still a round", m.state.DesignRound)
	}
}

// R9/R21: every design turn runs in plan mode against the SAME pinned plan
// file, whatever the agent did to plan mode in the previous turn.
func TestDesignPhase_ForcesPlanModeAndPinsOneFileEveryTurn(t *testing.T) {
	fake := &fakeTaskTool{content: designFailJSON()}
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)
	m, _ := createMission(dir, "brief")
	r.mission = m

	var sawPlanMode []bool
	var sawPlanFile []string
	r.missionTurn = func(_ context.Context, _ string) *turnError {
		sawPlanMode = append(sawPlanMode, r.planMode)
		sawPlanFile = append(sawPlanFile, r.planFile)
		writeFileOrFatal(t, m.designPath(), "# plan")
		// The agent "exits" plan mode mid-turn, as the readback would.
		r.planMode = false
		return nil
	}
	r.runDesignPhase(context.Background())

	if len(sawPlanMode) != maxDesignRounds {
		t.Fatalf("ran %d turns", len(sawPlanMode))
	}
	for i := range sawPlanMode {
		if !sawPlanMode[i] {
			t.Errorf("turn %d ran outside plan mode — the design phase must be read-only", i+1)
		}
		if sawPlanFile[i] != m.designPath() {
			t.Errorf("turn %d wrote to %q, want the pinned %q", i+1, sawPlanFile[i], m.designPath())
		}
	}
}

func TestDesignPhase_InterruptedTurnKeepsTheMissionActive(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, ui, _ := newDesignRepl(t, fake, nil)
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m
	r.missionTurn = func(_ context.Context, _ string) *turnError {
		return &turnError{cancelled: true}
	}

	if r.runDesignPhase(context.Background()) {
		t.Fatal("an interrupted turn must not advance the phase")
	}
	if r.mission == nil {
		t.Fatal("Ctrl+C ends the turn, not the mission")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusActive {
		t.Fatalf("status = %q, want active", got)
	}
	if fake.calls != 0 {
		t.Error("a half-written plan must not be reviewed")
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "no design review ran") {
		t.Error("the user must be told the plan was not reviewed")
	}
}

func TestBuildDesignReviewPrompt_CarriesAnchorsAndBudget(t *testing.T) {
	got := buildDesignReviewPrompt(designReviewInput{
		brief:    "the original ask",
		plan:     "# the plan",
		planPath: "/repo/.deepai/missions/m/design.md",
		prev: &agent.DesignReviewResult{Issues: []agent.Issue{{
			Severity: "high", File: "plan", Message: "old finding", Scenario: "old scenario"}}},
		escalation:   "the charter's interface cannot express escalation",
		maxToolCalls: reviewMaxToolCalls,
	})
	for _, want := range []string{
		"the original ask", "# the plan", "old finding",
		"cannot express escalation", "locked charter", "tool calls",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// R28, checked where it actually matters: the design turn that runs AFTER
// an escalation must not be told it is bound by the charter the gate just
// rejected. The carry is what the trailing turn injection is built from
// (pkg/agent's TestTurnInjection_CarriesTheMissionCharter closes that half),
// so this asserts its state DURING the turn, not after the phase.
func TestDesignPhase_AfterEscalationTheTurnSeesNoLockedCharter(t *testing.T) {
	fake := &fakeTaskTool{content: designFailJSON()}
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = fake.registry(t)
	m, _ := createMission(dir, "brief")
	writeFileOrFatal(t, m.designPath(), "# old plan")
	_ = m.lockCharter(&Charter{ScopeFiles: []string{"a.go"}, Acceptance: []string{"Given a, when b, then c"}})
	r.mission = m
	r.carry.SetMissionCharter(renderCharter(m.charter))

	// The implementation gate escalates.
	r.reviewPrev = &agent.ReviewResult{Verdict: "fail", Issues: []agent.Issue{
		{File: "a.go", Message: "the charter cannot express this", FaultLayer: "design"}}}
	r.escalateToDesign(escalateFaultLayer)

	var charterDuringTurn []string
	var inputs []string
	r.missionTurn = func(_ context.Context, input string) *turnError {
		inputs = append(inputs, input)
		charterDuringTurn = append(charterDuringTurn, r.carry.MissionCharter())
		writeFileOrFatal(t, m.designPath(), "# a new plan")
		return nil
	}

	r.runDesignPhase(context.Background())

	if len(charterDuringTurn) == 0 {
		t.Fatal("no design turn ran")
	}
	for i, got := range charterDuringTurn {
		if got != "" {
			t.Errorf("escalated design turn %d still carries a locked charter into every request: %q", i+1, got)
		}
	}
	if !strings.HasPrefix(inputs[0], "[mission-escalate") {
		t.Fatalf("first escalated input = %q", inputs[0])
	}
	// R4: the escalated phase runs on its own, smaller budget.
	if len(inputs) != maxEscalatedDesignRounds {
		t.Fatalf("ran %d escalated design rounds, want %d", len(inputs), maxEscalatedDesignRounds)
	}
}

// M2: Ctrl+C already costs the user the turn; it must not also cost the
// mission one of its three chances at the plan.
func TestDesignPhase_InterruptedTurnDoesNotSpendARound(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, _, _ := newDesignRepl(t, fake, nil)
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m
	r.missionTurn = func(context.Context, string) *turnError { return &turnError{cancelled: true} }

	r.runDesignPhase(context.Background())

	if m.state.DesignRound != 0 {
		t.Fatalf("design_round = %d after an interrupted turn, want 0", m.state.DesignRound)
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.DesignRound; got != 0 {
		t.Fatalf("persisted design_round = %d, want 0", got)
	}
}

// An interrupted design REVIEW says nothing about the plan: the mission must
// stay active so the user can simply resume, instead of being ended because
// they interrupted the thing reading a finished plan.
func TestDesignPhase_InterruptedReviewKeepsTheMissionActive(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, ui, _ := newDesignRepl(t, fake, []string{"# plan"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m
	// Interrupt lands on the reviewer dispatch, not on the design turn.
	ui.interruptDuringTask = true
	fake.waitForCancel = true

	r.runDesignPhase(context.Background())

	if r.mission == nil {
		t.Fatal("an interrupted review must not end the mission")
	}
	if got := openOrFatal(t, r.cfg.WorkDir, m.state.ID).state.Status; got != missionStatusActive {
		t.Fatalf("status = %q, want active", got)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "design review interrupted") {
		t.Errorf("the user must be told the plan is unreviewed: %v", ui.infoMsgs)
	}
}

// R37, on the fail-soft path too: after an escalation the worktree holds
// edits that never passed a review, and every design-side exit has to say so.
func TestDesignPhase_FailSoftAfterEscalationWarnsAboutUnreviewedEdits(t *testing.T) {
	fake := &fakeTaskTool{content: "not json"}
	r, ui, _ := newDesignRepl(t, fake, []string{"# plan"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	m.state.Escalation = 1
	r.mission = m

	r.runDesignPhase(context.Background())

	joined := strings.Join(ui.infoMsgs, "\n")
	if !strings.Contains(joined, "NEVER passed by a review") {
		t.Errorf("an escalated mission's fail-soft must warn about the unreviewed edits:\n%s", joined)
	}
}

// L1: design_failed means two different things, and the first sentence the
// user reads must be the true one for their case.
func TestFailDesign_WordsTheEscalatedCaseDifferently(t *testing.T) {
	fake := &fakeTaskTool{content: designFailJSON()}
	r, ui, _ := newDesignRepl(t, fake, []string{"# p1", "# p2", "# p3"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	m.state.Escalation = 1
	r.mission = m

	r.runDesignPhase(context.Background())

	joined := strings.Join(ui.infoMsgs, "\n")
	if strings.Contains(joined, "nothing was implemented from it") {
		t.Error("after an escalation that sentence is false — edits from the previous charter are in the worktree")
	}
	if !strings.Contains(joined, "NEVER passed by a review") {
		t.Errorf("the escalated case must say what is actually in the worktree:\n%s", joined)
	}
}
