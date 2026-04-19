package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// --- Mock Infrastructure ---

type mockExecutor struct {
	mu      sync.Mutex
	calls   []string
	results map[string]string // prompt substring -> result
	err     error
	errN    int32 // fail first N calls, then succeed
	delay   time.Duration
	callCount int32
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

	n := atomic.AddInt32(&m.callCount, 1)
	if m.errN > 0 && n <= m.errN {
		return subagent.ExecutionResult{}, m.err
	}
	if m.err != nil && m.errN == 0 {
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

func newTestPool(exec subagent.Executor) *subagent.Pool {
	return subagent.NewPool(exec, subagent.PoolConfig{
		MaxConcurrent: 3,
		Timeout:       5 * time.Second,
	})
}

// --- Engine Tests ---

func TestEngineRun_Linear(t *testing.T) {
	exec := &mockExecutor{results: map[string]string{
		"step1": "output-1",
		"step2": "output-2",
		"step3": "output-3",
	}}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "linear-test",
		Stages: []WorkflowStage{
			{Name: "step1", Role: agent.AgentTypeCoder, Prompt: "step1 {{.UserInput}}"},
			{Name: "step2", Role: agent.AgentTypeCoder, Prompt: "step2 {{.outputs.step1}}", InputFrom: []string{"step1"}},
			{Name: "step3", Role: agent.AgentTypeCoder, Prompt: "step3 {{.outputs.step2}}", InputFrom: []string{"step2"}},
		},
	}

	result, err := engine.Run(context.Background(), wf, "hello")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Status != "completed" {
		t.Errorf("Status = %q, want completed", result.Status)
	}
	if result.Stages["step1"].Status != "completed" {
		t.Error("step1 not completed")
	}
	if result.Stages["step2"].Status != "completed" {
		t.Error("step2 not completed")
	}
	if result.Stages["step3"].Status != "completed" {
		t.Error("step3 not completed")
	}

	// Verify context passing: step2 prompt should contain step1 output
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(exec.calls))
	}
	if !strings.Contains(exec.calls[1], "output-1") {
		t.Errorf("step2 prompt should contain output-1, got: %s", exec.calls[1])
	}
}

func TestEngineRun_ParallelReviewers(t *testing.T) {
	passReview, _ := json.Marshal(agent.ReviewResult{
		Verdict: "pass", Summary: "clean",
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code output",
		"Review":    string(passReview),
	}}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "parallel-test",
		Stages: []WorkflowStage{
			{Name: "implement", Role: agent.AgentTypeCoder, Prompt: "implement {{.UserInput}}"},
			{Name: "security", Role: agent.AgentTypeSecurityReviewer, InputFrom: []string{"implement"}, Prompt: "Review security: {{.outputs.implement}}"},
			{Name: "arch", Role: agent.AgentTypeArchReviewer, InputFrom: []string{"implement"}, Prompt: "Review arch: {{.outputs.implement}}"},
		},
	}

	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Status != "completed" {
		t.Errorf("Status = %q, want completed", result.Status)
	}
	if result.Stages["security"].Status != "completed" {
		t.Error("security not completed")
	}
	if result.Stages["arch"].Status != "completed" {
		t.Error("arch not completed")
	}
}

func TestEngineRun_ConditionalSkip(t *testing.T) {
	passReview, _ := json.Marshal(agent.ReviewResult{
		Verdict: "pass", Summary: "clean",
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code",
		"Review":    string(passReview),
	}}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "conditional-skip",
		Stages: []WorkflowStage{
			{Name: "implement", Role: agent.AgentTypeCoder, Prompt: "implement"},
			{Name: "review", Role: agent.AgentTypeSecurityReviewer, InputFrom: []string{"implement"}, Prompt: "Review"},
			{Name: "fix", Role: agent.AgentTypeCoder, InputFrom: []string{"implement", "review"}, Condition: "has_critical_issues", Prompt: "Fix"},
		},
	}

	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Stages["fix"].Status != "skipped" {
		t.Errorf("fix should be skipped, got %q", result.Stages["fix"].Status)
	}
	if result.Status != "completed" {
		t.Errorf("workflow should be completed (not partial), got %q", result.Status)
	}
}

func TestEngineRun_ConditionCritical(t *testing.T) {
	failReview, _ := json.Marshal(agent.ReviewResult{
		Verdict: "issues_found", Summary: "bad",
		Issues: []agent.Issue{
			{Severity: "critical", File: "a.go", Line: 1, Message: "bug"},
		},
	})

	exec := &mockExecutor{results: map[string]string{
		"implement": "code",
		"Review":    string(failReview),
		"Fix":       "fixed code",
	}}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "condition-critical",
		Stages: []WorkflowStage{
			{Name: "implement", Role: agent.AgentTypeCoder, Prompt: "implement"},
			{Name: "review", Role: agent.AgentTypeSecurityReviewer, InputFrom: []string{"implement"}, Prompt: "Review"},
			{Name: "fix", Role: agent.AgentTypeCoder, InputFrom: []string{"implement", "review"}, Condition: "has_critical_issues", Prompt: "Fix issues"},
		},
	}

	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Stages["fix"].Status != "completed" {
		t.Errorf("fix should execute (critical found), got %q", result.Stages["fix"].Status)
	}
}

