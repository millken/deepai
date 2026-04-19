package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkflow(t *testing.T) {
	t.Run("builtin code-with-review", func(t *testing.T) {
		wf, err := ResolveWorkflow("code-with-review", "")
		if err != nil {
			t.Fatal(err)
		}
		if wf.Name != "code-with-review" {
			t.Errorf("Name = %q", wf.Name)
		}
		if len(wf.Stages) != 4 {
			t.Errorf("Stages = %d, want 4", len(wf.Stages))
		}
	})

	t.Run("builtin feature-planning", func(t *testing.T) {
		wf, err := ResolveWorkflow("feature-planning", "")
		if err != nil {
			t.Fatal(err)
		}
		if wf.Name != "feature-planning" {
			t.Errorf("Name = %q", wf.Name)
		}
		if len(wf.Stages) != 5 {
			t.Errorf("Stages = %d, want 5", len(wf.Stages))
		}
	})

	t.Run("unknown workflow", func(t *testing.T) {
		_, err := ResolveWorkflow("nonexistent", "")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		_, err := ResolveWorkflow("../../../etc/passwd", "")
		if err == nil {
			t.Error("expected path traversal error")
		}
	})
}

func TestLoadWorkflowYAML(t *testing.T) {
	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, ".deepai", "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("valid custom workflow", func(t *testing.T) {
		yaml := `name: my-workflow
description: A test workflow
stages:
  - name: step1
    role: coder
    prompt: "{{.UserInput}}"
  - name: step2
    role: security-reviewer
    input_from: ["step1"]
    prompt: "Review: {{.outputs.step1}}"
`
		if err := os.WriteFile(filepath.Join(workflowsDir, "my-workflow.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		wf, err := ResolveWorkflow("my-workflow", dir)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if wf.Name != "my-workflow" {
			t.Errorf("Name = %q", wf.Name)
		}
		if len(wf.Stages) != 2 {
			t.Fatalf("Stages = %d, want 2", len(wf.Stages))
		}
		if wf.Stages[1].InputFrom[0] != "step1" {
			t.Errorf("InputFrom = %v", wf.Stages[1].InputFrom)
		}
	})

	t.Run("invalid workflow rejected", func(t *testing.T) {
		yaml := `name: bad-workflow
stages:
  - name: step1
`
		if err := os.WriteFile(filepath.Join(workflowsDir, "bad-workflow.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveWorkflow("bad-workflow", dir)
		if err == nil {
			t.Error("expected validation error")
		}
	})
}

func TestListWorkflows(t *testing.T) {
	t.Run("builtin only", func(t *testing.T) {
		names := ListWorkflows("")
		if len(names) < 2 {
			t.Errorf("expected at least 2 workflows, got %d", len(names))
		}
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
		workflowsDir := filepath.Join(dir, ".deepai", "workflows")
		if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		yaml := `name: custom-wf
stages:
  - name: s1
    role: coder
    prompt: "{{.UserInput}}"
`
		if err := os.WriteFile(filepath.Join(workflowsDir, "custom-wf.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		names := ListWorkflows(dir)
		found := false
		for _, n := range names {
			if n == "custom-wf" {
				found = true
			}
		}
		if !found {
			t.Error("custom-wf not in list")
		}
	})
}
