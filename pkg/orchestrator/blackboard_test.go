package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestBlackboard_Render(t *testing.T) {
	var b *Blackboard
	if b.Render() != "" {
		t.Fatal("nil blackboard should render empty")
	}
	bb := &Blackboard{}
	if bb.Render() != "" {
		t.Fatal("empty blackboard should render empty")
	}
	bb.Plan = "use approach X"
	bb.AddNote("constraint: keep API stable")
	bb.AddNote("  ") // ignored
	out := bb.Render()
	if !strings.Contains(out, "use approach X") || !strings.Contains(out, "keep API stable") {
		t.Fatalf("render missing content: %q", out)
	}
}

type promptCapturingRunner struct {
	mu            sync.Mutex
	coderPrompts  []string
	reviewPrompts []string
}

func (r *promptCapturingRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch phaseOf(description) {
	case "implement":
		r.coderPrompts = append(r.coderPrompts, prompt)
		return "done", nil
	case "review":
		r.reviewPrompts = append(r.reviewPrompts, prompt)
		return `{"verdict":"pass"}`, nil
	}
	return "", nil
}

func TestRun_SharesPlanWithCoderAndReviewer(t *testing.T) {
	r := &promptCapturingRunner{}
	_, err := Run(context.Background(),
		Config{MaxRounds: 1, Reviewers: []string{"arch-reviewer"}, Plan: "PLAN-MARKER-42"},
		"do the task", r, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.coderPrompts) == 0 || !strings.Contains(r.coderPrompts[0], "PLAN-MARKER-42") {
		t.Fatalf("coder prompt missing shared plan: %+v", r.coderPrompts)
	}
	if len(r.reviewPrompts) == 0 || !strings.Contains(r.reviewPrompts[0], "PLAN-MARKER-42") {
		t.Fatalf("reviewer prompt missing shared plan (the core fix): %+v", r.reviewPrompts)
	}
}

func TestRun_NotesAccumulateAcrossRounds(t *testing.T) {
	// Round 1 review fails with a summary; round 2 should carry that as a note.
	runner := &promptCapturingRunner2{
		reviews: []string{`{"verdict":"fail","summary":"missing error handling"}`, `{"verdict":"pass"}`},
	}
	_, err := Run(context.Background(),
		Config{MaxRounds: 2, Reviewers: []string{"arch-reviewer"}},
		"task", runner, &fixedVerifier{passes: []bool{true, true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The 2nd-round coder prompt should reference the round-1 review note.
	if len(runner.coderPrompts) < 2 || !strings.Contains(runner.coderPrompts[1], "missing error handling") {
		t.Fatalf("round-2 coder prompt missing accumulated note: %+v", runner.coderPrompts)
	}
}

type promptCapturingRunner2 struct {
	mu           sync.Mutex
	reviews      []string
	reviewI      int
	coderPrompts []string
}

func (r *promptCapturingRunner2) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch phaseOf(description) {
	case "implement":
		r.coderPrompts = append(r.coderPrompts, prompt)
		return "done", nil
	case "review":
		out := r.reviews[r.reviewI]
		if r.reviewI < len(r.reviews)-1 {
			r.reviewI++
		}
		return out, nil
	}
	return "", nil
}
