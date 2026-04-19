package workflow

import "github.com/millken/deepai/pkg/agent"

// BuiltinWorkflows contains predefined workflow definitions.
var BuiltinWorkflows = map[string]Workflow{
	"code-with-review": {
		Name:        "code-with-review",
		Description: "Implement code with parallel security and architecture review, auto-fix on critical issues",
		Stages: []WorkflowStage{
			{
				Name:   "implement",
				Role:   agent.AgentTypeCoder,
				Prompt: "{{.UserInput}}",
			},
			{
				Name:      "security",
				Role:      agent.AgentTypeSecurityReviewer,
				InputFrom: []string{"implement"},
				Prompt:    "Review the following code changes for security issues:\n{{.outputs.implement}}",
			},
			{
				Name:      "arch",
				Role:      agent.AgentTypeArchReviewer,
				InputFrom: []string{"implement"},
				Prompt:    "Review the following code changes for architecture design:\n{{.outputs.implement}}",
			},
			{
				Name:       "fix",
				Role:       agent.AgentTypeCoder,
				InputFrom:  []string{"implement", "security", "arch"},
				Condition:  "has_critical_issues",
				MaxRetries: 3,
				Prompt:     "Fix the issues found by reviewers.\n\nOriginal implementation:\n{{.outputs.implement}}\n\nSecurity review:\n{{.outputs.security}}\n\nArchitecture review:\n{{.outputs.arch}}",
			},
		},
	},
	"feature-planning": {
		Name:        "feature-planning",
		Description: "Product requirement -> architecture design -> implementation -> review -> fix",
		Stages: []WorkflowStage{
			{
				Name:   "prd",
				Role:   agent.AgentTypeProductManager,
				Prompt: "{{.UserInput}}",
			},
			{
				Name:      "design",
				Role:      agent.AgentTypeArchitect,
				InputFrom: []string{"prd"},
				Prompt:    "Based on the following product requirements, produce a technical design document:\n{{.outputs.prd}}",
			},
			{
				Name:      "implement",
				Role:      agent.AgentTypeCoder,
				InputFrom: []string{"prd", "design"},
				Prompt:    "Implement the feature based on these requirements and design:\n\nRequirements:\n{{.outputs.prd}}\n\nDesign:\n{{.outputs.design}}",
			},
			{
				Name:      "review",
				Role:      agent.AgentTypeSecurityReviewer,
				InputFrom: []string{"implement"},
				Prompt:    "Review the following implementation for issues:\n{{.outputs.implement}}",
			},
			{
				Name:       "fix",
				Role:       agent.AgentTypeCoder,
				InputFrom:  []string{"implement", "review"},
				Condition:  "has_critical_issues",
				MaxRetries: 2,
				Prompt:     "Fix the issues found by the reviewer.\n\nImplementation:\n{{.outputs.implement}}\n\nReview:\n{{.outputs.review}}",
			},
		},
	},
}
