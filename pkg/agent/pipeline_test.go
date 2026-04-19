package agent

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestExpandTemplate(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		vars map[string]string
		want string
	}{
		{"single var", "Hello {{.Name}}", map[string]string{"Name": "World"}, "Hello World"},
		{"multiple vars", "{{.a}} and {{.b}}", map[string]string{"a": "x", "b": "y"}, "x and y"},
		{"no vars", "plain text", map[string]string{}, "plain text"},
		{"missing var", "Hello {{.Name}}", map[string]string{}, "Hello {{.Name}}"},
		{"diff output files", "{{.diff}} {{.output}} {{.files}}",
			map[string]string{"diff": "D", "output": "O", "files": "F"}, "D O F"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandTemplate(tt.tmpl, tt.vars)
			if got != tt.want {
				t.Errorf("expandTemplate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAggregateVerdict(t *testing.T) {
	t.Run("all pass", func(t *testing.T) {
		results := map[string]ReviewResult{
			"a": {Verdict: "pass"},
			"b": {Verdict: "pass"},
		}
		if v := aggregateVerdict(results); v != "pass" {
			t.Errorf("got %q, want pass", v)
		}
	})

	t.Run("one fail", func(t *testing.T) {
		results := map[string]ReviewResult{
			"a": {Verdict: "pass"},
			"b": {Verdict: "issues_found"},
		}
		if v := aggregateVerdict(results); v != "issues_found" {
			t.Errorf("got %q, want issues_found", v)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if v := aggregateVerdict(map[string]ReviewResult{}); v != "pass" {
			t.Errorf("got %q, want pass", v)
		}
	})
}

func TestHasCriticalIssues(t *testing.T) {
	t.Run("no issues", func(t *testing.T) {
		if hasCriticalIssues(map[string]ReviewResult{"a": {Verdict: "pass"}}) {
			t.Error("should be false")
		}
	})
	t.Run("warning only", func(t *testing.T) {
		results := map[string]ReviewResult{
			"a": {Issues: []Issue{{Severity: "warning"}}},
		}
		if hasCriticalIssues(results) {
			t.Error("should be false for warning")
		}
	})
	t.Run("has critical", func(t *testing.T) {
		results := map[string]ReviewResult{
			"a": {Issues: []Issue{{Severity: "warning"}, {Severity: "critical"}}},
		}
		if !hasCriticalIssues(results) {
			t.Error("should be true for critical")
		}
	})
}

func TestSummarizeReviews(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if s := summarizeReviews(nil); s != "" {
			t.Errorf("got %q, want empty", s)
		}
	})
	t.Run("with issues", func(t *testing.T) {
		reviews := map[string]ReviewResult{
			"security": {
				Summary: "SQL injection found",
				Issues: []Issue{
					{Severity: "critical", File: "db.go", Line: 10, Message: "injection", Suggestion: "sanitize"},
					{Severity: "warning", File: "util.go", Line: 5, Message: "missing check", Suggestion: "add nil check"},
				},
			},
		}
		s := summarizeReviews(reviews)
		if !containsAny(s, "CRITICAL", "WARNING", "injection", "missing check") {
			t.Errorf("summary should contain all severity levels: %s", s)
		}
	})
}

func TestResolvePipeline(t *testing.T) {
	t.Run("builtin pipeline", func(t *testing.T) {
		p, err := ResolvePipeline("code-with-review", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if p.Name != "code-with-review" {
			t.Errorf("Name = %q", p.Name)
		}
		if len(p.Reviewers) != 3 {
			t.Errorf("Reviewers = %d, want 3", len(p.Reviewers))
		}
	})

	t.Run("code-quick has no reviewers", func(t *testing.T) {
		p, err := ResolvePipeline("code-quick", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(p.Reviewers) != 0 {
			t.Errorf("Reviewers = %d, want 0", len(p.Reviewers))
		}
	})

	t.Run("unknown pipeline", func(t *testing.T) {
		_, err := ResolvePipeline("nonexistent", "")
		if err == nil {
			t.Error("expected error for unknown pipeline")
		}
	})
}

func TestLoadPipelineYAML(t *testing.T) {
	dir := t.TempDir()
	pipelinesDir := filepath.Join(dir, ".deepai", "pipelines")
	if err := os.MkdirAll(pipelinesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("valid pipeline", func(t *testing.T) {
		yaml := `name: custom-review
actor:
  agent_type: coder
  prompt: "{{.UserInput}}"
reviewers:
  - agent_type: security-reviewer
    prompt: "Review:\n{{.diff}}"
on_issues: retry
max_rounds: 2
`
		if err := os.WriteFile(filepath.Join(pipelinesDir, "custom-review.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		p, err := ResolvePipeline("custom-review", dir)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if p.Name != "custom-review" {
			t.Errorf("Name = %q", p.Name)
		}
		if p.Actor.AgentType != AgentTypeCoder {
			t.Errorf("Actor.AgentType = %q", p.Actor.AgentType)
		}
		if len(p.Reviewers) != 1 {
			t.Fatalf("Reviewers = %d, want 1", len(p.Reviewers))
		}
		if p.Reviewers[0].AgentType != AgentTypeSecurityReviewer {
			t.Errorf("Reviewer AgentType = %q", p.Reviewers[0].AgentType)
		}
		if p.OnIssues != "retry" {
			t.Errorf("OnIssues = %q", p.OnIssues)
		}
		if p.MaxRounds != 2 {
			t.Errorf("MaxRounds = %d, want 2", p.MaxRounds)
		}
	})

	t.Run("invalid on_issues rejected", func(t *testing.T) {
		yaml := `name: bad-pipeline
actor:
  agent_type: coder
  prompt: "{{.UserInput}}"
on_issues: explode
`
		if err := os.WriteFile(filepath.Join(pipelinesDir, "bad-pipeline.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := ResolvePipeline("bad-pipeline", dir)
		if err == nil {
			t.Error("expected validation error for invalid on_issues")
		}
	})
}

func TestListPipelines(t *testing.T) {
	t.Run("builtin only", func(t *testing.T) {
		names := ListPipelines("")
		if len(names) < 2 {
			t.Errorf("expected at least 2 builtin pipelines, got %d", len(names))
		}
		sort.Strings(names)
		found := false
		for _, n := range names {
			if n == "code-with-review" {
				found = true
			}
		}
		if !found {
			t.Error("code-with-review not in list")
		}
	})

	t.Run("yaml adds to list", func(t *testing.T) {
		dir := t.TempDir()
		pipelinesDir := filepath.Join(dir, ".deepai", "pipelines")
		if err := os.MkdirAll(pipelinesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		yaml := `name: my-pipeline
actor:
  agent_type: coder
  prompt: "{{.UserInput}}"
`
		if err := os.WriteFile(filepath.Join(pipelinesDir, "my-pipeline.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		names := ListPipelines(dir)
		found := false
		for _, n := range names {
			if n == "my-pipeline" {
				found = true
			}
		}
		if !found {
			t.Error("my-pipeline not in list")
		}
	})
}

func TestReviewerKey(t *testing.T) {
	t.Run("explicit name", func(t *testing.T) {
		r := ReviewerRef{AgentType: AgentTypeSecurityReviewer, Name: "sec-1"}
		if k := r.ReviewerKey(); k != "sec-1" {
			t.Errorf("got %q, want sec-1", k)
		}
	})
	t.Run("falls back to agent type", func(t *testing.T) {
		r := ReviewerRef{AgentType: AgentTypeSecurityReviewer}
		if k := r.ReviewerKey(); k != "security-reviewer" {
			t.Errorf("got %q, want security-reviewer", k)
		}
	})
}
