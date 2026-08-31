package chat

import (
	"context"
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
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("want pass, got %q", got)
	}
	if got := fake.args["max_tool_calls"]; got != reviewMaxToolCalls {
		t.Fatalf("max_tool_calls = %v, want %d", got, reviewMaxToolCalls)
	}
}

func TestReviewGate_PrevVerdictLifecycle(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got == "" {
		t.Fatal("a failing verdict must open a fix round")
	}
	if r.reviewPrev == nil {
		t.Fatal("the failing verdict must be carried into the next round's reviewer")
	}

	// Round cap: the episode ends and the carry-over must not leak into the
	// next episode's first review.
	seedEditedFile(t, r, "a.go", "package a // again\n")
	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, maxReviewRounds); got != "" {
		t.Fatalf("at the round cap the episode must end, got %q", got)
	}
	if r.reviewPrev != nil {
		t.Fatal("ending an episode must clear the carried verdict")
	}
}
