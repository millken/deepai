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
	AgentTypeArchitect        AgentType = "architect"
	AgentTypeBash             AgentType = "bash"
	AgentTypeFrontend         AgentType = "frontend"
	AgentTypeUIDesigner       AgentType = "ui-designer"
	AgentTypeNews             AgentType = "news"
)

type AgentTypeConfig struct {
	Type         AgentType     `json:"type" yaml:"type"`
	Name         string        `json:"name" yaml:"name"`
	Description  string        `json:"description" yaml:"description"`
	SystemPrompt string        `json:"system_prompt" yaml:"system_prompt"`
	DefaultTools []string      `json:"default_tools,omitempty" yaml:"default_tools,omitempty"`
	MaxTurns     int           `json:"max_turns" yaml:"max_turns"`
	Temperature  float64       `json:"temperature" yaml:"temperature"`
	Model        string        `json:"model,omitempty" yaml:"model,omitempty"`
	OutputSchema *OutputSchema `json:"-" yaml:"-"`

	// maxTurnsSet/temperatureSet mark MaxTurns/Temperature as an explicit
	// override even when the value is the zero value (0). Only the YAML
	// loader (yaml_loader.go) sets these, since only YAML can distinguish an
	// explicit `max_turns: 0` from the key being absent; mergeConfig reads
	// them to avoid treating an explicit 0 as "unset".
	maxTurnsSet    bool
	temperatureSet bool
}

