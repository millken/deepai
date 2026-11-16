package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// Phase 3 (RUN_CODE_DESIGN §9): the eval harness must be able to run the
// same corpus as two arms — A (baseline, no run_code) and B (run_code enabled
// for the coder subagent via task-level tool selectors, NOT by touching any
// production AgentTypeConfig) — and compare must judge the three default-on
// criteria: completion rate not down, input tokens down >= 25%, LLM turns
// not up.

func TestEvalRunCodeArmGate(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Setenv("DEEPAI_RUN_CODE", "")
		os.Unsetenv("DEEPAI_RUN_CODE")
		if evalRunCodeArm(Config{}) {
			t.Fatal("arm must default off")
		}
		for _, tl := range evalToolCandidates(Config{}) {
			if tl.Name == "run_code" {
				t.Fatal("run_code leaked into eval candidates with arm off")
			}
		}
	})
	t.Run("env enables", func(t *testing.T) {
		t.Setenv("DEEPAI_RUN_CODE", "1")
		if !evalRunCodeArm(Config{}) {
			t.Fatal("DEEPAI_RUN_CODE=1 must enable the arm")
		}
		if !hasToolName(evalToolCandidates(Config{}), "run_code") {
			t.Fatal("run_code missing from eval candidates with arm on")
		}
	})
	t.Run("config enables", func(t *testing.T) {
		t.Setenv("DEEPAI_RUN_CODE", "")
		os.Unsetenv("DEEPAI_RUN_CODE")
		if !evalRunCodeArm(Config{RunCode: true}) {
			t.Fatal("run_code: true must enable the arm")
		}
		if !hasToolName(evalToolCandidates(Config{RunCode: true}), "run_code") {
			t.Fatal("run_code missing from candidates with cfg.RunCode")
		}
	})
}

func hasToolName(tools []models.Tool, name string) bool {
	for _, x := range tools {
		if x.Name == name {
			return true
		}
	}
	return false
}

