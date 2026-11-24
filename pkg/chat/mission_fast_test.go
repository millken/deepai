package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func writeNestedFileOrFatal(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileOrFatal(t, p, content)
}

func capturingTaskRegistry(t *testing.T, contents []string, captured *[]map[string]any) *tools.Registry {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	reg := tools.NewRegistry()
	err := reg.Register(models.Tool{
		Name: "task",
		Handler: func(_ context.Context, call models.ToolCall) (models.ToolResult, error) {
			mu.Lock()
			calls++
			idx := calls - 1
			if idx >= len(contents) {
				idx = len(contents) - 1
			}
			cp := make(map[string]any, len(call.Arguments))
			for k, v := range call.Arguments {
				cp[k] = v
			}
			*captured = append(*captured, cp)
			content := contents[idx]
			mu.Unlock()
			return models.ToolResult{Content: content, Status: models.CallStatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("register capturing task tool: %v", err)
	}
	return reg
}

func TestDesignReview_ReasoningEffortConvergenceAndContextBundle(t *testing.T) {
	dir := t.TempDir()
	// Create two existing repo files that the plan cites, plus verify that
	// non-existent files, globs, URLs, and .deepai/ paths are NOT attached.
	writeNestedFileOrFatal(t, filepath.Join(dir, "pkg", "alpha", "core.go"), "package alpha\nfunc Core() {}\n")
	writeNestedFileOrFatal(t, filepath.Join(dir, "pkg", "alpha", "helper.go"), "package alpha\nfunc Helper() {}\n")

	var captured []map[string]any
	r, _ := newMissionRepl(t, dir)
	r.cfg.ToolRegistry = capturingTaskRegistry(t, []string{designFailJSON(), designPassJSON()}, &captured)
	r.cfg.ReviewReasoningEffort = "low"
	m, err := createMission(dir, "update alpha core")
	if err != nil {
		t.Fatal(err)
	}
	r.mission = m

	plan := strings.Join([]string{
		"# Plan",
		"- Modify `pkg/alpha/core.go:2` and pkg/alpha/helper.go",
		"- Create pkg/alpha/new_file.go",
		"- Scope glob pkg/alpha/**",
		"- Ref https://example.com/pkg/alpha/core.go",
		"- Mission plan at .deepai/missions/" + m.state.ID + "/design.md",
	}, "\n")

	turn := 0
	r.missionTurn = func(_ context.Context, _ string) *turnError {
		turn++
		writeFileOrFatal(t, m.designPath(), plan+fmt.Sprintf("\n<!-- rev %d -->\n", turn))
		return nil
	}

	if !r.runDesignPhase(context.Background()) {
		t.Fatal("design phase should pass on round 2")
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d design review calls, want 2", len(captured))
	}

	// Round 1 checks: reasoning_effort propagated, full default budget, context_files bundled.
	r1 := captured[0]
	if got := r1["reasoning_effort"]; got != "low" {
		t.Errorf("round 1 reasoning_effort = %v, want low", got)
	}
	if got := r1["max_tool_calls"]; got != DefaultReviewMaxToolCalls {
		t.Errorf("round 1 max_tool_calls = %v, want %d", got, DefaultReviewMaxToolCalls)
	}
	ctxFiles, _ := r1["context_files"].([]any)
	if len(ctxFiles) != 2 {
		t.Fatalf("round 1 context_files = %v, want 2 existing files", ctxFiles)
	}
	p1, _ := r1["prompt"].(string)
	if strings.Contains(p1, "Convergence round:") {
		t.Error("round 1 design review prompt must not include convergence round header")
	}
	if !strings.Contains(p1, "Pre-attached referenced files") {
		t.Error("round 1 design review prompt must list pre-attached files")
	}

	// Round 2 checks: tighter re-review tool budget + convergence prompt.
	r2 := captured[1]
	if got := r2["reasoning_effort"]; got != "low" {
		t.Errorf("round 2 reasoning_effort = %v, want low", got)
	}
	if got := r2["max_tool_calls"]; got != designReReviewMaxToolCalls {
		t.Errorf("round 2 max_tool_calls = %v, want %d", got, designReReviewMaxToolCalls)
	}
	p2, _ := r2["prompt"].(string)
	if !strings.Contains(p2, "Convergence round:") {
		t.Errorf("round 2 design review prompt missing convergence instruction:\n%s", p2)
	}
}

func TestDesignReview_DefaultReasoningEffortIsMedium(t *testing.T) {
	fake := &fakeTaskTool{content: designPassJSON()}
	r, _, _ := newDesignRepl(t, fake, []string{"# plan\n- do it"})
	m, _ := createMission(r.cfg.WorkDir, "brief")
	r.mission = m

	if !r.runDesignPhase(context.Background()) {
		t.Fatal("expected pass")
	}
	if got := fake.args["reasoning_effort"]; got != defaultReviewReasoningEffort {
		t.Errorf("default design review reasoning_effort = %v, want %q", got, defaultReviewReasoningEffort)
	}
}

func TestDesignOutcome_LowOnlyFailWithValidCharterPasses(t *testing.T) {
	dir := t.TempDir()
	lowOnly := &agent.DesignReviewResult{
		Verdict: "fail",
		Summary: "minor wording nit",
		Issues: []agent.Issue{
			{Severity: "low", File: "plan", Message: "could phrase step 2 more concisely"},
		},
		ScopeFiles: []string{"`pkg/chat/mission.go`"},
		Acceptance: []string{"Given a mission, when design passes, then charter locks cleanly"},
	}
	scope, pass, reason := designOutcomeWithSeverity(dir, lowOnly, "medium")
	if !pass || reason != "" {
		t.Fatalf("low-only design fail with valid charter should pass: pass=%v reason=%q", pass, reason)
	}
	if len(scope) != 1 || scope[0] != "pkg/chat/mission.go" {
		t.Errorf("scope = %v, want backtick-stripped [pkg/chat/mission.go]", scope)
	}

	// Low-only fail WITHOUT charter fields must NOT pass (cannot lock empty charter).
	lowNoCharter := &agent.DesignReviewResult{
		Verdict: "fail",
		Issues:  []agent.Issue{{Severity: "low", Message: "nit"}},
	}
	if _, pass, reason := designOutcomeWithSeverity(dir, lowNoCharter, "medium"); pass || reason != "" {
		t.Fatalf("low-only fail without charter must stay ordinary fail: pass=%v reason=%q", pass, reason)
	}

	// When minSeverity is "low", low issues DO block.
	if _, pass, _ := designOutcomeWithSeverity(dir, lowOnly, "low"); pass {
		t.Fatal("low issue must block when minSeverity is low")
	}
}

func TestMissionGate_LowOnlyIssuesPassWithoutExtraFixRound(t *testing.T) {
	lowJSON := `{"agent":"correctness-reviewer","verdict":"fail","summary":"only a minor naming nit",
		"issues":[{"severity":"low","file":"a.go","line":1,"message":"rename local variable","scenario":"style only"}]}`
	fake := &fakeTaskTool{content: lowJSON}
	r, ui, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x\n")

	got := r.missionReviewGate(context.Background(), 0)
	if !got.passed {
		t.Fatalf("low-only verdict should pass under default medium threshold: %+v", got)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "ignoring 1 low-severity note(s)") {
		t.Errorf("expected advisory low-severity log, got: %v", ui.infoMsgs)
	}
}

func TestMissionGate_LowSeverityDoesNotTriggerEscalation(t *testing.T) {
	// S1: low-severity design fault alongside a medium code issue must NOT escalate to design.
	mixedJSON := `{"agent":"correctness-reviewer","verdict":"fail","summary":"one code bug and one low plan nit",
		"issues":[
			{"severity":"medium","file":"a.go","line":1,"message":"nil check missing","scenario":"panics on nil","fault_layer":"code"},
			{"severity":"low","file":"a.go","line":2,"message":"plan doc wording","scenario":"minor","fault_layer":"design"}
		]}`
	fake := &fakeTaskTool{content: mixedJSON}
	r, _, _ := newImplementRepl(t, fake, []string{"a.go"})
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x\n")

	got := r.missionReviewGate(context.Background(), 0)
	if got.escalate != "" {
		t.Fatalf("low-severity fault_layer=design must not escalate, got %+v", got)
	}
	if got.next == "" {
		t.Fatalf("medium code issue should proceed to a normal fix round, got %+v", got)
	}

	// S2: low-severity issue on same file as previous round must not trigger repeat-file escalation.
	prev := &agent.ReviewResult{
		Verdict: "fail",
		Issues:  []agent.Issue{{Severity: "low", File: "a.go", Message: "nit 1"}},
	}
	cur := &agent.ReviewResult{
		Verdict: "fail",
		Issues:  []agent.Issue{{Severity: "medium", File: "a.go", Message: "real bug"}},
	}
	if sharesFailingFileWithMinRank(prev, cur, prSeverityRank("medium")) {
		t.Error("S2 must ignore low-severity issues when matching repeated failing files")
	}
	prev.Issues[0].Severity = "medium"
	if !sharesFailingFileWithMinRank(prev, cur, prSeverityRank("medium")) {
		t.Error("S2 must trigger when both rounds had blocking issues on the same file")
	}
}

func TestMissionGate_IncrementalReReviewAndConvergencePrompt(t *testing.T) {
	failA := `{"agent":"correctness-reviewer","verdict":"fail","summary":"bug in a.go",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"wrong return","scenario":"fails on empty"}]}`

	var captured []map[string]any
	r, ui, _ := newImplementRepl(t, &fakeTaskTool{}, []string{"a.go", "b.go", "c.go"})
	r.cfg.ToolRegistry = capturingTaskRegistry(t, []string{failA, passVerdictJSON()}, &captured)

	// Round 0: implementer edits a.go, b.go, c.go.
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x\n// v1\n")
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "b.go"), "package x\n// b1\n")
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "c.go"), "package x\n// c1\n")

	res0 := r.missionReviewGate(context.Background(), 0)
	if res0.next == "" || res0.passed {
		t.Fatalf("round 0 should fail and request fix: %+v", res0)
	}

	// Fix round 1: implementer modifies ONLY a.go (different size so fileStamp changes).
	writeFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "a.go"), "package x\n// v2 fixed with proper handling\n")

	res1 := r.missionReviewGate(context.Background(), 1)
	if !res1.passed {
		t.Fatalf("round 1 should pass: %+v", res1)
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d review calls, want 2", len(captured))
	}

	p0, _ := captured[0]["prompt"].(string)
	if strings.Contains(p0, "Convergence round:") {
		t.Error("round 0 must not be marked as convergence round")
	}
	if !strings.Contains(p0, "- b.go") || !strings.Contains(p0, "- c.go") {
		t.Errorf("round 0 should include all 3 changed files:\n%s", p0)
	}

	p1, _ := captured[1]["prompt"].(string)
	if !strings.Contains(p1, "Convergence round:") {
		t.Errorf("round 1 must include convergence instruction:\n%s", p1)
	}
	if !strings.Contains(p1, "scoped ONLY to the files touched in the fix round") {
		t.Errorf("round 1 must explain incremental delta scope:\n%s", p1)
	}
	_, changedSection, hasChanged := strings.Cut(p1, "## Files changed")
	if !hasChanged {
		t.Fatalf("round 1 missing Files changed section:\n%s", p1)
	}
	if !strings.Contains(changedSection, "- a.go") {
		t.Errorf("round 1 must include fixed file a.go:\n%s", changedSection)
	}
	if strings.Contains(changedSection, "- b.go") || strings.Contains(changedSection, "- c.go") {
		t.Errorf("round 1 incremental review must exclude untouched b.go/c.go:\n%s", changedSection)
	}
	// Incremental tool budget should be tightly bounded [8, 16].
	if cap1, _ := captured[1]["max_tool_calls"].(int); cap1 < 8 || cap1 > 16 {
		t.Errorf("round 1 incremental max_tool_calls = %d, want in [8, 16]", cap1)
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "reads 1 delta file(s) (of 3 in scope)") {
		t.Errorf("expected incremental delta log line, got: %v", ui.infoMsgs)
	}
}

