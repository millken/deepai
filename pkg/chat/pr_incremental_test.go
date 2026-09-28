package chat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
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
