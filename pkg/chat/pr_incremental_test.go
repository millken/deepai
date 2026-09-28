package chat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

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