const (
	// generalPurposeSystemPrompt is the default profile prompt for balanced assistant behavior.
	// T5c: the file-operation routing guidance lives in the single authoritative
	// rule appended by BuildSystemPrompt (react.go), so it is not duplicated here.
	generalPurposeSystemPrompt = "You are a helpful assistant. Work step by step, use tools when needed, ask for clarification with ask_clarification instead of guessing when requirements are ambiguous, and stop when you have a complete answer."
	// researcherSystemPrompt keeps the agent focused on gathering evidence and synthesizing findings.
	researcherSystemPrompt = "You are a research assistant. Prioritize gathering evidence, reading available material carefully, summarizing findings precisely, and asking for clarification with ask_clarification when the research scope is unclear."
	// coderSystemPrompt keeps the agent focused on code changes, debugging, and verification.
	coderSystemPrompt = "You are a coding assistant.\n\nIntent matching: if asked to review, analyze, or explain, provide findings without modifying files or running git commit/push; only edit files when explicitly asked to change, fix, implement, or refactor.\n\nBehavior rules: (1) Use tools to take action — do not describe what you would do without actually doing it. (2) Every response should either contain tool calls that make progress, or deliver a final result. (3) Do not add features, abstractions, comments, or error handling beyond what was asked. (4) Keep responses concise — go straight to the point. (5) Ask for clarification with ask_clarification before making risky assumptions. (6) When the task is complete, respond with a brief text summary — do NOT continue calling tools.\n\nGit workflow: use bash for git inspection and any manual git operations (status, diff, log, add, etc.) — do NOT commit or push automatically. Leave your changes uncommitted in the working tree so the user can review them. git_auto_commit is the only dedicated git tool; call it only when the user's request explicitly asks you to commit or push (e.g. it contains \"commit\", \"提交\", \"push\", or \"auto-push\"). When you do commit, stage only the files you changed, and commit only a complete logical unit of work — never partial progress. Set auto_push to true only if the request explicitly mentions pushing."
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
	// architectSystemPrompt focuses on system design, module decomposition, and interface definition.
	architectSystemPrompt = "You are a software architect. Focus on system design, module decomposition, interface definition, data flow, and technology selection. Produce clear technical design documents with concrete decisions and rationale. Ask for clarification with ask_clarification when requirements are ambiguous."
	// bashSystemPrompt is a minimal prompt for command execution.
	bashSystemPrompt = "You are a bash command executor. Run the requested commands and report results."
	// frontendSystemPrompt focuses on frontend web development.
	frontendSystemPrompt = "You are a frontend development expert. Focus on HTML, CSS, JavaScript, TypeScript, React, Vue, Angular, and other frontend frameworks. Prioritize responsive design, accessibility (a11y), cross-browser compatibility, performance optimization, and modern web standards. Write clean, maintainable code with proper component structure. Ask for clarification with ask_clarification when requirements are ambiguous."
	// uiDesignerSystemPrompt focuses on UI/UX design.
	uiDesignerSystemPrompt = "You are a UI/UX designer. Focus on user interface design, user experience optimization, design systems, color theory, typography, layout composition, and interaction patterns. Produce detailed design specifications, wireframe descriptions, component specifications, and style guides. Consider accessibility, responsive design, and platform conventions. Ask for clarification with ask_clarification when design requirements are unclear."
	// newsSystemPrompt focuses on news gathering and summarization.
	newsSystemPrompt = "You are a news research assistant. Focus on searching, gathering, and summarizing news from the web. Prioritize accuracy, recency, and source credibility. Present news in a structured format with headlines, summaries, sources, and timestamps. Cover multiple perspectives on controversial topics. Ask for clarification with ask_clarification when the news topic or scope is unclear."
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
		DefaultTools: []string{"read_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.1,
	},
	AgentTypeCoder: {
		Type:         AgentTypeCoder,
		Name:         "Coder",
		Description:  "Profile for code generation, debugging, and implementation tasks.",
		SystemPrompt: coderSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "skill", "git_auto_commit"},
		MaxTurns:     0,
		Temperature:  0.1,
	},
	AgentTypeAnalyst: {
		Type:         AgentTypeAnalyst,
		Name:         "Analyst",
		Description:  "Profile for structured analysis and artifact generation.",
		SystemPrompt: analystSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.15,
	},
	AgentTypeSecurityReviewer: {
		Type:         AgentTypeSecurityReviewer,
		Name:         "Security Reviewer",
		Description:  "Reviews code for security vulnerabilities, injection risks, and permission issues.",
		SystemPrompt: securityReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypeArchReviewer: {
		Type:         AgentTypeArchReviewer,
		Name:         "Architecture Reviewer",
		Description:  "Reviews code for design patterns, coupling, extensibility, and maintainability.",
		SystemPrompt: archReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypePerfReviewer: {
		Type:         AgentTypePerfReviewer,
		Name:         "Performance Reviewer",
		Description:  "Reviews code for algorithm complexity, memory, I/O, and concurrency issues.",
		SystemPrompt: perfReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "bash"},
		MaxTurns:     10,
		Temperature:  0.2,
	},
	AgentTypeProductManager: {
		Type:         AgentTypeProductManager,
		Name:         "Product Manager",
		Description:  "Plans features, decomposes requirements, and defines acceptance criteria.",
		SystemPrompt: productManagerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.15,
	},
	AgentTypeArchitect: {
		Type:         AgentTypeArchitect,
		Name:         "Architect",
		Description:  "Produces technical design documents, system decomposition, and interface definitions.",
		SystemPrompt: architectSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxTurns:     0,
		Temperature:  0.2,
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
	AgentTypeFrontend: {
		Type:         AgentTypeFrontend,
		Name:         "Frontend Developer",
		Description:  "Profile for frontend development: HTML/CSS/JS, React/Vue/Angular, responsive design, accessibility, and performance.",
		SystemPrompt: frontendSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxTurns:     0,
		Temperature:  0.15,
	},
	AgentTypeUIDesigner: {
		Type:         AgentTypeUIDesigner,
		Name:         "UI Designer",
		Description:  "Profile for UI/UX design: design systems, wireframes, component specs, color, typography, and interaction patterns.",
		SystemPrompt: uiDesignerSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxTurns:     0,
		Temperature:  0.2,
	},
	AgentTypeNews: {
		Type:         AgentTypeNews,
		Name:         "News Researcher",
		Description:  "Profile for news gathering and summarization: web search, source verification, and structured news reporting.",
		SystemPrompt: newsSystemPrompt,
		DefaultTools: []string{"web_search", "web_fetch", "web_fetch_batch", "read_file", "present_file", "ask_clarification"},
		MaxTurns:     0,
		Temperature:  0.1,
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
