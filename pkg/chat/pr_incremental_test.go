package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools/builtin"
)

// gitTestRepo creates a real git repo with two commits; returns the dir,
// the base sha (after commit 1) and the head sha (after commit 2).
func gitTestRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sha := func() string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	os.WriteFile(dir+"/a.txt", []byte("base line\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base = sha()
	os.WriteFile(dir+"/b.txt", []byte("fix line\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "fix")
	head = sha()
	return dir, base, head
}

func TestPRIncrementalDiff(t *testing.T) {
	dir, base, _ := gitTestRepo(t)

	diff, files, ok := prIncrementalDiff(dir, base)
	if !ok {
		t.Fatal("incremental diff must resolve against a valid base")
	}
	if len(files) != 1 || files[0] != "b.txt" {
		t.Fatalf("files = %v, want [b.txt] only", files)
	}
	if !strings.Contains(diff, "b.txt") || strings.Contains(diff, "a.txt") {
		t.Fatalf("diff must contain only the fix commit's change:\n%s", diff)
	}

	if _, _, ok := prIncrementalDiff(dir, ""); ok {
		t.Fatal("empty base must not claim an incremental diff")
	}
	if _, _, ok := prIncrementalDiff(dir, "0123456789abcdef0123456789abcdef01234567"); ok {
		t.Fatal("unresolvable base must fall back, not guess")
	}
}

// A re-review reads the commits since LastReviewHead and never calls
// gh pr diff; the reviewed head then advances to the current HEAD.
func TestDispatchPRReview_IncrementalReReview(t *testing.T) {
	dir, base, head := gitTestRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL-PR-SENTINEL", files: []string{"a.txt", "b.txt"}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh

	st, err := newPRState(dir, 42, "", "feature/x", "main", "", "brief")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	st.Round = 2
	st.LastReviewHead = base

	verdict, ok := r.dispatchPRReview(context.Background(), st, gh, nil)
	if !ok || verdict == nil || !isPassVerdict(verdict) {
		t.Fatalf("dispatch failed: ok=%v verdict=%+v", ok, verdict)
	}

	prompt := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "second reviewer") || !strings.Contains(prompt, base[:8]) {
		t.Fatalf("re-review prompt missing second-reviewer framing or since-sha:\n%.300s", prompt)
	}
	if strings.Contains(prompt, "FULL-PR-SENTINEL") {
		t.Fatal("incremental re-review must not read the full PR diff")
	}
	if !strings.Contains(prompt, "b.txt") || strings.Contains(prompt, "a.txt") {
		t.Fatalf("scope must list only the fix commit's file:\n%.300s", prompt)
	}
	if st.LastReviewHead != head {
		t.Fatalf("LastReviewHead = %s, want advanced to %s", st.LastReviewHead, head)
	}
}

// Unresolvable base (rebased away, fresh clone, non-git dir): fall back to
// the full PR diff — a stale anchor must never silently narrow the review.
func TestDispatchPRReview_FallbackWhenBaseMissing(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL-PR-SENTINEL", files: []string{"a.txt", "b.txt"}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh

	st, _ := newPRState(dir, 43, "", "feature/x", "main", "", "brief")
	st.Round = 2
	st.LastReviewHead = "0123456789abcdef0123456789abcdef01234567"

	if _, ok := r.dispatchPRReview(context.Background(), st, gh, nil); !ok {
		t.Fatal("fallback dispatch failed")
	}
	prompt := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "FULL-PR-SENTINEL") {
		t.Fatal("unresolvable base must fall back to the full PR diff")
	}
	if strings.Contains(prompt, "second reviewer") {
		t.Fatal("fallback is a full review, not the second-reviewer framing")
	}
}

func TestBuildReviewPrompt_PRModeIncremental(t *testing.T) {
	p := buildReviewPrompt(reviewPromptInput{
		diff: "d", prNumber: 9, prRound: 3, incremental: true, sinceSHA: "abcd1234ef56",
	})
	for _, want := range []string{
		"re-review round 3",
		"second reviewer",
		"since abcd1234ef56",
		"ONLY if the failure scenario survives the fix",
		"defects the fixes introduced",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("incremental prompt missing %q:\n%s", want, p)
		}
	}
	// The full-review framing must not leak into the incremental one.
	if strings.Contains(p, "the diff is the PR's current diff") {
		t.Fatalf("full-review framing leaked into incremental prompt:\n%s", p)
	}
}

