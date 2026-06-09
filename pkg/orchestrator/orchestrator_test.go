package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedRunner struct {
	mu      sync.Mutex
	calls   []string
	reviews []string
	reviewI int
}

func (r *scriptedRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, agentType)
	if agentType == "arch-reviewer" || agentType == "security-reviewer" {
		out := r.reviews[r.reviewI]
		if r.reviewI < len(r.reviews)-1 {
			r.reviewI++
		}
		return out, nil
	}
	return "implemented", nil
}

type fixedVerifier struct {
	passes []bool
	i      int
}

func (v *fixedVerifier) Verify(ctx context.Context) (VerifyResult, error) {
	p := v.passes[v.i]
	if v.i < len(v.passes)-1 {
		v.i++
	}
	out := "ok"
	if !p {
		out = "FAIL: build error"
	}
	return VerifyResult{Ran: true, Passed: p, Output: out}, nil
}

type staticDiffer struct{ diff string }

func (d staticDiffer) Diff(ctx context.Context) (string, error) { return d.diff, nil }

func TestRun_ConvergesWhenVerifyAndReviewPass(t *testing.T) {
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass","summary":"good"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 4}, "do X",
		runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "diff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done {
		t.Fatalf("expected Done, got reason=%q", res.Reason)
	}
	if len(res.Rounds) != 1 {
		t.Fatalf("expected 1 round, got %d", len(res.Rounds))
	}
}

func TestRun_LoopsThenConverges(t *testing.T) {
	// Round 1: verify fails. Round 2: verify passes + review passes.
	runner := &scriptedRunner{reviews: []string{
		`{"verdict":"fail","summary":"x","issues":[{"severity":"high","file":"a.go","line":3,"message":"bug"}]}`,
		`{"verdict":"pass","summary":"fixed"}`,
	}}
	res, err := Run(context.Background(), Config{MaxRounds: 4}, "do X",
		runner, &fixedVerifier{passes: []bool{false, true}}, staticDiffer{diff: "diff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done || len(res.Rounds) != 2 {
		t.Fatalf("expected Done in 2 rounds, got done=%v rounds=%d reason=%q", res.Done, len(res.Rounds), res.Reason)
	}
	// coder must be re-invoked in round 2 with the feedback (4 subagent calls total: coder,review,coder,review).
	if got := countType(runner.calls, "coder"); got != 2 {
		t.Fatalf("coder invoked %d times, want 2 (a fix round is required)", got)
	}
}

func TestRun_VerifyFailBlocksDoneEvenIfReviewPasses(t *testing.T) {
	// Reviewer always says pass, but verification keeps failing → must NOT declare Done.
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2}, "do X",
		runner, &fixedVerifier{passes: []bool{false, false}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done {
		t.Fatal("must not be Done while verification fails, even with review pass")
	}
	if !strings.Contains(res.Reason, "max rounds") {
		t.Fatalf("reason = %q, want max-rounds give-up", res.Reason)
	}
}

func TestRun_GivesUpAtMaxRounds(t *testing.T) {
	runner := &scriptedRunner{reviews: []string{`{"verdict":"fail","summary":"nope"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 3}, "do X",
		runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done || len(res.Rounds) != 3 {
		t.Fatalf("expected give-up after 3 rounds, got done=%v rounds=%d", res.Done, len(res.Rounds))
	}
}

func TestRun_NoVerifierUsesReviewOnly(t *testing.T) {
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2}, "do X", runner, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done {
		t.Fatalf("expected Done with review-only, reason=%q", res.Reason)
	}
}

func TestRun_CoderErrorSurfaces(t *testing.T) {
	r := runnerFunc(func(ctx context.Context, at, d, p string) (string, error) {
		return "", errors.New("boom")
	})
	_, err := Run(context.Background(), Config{MaxRounds: 2}, "do X", r, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected coder error to surface, got %v", err)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		pass bool
		ok   bool
	}{
		{"pass", `{"verdict":"pass"}`, true, true},
		{"fail", `{"verdict":"fail"}`, false, true},
		{"pass bool", `{"pass":true}`, true, true},
		{"prose wrapped", "Here is my review:\n```json\n{\"verdict\": \"pass\", \"summary\":\"ok\"}\n```\nDone.", true, true},
		{"case insensitive", `{"verdict":"PASS"}`, true, true},
		{"unparseable", "looks good to me", false, false},
		{"nested braces in string", `{"verdict":"fail","summary":"use {x} here"}`, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := parseVerdict(c.in)
			if v.Parsed != c.ok {
				t.Fatalf("Parsed = %v, want %v", v.Parsed, c.ok)
			}
			if v.Pass != c.pass {
				t.Fatalf("Pass = %v, want %v", v.Pass, c.pass)
			}
		})
	}
}

func countType(calls []string, t string) int {
	n := 0
	for _, c := range calls {
		if c == t {
			n++
		}
	}
	return n
}

type runnerFunc func(ctx context.Context, agentType, description, prompt string) (string, error)

func (f runnerFunc) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	return f(ctx, agentType, description, prompt)
}

