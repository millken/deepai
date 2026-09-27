package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeGH stands in for ghPRClient: scripted diff/files/checks plus a record
// of every posted comment.
type fakeGH struct {
	diff         string
	files        []string
	comments     []prComment
	checksScript []fakeChecks // consumed per Checks call; last repeats
	mergeErr     error
	// login is what Login reports; empty defaults to "deepai".
	login string
	// viewTitle is what View reports.
	viewTitle string

	commentsPosted []string
	checksCalls    int
}

func (f *fakeGH) Login(context.Context) (string, error) {
	if f.login == "" {
		return "deepai", nil
	}
	return f.login, nil
}

func (f *fakeGH) View(context.Context, string, int) (string, string, string, string, error) {
	title := f.viewTitle
	if title == "" {
		title = "PR review pipeline"
	}
	return title, "feature/pr-pipeline", "main", "", nil
}

type fakeChecks struct {
	done bool
	ok   bool
	err  error
}

func (f *fakeGH) PostComment(_ context.Context, _ string, _ int, body string) error {
	f.commentsPosted = append(f.commentsPosted, body)
	return nil
}

func (f *fakeGH) ListComments(context.Context, string, int) ([]prComment, error) {
	return f.comments, nil
}

func (f *fakeGH) Diff(context.Context, string, int) (string, error) { return f.diff, nil }

func (f *fakeGH) ChangedFiles(context.Context, string, int) ([]string, error) { return f.files, nil }

func (f *fakeGH) Checks(context.Context, string, int) (bool, bool, string, error) {
	if len(f.checksScript) == 0 {
		return true, true, "all pass", nil
	}
	c := f.checksScript[0]
	if len(f.checksScript) > 1 {
		f.checksScript = f.checksScript[1:]
	}
	f.checksCalls++
	return c.done, c.ok, "checks output", c.err
}

func (f *fakeGH) Merge(context.Context, string, int) error { return f.mergeErr }

func newPRLoopRepl(t *testing.T, fake *fakeTaskTool, gh *fakeGH) *ChatRepl {
	t.Helper()
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	r.prGH = gh
	r.prCIPollInterval = time.Millisecond
	r.prCIWaitTimeout = 50 * time.Millisecond
	r.missionTurn = func(ctx context.Context, input string) *turnError { return nil }
	return r
}

func newTrackedPR(t *testing.T, r *ChatRepl, number, round int, status prStatus) *prState {
	t.Helper()
	st, err := newPRState(r.cfg.WorkDir, number, "", "feature/x", "develop", "", "ledger export")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	st.Round = round
	st.Status = status
	if err := st.save(r.cfg.WorkDir); err != nil {
		t.Fatalf("save: %v", err)
	}
	return st
}

func TestPRLoop_FailFixPass(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 1, prStatusReviewing)

	// The fix turn flips the reviewer's next verdict to pass.
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		if strings.Contains(input, "[pr-review round 1/5]") {
			fake.content = passVerdictJSON()
		}
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge", st.Status)
	}
	if fake.calls != 2 {
		t.Fatalf("review dispatched %d times, want 2 (fail then pass)", fake.calls)
	}
	if st.Round != 2 {
		t.Fatalf("round = %d, want 2", st.Round)
	}
	if len(gh.commentsPosted) != 2 {
		t.Fatalf("posted %d comments, want 2 (reviewer + coder)", len(gh.commentsPosted))
	}
	if !strings.HasPrefix(gh.commentsPosted[0], "**deepai review — round 1: fail**") {
		t.Fatalf("first comment must be the round-1 review body: %q", gh.commentsPosted[0])
	}
	if !strings.HasPrefix(gh.commentsPosted[1], "**deepai fix — round 1**") {
		t.Fatalf("second comment must be the round-1 fix body: %q", gh.commentsPosted[1])
	}
	for _, body := range gh.commentsPosted {
		if strings.Contains(body, "<!--") {
			t.Fatalf("comment bodies must stay marker-free: %q", body)
		}
	}
	// The LAST dispatch (the re-review that passed) must have been in PR mode.
	if got := fake.args["prompt"]; !strings.Contains(fmt.Sprint(got), "PR #42, review round 2") {
		t.Fatalf("re-review prompt missing PR mode header: %v", got)
	}
}

func TestPRLoop_RoundCapAborts(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	gh := &fakeGH{diff: "+line"}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 1, prStatusReviewing)

	r.runPRLoop(context.Background(), st)

	if st.Status != prStatusAborted {
		t.Fatalf("status = %s, want aborted at the round cap", st.Status)
	}
	if fake.calls != maxPRReviewRounds {
		t.Fatalf("review dispatched %d times, want %d", fake.calls, maxPRReviewRounds)
	}
	if st.Round != maxPRReviewRounds+1 {
		t.Fatalf("round = %d, want %d", st.Round, maxPRReviewRounds+1)
	}
}

func TestPRLoop_FailSoftReviewConsumesNothing(t *testing.T) {
	fake := &fakeTaskTool{err: errors.New("boom")}
	gh := &fakeGH{diff: "+line"}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 1, prStatusReviewing)

	r.runPRLoop(context.Background(), st)

	if st.Status != prStatusReviewing || st.Round != 1 {
		t.Fatalf("fail-soft must leave state untouched, got %s round %d", st.Status, st.Round)
	}
	if len(gh.commentsPosted) != 0 {
		t.Fatalf("fail-soft posted %d comments, want 0", len(gh.commentsPosted))
	}
}

func TestPRLoop_CIFailEntersFixRound(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{
		diff: "+line",
		// review passes instantly; CI fails once, then passes after the fix.
		checksScript: []fakeChecks{{done: true, ok: false}, {done: true, ok: true}},
	}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 1, prStatusReviewing)

	sawCI := false
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		if strings.Contains(input, "CI on the pull request failed") {
			sawCI = true
		}
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if !sawCI {
		t.Fatal("CI failure never reached a fix turn")
	}
	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge after CI goes green", st.Status)
	}
	if st.Round != 2 {
		t.Fatalf("round = %d, want 2 (CI fix consumed a round)", st.Round)
	}
}

func TestPRLoop_CIPendingTimesOutFailSoft(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: false, ok: false}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 42, 1, prStatusReviewing)

	r.runPRLoop(context.Background(), st)

	if st.Status != prStatusAwaitingCI {
		t.Fatalf("pending-at-deadline must stay awaiting_ci, got %s", st.Status)
	}
	if gh.checksCalls < 2 {
		t.Fatalf("Checks called %d times, want polling (>=2)", gh.checksCalls)
	}
}
