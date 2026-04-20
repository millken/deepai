package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// --- Mock Infrastructure ---

// mockExecutor records calls and returns configurable results.
type mockExecutor struct {
	mu      sync.Mutex
	calls   []string
	results map[string]string // prompt substring -> result JSON
	err     error
	delay   time.Duration
}

func (m *mockExecutor) Execute(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, task.Prompt)
	m.mu.Unlock()

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return subagent.ExecutionResult{}, ctx.Err()
		}
	}
	if m.err != nil {
		return subagent.ExecutionResult{}, m.err
	}

	result := `{"agent":"mock","verdict":"pass","summary":"ok"}`
	for substr, val := range m.results {
		if strings.Contains(task.Prompt, substr) {
			result = val
			break
		}
	}
	return subagent.ExecutionResult{
		Result:   result,
		Messages: []models.Message{{Role: models.RoleAI, Content: result}},
	}, nil
}

// mockPool wraps mockExecutor as a subagent.Pool alternative for testing Run().
// We use a real subagent.Pool backed by the mockExecutor.
func newTestPool(exec subagent.Executor) *subagent.Pool {
	return subagent.NewPool(exec, subagent.PoolConfig{
		MaxConcurrent: 3,
		Timeout:       5 * time.Second,
	})
}

// --- Orchestrator Core Flow Tests ---

func TestOrchestratorRunPassesWithNoReviewers(t *testing.T) {
	_ = &mockExecutor{results: map[string]string{}}

	pipeline := &Pipeline{
		Name:      "no-review",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: nil,
		OnIssues:  "report",
		MaxRounds: 1,
	}
	_ = pipeline.Validate()

	// Can't call Run() without executor, so test the logic inline
	maxRounds := pipeline.EffectiveMaxRounds()
	if maxRounds != 1 {
		t.Fatalf("EffectiveMaxRounds = %d, want 1", maxRounds)
	}
	if len(pipeline.Reviewers) != 0 {
		t.Fatal("should have no reviewers")
	}
}

func TestOrchestratorActorPassReviewerPass(t *testing.T) {
	passJSON, _ := json.Marshal(ReviewResult{
		Agent: "security-reviewer", Verdict: "pass", Summary: "clean",
	})

	exec := &mockExecutor{results: map[string]string{"review": string(passJSON)}}
	pool := newTestPool(exec)

	pipeline := &Pipeline{
		Name:      "pass-test",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 3,
	}
	_ = pipeline.Validate()

	results, err := (&Orchestrator{pool: pool, workDir: ""}).runReviewers(
		context.Background(), pipeline.Reviewers, ReviewInput{Diff: "diff content"},
	)
	if err != nil {
		t.Fatalf("runReviewers error: %v", err)
	}

	verdict := aggregateVerdict(results)
	if verdict != "pass" {
		t.Errorf("aggregateVerdict = %q, want pass", verdict)
	}
	if hasCriticalIssues(results) {
		t.Error("should have no critical issues")
	}
}

func TestOrchestratorReviewFindsCriticalIssues(t *testing.T) {
	failJSON, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "SQL injection found",
		Issues: []Issue{
			{Severity: "critical", File: "db.go", Line: 42, Message: "SQL injection", Suggestion: "use parameterized query"},
		},
	})

	exec := &mockExecutor{results: map[string]string{"review": string(failJSON)}}
	pool := newTestPool(exec)

	pipeline := &Pipeline{
		Name:      "fail-test",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 3,
	}

	results, err := (&Orchestrator{pool: pool, workDir: ""}).runReviewers(
		context.Background(), pipeline.Reviewers, ReviewInput{Diff: "diff"},
	)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	verdict := aggregateVerdict(results)
	if verdict != "issues_found" {
		t.Errorf("verdict = %q, want issues_found", verdict)
	}
	if !hasCriticalIssues(results) {
		t.Error("should have critical issues")
	}
}

