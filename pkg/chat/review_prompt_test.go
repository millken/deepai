package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
)

func TestBuildReviewPrompt_FirstReview(t *testing.T) {
	p := buildReviewPrompt(reviewPromptInput{
		initialRequest: "make Parse handle empty input",
		diff:           "@@ -1 +1 @@\n-old\n+new\n",
		scope:          []string{"pkg/parse/parse.go"},
		bundled:        true,
		maxToolCalls:   20,
		timeout:        10 * time.Minute,
	})
	for _, want := range []string{
		"make Parse handle empty input", // the anchor, verbatim
		"pkg/parse/parse.go",            // the scope list
		"@@ -1 +1 @@",                   // the diff
		"20 tool calls",                 // the budget it is actually given
		"10m0s",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	// The bundle was attached — telling it to go read the files anyway wastes
	// its budget on files it already has.
	if strings.Contains(p, "NOT attached") {
		t.Fatalf("bundled review must not claim contents are missing:\n%s", p)
	}
	if strings.Contains(p, "Previously reported") {
		t.Fatalf("a first review has no previous round:\n%s", p)
	}
}

// A re-review's job is to check the fixes. Without the previous round's
// findings the next reviewer starts from zero: it re-derives (or misses) the
// same defect and can drift onto unrelated nitpicks while the reported one
// goes unverified.
func TestBuildReviewPrompt_ReReviewCarriesPreviousIssues(t *testing.T) {
	prev := &agent.ReviewResult{
		Verdict: "fail",
		Issues: []agent.Issue{{
			Severity: "high", File: "pkg/parse/parse.go", Line: 42,
			Message: "nil deref on empty input", Scenario: "Parse(\"\") → panic",
		}},
	}
	p := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d", prev: prev, maxToolCalls: 20, timeout: time.Minute,
	})
	for _, want := range []string{
		"Previously reported",
		"nil deref on empty input",
		"report it again ONLY if you can still construct",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("re-review prompt missing %q:\n%s", want, p)
		}
	}
}

func TestBuildReviewPrompt_PassingVerdictCarriesNothing(t *testing.T) {
	p := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d",
		prev: &agent.ReviewResult{Verdict: "pass"}, // no issues to verify
	})
	if strings.Contains(p, "Previously reported") {
		t.Fatalf("an empty issue list must not produce a re-review section:\n%s", p)
	}
}

// The reviewer's workload bound has to be one that degrades gracefully: the
// wall clock kills the run and loses everything, the tool-call cap forces a
// tool-less wrap-up that must still emit the verdict JSON.
func TestReviewDispatch_PassesGracefulToolCallCap(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	// A configured value must be what enforces the cap AND what the prompt
	// announces — asserting the constant again would pin a desync (round-2
	// review issue 1). 7 also beats 3×1 file, so the floor is the config's.
	r.cfg.ReviewMaxToolCalls = 7
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0).next; got != "" {
		t.Fatalf("want pass, got %q", got)
	}
	if got := fake.args["max_tool_calls"]; got != 7 {
		t.Fatalf("max_tool_calls = %v, want the configured 7", got)
	}
}

func TestReviewGate_PrevVerdictLifecycle(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0).next; got == "" {
		t.Fatal("a failing verdict must open a fix round")
	}
	if r.reviewPrev == nil {
		t.Fatal("the failing verdict must be carried into the next round's reviewer")
	}

	// Round cap: the episode ends and the carry-over must not leak into the
	// next episode's first review.
	seedEditedFile(t, r, "a.go", "package a // again\n")
	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, maxReviewRounds).next; got != "" {
		t.Fatalf("at the round cap the episode must end, got %q", got)
	}
	if r.reviewPrev != nil {
		t.Fatal("ending an episode must clear the carried verdict")
	}
}

func TestBuildReviewPrompt_PRMode(t *testing.T) {
	p := buildReviewPrompt(reviewPromptInput{
		initialRequest: "ledger export", diff: "d",
		scope:    []string{"internal/ledger.go"},
		prNumber: 42, prRound: 2, maxToolCalls: 20, timeout: time.Minute,
	})
	for _, want := range []string{
		"PR #42, review round 2",
		"already pushed",
		"report a previously reported issue ONLY if you can still construct",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("PR-mode prompt missing %q:\n%s", want, p)
		}
	}
	if strings.HasPrefix(p, "Adversarially review the code changes below.\n") {
		t.Fatalf("PR mode must replace the local-mode header:\n%.120s", p)
	}

	local := buildReviewPrompt(reviewPromptInput{initialRequest: "r", diff: "d"})
	if !strings.HasPrefix(local, "Adversarially review the code changes below.") {
		t.Fatalf("local mode (prNumber=0) header changed:\n%.120s", local)
	}
}

