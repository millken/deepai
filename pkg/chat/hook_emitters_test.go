package chat

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/models"
)

// Acceptance #7: every mission terminal status emits exactly one mission_end
// carrying the status string.
func TestLeaveMission_EmitsMissionEnd(t *testing.T) {
	for _, st := range []missionStatus{missionStatusAborted, missionStatusDone, missionStatusHandedOver, missionStatusDesignFailed} {
		r, _ := newMissionRepl(t, t.TempDir())
		m, err := createMission(r.cfg.WorkDir, "hook brief")
		if err != nil {
			t.Fatalf("createMission: %v", err)
		}
		r.mission = m
		em := &fakeEmitter{}
		r.cfg.Hooks = em

		r.leaveMission(st)

		if got := em.count(hook.EventMissionEnd); got != 1 {
			t.Fatalf("status %s: mission_end events = %d, want 1", st, got)
		}
		if msg := em.events[0].Message; !strings.Contains(msg, string(st)) {
			t.Fatalf("status %s: message = %q, want it to contain the status", st, msg)
		}
	}
}

// Acceptance #6: reaching awaiting_merge with pr_auto_merge:false emits
// exactly one pr_awaiting_merge carrying the PR number.
func TestPRLoop_AwaitingMergeEmitsHook(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	r.sess = &models.Session{ID: "sess-pr-hook"}
	r.cfg.PRAutoMerge = false
	em := &fakeEmitter{}
	r.cfg.Hooks = em
	st := newTrackedPR(t, r, 30, 1, prStatusAwaitingCI)

	r.runPRLoop(t.Context(), st)

	if got := em.count(hook.EventPRAwaitingMerge); got != 1 {
		t.Fatalf("pr_awaiting_merge events = %d, want 1 (events: %+v)", got, em.events)
	}
	if msg := em.events[0].Message; !strings.Contains(msg, "#30") {
		t.Fatalf("message = %q, want it to contain PR #30", msg)
	}
	// PR #7 review issue 3: the hint must name a word the control protocol can
	// actually deliver (bare `merge`; the raw `/pr merge` line is dropped by
	// parseControlLine and would strand the PR in awaiting_merge).
	if msg := em.events[0].Message; !strings.Contains(msg, "send: merge") {
		t.Fatalf("message = %q, want a protocol-deliverable merge hint", msg)
	}
}

// The auto-merge path does not wait for the user, so it must NOT emit.
func TestPRLoop_AutoMergeDoesNotEmitAwaitingMerge(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	r.sess = &models.Session{ID: "sess-pr-hook"}
	r.cfg.PRAutoMerge = true
	em := &fakeEmitter{}
	r.cfg.Hooks = em
	st := newTrackedPR(t, r, 31, 1, prStatusAwaitingCI)

	r.runPRLoop(t.Context(), st)

	if got := em.count(hook.EventPRAwaitingMerge); got != 0 {
		t.Fatalf("pr_awaiting_merge events = %d, want 0 under pr_auto_merge", got)
	}
}