func TestMissionGate_ShardsLargeMultiModuleDiff(t *testing.T) {
	var captured []map[string]any
	r, ui, _ := newImplementRepl(t, &fakeTaskTool{}, []string{"pkg/modA/**", "pkg/modB/**"})
	r.cfg.ToolRegistry = capturingTaskRegistry(t, []string{passVerdictJSON()}, &captured)

	// Create 10 files across 2 modules (hits prShardFileThreshold = 10).
	for i := 0; i < 5; i++ {
		writeNestedFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "pkg", "modA", fmt.Sprintf("a%d.go", i)), fmt.Sprintf("package modA\nvar A%d = %d\n", i, i))
		writeNestedFileOrFatal(t, filepath.Join(r.cfg.WorkDir, "pkg", "modB", fmt.Sprintf("b%d.go", i)), fmt.Sprintf("package modB\nvar B%d = %d\n", i, i))
	}

	got := r.missionReviewGate(context.Background(), 0)
	if !got.passed {
		t.Fatalf("sharded mission review should pass: %+v", got)
	}
	if len(captured) != 2 {
		t.Fatalf("expected 2 parallel module shard calls, got %d", len(captured))
	}
	if !strings.Contains(strings.Join(ui.infoMsgs, "\n"), "mission: large diff") {
		t.Errorf("expected mission sharding log line, got: %v", ui.infoMsgs)
	}
}

func TestIsScopeExempt_GeneratedLockfiles(t *testing.T) {
	exempt := []string{
		".deepai/missions/m1/state.json",
		"go.sum",
		"sub/go.sum",
		"go.work.sum",
		"package-lock.json",
		"web/package-lock.json",
		"pnpm-lock.yaml",
		"web/pnpm-lock.yaml",
		"npm-shrinkwrap.json",
		"yarn.lock",
		"Cargo.lock",
	}
	for _, rel := range exempt {
		if !isScopeExempt(rel) {
			t.Errorf("isScopeExempt(%q) = false, want true", rel)
		}
	}
	notExempt := []string{"go.mod", "package.json", "pkg/chat/mission.go"}
	for _, rel := range notExempt {
		if isScopeExempt(rel) {
			t.Errorf("isScopeExempt(%q) = true, want false", rel)
		}
	}
}
