package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pipeline defines an independent workflow: actor + parallel reviewers + retry.
type Pipeline struct {
	Name      string        `json:"name" yaml:"name"`
	Actor     ActorRef      `json:"actor" yaml:"actor"`
	Reviewers []ReviewerRef `json:"reviewers,omitempty" yaml:"reviewers,omitempty"`
	OnIssues  string        `json:"on_issues" yaml:"on_issues"`   // "retry" | "report"
	MaxRounds int           `json:"max_rounds" yaml:"max_rounds"` // 0 → defaults to 1
}

// ActorRef specifies the executing agent in a pipeline.
type ActorRef struct {
	AgentType AgentType `json:"agent_type" yaml:"agent_type"`
	Prompt    string    `json:"prompt" yaml:"prompt"` // template: {{.UserInput}}
}

// ReviewerRef specifies a reviewing agent in a pipeline.
type ReviewerRef struct {
	AgentType AgentType `json:"agent_type" yaml:"agent_type"`
	Name      string    `json:"name,omitempty" yaml:"name"` // unique key for same AgentType multi-instance
	Prompt    string    `json:"prompt" yaml:"prompt"`       // template: {{.diff}}, {{.output}}, {{.files}}
}

// ReviewerKey returns the unique identifier for this reviewer.
func (r ReviewerRef) ReviewerKey() string {
	if r.Name != "" {
		return r.Name
	}
	return string(r.AgentType)
}

// ReviewInput holds the data available to reviewer prompt templates.
type ReviewInput struct {
	Diff   string
	Output string
	Files  string
}

// Validate checks pipeline configuration for errors.
func (p *Pipeline) Validate() error {
	if p == nil {
		return fmt.Errorf("pipeline is nil")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("pipeline name is required")
	}
	if p.Actor.AgentType == "" {
		return fmt.Errorf("pipeline actor agent_type is required")
	}
	switch p.OnIssues {
	case "", "retry", "report":
	default:
		return fmt.Errorf("invalid on_issues value %q: must be \"retry\" or \"report\"", p.OnIssues)
	}
	for i, r := range p.Reviewers {
		if r.AgentType == "" {
			return fmt.Errorf("reviewer[%d] agent_type is required", i)
		}
	}
	return nil
}

// EffectiveMaxRounds returns MaxRounds, defaulting to 1 if <= 0.
func (p *Pipeline) EffectiveMaxRounds() int {
	if p.MaxRounds <= 0 {
		return 1
	}
	return p.MaxRounds
}

// expandTemplate replaces {{.Key}} placeholders in tmpl.
func expandTemplate(tmpl string, vars map[string]string) string {
	result := tmpl
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{{."+k+"}}", v)
	}
	return result
}

// aggregateVerdict returns "pass" only if all reviews pass.
func resolveOutputSchema(t AgentType, workDir string) *OutputSchema {
	cfg := resolveAgentTypeConfig(t, workDir)
	return cfg.OutputSchema
}

func aggregateVerdict(results map[string]ReviewResult) string {
	for _, r := range results {
		if r.Verdict != "pass" {
			return "issues_found"
		}
	}
	return "pass"
}

// hasCriticalIssues reports whether any review has a critical-severity issue.
func hasCriticalIssues(results map[string]ReviewResult) bool {
	for _, r := range results {
		for _, issue := range r.Issues {
			if issue.Severity == "critical" {
				return true
			}
		}
	}
	return false
}

// summarizeReviews formats all review results as a feedback text.
func summarizeReviews(reviews map[string]ReviewResult) string {
	if reviews == nil {
		return ""
	}
	var b strings.Builder
	for key, r := range reviews {
		fmt.Fprintf(&b, "\n[%s] %s\n", key, r.Summary)
		for _, issue := range r.Issues {
			switch issue.Severity {
			case "critical":
				fmt.Fprintf(&b, "  - CRITICAL: %s (%s:%d) -> %s\n", issue.Message, issue.File, issue.Line, issue.Suggestion)
			case "warning":
				fmt.Fprintf(&b, "  - WARNING: %s (%s:%d) -> %s\n", issue.Message, issue.File, issue.Line, issue.Suggestion)
			default:
				fmt.Fprintf(&b, "  - %s: %s -> %s\n", issue.Severity, issue.Message, issue.Suggestion)
			}
		}
	}
	return b.String()
}

// BuiltinPipelines defines the built-in pipeline configurations.
var BuiltinPipelines = map[string]Pipeline{
	"code-with-review": {
		Name: "code-with-review",
		Actor: ActorRef{
			AgentType: AgentTypeCoder,
			Prompt:    "{{.UserInput}}",
		},
		Reviewers: []ReviewerRef{
			{AgentType: AgentTypeSecurityReviewer, Prompt: "Review the following code changes for security issues:\n{{.diff}}"},
			{AgentType: AgentTypeArchReviewer, Prompt: "Review the following code changes for architecture design:\n{{.diff}}"},
			{AgentType: AgentTypePerfReviewer, Prompt: "Review the following code changes for performance impact:\n{{.diff}}"},
		},
		OnIssues:  "retry",
		MaxRounds: 3,
	},
	"code-quick": {
		Name: "code-quick",
		Actor: ActorRef{
			AgentType: AgentTypeCoder,
			Prompt:    "{{.UserInput}}",
		},
		Reviewers: nil,
		OnIssues:  "report",
		MaxRounds: 1,
	},
}

// ResolvePipeline loads a pipeline by name: YAML > builtin.
func ResolvePipeline(name, workDir string) (*Pipeline, error) {
	if workDir != "" {
		if p, err := loadPipelineYAML(name, workDir); err != nil {
			return nil, fmt.Errorf("load pipeline yaml: %w", err)
		} else if p != nil {
			return p, nil
		}
	}
	if p, ok := BuiltinPipelines[name]; ok {
		return &p, nil
	}
	return nil, fmt.Errorf("pipeline %q not found", name)
}

// ListPipelines returns all available pipeline names (builtin + YAML).
func ListPipelines(workDir string) []string {
	names := make(map[string]bool)
	for name := range BuiltinPipelines {
		names[name] = true
	}
	if workDir != "" {
		dir := filepath.Join(workDir, ".deepai", "pipelines")
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
					names[strings.TrimSuffix(entry.Name(), ".yaml")] = true
				}
			}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	return result
}

func loadPipelineYAML(name, workDir string) (*Pipeline, error) {
	if err := validateSafeName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(workDir, ".deepai", "pipelines", name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pipeline yaml %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