func TestBuildReviewPrompt_PRModeComposesWithPrevIssues(t *testing.T) {
	prev := &agent.ReviewResult{
		Verdict: "fail",
		Issues:  []agent.Issue{{Severity: "high", File: "a.go", Line: 3, Message: "off by one", Scenario: "empty slice panics"}},
	}
	p := buildReviewPrompt(reviewPromptInput{
		diff: "d", prev: prev, prNumber: 7, prRound: 3,
	})
	if !strings.Contains(p, "PR #7, review round 3") || !strings.Contains(p, "off by one") {
		t.Fatalf("PR mode must still carry previous issues for re-review:\n%s", p)
	}
}

func TestScaleReviewToolBudget(t *testing.T) {
	cases := []struct {
		configured, files, want int
	}{
		{0, 14, 42},    // 3×14 beats the default — PR #3's exact scope
		{0, 10, 40},    // default floor wins on small scopes
		{0, 0, 40},     // degenerate scope keeps the floor
		{60, 30, 90},   // scale beats a higher configured floor
		{60, 10, 60},   // configured floor wins over scale
		{0, 100, 80},   // clamp: 3×100 capped at 2× the floor
		{0, 300, 80},   // clamp: the 300-file PR scenario (round-3 review)
		{60, 300, 120}, // clamp follows a raised floor, not the default
	}
	for _, c := range cases {
		fake := &fakeTaskTool{content: passVerdictJSON()}
		r, _ := newReviewRepl(t, t.TempDir(), fake)
		r.cfg.ReviewMaxToolCalls = c.configured
		if got := r.scaleReviewToolBudget(c.files); got != c.want {
			t.Errorf("scaleReviewToolBudget(cfg=%d, files=%d) = %d, want %d", c.configured, c.files, got, c.want)
		}
	}
}

// The dispatch must carry the SCALED value, not the constant — a 14-file
// scope reaches the subagent as 42 while the single-file gate tests still see
// the 40 floor.
func TestReviewGateDispatchesScaledBudget(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	for i := 0; i < 14; i++ {
		seedEditedFile(t, r, fmt.Sprintf("f%02d.go", i), "package a\n")
	}
	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0).next; got != "" {
		t.Fatalf("want pass, got %q", got)
	}
	if got := fake.args["max_tool_calls"]; got != 42 {
		t.Fatalf("max_tool_calls = %v, want 42 (3×14 files)", got)
	}
}

// Round 3+ is the PR loop's convergence phase (docs/review.md §3): two fix
// rounds have already answered the earlier findings, and every NEW medium/low
// raised now only buys another fix round — the loop can never close that way.
func TestBuildReviewPrompt_PRRound3IsConvergence(t *testing.T) {
	late := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d",
		prNumber: 7, prRound: 3,
	})
	for _, want := range []string{
		"Convergence round",
		"only if it is critical or high",
		"an empty issue list never passes",
	} {
		if !strings.Contains(late, want) {
			t.Errorf("round-3+ prompt missing %q:\n%s", want, late)
		}
	}

	early := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d",
		prNumber: 7, prRound: 2,
	})
	if strings.Contains(early, "Convergence round") {
		t.Fatalf("rounds 1-2 keep the ordinary charter — findings are not yet throttled:\n%s", early)
	}

	inc := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d",
		prNumber: 7, prRound: 4, incremental: true, sinceSHA: "abc123",
	})
	if !strings.Contains(inc, "Convergence round") {
		t.Fatalf("an incremental late round must carry the convergence discipline too:\n%s", inc)
	}

	local := buildReviewPrompt(reviewPromptInput{
		initialRequest: "req", diff: "d",
		prev: &agent.ReviewResult{Verdict: "fail", Issues: []agent.Issue{{Severity: "high", File: "a.go", Line: 1, Message: "m", Scenario: "s"}}},
	})
	if strings.Contains(local, "Convergence round") {
		t.Fatalf("local re-reviews have their own 2-round cap — no PR convergence language:\n%s", local)
	}
}