func TestRun_EmptyDiffForcesFail(t *testing.T) {
	// Coder claims success and verify passes, but no files actually changed
	// (empty diff). Must NOT be Done — and the reviewer must not even be consulted.
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2}, "do X",
		runner, &fixedVerifier{passes: []bool{true, true}}, staticDiffer{diff: "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done {
		t.Fatal("must not be Done when the diff is empty (no real change made)")
	}
	if got := countType(runner.calls, "arch-reviewer"); got != 0 {
		t.Fatalf("reviewer called %d times on empty diff, want 0 (skip review when nothing changed)", got)
	}
}

func TestRun_ReviewOnlyIsDoneButUnverified(t *testing.T) {
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2}, "do X", runner, nil, staticDiffer{diff: "diff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done {
		t.Fatalf("review-only should still complete, reason=%q", res.Reason)
	}
	if res.Verified {
		t.Fatal("review-only completion must report Verified=false")
	}
	if !strings.Contains(res.Reason, "UNVERIFIED") {
		t.Fatalf("reason should flag unverified, got %q", res.Reason)
	}
}

func TestRun_RequireVerificationBlocksReviewOnly(t *testing.T) {
	// No verifier (skipped) + RequireVerification → can never be Done.
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2, RequireVerification: true}, "do X",
		runner, nil, staticDiffer{diff: "diff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done {
		t.Fatal("RequireVerification with no verification must never be Done")
	}
}

func TestRun_VerifiedTrueWhenVerifyRanAndPassed(t *testing.T) {
	runner := &scriptedRunner{reviews: []string{`{"verdict":"pass"}`}}
	res, err := Run(context.Background(), Config{MaxRounds: 2, RequireVerification: true}, "do X",
		runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "diff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done || !res.Verified {
		t.Fatalf("expected Done+Verified, got done=%v verified=%v reason=%q", res.Done, res.Verified, res.Reason)
	}
}

type perTypeRunner struct {
	mu       sync.Mutex
	verdicts map[string]string
	calls    []string
}

func (r *perTypeRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, agentType)
	r.mu.Unlock()
	if v, ok := r.verdicts[agentType]; ok {
		return v, nil
	}
	return "implemented", nil
}