func TestEngineRun_RetryOnFailure(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]string{"step": "success"},
		err:     fmt.Errorf("transient error"),
		errN:    1, // fail first call, succeed on second
	}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "retry-test",
		Stages: []WorkflowStage{
			{Name: "step", Role: agent.AgentTypeCoder, Prompt: "step", MaxRetries: 2},
		},
	}

	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Stages["step"].Status != "completed" {
		t.Errorf("step should succeed after retry, got %q", result.Stages["step"].Status)
	}
}

func TestEngineRun_MaxRetriesExhausted(t *testing.T) {
	exec := &mockExecutor{
		err:  fmt.Errorf("permanent error"),
		errN: 5, // always fail
	}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "retry-exhaust",
		Stages: []WorkflowStage{
			{Name: "step", Role: agent.AgentTypeCoder, Prompt: "step", MaxRetries: 2},
		},
	}

	_, err := engine.Run(context.Background(), wf, "test")
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "failed after 3 attempts") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEngineRun_ContextCancel(t *testing.T) {
	exec := &mockExecutor{
		delay: 10 * time.Second,
	}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	wf := &Workflow{
		Name: "cancel-test",
		Stages: []WorkflowStage{
			{Name: "step", Role: agent.AgentTypeCoder, Prompt: "step"},
		},
	}

	_, err := engine.Run(ctx, wf, "test")
	if err == nil {
		t.Error("expected cancellation error")
	}
}

// --- Condition Tests ---

func TestConditionEvaluation(t *testing.T) {
	t.Run("empty condition always executes", func(t *testing.T) {
		ok, err := evaluateCondition("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("empty condition should return true")
		}
	})

	t.Run("always condition", func(t *testing.T) {
		ok, err := evaluateCondition("always", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("always should return true")
		}
	})

	t.Run("never condition", func(t *testing.T) {
		ok, err := evaluateCondition("never", nil)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("never should return false")
		}
	})

	t.Run("unknown condition errors", func(t *testing.T) {
		_, err := evaluateCondition("nonexistent", nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("has_critical_issues true", func(t *testing.T) {
		failJSON, _ := json.Marshal(agent.ReviewResult{
			Issues: []agent.Issue{{Severity: "critical"}},
		})
		results := map[string]*StageResult{
			"review": {Name: "review", Output: string(failJSON), Status: "completed"},
		}
		ok, err := evaluateCondition("has_critical_issues", results)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("should detect critical issues")
		}
	})

	t.Run("has_critical_issues false with warning", func(t *testing.T) {
		warnJSON, _ := json.Marshal(agent.ReviewResult{
			Issues: []agent.Issue{{Severity: "warning"}},
		})
		results := map[string]*StageResult{
			"review": {Name: "review", Output: string(warnJSON), Status: "completed"},
		}
		ok, err := evaluateCondition("has_critical_issues", results)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("warning should not trigger critical")
		}
	})

	t.Run("has_critical_issues ignores skipped stages", func(t *testing.T) {
		results := map[string]*StageResult{
			"review": {Name: "review", Status: "skipped"},
		}
		ok, err := evaluateCondition("has_critical_issues", results)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("skipped stage should not trigger critical")
		}
	})
}

// --- Template Tests ---