// New-loop round-1 issue 1, pinned: a verdict logged for the round whose
// comment never reached the PR (transient gh failure between appendVerdict
// and PostComment) re-posts the comment on resume instead of skipping to
// the fix turn with the timeline missing its review.
func TestPRLoop_ResumeRepostsUnpostedVerdict(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	r.missionTurn = func(ctx context.Context, input string) *turnError { return nil }

	st, err := newPRState(dir, 50, "", "feature/x", "main", "", "brief")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	// Crash-point state: verdict for round 1 logged, comment never posted,
	// CommentPostedRound still 0.
	var v *agent.ReviewResult
	if err := json.Unmarshal([]byte(failVerdictJSON()), &v); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if err := st.appendVerdict(dir, 1, v); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}
	st.Round = 1
	st.Status = prStatusReviewing
	if err := st.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	// The very first posted comment must be the round-1 REVIEW comment —
	// not a fix comment from a skipped-post path.
	if len(gh.commentsPosted) == 0 {
		t.Fatal("resume posted nothing")
	}
	first := gh.commentsPosted[0]
	if m, ok := markerFromBody(first); !ok || m.Role != prRoleReviewer || m.Round != 1 {
		t.Fatalf("first comment must be the re-posted round-1 review: %q", first)
	}
	if st.CommentPostedRound != 1 {
		t.Fatalf("CommentPostedRound = %d, want 1", st.CommentPostedRound)
	}
	// The loop then completes: fix turn ran, comment sequence ends merged.
	if st.Status != prStatusAwaitingMerge && st.Status != prStatusMerged {
		t.Fatalf("status = %s, want terminal awaiting_merge/merged", st.Status)
	}
}

// New-loop round-1 issue 2, pinned: the merge handoff's next-task turn gets
// the auto-attach hook, so a PR created inside that turn enters its own
// review loop — the chained-auto-review contract.
func TestMergeHandoff_AttachesNextPR(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", checksScript: []fakeChecks{{done: true, ok: true}}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	r.cfg.PRReviewAuto = true
	r.missionTurn = func(ctx context.Context, input string) *turnError { return nil }

	// Session with a pending task and a merge-handoff turn whose bash output
	// shows a freshly created NEXT PR.
	r.carry.SetTodos([]builtin.TodoItem{
		{Content: "task A", Status: builtin.TodoInProgress},
		{Content: "task B", Status: builtin.TodoPending},
	})
	r.sess = &models.Session{Messages: []models.Message{
		{Role: models.RoleHuman, Content: "merge"},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash",
			Content: `{"stdout":"Creating pull request for feature/b into main in millken/deepai\n\nhttps://github.com/millken/deepai/pull/31\n"}`}},
	}}

	st, err := newPRState(dir, 30, "", "feature/a", "main", "", "task A")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	st.setStatus(dir, prStatusAwaitingMerge)

	r.mergePRAndContinue(context.Background(), st, gh)

	nextSt, err := openPRState(dir, 31)
	if err != nil {
		t.Fatalf("the chained PR #31 never attached: %v", err)
	}
	if nextSt.Status != prStatusAwaitingMerge {
		t.Fatalf("chained PR status = %s, want awaiting_merge (pass verdict + green checks in the fake)", nextSt.Status)
	}
}

// parseRoundVerdict helper removed: inline unmarshal keeps the crash-point
// state (single prState) exact — a helper re-creating the state would
// overwrite the round/status the test is asserting against.