func TestRun_UnanimousReviewBlockedByOneFail(t *testing.T) {
	runner := &perTypeRunner{verdicts: map[string]string{
		"arch-reviewer":     `{"verdict":"pass"}`,
		"security-reviewer": `{"verdict":"fail","issues":[{"file":"a.go","line":1,"message":"injection"}]}`,
	}}
	res, err := Run(context.Background(),
		Config{MaxRounds: 1, Reviewers: []string{"arch-reviewer", "security-reviewer"}},
		"do X", runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done {
		t.Fatal("unanimous policy must fail when any reviewer fails")
	}
	if got := len(res.Rounds[0].Reviews); got != 2 {
		t.Fatalf("expected 2 reviews recorded, got %d", got)
	}
}

func TestRun_MajorityReviewPasses(t *testing.T) {
	runner := &perTypeRunner{verdicts: map[string]string{
		"arch-reviewer":     `{"verdict":"pass"}`,
		"security-reviewer": `{"verdict":"pass"}`,
		"perf-reviewer":     `{"verdict":"fail"}`,
	}}
	res, err := Run(context.Background(),
		Config{MaxRounds: 1, MajorityReview: true,
			Reviewers: []string{"arch-reviewer", "security-reviewer", "perf-reviewer"}},
		"do X", runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done {
		t.Fatalf("majority (2/3) should pass, reason=%q", res.Reason)
	}
}

func TestAggregateVerdicts(t *testing.T) {
	pass := Verdict{Pass: true, Parsed: true}
	fail := Verdict{Pass: false, Parsed: true}
	unparsed := Verdict{Pass: false, Parsed: false}
	names := []string{"a", "b", "c"}

	if !aggregateVerdicts([]Verdict{pass, pass}, names, false).Pass {
		t.Error("unanimous: all pass should pass")
	}
	if aggregateVerdicts([]Verdict{pass, fail}, names, false).Pass {
		t.Error("unanimous: one fail should fail")
	}
	if aggregateVerdicts([]Verdict{pass, unparsed}, names, false).Pass {
		t.Error("unanimous: unparseable verdict counts as fail")
	}
	if !aggregateVerdicts([]Verdict{pass, pass, fail}, names, true).Pass {
		t.Error("majority: 2/3 should pass")
	}
	if aggregateVerdicts([]Verdict{pass, fail, fail}, names, true).Pass {
		t.Error("majority: 1/3 should fail")
	}
	// issues from all reviewers are merged
	merged := aggregateVerdicts([]Verdict{
		{Pass: false, Parsed: true, Issues: []Issue{{File: "a.go"}}},
		{Pass: false, Parsed: true, Issues: []Issue{{File: "b.go"}}},
	}, names, false)
	if len(merged.Issues) != 2 {
		t.Fatalf("expected merged 2 issues, got %d", len(merged.Issues))
	}
}

func TestFanOutReviews_OrderDeterministicDespiteCompletionOrder(t *testing.T) {
	// Reviewer 0 is slow and completes last; results must still be indexed by
	// reviewer position, not completion order.
	reviewers := []string{"arch-reviewer", "security-reviewer", "perf-reviewer"}
	r := runnerFunc(func(ctx context.Context, agentType, d, p string) (string, error) {
		if agentType == "arch-reviewer" {
			time.Sleep(20 * time.Millisecond)
		}
		return `{"verdict":"fail","issues":[{"file":"` + agentType + `","line":1,"message":"x"}]}`, nil
	})
	verdicts, err := fanOutReviews(context.Background(), r, reviewers, "p", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(verdicts) != 3 {
		t.Fatalf("got %d verdicts, want 3", len(verdicts))
	}
	for i, rt := range reviewers {
		if len(verdicts[i].Issues) == 0 || verdicts[i].Issues[0].File != rt {
			t.Fatalf("verdict[%d] not from reviewer %q: %+v", i, rt, verdicts[i])
		}
	}
}

func TestFanOutReviews_ErrorPropagates(t *testing.T) {
	r := runnerFunc(func(ctx context.Context, agentType, d, p string) (string, error) {
		if agentType == "security-reviewer" {
			return "", errors.New("reviewer down")
		}
		return `{"verdict":"pass"}`, nil
	})
	_, err := fanOutReviews(context.Background(), r, []string{"arch-reviewer", "security-reviewer"}, "p", 2)
	if err == nil || !strings.Contains(err.Error(), "reviewer down") {
		t.Fatalf("expected reviewer error to propagate, got %v", err)
	}
}

func TestRun_StopsAtAgentCallBudget(t *testing.T) {
	// 1 reviewer → 2 calls/round. Budget 2 → exactly one round, then stop before round 2.
	runner := &scriptedRunner{reviews: []string{`{"verdict":"fail","summary":"nope"}`}}
	res, err := Run(context.Background(),
		Config{MaxRounds: 10, MaxAgentCalls: 2, Reviewers: []string{"arch-reviewer"}},
		"do X", runner, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Done {
		t.Fatal("should not be Done")
	}
	if res.AgentCalls != 2 {
		t.Fatalf("AgentCalls = %d, want 2 (one full round only)", res.AgentCalls)
	}
	if !strings.Contains(res.Reason, "budget") {
		t.Fatalf("reason = %q, want budget stop", res.Reason)
	}
	if len(res.Rounds) != 1 {
		t.Fatalf("ran %d rounds, want 1 within budget", len(res.Rounds))
	}
}
