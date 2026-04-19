package agent

import (
	"fmt"
	"strings"
)

type AgentType string

const (
	AgentTypeGeneral          AgentType = "general-purpose"
	AgentTypeResearch         AgentType = "researcher"
	AgentTypeCoder            AgentType = "coder"
	AgentTypeAnalyst          AgentType = "analyst"
	AgentTypeSecurityReviewer AgentType = "security-reviewer"
	AgentTypeArchReviewer     AgentType = "arch-reviewer"
	AgentTypePerfReviewer     AgentType = "perf-reviewer"
	AgentTypeProductManager   AgentType = "product-manager"
	AgentTypeBash             AgentType = "bash"
)

type AgentTypeConfig struct {
	Type         AgentType     `json:"type" yaml:"type"`
	Name         string        `json:"name" yaml:"name"`
	Description  string        `json:"description" yaml:"description"`
	SystemPrompt string        `json:"system_prompt" yaml:"system_prompt"`
	DefaultTools []string      `json:"default_tools,omitempty" yaml:"default_tools,omitempty"`
	MaxTurns     int           `json:"max_turns" yaml:"max_turns"`
	Temperature  float64       `json:"temperature" yaml:"temperature"`
	OutputSchema *OutputSchema `json:"-" yaml:"-"`
}

const (
	// generalPurposeSystemPrompt is the default profile prompt for balanced assistant behavior.
	generalPurposeSystemPrompt = "You are a helpful assistant. Work step by step, use tools when needed, ask for clarification with ask_clarification instead of guessing when requirements are ambiguous, and stop when you have a complete answer."
	// researcherSystemPrompt keeps the agent focused on gathering evidence and synthesizing findings.
	researcherSystemPrompt = "You are a research assistant. Prioritize gathering evidence, reading available material carefully, summarizing findings precisely, and asking for clarification with ask_clarification when the research scope is unclear."
	// coderSystemPrompt keeps the agent focused on code changes, debugging, and verification.
	coderSystemPrompt = "You are a coding assistant.\n\nIntent matching: if asked to review, analyze, or explain, provide findings without modifying files or running git commit/push; only edit files when explicitly asked to change, fix, implement, or refactor.\n\nBehavior rules: (1) Use tools to take action — do not describe what you would do without actually doing it. (2) Every response should either contain tool calls that make progress, or deliver a final result. (3) Do not add features, abstractions, comments, or error handling beyond what was asked. (4) Keep responses concise — go straight to the point. (5) Ask for clarification with ask_clarification before making risky assumptions.\n\nGit workflow: after completing a feature implementation or bug fix (i.e., files have been modified and the task is done), automatically commit the changes using git_auto_commit with the modified files. Only stage the files you changed. Do NOT commit on partial progress — only when the logical unit of work is complete. Do NOT auto-commit when the user only asks to review, explain, or analyze — these are read-only tasks. If the user's request includes \"push\" or \"auto-push\", set auto_push to true."
	// analystSystemPrompt keeps the agent focused on structured analysis and communicating results clearly.
	analystSystemPrompt = "You are a data analyst. Inspect the available data carefully, explain conclusions clearly, generate artifacts when useful, and ask for clarification with ask_clarification when the analytical objective is underspecified."
	// securityReviewerSystemPrompt focuses on security vulnerabilities and risks.
	securityReviewerSystemPrompt = "You are an independent security code reviewer. You must make objective judgments based on the code you see.\n\nFocus on: injection vulnerabilities (SQL, command, XSS), authentication and authorization flaws, sensitive data exposure, insecure defaults, and cryptographic weaknesses.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// archReviewerSystemPrompt focuses on architectural design quality.
	archReviewerSystemPrompt = "You are an independent architecture reviewer. You must make objective judgments based on the code you see.\n\nFocus on: design patterns, coupling and cohesion, extensibility, maintainability, error handling patterns, and API design.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// perfReviewerSystemPrompt focuses on performance characteristics.
	perfReviewerSystemPrompt = "You are an independent performance reviewer. You must make objective judgments based on the code you see.\n\nFocus on: algorithm complexity, memory allocations, I/O patterns, concurrency bottlenecks, and resource leaks.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// productManagerSystemPrompt focuses on user needs and feature planning.
	productManagerSystemPrompt = "You are a product manager. Focus on user needs, feature decomposition, priority assessment, and acceptance criteria. Ask for clarification with ask_clarification when requirements are ambiguous."
	// bashSystemPrompt is a minimal prompt for command execution.
	bashSystemPrompt = "You are a bash command executor. Run the requested commands and report results."
)

