package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type buildRunner struct {
	mu          sync.Mutex
	coderPrompt string
}

func (r *buildRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch description {
	case "judge":
		return `{"plan":"PLAN-XYZ"}`, nil
	case "propose":
		return "a proposal", nil
	case "implement":
		r.coderPrompt = prompt
		return "implemented", nil
	case "review":
		return `{"verdict":"pass"}`, nil
	}
	return "", nil
}

func TestBuild_PlanFlowsIntoImplement(t *testing.T) {
	r := &buildRunner{}
	res, err := Build(context.Background(),
		BuildConfig{
			Design:    DesignConfig{Proposers: []string{"architect"}},
			Implement: Config{MaxRounds: 2},
		},
		"task T", r, &fixedVerifier{passes: []bool{true}}, staticDiffer{diff: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Design == nil || res.Design.Plan != "PLAN-XYZ" {
		t.Fatalf("design plan not produced: %+v", res.Design)
	}
	if res.Implement == nil || !res.Implement.Done {
		t.Fatalf("implement phase did not complete: %+v", res.Implement)
	}
	r.mu.Lock()
	cp := r.coderPrompt
	r.mu.Unlock()
	if !strings.Contains(cp, "PLAN-XYZ") {
		t.Fatalf("synthesized plan did not flow into the coder prompt: %q", cp)
	}
	if !strings.Contains(cp, "task T") {
		t.Fatalf("original task missing from coder prompt: %q", cp)
	}
}

func TestBuild_DesignErrorSkipsImplement(t *testing.T) {
	r := runnerFunc(func(ctx context.Context, at, d, p string) (string, error) {
		if d == "judge" {
			return "", errors.New("judge down")
		}
		return "x", nil
	})
	res, err := Build(context.Background(),
		BuildConfig{Design: DesignConfig{Proposers: []string{"architect"}}},
		"t", r, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "judge down") {
		t.Fatalf("expected design error to surface, got %v", err)
	}
	if res.Implement != nil {
		t.Fatal("implement phase must not run after a design failure")
	}
}

func TestCombineTaskAndPlan(t *testing.T) {
	if got := combineTaskAndPlan("T", ""); got != "T" {
		t.Fatalf("empty plan should pass task through, got %q", got)
	}
	if got := combineTaskAndPlan("T", "   "); got != "T" {
		t.Fatalf("whitespace plan should pass task through, got %q", got)
	}
	got := combineTaskAndPlan("TASK", "PLAN")
	if !strings.Contains(got, "TASK") || !strings.Contains(got, "PLAN") {
		t.Fatalf("combined prompt missing task or plan: %q", got)
	}
}
