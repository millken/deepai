package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

func newMissionRepl(t *testing.T, workDir string) (*ChatRepl, *mockUI) {
	t.Helper()
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:   ReplConfig{WorkDir: workDir},
		ui:    ui,
		carry: agent.NewSessionCarry(),
		sess:  &models.Session{ID: "sess-1", Metadata: map[string]string{}},
	}
	return r, ui
}

func TestCreateMission_WritesBriefAndActiveState(t *testing.T) {
	dir := t.TempDir()
	m, err := createMission(dir, "make the loop bounded")
	if err != nil {
		t.Fatalf("createMission: %v", err)
	}
	if m.state.Status != missionStatusActive || m.state.Phase != missionPhaseDesign {
		t.Fatalf("new mission = %s/%s, want active/design", m.state.Status, m.state.Phase)
	}
	brief, err := os.ReadFile(filepath.Join(m.dir, missionBriefFile))
	if err != nil || string(brief) != "make the loop bounded" {
		t.Fatalf("brief.md = %q, %v", brief, err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, missionStateFile)); err != nil {
		t.Fatalf("state.json missing: %v", err)
	}
	if !strings.HasPrefix(m.dir, missionsRoot(dir)) {
		t.Fatalf("mission dir %q is not under %q", m.dir, missionsRoot(dir))
	}
}

func TestOpenMission_RoundTripsStateAndCharter(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "brief text")
	m.state.Phase = missionPhaseImplement
	m.state.DesignRound = 2
	m.state.Escalation = 1
	m.state.Reviewed = true
	if err := m.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	writeFileOrFatal(t, m.designPath(), "# plan\nstuff")
	if err := m.lockCharter(&Charter{ScopeFiles: []string{"a.go"}, Acceptance: []string{"Given x, when y, then z"}}); err != nil {
		t.Fatalf("lockCharter: %v", err)
	}

	got, err := openMission(dir, m.state.ID)
	if err != nil {
		t.Fatalf("openMission: %v", err)
	}
	if got.state.Phase != missionPhaseImplement || got.state.DesignRound != 2 || got.state.Escalation != 1 || !got.state.Reviewed {
		t.Fatalf("state not round-tripped: %+v", got.state)
	}
	if got.brief != "brief text" {
		t.Fatalf("brief = %q", got.brief)
	}
	if got.charter == nil || len(got.charter.ScopeFiles) != 1 || got.charter.Brief != "brief text" {
		t.Fatalf("charter = %+v", got.charter)
	}
	// The locked plan is a snapshot: later edits to design.md must not change it.
	writeFileOrFatal(t, m.designPath(), "# a different plan")
	if got.lockedPlan() != "# plan\nstuff" {
		t.Fatalf("locked plan followed design.md: %q", got.lockedPlan())
	}
	if got.charter.DesignHash != hashText("# plan\nstuff") {
		t.Fatal("design_hash does not fingerprint the plan the charter was locked from")
	}
}

func TestOpenMission_UnknownIDIsAnError(t *testing.T) {
	if _, err := openMission(t.TempDir(), "nope"); err == nil {
		t.Fatal("openMission on a missing mission must fail, not invent one")
	}
}

func TestArchiveCharter_RenamesAndDetaches(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "b")
	writeFileOrFatal(t, m.designPath(), "plan")
	_ = m.lockCharter(&Charter{ScopeFiles: []string{"a.go"}, Acceptance: []string{"crit"}})
	if err := m.archiveCharter(1); err != nil {
		t.Fatalf("archiveCharter: %v", err)
	}
	if _, err := os.Stat(m.path(missionCharterFile)); !os.IsNotExist(err) {
		t.Fatal("charter.lock.json must be gone after an escalation")
	}
	if _, err := os.Stat(m.path("charter.v1.json")); err != nil {
		t.Fatalf("charter.v1.json missing: %v", err)
	}
	if m.charter != nil {
		t.Fatal("archiveCharter must detach the in-memory charter — the implementer must not be told to obey a charter the gate just rejected")
	}
}

func TestAppendReview_OneJSONLinePerDecision(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "b")
	m.appendReview(missionReviewRecord{Phase: "design", Round: 1, Verdict: "fail", Summary: "s"})
	m.appendReview(missionReviewRecord{Phase: "design", Round: 2, Verdict: "pass", Summary: "s2"})
	data, err := os.ReadFile(m.path(missionReviewsFile))
	if err != nil {
		t.Fatalf("read reviews.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), data)
	}
	var rec missionReviewRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec.Verdict != "pass" {
		t.Fatalf("second record = %+v, err %v", rec, err)
	}
}

func TestSaveAndLoadBaseline_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "b")
	snap := worktreeSnapshot{root: "/tmp/repo", entries: map[string]fileStamp{
		"a.go": {status: " M", size: 12, modTime: 99},
	}}
	m.saveBaseline(snap)
	got := openOrFatal(t, dir, m.state.ID).baseline
	if got.root != "/tmp/repo" || got.entries["a.go"] != (fileStamp{status: " M", size: 12, modTime: 99}) {
		t.Fatalf("baseline not round-tripped: %+v", got)
	}
}

func TestLoadBaseline_MissingFileIsTheZeroSnapshot(t *testing.T) {
	dir := t.TempDir()
	m, _ := createMission(dir, "b")
	if got := m.loadBaseline(); got.root != "" {
		t.Fatalf("missing baseline = %+v, want the zero snapshot (gate degrades, never blames the whole tree)", got)
	}
}