var BuiltinAgentTypes = map[AgentType]AgentTypeConfig{
	AgentTypeGeneral: {
		Type:         AgentTypeGeneral,
		Name:         "General Purpose",
		Description:  "Balanced assistant profile for general tasks.",
		SystemPrompt: generalPurposeSystemPrompt,
		DefaultTools: nil,
		MaxTurns:     0,
		Temperature:  0.2,
	},
	AgentTypeResearch: {
		Type:         AgentTypeResearch,
		Name:         "Researcher",
		Description:  "Profile for research, reading, and synthesis tasks.",
		SystemPrompt: researcherSystemPrompt,
		DefaultTools: []string{"read_file", "list_dir", "glob", "grep", "find", "present_file", "ask_clarification", "task"},
		MaxTurns:     0,
		Temperature:  0.1,
	},
	AgentTypeCoder: {
		Type:         AgentTypeCoder,
		Name:         "Coder",
		Description:  "Profile for code generation, debugging, and implementation tasks.",
		SystemPrompt: coderSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "present_file", "ask_clarification", "task", "skill", "git_status", "git_diff", "git_log", "git_add", "git_commit", "git_reset", "git_auto_commit", "git_push"},
		MaxTurns:     0,
		Temperature:  0.1,
	},
	AgentTypeAnalyst: {
		Type:         AgentTypeAnalyst,
		Name:         "Analyst",
		Description:  "Profile for structured analysis and artifact generation.",
		SystemPrompt: analystSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "present_file", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.15,
	},
	AgentTypeSecurityReviewer: {
		Type:         AgentTypeSecurityReviewer,
		Name:         "Security Reviewer",
		Description:  "Reviews code for security vulnerabilities, injection risks, and permission issues.",
		SystemPrompt: securityReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypeArchReviewer: {
		Type:         AgentTypeArchReviewer,
		Name:         "Architecture Reviewer",
		Description:  "Reviews code for design patterns, coupling, extensibility, and maintainability.",
		SystemPrompt: archReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypePerfReviewer: {
		Type:         AgentTypePerfReviewer,
		Name:         "Performance Reviewer",
		Description:  "Reviews code for algorithm complexity, memory, I/O, and concurrency issues.",
		SystemPrompt: perfReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "bash"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypeProductManager: {
		Type:         AgentTypeProductManager,
		Name:         "Product Manager",
		Description:  "Plans features, decomposes requirements, and defines acceptance criteria.",
		SystemPrompt: productManagerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.15,
	},
	AgentTypeBash: {
		Type:         AgentTypeBash,
		Name:         "Bash Executor",
		Description:  "Execute bash commands and report results.",
		SystemPrompt: bashSystemPrompt,
		DefaultTools: []string{"bash"},
		MaxTurns:     3,
		Temperature:  0.0,
	},
}

func init() {
	reviewSchema := FromStruct[ReviewResult](WithStrict(true), WithMaxRetries(1))
	for _, at := range []AgentType{AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer} {
		if cfg, ok := BuiltinAgentTypes[at]; ok {
			cfg.OutputSchema = reviewSchema
			BuiltinAgentTypes[at] = cfg
		}
	}
}

func GetAgentTypeConfig(t AgentType) AgentTypeConfig {
	t = normalizeAgentType(t)
	if cfg, ok := BuiltinAgentTypes[t]; ok {
		return cfg
	}
	return BuiltinAgentTypes[AgentTypeGeneral]
}

func ApplyAgentType(cfg *AgentConfig, t AgentType) error {
	if cfg == nil {
		return fmt.Errorf("agent config is nil")
	}

	t = normalizeAgentType(t)
	if t == "" {
		t = AgentTypeGeneral
	}

	profile := resolveAgentTypeConfig(t, cfg.WorkDir)
	cfg.AgentType = profile.Type
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		cfg.SystemPrompt = profile.SystemPrompt
	}
	if cfg.Temperature == nil {
		temp := profile.Temperature
		cfg.Temperature = &temp
	}
	if cfg.Tools != nil && len(profile.DefaultTools) > 0 {
		cfg.Tools = cfg.Tools.Restrict(profile.DefaultTools)
	}
	return nil
}

func normalizeAgentType(t AgentType) AgentType {
	return AgentType(strings.TrimSpace(string(t)))
}
