package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type designRunner struct {
	mu       sync.Mutex
	proposed []string
	judge    string
	judgeErr error
}

func (r *designRunner) Run(ctx context.Context, agentType, description, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if phaseOf(description) == "judge" {
		if r.judgeErr != nil {
			return "", r.judgeErr
		}
		return r.judge, nil
	}
	r.proposed = append(r.proposed, agentType)
	return "proposal from " + agentType, nil
}

func TestDesign_FansOutProposersThenSynthesizes(t *testing.T) {
	r := &designRunner{judge: `{"best":1,"rationale":"merged 0 and 1","plan":"final consolidated plan"}`}
	res, err := Design(context.Background(),
		DesignConfig{Proposers: []string{"architect", "coder", "product-manager"}, Judge: "architect"},
		"build feature X", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Proposals) != 3 {
		t.Fatalf("got %d proposals, want 3", len(res.Proposals))
	}
	if len(r.proposed) != 3 {
		t.Fatalf("proposers run = %d, want 3", len(r.proposed))
	}
	if res.Plan != "final consolidated plan" {
		t.Fatalf("plan = %q, want synthesized plan", res.Plan)
	}
	if res.BestIndex != 1 || !res.Parsed {
		t.Fatalf("best=%d parsed=%v, want 1/true", res.BestIndex, res.Parsed)
	}
	if res.Proposals[0].Agent != "architect" || res.Proposals[0].Angle == "" {
		t.Fatalf("proposal[0] = %+v, want ordered with an angle", res.Proposals[0])
	}
}

func TestDesign_UnparseableJudgeFallsBackToRawPlan(t *testing.T) {
	r := &designRunner{judge: "I think proposal 2 is best; here is the plan: do A then B."}
	res, err := Design(context.Background(), DesignConfig{Proposers: []string{"architect"}}, "task", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Parsed {
		t.Fatal("should not parse free-form judge output")
	}
	if !strings.Contains(res.Plan, "do A then B") {
		t.Fatalf("plan should fall back to raw judge text, got %q", res.Plan)
	}
}

func TestDesign_DefaultsApplied(t *testing.T) {
	r := &designRunner{judge: `{"plan":"p"}`}
	res, err := Design(context.Background(), DesignConfig{}, "task", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Proposals) != 2 {
		t.Fatalf("default proposers = %d, want 2", len(res.Proposals))
	}
}

func TestDesign_JudgeErrorSurfaces(t *testing.T) {
	r := &designRunner{judgeErr: errors.New("judge down")}
	_, err := Design(context.Background(), DesignConfig{Proposers: []string{"architect"}}, "task", r)
	if err == nil || !strings.Contains(err.Error(), "judge down") {
		t.Fatalf("expected judge error to surface, got %v", err)
	}
}

func TestDesign_ProposerErrorSurfaces(t *testing.T) {
	r := runnerFunc(func(ctx context.Context, agentType, d, p string) (string, error) {
		if phaseOf(d) == "propose" {
			return "", errors.New("proposer down")
		}
		return `{"plan":"p"}`, nil
	})
	_, err := Design(context.Background(), DesignConfig{Proposers: []string{"architect", "coder"}}, "task", r)
	if err == nil || !strings.Contains(err.Error(), "proposer down") {
		t.Fatalf("expected proposer error to surface, got %v", err)
	}
}

func TestDesign_ProposalsOrderedDespiteConcurrency(t *testing.T) {
	r := runnerFunc(func(ctx context.Context, agentType, d, p string) (string, error) {
		if phaseOf(d) == "judge" {
			return `{"plan":"x"}`, nil
		}
		if agentType == "architect" {
			time.Sleep(15 * time.Millisecond)
		}
		return "by " + agentType, nil
	})
	res, err := Design(context.Background(),
		DesignConfig{Proposers: []string{"architect", "coder", "product-manager"}}, "task", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"architect", "coder", "product-manager"}
	for i, w := range want {
		if res.Proposals[i].Agent != w {
			t.Fatalf("proposal[%d].Agent = %q, want %q (order must be stable)", i, res.Proposals[i].Agent, w)
		}
	}
}
