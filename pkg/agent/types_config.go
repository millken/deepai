package agent

import (
	"fmt"
	"strings"
)

type AgentType string

const (
	AgentTypeGeneral  AgentType = "general-purpose"
	AgentTypeResearch AgentType = "researcher"
	AgentTypeCoder    AgentType = "coder"
	AgentTypeAnalyst  AgentType = "analyst"
)

type AgentTypeConfig struct {
	Type         AgentType `json:"type"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	SystemPrompt string    `json:"system_prompt"`
	DefaultTools []string  `json:"default_tools,omitempty"`
	MaxTurns     int       `json:"max_turns"`
	Temperature  float64   `json:"temperature"`
}

const (
	// generalPurposeSystemPrompt is the default profile prompt for balanced assistant behavior.
	generalPurposeSystemPrompt = "You are a helpful assistant. Work step by step, use tools when needed, ask for clarification with ask_clarification instead of guessing when requirements are ambiguous, and stop when you have a complete answer."
	// researcherSystemPrompt keeps the agent focused on gathering evidence and synthesizing findings.
	researcherSystemPrompt = "You are a research assistant. Prioritize gathering evidence, reading available material carefully, summarizing findings precisely, and asking for clarification with ask_clarification when the research scope is unclear."
	// coderSystemPrompt keeps the agent focused on code changes, debugging, and verification.
	coderSystemPrompt = "You are a coding assistant. Focus on understanding the codebase, making correct code changes, verifying them with available tools, and asking for clarification with ask_clarification before making risky assumptions."
	// analystSystemPrompt keeps the agent focused on structured analysis and communicating results clearly.
	analystSystemPrompt = "You are a data analyst. Inspect the available data carefully, explain conclusions clearly, generate artifacts when useful, and ask for clarification with ask_clarification when the analytical objective is underspecified."
)

var BuiltinAgentTypes = map[AgentType]AgentTypeConfig{
	AgentTypeGeneral: {
		Type:         AgentTypeGeneral,
		Name:         "General Purpose",
		Description:  "Balanced assistant profile for general tasks.",
		SystemPrompt: generalPurposeSystemPrompt,
		DefaultTools: nil,
		MaxTurns:     defaultMaxTurns,
		Temperature:  0.2,
	},
	AgentTypeResearch: {
		Type:         AgentTypeResearch,
		Name:         "Researcher",
		Description:  "Profile for research, reading, and synthesis tasks.",
		SystemPrompt: researcherSystemPrompt,
		DefaultTools: []string{"read_file", "glob", "present_file", "ask_clarification", "task"},
		MaxTurns:     10,
		Temperature:  0.1,
	},
	AgentTypeCoder: {
		Type:         AgentTypeCoder,
		Name:         "Coder",
		Description:  "Profile for code generation, debugging, and implementation tasks.",
		SystemPrompt: coderSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "glob", "present_file", "ask_clarification", "task"},
		MaxTurns:     12,
		Temperature:  0.1,
	},
	AgentTypeAnalyst: {
		Type:         AgentTypeAnalyst,
		Name:         "Analyst",
		Description:  "Profile for structured analysis and artifact generation.",
		SystemPrompt: analystSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "glob", "present_file", "ask_clarification"},
		MaxTurns:     10,
		Temperature:  0.15,
	},
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
	if _, ok := BuiltinAgentTypes[t]; !ok {
		return fmt.Errorf("unsupported agent type %q", t)
	}

	profile := GetAgentTypeConfig(t)
	cfg.AgentType = profile.Type
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = profile.MaxTurns
	}
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