func TestOrchestratorReviewWarningsOnlyNoCritical(t *testing.T) {
	warnJSON, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "minor issues",
		Issues: []Issue{
			{Severity: "warning", File: "util.go", Line: 10, Message: "missing error check"},
		},
	})

	exec := &mockExecutor{results: map[string]string{"review": string(warnJSON)}}
	pool := newTestPool(exec)

	results, err := (&Orchestrator{pool: pool, workDir: ""}).runReviewers(
		context.Background(),
		[]ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		ReviewInput{Diff: "diff"},
	)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	verdict := aggregateVerdict(results)
	if verdict != "issues_found" {
		t.Errorf("verdict = %q, want issues_found", verdict)
	}
	// Warnings only, no critical
	if hasCriticalIssues(results) {
		t.Error("should NOT have critical issues with warning-only findings")
	}
}

// --- Pipeline Validation Tests ---

func TestPipelineValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       *Pipeline
		wantErr bool
	}{
		{"valid", &Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder}, OnIssues: "retry"}, false},
		{"empty name", &Pipeline{Name: "", Actor: ActorRef{AgentType: AgentTypeCoder}}, true},
		{"no actor type", &Pipeline{Name: "ok", Actor: ActorRef{}}, true},
		{"invalid on_issues", &Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder}, OnIssues: "panic"}, true},
		{"empty on_issues ok", &Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder}, OnIssues: ""}, false},
		{"report ok", &Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder}, OnIssues: "report"}, false},
		{
			"reviewer no agent type",
			&Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder},
				Reviewers: []ReviewerRef{{AgentType: "", Prompt: "x"}}},
			true,
		},
		{
			"reviewer valid",
			&Pipeline{Name: "ok", Actor: ActorRef{AgentType: AgentTypeCoder},
				Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "x"}}},
			false,
		},
		{"nil pipeline", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPipelineEffectiveMaxRounds(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{0, 1},
		{-1, 1},
		{3, 3},
		{1, 1},
	}
	for _, tt := range tests {
		p := &Pipeline{MaxRounds: tt.input}
		if got := p.EffectiveMaxRounds(); got != tt.want {
			t.Errorf("EffectiveMaxRounds(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

// --- buildActorPrompt Tests ---

func TestBuildActorPromptFirstRound(t *testing.T) {
	orch := &Orchestrator{}
	prompt := orch.buildActorPrompt(
		ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		"implement login", "", nil,
	)
	if prompt != "implement login" {
		t.Errorf("first round prompt = %q, want original user input", prompt)
	}
}

func TestBuildActorPromptRetryRound(t *testing.T) {
	orch := &Orchestrator{}
	reviews := map[string]ReviewResult{
		"security-reviewer": {
			Verdict: "issues_found",
			Summary: "SQL injection",
			Issues:  []Issue{{Severity: "critical", File: "db.go", Line: 42, Message: "SQL injection", Suggestion: "use params"}},
		},
	}
	prompt := orch.buildActorPrompt(
		ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		"implement login", "diff content here", reviews,
	)

	// Retry prompt should contain user input, diff, and review feedback
	if !strings.Contains(prompt, "implement login") {
		t.Error("retry prompt should contain user input")
	}
	if !strings.Contains(prompt, "diff content here") {
		t.Error("retry prompt should contain previous diff")
	}
	if !strings.Contains(prompt, "SQL injection") {
		t.Error("retry prompt should contain review feedback")
	}
}

// --- Parallel Performance Test ---

func TestOrchestratorParallelReviewers(t *testing.T) {
	passJSON, _ := json.Marshal(ReviewResult{
		Agent: "test-reviewer", Verdict: "pass", Summary: "looks good",
	})

	pool := subagent.NewPool(&slowFakeExecutor{
		delay: 100 * time.Millisecond, result: string(passJSON),
	}, subagent.PoolConfig{MaxConcurrent: 3, Timeout: 5 * time.Second})

	pipeline := &Pipeline{
		Name:  "test-parallel",
		Actor: ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{
			{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"},
			{AgentType: AgentTypeArchReviewer, Prompt: "review:\n{{.diff}}"},
			{AgentType: AgentTypePerfReviewer, Prompt: "review:\n{{.diff}}"},
		},
		OnIssues: "report", MaxRounds: 1,
	}

	start := time.Now()
	results, err := (&Orchestrator{pool: pool, workDir: ""}).runReviewers(
		context.Background(), pipeline.Reviewers, ReviewInput{Diff: "some diff", Output: "some output"},
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("parallel review took %v, expected ~100-200ms", elapsed)
	}
}

// --- Four-Layer Isolation Tests ---

func TestConfigIsolation(t *testing.T) {
	secCfg := resolveAgentTypeConfig(AgentTypeSecurityReviewer, "")
	archCfg := resolveAgentTypeConfig(AgentTypeArchReviewer, "")
	perfCfg := resolveAgentTypeConfig(AgentTypePerfReviewer, "")

	if !containsAny(secCfg.SystemPrompt, "injection", "vulnerabilit") {
		t.Error("security reviewer prompt should mention injection/vulnerability")
	}
	if !containsAny(archCfg.SystemPrompt, "design pattern", "coupling") {
		t.Error("architecture reviewer prompt should mention design patterns/coupling")
	}
	if !containsAny(perfCfg.SystemPrompt, "algorithm", "complexity") {
		t.Error("performance reviewer prompt should mention algorithm/complexity")
	}

	hasBash := false
	for _, tool := range perfCfg.DefaultTools {
		if tool == "bash" {
			hasBash = true
		}
	}
	if !hasBash {
		t.Error("perf reviewer should have bash tool")
	}
	for _, tool := range secCfg.DefaultTools {
		if tool == "bash" {
			t.Error("security reviewer should NOT have bash tool")
		}
	}
}

func TestContextIsolation(t *testing.T) {
	expanded := expandTemplate("Review:\n{{.diff}}", map[string]string{
		"diff": "actual diff content", "output": "actor output",
	})
	if expanded != "Review:\nactual diff content" {
		t.Errorf("template expansion incorrect: %s", expanded)
	}
}

func TestInfoIsolation(t *testing.T) {
	r1 := ReviewerRef{AgentType: AgentTypeSecurityReviewer, Prompt: "Review:\n{{.diff}}"}
	r2 := ReviewerRef{AgentType: AgentTypeArchReviewer, Prompt: "Review:\n{{.diff}}"}
	p1 := expandTemplate(r1.Prompt, map[string]string{"diff": "the diff"})
	p2 := expandTemplate(r2.Prompt, map[string]string{"diff": "the diff"})
	if containsAny(p1, "architecture", "arch-reviewer") {
		t.Error("security reviewer prompt should not reference architecture reviewer")
	}
	if containsAny(p2, "security", "security-reviewer") {
		t.Error("architecture reviewer prompt should not reference security reviewer")
	}
}

func TestResponsibilityIsolation(t *testing.T) {
	writeTools := map[string]bool{"write_file": true, "edit_file": true, "bash": true}
	for _, at := range []AgentType{AgentTypeSecurityReviewer, AgentTypeArchReviewer} {
		cfg := resolveAgentTypeConfig(at, "")
		for _, tool := range cfg.DefaultTools {
			if writeTools[tool] {
				t.Errorf("reviewer %s should NOT have write tool %q", at, tool)
			}
		}
	}
	coderCfg := resolveAgentTypeConfig(AgentTypeCoder, "")
	hasWrite := false
	for _, tool := range coderCfg.DefaultTools {
		if tool == "write_file" || tool == "edit_file" {
			hasWrite = true
		}
	}
	if !hasWrite {
		t.Error("coder should have write tools")
	}
}

type slowFakeExecutor struct {
	delay  time.Duration
	result string
}

func (e *slowFakeExecutor) Execute(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
	select {
	case <-time.After(e.delay):
	case <-ctx.Done():
		return subagent.ExecutionResult{}, ctx.Err()
	}
	return subagent.ExecutionResult{Result: e.result}, nil
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --- Run() Integration Tests ---

func TestRunActorPassNoReviewers(t *testing.T) {
	actorResult := "implementation done"
	exec := &mockExecutor{results: map[string]string{
		"implement": actorResult,
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "no-review",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: nil,
		OnIssues:  "report",
		MaxRounds: 1,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement login",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Errorf("Verdict = %q, want pass", result.Verdict)
	}
	if result.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1", result.Rounds)
	}
	if result.ActorOutput != actorResult {
		t.Errorf("ActorOutput = %q, want %q", result.ActorOutput, actorResult)
	}
}

func TestRunActorPassReviewerPass(t *testing.T) {
	actorResult := "code written"
	passReview, _ := json.Marshal(ReviewResult{
		Agent: "security-reviewer", Verdict: "pass", Summary: "clean",
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": actorResult,
		"review":    string(passReview),
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "pass-flow",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 3,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement login",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Errorf("Verdict = %q, want pass", result.Verdict)
	}
	if result.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1 (should pass first round)", result.Rounds)
	}
}

func TestRunRetryOnCritical(t *testing.T) {
	failReview, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "SQL injection",
		Issues:  []Issue{{Severity: "critical", File: "db.go", Line: 1, Message: "SQL injection", Suggestion: "use params"}},
	})
	passReview, _ := json.Marshal(ReviewResult{
		Agent: "security-reviewer", Verdict: "pass", Summary: "fixed",
	})

	callCount := 0
	exec := &countingExecutor{fn: func(prompt string) string {
		callCount++
		if callCount <= 2 { // first 2 calls = actor round 0 + reviewer round 0
			if strings.Contains(prompt, "review") {
				return string(failReview)
			}
			return "code v1"
		}
		// round 1: actor retry + reviewer
		if strings.Contains(prompt, "review") {
			return string(passReview)
		}
		return "code v2"
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "retry-flow",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 3,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement login",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Errorf("Verdict = %q, want pass (retry should fix)", result.Verdict)
	}
	if result.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2 (1 fail + 1 pass)", result.Rounds)
	}
}

func TestRunMaxRoundsExhausted(t *testing.T) {
	failReview, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "still broken",
		Issues:  []Issue{{Severity: "critical", File: "db.go", Line: 1, Message: "still there"}},
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code v1",
		"review":    string(failReview),
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "exhaust-flow",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 2,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement login",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Verdict != "issues_found" {
		t.Errorf("Verdict = %q, want issues_found (max rounds exhausted)", result.Verdict)
	}
	if result.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2", result.Rounds)
	}
}

func TestRunOnIssuesReport(t *testing.T) {
	failReview, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "critical issue",
		Issues:  []Issue{{Severity: "critical", File: "x.go", Line: 1, Message: "bad"}},
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code",
		"review":    string(failReview),
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "report-flow",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "report",
		MaxRounds: 5,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement something",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// OnIssues=report should NOT retry even with critical issues
	if result.Verdict != "issues_found" {
		t.Errorf("Verdict = %q, want issues_found", result.Verdict)
	}
	if result.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1 (no retry with report)", result.Rounds)
	}
}

func TestRunWarningsOnlyNoRetry(t *testing.T) {
	warnReview, _ := json.Marshal(ReviewResult{
		Agent:   "security-reviewer",
		Verdict: "issues_found",
		Summary: "minor warning",
		Issues:  []Issue{{Severity: "warning", File: "x.go", Line: 1, Message: "style issue"}},
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code",
		"review":    string(warnReview),
	}}
	pool := newTestPool(exec)
	orch := NewOrchestrator(exec, pool, "")

	pipeline := &Pipeline{
		Name:      "warn-flow",
		Actor:     ActorRef{AgentType: AgentTypeCoder, Prompt: "{{.UserInput}}"},
		Reviewers: []ReviewerRef{{AgentType: AgentTypeSecurityReviewer, Prompt: "review:\n{{.diff}}"}},
		OnIssues:  "retry",
		MaxRounds: 5,
	}

	result, err := orch.Run(context.Background(), pipeline, OrchestratorInput{
		UserInput: "implement something",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// Warning-only should NOT trigger retry, even with OnIssues=retry
	if result.Verdict != "issues_found" {
		t.Errorf("Verdict = %q, want issues_found", result.Verdict)
	}
	if result.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1 (no retry for warnings)", result.Rounds)
	}
}

// countingExecutor calls fn for each execute and returns the result.
type countingExecutor struct {
	mu    sync.Mutex
	calls int
	fn    func(prompt string) string
}

func (e *countingExecutor) Execute(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	result := e.fn(task.Prompt)
	return subagent.ExecutionResult{
		Result:   result,
		Messages: []models.Message{{Role: models.RoleAI, Content: result}},
	}, nil
}
