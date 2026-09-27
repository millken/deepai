package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
)

func mkComment(id, author, body string, ts time.Time) prComment {
	return prComment{ID: id, Author: author, Body: body, CreatedAt: ts}
}

func TestPendingExternalComments(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour)
	st := &prState{Number: 1, LastExternalCommentAt: base}
	gh := &fakeGH{comments: []prComment{
		mkComment("5", "alice", "old external", base.Add(-time.Minute)),
		mkComment("11", "deepai", "own review post", base.Add(time.Minute)),
		mkComment("20", "cursor", "cursor asks about tests", base.Add(2*time.Minute)),
		mkComment("30", "alice", "human nit", base.Add(3*time.Minute)),
		mkComment("40", "bob", "newest human note", base.Add(4*time.Minute)),
	}}

	got := pendingExternalComments(context.Background(), st, gh, "deepai")
	if len(got) != 3 || got[0].ID != "20" || got[2].ID != "40" {
		ids := make([]string, len(got))
		for i, c := range got {
			ids[i] = c.ID
		}
		t.Fatalf("pending = %v, want [20 30 40] (watermark, own login skipped)", ids)
	}
}

func TestPendingExternalComments_EmptyOwnLoginDisables(t *testing.T) {
	st := &prState{Number: 1}
	gh := &fakeGH{comments: []prComment{mkComment("1", "alice", "nit", time.Now().UTC())}}
	if got := pendingExternalComments(context.Background(), st, gh, ""); got != nil {
		t.Fatalf("unknown login must disable the passthrough, got %v", got)
	}
}

// The round-1 medium bug, pinned: a single comment larger than the 8KB cap
// must still reach the fix turn (kept newest, body clipped downstream) —
// not silently empty the whole passthrough.
func TestPendingExternalComments_SingleOverBudgetCommentSurvives(t *testing.T) {
	st := &prState{Number: 1}
	gh := &fakeGH{comments: []prComment{
		mkComment("1", "alice", strings.Repeat("x", 9<<10), time.Now().UTC()),
	}}
	got := pendingExternalComments(context.Background(), st, gh, "deepai")
	if len(got) != 1 {
		t.Fatalf("the only fresh comment must survive the cap, got %d", len(got))
	}
}

func TestPendingExternalComments_CapsAtNewest8KB(t *testing.T) {
	base := time.Now().UTC()
	var comments []prComment
	for i := 0; i < 6; i++ {
		comments = append(comments, mkComment(
			fmt.Sprintf("%d", i+1), "alice", strings.Repeat("x", 3000), base))
	}
	st := &prState{Number: 1}
	got := pendingExternalComments(context.Background(), st, &fakeGH{comments: comments}, "deepai")
	total := 0
	for _, c := range got {
		total += len(c.Body)
	}
	if total > (8<<10)+3000 { // cap plus at most one over-budget comment
		t.Fatalf("external block = %d bytes, want capped near 8KB", total)
	}
	if len(got) >= len(comments) {
		t.Fatalf("all %d comments passed the cap", len(got))
	}
	if got[len(got)-1].ID != "6" {
		t.Fatalf("cap must keep the NEWEST, got tail id %s", got[len(got)-1].ID)
	}
}

func TestExternalCommentsBlock(t *testing.T) {
	if externalCommentsBlock(nil) != "" {
		t.Fatal("no fresh comments must render no section")
	}
	b := externalCommentsBlock([]prComment{{ID: "9", Author: "alice", Body: "please add a test"}})
	if !strings.Contains(b, "alice") || !strings.Contains(b, "please add a test") {
		t.Fatalf("block = %q", b)
	}
}

func TestVerdictLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := newPRState(dir, 7, "", "", "", "", "")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	v1 := &agent.ReviewResult{Verdict: "fail", Summary: "round1", Issues: nil}
	v2 := &agent.ReviewResult{Verdict: "fail", Summary: "round2"}
	if err := st.appendVerdict(dir, 1, v1); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}
	if err := st.appendVerdict(dir, 2, v2); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}
	got, round := loadLastVerdict(dir, 7)
	if round != 2 || got == nil || got.Summary != "round2" {
		t.Fatalf("loadLastVerdict = (%+v, %d)", got, round)
	}
	if v, r := loadLastVerdict(dir, 999); v != nil || r != 0 {
		t.Fatalf("untracked PR returned (%+v, %d)", v, r)
	}
}

// The resume path in runPRLoop only adopts the logged verdict when its round
// is exactly st.Round-1: a PR parked in reviewing at round 2 with a
// round-1 fail verdict gets it as prev; one whose last verdict is from an
// older cycle does not.
func TestRunPRLoop_ResumeCarriesLastFailVerdict(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 2, prStatusReviewing)

	prev := &agent.ReviewResult{Verdict: "fail", Summary: "old findings",
		Issues: []agent.Issue{{Severity: "high", File: "a.go", Line: 3, Message: "nil deref", Scenario: "F(nil) panics"}}}
	if err := st.appendVerdict(r.cfg.WorkDir, 1, prev); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	prompt := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "Previously reported") || !strings.Contains(prompt, "nil deref") {
		t.Fatalf("resumed re-review must carry the round-1 verdict's issues as prev:\n%s", prompt)
	}
	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge after pass", st.Status)
	}
}

func TestRunPRLoop_ResumeIgnoresStaleVerdict(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	// Round 4 (post-CI-fix review), but the logged verdict is round 1.
	st := newTrackedPR(t, r, 43, 4, prStatusReviewing)
	prev := &agent.ReviewResult{Verdict: "fail", Summary: "ancient"}
	if err := st.appendVerdict(r.cfg.WorkDir, 1, prev); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	if strings.Contains(fake.args["prompt"].(string), "ancient") {
		t.Fatal("a verdict from an older cycle must not be adopted as prev")
	}
}

// External comments ride along on a fix turn, and the watermark advances so
// a second round does not re-surface them.
func TestRunPRLoop_ExternalCommentsReachFixTurn(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	gh := &fakeGH{
		diff: "+line",
		comments: []prComment{
			mkComment("5", "cursor", "please also fix the migration", time.Now().UTC()),
		},
		checksScript: []fakeChecks{{done: true, ok: true}},
	}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 44, 1, prStatusReviewing)

	var fixInput string
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		if strings.Contains(input, "[pr-review round 1/") {
			fixInput = input
			fake.content = passVerdictJSON()
		}
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if !strings.Contains(fixInput, "please also fix the migration") {
		t.Fatalf("external comment never reached the fix turn: %q", fixInput)
	}
	if st.LastExternalCommentAt.IsZero() {
		t.Fatal("watermark never advanced past the surfaced comment")
	}
}