func openOrFatal(t *testing.T, workDir, id string) *mission {
	t.Helper()
	m, err := openMission(workDir, id)
	if err != nil {
		t.Fatalf("openMission: %v", err)
	}
	return m
}

func TestNormalizeScopeFiles(t *testing.T) {
	dir := t.TempDir()
	got := normalizeScopeFiles(dir, []string{
		"  pkg/chat/mission.go  ",
		"./pkg/chat/mission.go", // dup after cleaning
		filepath.Join(dir, "pkg/chat/review.go"),
		"/etc/passwd",  // outside the worktree — dropped, never clamped
		"../escape.go", // ditto
		"",
		"pkg/chat/new_file_that_does_not_exist_yet.go",
	})
	want := []string{"pkg/chat/mission.go", "pkg/chat/review.go", "pkg/chat/new_file_that_does_not_exist_yet.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (order must follow the plan)", got, want)
		}
	}
}

func TestLeaveMission_ClearsCharterInjectionAndDetaches(t *testing.T) {
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	m, _ := createMission(dir, "b")
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.carry.SetMissionCharter("charter text")
	r.reviewPrev = &agent.ReviewResult{Verdict: "fail"}

	r.leaveMission(missionStatusDone)

	if r.mission != nil {
		t.Error("r.mission must be nil after a terminal status — otherwise reviewGate keeps taking the mission branch")
	}
	if r.carry.MissionCharter() != "" {
		t.Error("the charter injection must stop the turn a mission ends")
	}
	if r.reviewPrev != nil {
		t.Error("a stale verdict must not survive into the next episode")
	}
	if got := openOrFatal(t, dir, m.state.ID).state.Status; got != missionStatusDone {
		t.Errorf("persisted status = %q, want done", got)
	}
	// done keeps the pointer so /mission status still answers.
	if r.sess.Metadata[sessionMissionKey] != m.state.ID {
		t.Error("done must keep metadata.mission_id so /mission status can still report it")
	}
}

func TestLeaveMission_AbortClearsTheSessionPointer(t *testing.T) {
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	m, _ := createMission(dir, "b")
	r.mission = m
	r.setSessionMission(m.state.ID)

	r.leaveMission(missionStatusAborted)

	if _, ok := r.sess.Metadata[sessionMissionKey]; ok {
		t.Error("abort must clear metadata.mission_id")
	}
	if got := openOrFatal(t, dir, m.state.ID).state.Status; got != missionStatusAborted {
		t.Errorf("persisted status = %q, want aborted", got)
	}
}

func TestAttachSessionMission_OnlyResumesActive(t *testing.T) {
	dir := t.TempDir()
	for _, st := range []missionStatus{missionStatusDone, missionStatusHandedOver, missionStatusDesignFailed, missionStatusAborted} {
		r, _ := newMissionRepl(t, dir)
		m, _ := createMission(dir, "b")
		_ = m.setStatus(st)
		r.sess.Metadata[sessionMissionKey] = m.state.ID
		r.attachSessionMission()
		if r.mission != nil {
			t.Errorf("status %q must not resume", st)
		}
	}

	r, ui := newMissionRepl(t, dir)
	m, _ := createMission(dir, "b")
	writeFileOrFatal(t, m.designPath(), "plan")
	_ = m.lockCharter(&Charter{ScopeFiles: []string{"a.go"}, Acceptance: []string{"crit"}})
	r.sess.Metadata[sessionMissionKey] = m.state.ID
	r.attachSessionMission()
	if r.mission == nil {
		t.Fatal("an active mission must re-attach")
	}
	if r.carry.MissionCharter() == "" {
		t.Error("a resumed mission must put its locked charter back on the injection")
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "ANY change to the worktree") {
		t.Error("resume must warn that external writers are attributed to the mission (R34/R39)")
	}
}

func TestAttachSessionMission_IgnoresOtherSessionsMissions(t *testing.T) {
	dir := t.TempDir()
	// An active mission exists on disk, but this session's metadata never
	// pointed at it (R23): it belongs to some other session, possibly one
	// still running in another terminal.
	_, _ = createMission(dir, "someone else's mission")
	r, _ := newMissionRepl(t, dir)
	r.attachSessionMission()
	if r.mission != nil {
		t.Fatal("a mission this session never started must not be adopted")
	}
}

func TestMissionCommand_NoArgsWithoutActiveMission(t *testing.T) {
	r, ui := newMissionRepl(t, t.TempDir())
	r.handleMissionCommand(context.Background(), "")
	if !strings.Contains(ui.lastInfo(), "no active mission") {
		t.Fatalf("got %q, want a request for a task description", ui.lastInfo())
	}
}

func TestMissionCommand_AbortWithoutMission(t *testing.T) {
	r, ui := newMissionRepl(t, t.TempDir())
	r.handleMissionCommand(context.Background(), "abort")
	if !strings.Contains(ui.lastInfo(), "no active mission") {
		t.Fatalf("got %q", ui.lastInfo())
	}
}

func TestMissionStatusText_ReadsDiskAfterTerminalStatus(t *testing.T) {
	dir := t.TempDir()
	r, _ := newMissionRepl(t, dir)
	m, _ := createMission(dir, "b")
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.leaveMission(missionStatusHandedOver)

	text := r.missionStatusText()
	if !strings.Contains(text, "handed_over") || !strings.Contains(text, m.state.ID) {
		t.Fatalf("/mission status after the end = %q", text)
	}
}