// armToolSelectors: nil on the A arm; the profile's DefaultTools plus
// run_code on the B arm; nil (no restriction override) for a profile with no
// DefaultTools — overriding an empty list with ["run_code"] would restrict
// the subagent to ONLY run_code.
func TestArmToolSelectors(t *testing.T) {
	t.Run("arm off is nil", func(t *testing.T) {
		if got := armToolSelectors("coder", t.TempDir(), false); got != nil {
			t.Fatalf("A arm selectors = %v, want nil", got)
		}
	})
	t.Run("coder B arm adds run_code to DefaultTools", func(t *testing.T) {
		got := armToolSelectors("coder", t.TempDir(), true)
		if got == nil {
			t.Fatal("B arm selectors = nil")
		}
		if !containsStr(got, "run_code") {
			t.Fatalf("B arm selectors lack run_code: %v", got)
		}
		for _, want := range []string{"bash", "edit_file", "read_file", "grep"} {
			if !containsStr(got, want) {
				t.Fatalf("B arm selectors dropped %q: %v", want, got)
			}
		}
	})
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The A/B arms must be distinguishable in the fingerprint: the system prompt
// is byte-identical between arms (run_code triggers none of the gated
// sections), so the fingerprint hashes the selected tool NAMES too — equal
// prompts with different toolsets must not compare as "unchanged".
func TestCaseFingerprintDistinguishesArms(t *testing.T) {
	repoRoot := t.TempDir()
	evalTools := evalToolCandidates(Config{})
	t.Setenv("DEEPAI_RUN_CODE", "1")
	bTools := evalToolCandidates(Config{})

	aFP, err := caseFingerprint("coder", repoRoot, evalTools, nil, nil)
	if err != nil {
		t.Fatalf("A arm fingerprint: %v", err)
	}
	sameA, err := caseFingerprint("coder", repoRoot, evalTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if aFP != sameA {
		t.Fatal("same arm fingerprint not stable")
	}
	bFP, err := caseFingerprint("coder", repoRoot, bTools, nil, armToolSelectors("coder", repoRoot, true))
	if err != nil {
		t.Fatalf("B arm fingerprint: %v", err)
	}
	if aFP == bFP {
		t.Fatalf("arms share fingerprint %q — compare would call a real A/B \"unchanged\"", aFP)
	}
}

// The three default-on metrics aggregate into roleSummary: completion rate
// (dispatched runs with zero failed assertions), average input tokens, and
// average LLM turns.
func TestBuildEvalSummaryRunCodeMetrics(t *testing.T) {
	records := []runRecord{
		{
			AgentType: "coder", Case: "c1", Run: 1, InputTokens: 1000, Tokens: 1200, LLMTurns: 8, HasStats: true,
			Assertions: []assertionResult{
				{Name: "mentions:x", Status: "pass"},
				{Name: "files_changed", Status: "pass"},
			},
		},
		{
			AgentType: "coder", Case: "c1", Run: 2, InputTokens: 3000, Tokens: 3400, LLMTurns: 12, HasStats: true,
			Assertions: []assertionResult{
				{Name: "mentions:x", Status: "pass"},
				{Name: "files_changed", Status: "fail"},
			},
		},
		{
			// dispatch timeout: contributes to neither completion nor token
			// averages, but its recovered stats DO count for the turns
			// distribution (the right-tail sample).
			AgentType: "coder", Case: "c2", Run: 1, Error: "timeout", LLMTurns: 30, HasStats: true,
		},
	}
	s := buildEvalSummary("m", 1, "5m", records)
	if len(s.Roles) != 1 {
		t.Fatalf("roles = %d", len(s.Roles))
	}
	r := s.Roles[0]
	if r.DispatchedRuns != 2 {
		t.Fatalf("DispatchedRuns = %d", r.DispatchedRuns)
	}
	if r.CompletedRuns != 1 || r.CompletionRate != 0.5 {
		t.Fatalf("CompletedRuns=%d CompletionRate=%v, want 1 / 0.5", r.CompletedRuns, r.CompletionRate)
	}
	if r.AvgInputTokens != 2000 {
		t.Fatalf("AvgInputTokens = %v, want 2000", r.AvgInputTokens)
	}
	if r.AvgLLMTurns != (8.0+12.0+30.0)/3 {
		t.Fatalf("AvgLLMTurns = %v, want %v (stats runs incl. timeout)", r.AvgLLMTurns, (8.0+12.0+30.0)/3)
	}
}

// compare renders the three default-on gates — but ONLY for a real A/B pair
// (differing fingerprints); an A/A comparison must not print them as if a
// run_code decision were being judged.
func TestRenderEvalCompareRunCodeCriteria(t *testing.T) {
	base := evalSummary{
		GeneratedAt: "t0", Model: "m", Runs: 3, Timeout: "5m",
		Roles: []roleSummary{{
			AgentType: "coder", Fingerprint: "aaaa",
			DispatchedRuns: 9, CompletionRate: 0.9, CompletedRuns: 8,
			AvgInputTokens: 10000, AvgLLMTurns: 10,
		}},
	}

	t.Run("25pct cut passes all three", func(t *testing.T) {
		after := deepCopySummary(base)
		after.GeneratedAt = "t1"
		after.Roles[0].Fingerprint = "bbbb"
		after.Roles[0].CompletionRate = 0.9
		after.Roles[0].AvgInputTokens = 7000
		after.Roles[0].AvgLLMTurns = 10
		out, err := renderEvalCompare(base, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"completion rate", "input tokens", "llm turns", "[PASS] completion rate"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "[FAIL]") {
			t.Fatalf("expected all gates PASS:\n%s", out)
		}
	})

	t.Run("regressions fail", func(t *testing.T) {
		after := deepCopySummary(base)
		after.Roles[0].Fingerprint = "bbbb"
		after.Roles[0].CompletionRate = 0.7  // down
		after.Roles[0].AvgInputTokens = 9000 // only 10% cut
		after.Roles[0].AvgLLMTurns = 13      // up
		out, err := renderEvalCompare(base, after)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(out, "[FAIL]"); got < 3 {
			t.Fatalf("want 3 FAIL gates, got %d:\n%s", got, out)
		}
	})

	t.Run("A/A pair skips the section", func(t *testing.T) {
		after := deepCopySummary(base)
		after.GeneratedAt = "t1"
		out, err := renderEvalCompare(base, after)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "run_code default-on criteria") {
			t.Fatalf("A/A comparison printed the run_code gates:\n%s", out)
		}
	})
}

func deepCopySummary(s evalSummary) evalSummary {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var out evalSummary
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// runOneCase threads the arm selectors (opts.ToolSelectors) into the
// dispatched task config: task.Config.Tools overrides DefaultTools, which is
// the only way the coder subagent's restricted registry (and therefore the
// nested run_code dispatch mirror) includes run_code without touching
// production config.
func TestRunOneCaseThreadsArmSelectors(t *testing.T) {
	pool := &selectorsCapturePool{TaskPool: &fakePoolWithResult{result: "done"}}
	c := evalCase{AgentType: "coder", ID: "x", Manifest: caseManifest{ID: "x", AgentType: "coder", Task: "t", Expect: []map[string]any{{"mentions": []any{"done"}}}}}
	_, err := runOneCase(context.Background(), pool, c, 1, "fp", evalOptions{ToolSelectors: []string{"run_code", "bash"}})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if !pool.gotSelectors {
		t.Fatal("runOneCase did not pass tool selectors to the dispatch")
	}
	if pool.selectors == nil || !containsStr(*pool.selectors, "run_code") {
		t.Fatalf("dispatched selectors = %v", pool.selectors)
	}
}

// fakePoolWithResult is the minimal evalTaskPool used above.
type fakePoolWithResult struct {
	result string
	usage  *subagent.TokenUsage
}

func (p *fakePoolWithResult) StartTask(ctx context.Context, id, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	return &subagent.Task{ID: id, Prompt: prompt}, nil
}

func (p *fakePoolWithResult) Wait(ctx context.Context, id string) (*subagent.Task, error) {
	return &subagent.Task{ID: id, Result: p.result, Usage: p.usage}, nil
}

// selectorsCapturePool wraps a pool and records the SubagentConfig it saw.
type selectorsCapturePool struct {
	TaskPool     evalTaskPool
	gotSelectors bool
	selectors    *[]string
}

func (p *selectorsCapturePool) StartTask(ctx context.Context, id, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	if len(cfg.Tools) > 0 {
		p.gotSelectors = true
		cp := append([]string(nil), cfg.Tools...)
		p.selectors = &cp
	}
	return p.TaskPool.StartTask(ctx, id, prompt, cfg)
}

func (p *selectorsCapturePool) Wait(ctx context.Context, id string) (*subagent.Task, error) {
	return p.TaskPool.Wait(ctx, id)
}

var _ = filepath.Join