func gitStep(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Round-3 issue 1 pin: a base that still RESOLVES but is no longer an
// ancestor of HEAD (rebase/reset between rounds) must fall back to the full
// PR diff — never diff upstream churn as if it were the implementer's fixes.
func TestPRIncrementalDiff_RebasedBaseFallsBack(t *testing.T) {
	dir, _, head := gitTestRepo(t)
	// Reset onto the base and commit a divergent head: `head` still resolves
	// in the object store but is no longer an ancestor.
	gitStep(t, dir, "reset", "-q", "--hard", "HEAD~1")
	os.WriteFile(dir+"/z.txt", []byte("z\n"), 0o644)
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "divergent")
	if _, _, ok := prIncrementalDiff(dir, head); ok {
		t.Fatal("a non-ancestor base must fall back, not diff upstream churn")
	}
}


// Round-3 issue 1, merge half: `git merge origin/main` between rounds keeps
// the anchor an ancestor (so --is-ancestor passes) while base..HEAD now
// carries upstream commits — the range must still fall back, never be read
// as the implementer's fixes.
func TestPRIncrementalDiff_MergedUpstreamFallsBack(t *testing.T) {
	dir, base, _ := gitTestRepo(t)
	gitStep(t, dir, "checkout", "-q", "-b", "upstream", base)
	os.WriteFile(dir+"/u.txt", []byte("upstream\n"), 0o644)
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "upstream change")
	gitStep(t, dir, "checkout", "-q", "main")
	gitStep(t, dir, "merge", "-q", "--no-edit", "upstream")
	os.WriteFile(dir+"/fix.txt", []byte("fix\n"), 0o644)
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "real fix")

	if _, _, ok := prIncrementalDiff(dir, base); ok {
		t.Fatal("a range containing a merge of upstream must fall back, not read upstream commits as the implementer's fixes")
	}
}

// Round-3 issue 2 pin: the incremental path applies the same byte cap — an
// oversized delta falls back to the full PR diff (which refuses if it too is
// oversized), never reaches the reviewer unchallenged.
func TestDispatchPRReview_OversizedIncrementalFallsBack(t *testing.T) {
	dir, base, _ := gitTestRepo(t)
	os.WriteFile(dir+"/big.txt", []byte(strings.Repeat("x", 210<<10)+"\n"), 0o644)
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "big delta")

	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL-PR-SENTINEL", files: []string{"a.txt"}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	st, _ := newPRState(dir, 61, "", "x", "main", "", "b")
	st.Round = 2
	st.LastReviewHead = base

	r.dispatchPRReview(context.Background(), st, gh, nil)
	prompt := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "FULL-PR-SENTINEL") {
		t.Fatal("an oversized incremental diff must fall back to the full PR diff")
	}
	if strings.Contains(prompt, "second reviewer") {
		t.Fatal("fallback is a full review, not the incremental framing")
	}
}

// Round-2 latency pin: a re-review's budget is scoped to the delta — 3×delta
// files + 2×previous issues, floor 10 — and must NOT inherit the full-review
// floor of 40 (the 871s deadline collision).
func TestReviewBudget_IncrementalScopesToDelta(t *testing.T) {
	dir, base, _ := gitTestRepo(t)
	os.WriteFile(dir+"/c.txt", []byte("c\n"), 0o644)
	os.WriteFile(dir+"/d.txt", []byte("d\n"), 0o644)
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "widen delta") // delta: b,c,d = 3 files

	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL", files: nil}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	st, _ := newPRState(dir, 60, "", "x", "main", "", "b")
	st.Round = 2
	st.LastReviewHead = base
	prev := &agent.ReviewResult{Verdict: "fail", Issues: []agent.Issue{{}, {}}}

	r.dispatchPRReview(context.Background(), st, gh, prev)
	if got := fake.args["max_tool_calls"]; got != 13 { // 3×3 + 2×2
		t.Fatalf("max_tool_calls = %v, want 13 (3×delta-files + 2×prev-issues)", got)
	}
}

