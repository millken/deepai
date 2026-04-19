package workflow

import (
	"fmt"

	"github.com/millken/deepai/pkg/agent"
)

// Workflow defines a multi-agent execution plan as a DAG of stages.
type Workflow struct {
	Name        string          `json:"name" yaml:"name"`
	Description string          `json:"description" yaml:"description"`
	Stages      []WorkflowStage `json:"stages" yaml:"stages"`
}

// WorkflowStage is a single execution unit within a workflow.
type WorkflowStage struct {
	Name       string          `json:"name" yaml:"name"`
	Role       agent.AgentType `json:"role" yaml:"role"`
	InputFrom  []string        `json:"input_from,omitempty" yaml:"input_from,omitempty"`
	Prompt     string          `json:"prompt" yaml:"prompt"`
	Condition  string          `json:"condition,omitempty" yaml:"condition,omitempty"`
	MaxRetries int             `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
}

// StageResult holds the output of a single stage execution.
type StageResult struct {
	Name   string `json:"name"`
	Output string `json:"output"`
	Status string `json:"status"` // "completed", "skipped", "failed"
}

// WorkflowResult holds the aggregate output of a workflow run.
type WorkflowResult struct {
	Name        string                  `json:"name"`
	Status      string                  `json:"status"` // "completed", "failed", "partial"
	Stages      map[string]*StageResult `json:"stages"`
	StageOrder  []string                `json:"stage_order"` // execution order of stage names
	FinalOutput string                  `json:"final_output"`
}

// Validate checks the workflow for structural errors.
func (wf *Workflow) Validate() error {
	if wf.Name == "" {
		return fmt.Errorf("workflow name is required")
	}
	if len(wf.Stages) == 0 {
		return fmt.Errorf("workflow %q has no stages", wf.Name)
	}

	seen := make(map[string]bool, len(wf.Stages))
	for i, s := range wf.Stages {
		if s.Name == "" {
			return fmt.Errorf("stage %d: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate stage name %q", s.Name)
		}
		seen[s.Name] = true
		if s.Role == "" {
			return fmt.Errorf("stage %q: role is required", s.Name)
		}
		for _, dep := range s.InputFrom {
			if dep == s.Name {
				return fmt.Errorf("stage %q: self-reference in input_from", s.Name)
			}
			if !seen[dep] {
				// Check against all stage names, not just previously seen
				if !hasStage(wf.Stages, dep) {
					return fmt.Errorf("stage %q: input_from references unknown stage %q", s.Name, dep)
				}
			}
		}
	}
	return nil
}

func hasStage(stages []WorkflowStage, name string) bool {
	for _, s := range stages {
		if s.Name == name {
			return true
		}
	}
	return false
}