func TestExpandWorkflowTemplate(t *testing.T) {
	vars := map[string]string{
		"UserInput":       "hello",
		"outputs.step1":   "result1",
		"outputs.step2":   "result2",
	}
	got := expandTemplate("{{.UserInput}} {{.outputs.step1}} {{.outputs.step2}}", vars)
	want := "hello result1 result2"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- Validate Tests ---

func TestWorkflowValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		wf := &Workflow{
			Name: "test",
			Stages: []WorkflowStage{
				{Name: "s1", Role: "coder", Prompt: "{{.UserInput}}"},
			},
		}
		if err := wf.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		wf := &Workflow{Stages: []WorkflowStage{{Name: "s1", Role: "coder"}}}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("no stages", func(t *testing.T) {
		wf := &Workflow{Name: "test"}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("duplicate stage names", func(t *testing.T) {
		wf := &Workflow{
			Name: "test",
			Stages: []WorkflowStage{
				{Name: "s1", Role: "coder"},
				{Name: "s1", Role: "coder"},
			},
		}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("empty role", func(t *testing.T) {
		wf := &Workflow{
			Name: "test",
			Stages: []WorkflowStage{
				{Name: "s1", Role: ""},
			},
		}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("unknown input_from", func(t *testing.T) {
		wf := &Workflow{
			Name: "test",
			Stages: []WorkflowStage{
				{Name: "s1", Role: "coder", InputFrom: []string{"missing"}},
			},
		}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("self reference", func(t *testing.T) {
		wf := &Workflow{
			Name: "test",
			Stages: []WorkflowStage{
				{Name: "s1", Role: "coder", InputFrom: []string{"s1"}},
			},
		}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("empty stage name", func(t *testing.T) {
		wf := &Workflow{
			Name:   "test",
			Stages: []WorkflowStage{{Role: "coder"}},
		}
		if err := wf.Validate(); err == nil {
			t.Error("expected error")
		}
	})
}

// --- Builtin Workflow Validation ---

func TestBuiltinWorkflows(t *testing.T) {
	for name, wf := range BuiltinWorkflows {
		t.Run(name, func(t *testing.T) {
			if err := wf.Validate(); err != nil {
				t.Errorf("builtin workflow %q invalid: %v", name, err)
			}
			waves, err := topologicalSort(wf.Stages)
			if err != nil {
				t.Errorf("builtin workflow %q has cycle: %v", name, err)
			}
			if len(waves) == 0 {
				t.Errorf("builtin workflow %q has no waves", name)
			}
		})
	}
}

// --- Additional Tests ---

func TestRegisterCondition(t *testing.T) {
	RegisterCondition("custom_test", func(results map[string]*StageResult) bool {
		return len(results) > 0
	})

	t.Run("custom condition evaluates", func(t *testing.T) {
		results := map[string]*StageResult{
			"a": {Name: "a", Status: "completed"},
		}
		ok, err := evaluateCondition("custom_test", results)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("custom condition should return true with results")
		}
	})

	t.Run("custom condition false on empty", func(t *testing.T) {
		ok, err := evaluateCondition("custom_test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("custom condition should return false with nil results")
		}
	})
}

func TestEngineRun_NilWorkflow(t *testing.T) {
	engine := NewEngine(nil, nil, "")
	_, err := engine.Run(context.Background(), nil, "test")
	if err == nil {
		t.Error("expected error for nil workflow")
	}
}

func TestEngineRun_NilPool(t *testing.T) {
	exec := &mockExecutor{results: map[string]string{}}
	engine := NewEngine(exec, nil, "")

	wf := &Workflow{
		Name: "nil-pool-test",
		Stages: []WorkflowStage{
			{Name: "s1", Role: agent.AgentTypeCoder, Prompt: "test"},
		},
	}
	// Should not panic — pool auto-created
	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stages["s1"].Status != "completed" {
		t.Errorf("s1 status = %q, want completed", result.Stages["s1"].Status)
	}
}

func TestEngineRun_ParallelStageFailure(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]string{"a": "ok"},
		err:     fmt.Errorf("parallel fail"),
		errN:    2,
	}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "parallel-fail-test",
		Stages: []WorkflowStage{
			{Name: "a", Role: agent.AgentTypeCoder, Prompt: "a"},
			{Name: "b", Role: agent.AgentTypeCoder, Prompt: "b"},
		},
	}

	_, err := engine.Run(context.Background(), wf, "test")
	if err == nil {
		t.Error("expected error from parallel stage failure")
	}
}

func TestEngineRun_StageOrder(t *testing.T) {
	exec := &mockExecutor{results: map[string]string{
		"a": "out-a",
		"b": "out-b",
		"c": "out-c",
	}}
	pool := newTestPool(exec)
	engine := NewEngine(exec, pool, "")

	wf := &Workflow{
		Name: "order-test",
		Stages: []WorkflowStage{
			{Name: "a", Role: agent.AgentTypeCoder, Prompt: "a"},
			{Name: "b", Role: agent.AgentTypeCoder, InputFrom: []string{"a"}, Prompt: "b"},
			{Name: "c", Role: agent.AgentTypeCoder, InputFrom: []string{"b"}, Prompt: "c"},
		},
	}

	result, err := engine.Run(context.Background(), wf, "test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if len(result.StageOrder) != len(want) {
		t.Fatalf("StageOrder len = %d, want %d", len(result.StageOrder), len(want))
	}
	for i, name := range want {
		if result.StageOrder[i] != name {
			t.Errorf("StageOrder[%d] = %q, want %q", i, result.StageOrder[i], name)
		}
	}
}

func TestTruncateOutput(t *testing.T) {
	t.Run("short output unchanged", func(t *testing.T) {
		got := truncateOutput("hello")
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("empty output", func(t *testing.T) {
		got := truncateOutput("")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("long output truncated", func(t *testing.T) {
		long := strings.Repeat("x", maxOutputLen+1000)
		got := truncateOutput(long)
		if len(got) > maxOutputLen+50 {
			t.Errorf("output too long: %d", len(got))
		}
		if !strings.HasSuffix(got, "\n... [truncated]") {
			t.Error("missing truncation marker")
		}
	})

	t.Run("exactly at limit", func(t *testing.T) {
		exact := strings.Repeat("x", maxOutputLen)
		got := truncateOutput(exact)
		if got != exact {
			t.Error("output at limit should not be truncated")
		}
	})
}

func TestConditionEvaluationError(t *testing.T) {
	_, err := evaluateCondition("unknown_condition", nil)
	if err == nil {
		t.Error("expected error for unknown condition")
	}
	if !strings.Contains(err.Error(), "unknown_condition") {
		t.Errorf("error should mention condition name: %v", err)
	}
}