func TestReviewBudget_IncrementalFloor(t *testing.T) {
	dir, base, _ := gitTestRepo(t) // delta: 1 file, no prev issues
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL", files: nil}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	st, _ := newPRState(dir, 63, "", "x", "main", "", "b")
	st.Round = 2
	st.LastReviewHead = base

	r.dispatchPRReview(context.Background(), st, gh, nil)
	if got := fake.args["max_tool_calls"]; got != 10 {
		t.Fatalf("max_tool_calls = %v, want floor 10 for a 1-file delta", got)
	}
}


// Round-3 issue 2 pin: an explicit review_max_tool_calls must be honored on
// the incremental path too — the hard-coded floor of 10 silently overrode
// operator config while the same PR's round-1 review got the full budget.
func TestReviewBudget_IncrementalHonorsConfiguredFloor(t *testing.T) {
	dir, base, _ := gitTestRepo(t) // delta: 1 file, no prev issues → formula floor 10
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "FULL", files: nil}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	r.cfg.ReviewMaxToolCalls = 80
	st, _ := newPRState(dir, 64, "", "x", "main", "", "b")
	st.Round = 2
	st.LastReviewHead = base

	r.dispatchPRReview(context.Background(), st, gh, nil)
	if got := fake.args["max_tool_calls"]; got != 80 {
		t.Fatalf("max_tool_calls = %v, want the configured 80 to beat the formula floor 10", got)
	}
}

// Round-3 issue 3 pin: crash after PostComment but before the posted-round
// marker persisted — the resume consults the PR timeline and records the
// marker instead of duplicating the public comment.
func TestPRLoop_ResumeDoesNotDuplicatePostedComment(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	var stored *agent.ReviewResult
	if err := json.Unmarshal([]byte(failVerdictJSON()), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{
		diff:         "+line",
		checksScript: []fakeChecks{{done: true, ok: true}},
		comments: []prComment{{ID: "1", Author: "millken",
			Body: reviewerCommentBody(1, stored), CreatedAt: time.Now().UTC()}},
	}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	r.missionTurn = func(ctx context.Context, input string) *turnError { return nil }

	st, _ := newPRState(dir, 62, "", "x", "main", "", "b")
	if err := st.appendVerdict(dir, 1, stored); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}
	st.Round = 1
	st.Status = prStatusReviewing
	if err := st.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	for _, body := range gh.commentsPosted {
		if m, ok := markerFromBody(body); ok && m.Role == prRoleReviewer && m.Round == 1 {
			t.Fatal("resume duplicated the round-1 review comment already on the PR")
		}
	}
	if st.CommentPostedRound != 1 {
		t.Fatalf("CommentPostedRound = %d, want recorded as 1 without re-posting", st.CommentPostedRound)
	}
}


// Round-3 issue 3 pin, lookup-failure half: when the timeline cannot be
// consulted (transient gh failure / rate limit), the resume must STOP —
// guessing "not posted" duplicates a public comment, guessing "posted"
// drops it. Neither; the marker stays unset and /pr resumes.
func TestPRLoop_ResumeCommentLookupFailureStops(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	var stored *agent.ReviewResult
	if err := json.Unmarshal([]byte(failVerdictJSON()), &stored); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{
		diff:            "+line",
		listCommentsErr: fmt.Errorf("rate limited"),
	}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh

	st, _ := newPRState(dir, 65, "", "x", "main", "", "b")
	if err := st.appendVerdict(dir, 1, stored); err != nil {
		t.Fatalf("appendVerdict: %v", err)
	}
	st.Round = 1
	st.Status = prStatusReviewing
	if err := st.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	r.runPRLoop(context.Background(), st)

	if len(gh.commentsPosted) != 0 {
		t.Fatalf("lookup failure must not post — posted %d comment(s)", len(gh.commentsPosted))
	}
	if st.CommentPostedRound != 0 {
		t.Fatalf("CommentPostedRound = %d, want 0 (unverified must not be recorded as posted)", st.CommentPostedRound)
	}
	if st.Status != prStatusReviewing {
		t.Fatalf("status = %s, want reviewing — resume retries the lookup, it does not consume the round", st.Status)
	}
}
