package chat

import (
	"context"
	"errors"
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

// Round-3 review issue 4, pinned: an interrupt aimed at the CI wait must not
// leak into a later turn. A token buffered while gh.Checks was in flight (the
// wait returned on the fast path, never parking in its select) is drained on
// the way out — otherwise the next runTurnWithSignal watcher reads it and
// cancels an unrelated turn at birth.
func TestWaitPRCI_DrainsStaleInterruptToken(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	ui.interruptCh = make(chan struct{}, 1)
	ui.interruptCh <- struct{}{} // as if Ctrl+C landed while Checks was running
	r.prGH = gh
	st := newTrackedPR(t, r, 10, 1, prStatusAwaitingCI)

	done, ok, _, err := r.waitPRCI(context.Background(), st, gh)
	if err != nil || !done || !ok {
		t.Fatalf("waitPRCI = (%v, %v, %v), want the green fast path", done, ok, err)
	}
	select {
	case <-ui.interruptCh:
		t.Fatal("stale interrupt token survived the wait — it would cancel the next turn")
	default:
	}
}

// The resume path adopts the logged verdict on both crash points: a PR
// parked in reviewing at round 2 with a round-1 fail verdict (fix finished,
// next review pending) gets it as prev; one whose last verdict is from an
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

// Round-4 review issue 2, pinned: the crash-between-post-and-fix case —
// verdict logged at round N while st.Round is still N (the fix turn was
// interrupted, the round not consumed) — must adopt that verdict as prev;
// the old round == st.Round-1 gate dropped exactly the case the resume
// contract promised to cover.
func TestRunPRLoop_ResumeCarriesUnconsumedRoundVerdict(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 45, 1, prStatusReviewing)

	prev := &agent.ReviewResult{Verdict: "fail", Summary: "posted but fix interrupted",
		Issues: []agent.Issue{{Severity: "medium", File: "b.go", Line: 7, Message: "off by one", Scenario: "empty slice"}}}
	if err := st.appendVerdict(r.cfg.WorkDir, 1, prev); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	prompt := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "Previously reported") || !strings.Contains(prompt, "off by one") {
		t.Fatalf("a verdict whose fix turn never ran must still reach the re-review as prev:\n%s", prompt)
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

func TestRepoFromPRURL(t *testing.T) {
	if got := repoFromPRURL("https://github.com/millken/deepai/pull/3"); got != "millken/deepai" {
		t.Fatalf("repoFromPRURL = %q, want millken/deepai", got)
	}
	if got := repoFromPRURL("https://github.com/other/libY/pull/55"); got != "other/libY" {
		t.Fatalf("repoFromPRURL = %q, want other/libY", got)
	}
	for _, bad := range []string{"", "https://example.com/x", "not a url"} {
		if got := repoFromPRURL(bad); got != "" {
			t.Fatalf("repoFromPRURL(%q) = %q, want empty", bad, got)
		}
	}
}

// Round-2 review issue 4, pinned: same-second external comments must not be
// silently dropped by the watermark — the second one is still pending until
// its id is surfaced.
func TestSameSecondCommentsNotDropped(t *testing.T) {
	sec := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	st := &prState{Number: 1, LastExternalCommentAt: sec, SurfacedCommentIDs: []string{"IC_a"}}
	r, _ := newReviewRepl(t, t.TempDir(), &fakeTaskTool{})

	if !externalCommentPending(prComment{ID: "IC_b", CreatedAt: sec}, st) {
		t.Fatal("same-second unseen comment must be pending")
	}
	if externalCommentPending(prComment{ID: "IC_a", CreatedAt: sec}, st) {
		t.Fatal("same-second already-surfaced comment must not re-surface")
	}
	if externalCommentPending(prComment{ID: "IC_old", CreatedAt: sec.Add(-time.Second)}, st) {
		t.Fatal("comment older than the watermark must not be pending")
	}
	if !externalCommentPending(prComment{ID: "IC_new", CreatedAt: sec.Add(time.Second)}, st) {
		t.Fatal("strictly newer comment must be pending")
	}

	// Advance over IC_b: the same-second set accumulates while the second
	// does not move — IC_a must stay recorded or it would resurrect.
	r.advanceExternalWatermark(st, []prComment{{ID: "IC_b", CreatedAt: sec}})
	if len(st.SurfacedCommentIDs) != 2 {
		t.Fatalf("surfaced set = %v, want both same-second ids", st.SurfacedCommentIDs)
	}

	// A later comment moves the watermark: the set rebuilds from that round
	// only, older ids sit behind the timestamp for good.
	later := sec.Add(2 * time.Second)
	r.advanceExternalWatermark(st, []prComment{{ID: "IC_c", CreatedAt: later}})
	if len(st.SurfacedCommentIDs) != 1 || st.SurfacedCommentIDs[0] != "IC_c" {
		t.Fatalf("set after second advance = %v, want [IC_c]", st.SurfacedCommentIDs)
	}
	if !st.LastExternalCommentAt.Equal(later) {
		t.Fatalf("watermark = %v, want %v", st.LastExternalCommentAt, later)
	}
}

// Round-2 review issue 2, pinned: Ctrl+C during the CI wait returns
// errPRCIInterrupted instead of freezing the REPL for the whole budget.
func TestWaitPRCIInterruptible(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: false, ok: false}}}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	r.prGH = gh
	r.prCIPollInterval = time.Millisecond
	r.prCIWaitTimeout = 50 * time.Millisecond
	ui.interruptDuringTask = true // InterruptCh fires as soon as waitPRCI selects
	st := newTrackedPR(t, r, 9, 1, prStatusAwaitingCI)

	_, _, _, err := r.waitPRCI(context.Background(), st, gh)
	if !errors.Is(err, errPRCIInterrupted) {
		t.Fatalf("err = %v, want errPRCIInterrupted", err)
	}
}
