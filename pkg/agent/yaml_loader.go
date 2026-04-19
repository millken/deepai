package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlAgentConfig represents the YAML file structure for agent definitions.
type yamlAgentConfig struct {
	Type             string   `yaml:"type"`
	Name             string   `yaml:"name"`
	Description      string   `yaml:"description"`
	SystemPrompt     string   `yaml:"system_prompt"`
	SystemPromptFile string   `yaml:"system_prompt_file"`
	DefaultTools     []string `yaml:"tools"`
	MaxTurns         int      `yaml:"max_turns"`
	Temperature      float64  `yaml:"temperature"`
}

// validateSafeName rejects names containing path separators or ".." to prevent path traversal.
func validateSafeName(name string) error {
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid name %q: must not contain \"..\" or path separators", name)
	}
	return nil
}

// loadAgentYAML loads an agent definition from .deepai/agents/{type}.yaml.
// Returns nil, nil if the file does not exist (not an error).
func loadAgentYAML(t AgentType, workDir string) (*AgentTypeConfig, error) {
	if workDir == "" {
		return nil, nil
	}
	if err := validateSafeName(string(t)); err != nil {
		return nil, err
	}
	path := filepath.Join(workDir, ".deepai", "agents", string(t)+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agent yaml %s: %w", path, err)
	}

	var yc yamlAgentConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return nil, fmt.Errorf("parse agent yaml %s: %w", path, err)
	}

	cfg := &AgentTypeConfig{
		Type:         AgentType(yc.Type),
		Name:         yc.Name,
		Description:  yc.Description,
		SystemPrompt: yc.SystemPrompt,
		DefaultTools: yc.DefaultTools,
		MaxTurns:     yc.MaxTurns,
		Temperature:  yc.Temperature,
	}

	if yc.SystemPromptFile != "" {
		agentsDir := filepath.Join(workDir, ".deepai", "agents")
		promptPath := filepath.Join(agentsDir, yc.SystemPromptFile)
		// Verify resolved path stays within agents directory
		absPath, err := filepath.Abs(promptPath)
		if err != nil {
			return nil, fmt.Errorf("resolve prompt file path: %w", err)
		}
		absDir, err := filepath.Abs(agentsDir)
		if err != nil {
			return nil, fmt.Errorf("resolve agents dir: %w", err)
		}
		rel, err := filepath.Rel(absDir, absPath)
		if err != nil {
			return nil, fmt.Errorf("prompt file escapes agents directory: %s", yc.SystemPromptFile)
		}
		if strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("prompt file escapes agents directory: %s", yc.SystemPromptFile)
		}
		promptData, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("read prompt file %s: %w", promptPath, err)
		}
		cfg.SystemPrompt = strings.TrimSpace(string(promptData))
	}

	if cfg.Type == "" {
		cfg.Type = t
	}

	return cfg, nil
}

// mergeConfig overlays YAML config onto the builtin base.
// Non-zero YAML fields override the base.
func mergeConfig(base AgentTypeConfig, override *AgentTypeConfig) AgentTypeConfig {
	if override == nil {
		return base
	}
	result := base

	if override.Type != "" {
		result.Type = override.Type
	}
	if override.Name != "" {
		result.Name = override.Name
	}
	if override.Description != "" {
		result.Description = override.Description
	}
	if strings.TrimSpace(override.SystemPrompt) != "" {
		result.SystemPrompt = override.SystemPrompt
	}
	if len(override.DefaultTools) > 0 {
		result.DefaultTools = append([]string(nil), override.DefaultTools...)
	}
	if override.MaxTurns > 0 {
		result.MaxTurns = override.MaxTurns
	}
	if override.Temperature > 0 {
		result.Temperature = override.Temperature
	}

	// No builtin tools and no YAML tools -> default minimal read-only set
	if len(result.DefaultTools) == 0 {
		result.DefaultTools = []string{"read_file", "list_dir", "glob", "grep", "find"}
	}

	return result
}

// resolveAgentTypeConfig is the unified agent type resolver.
// Priority: YAML file > builtin > fallback to general.
func resolveAgentTypeConfig(t AgentType, workDir string) AgentTypeConfig {
	if yamlCfg, err := loadAgentYAML(t, workDir); err == nil && yamlCfg != nil {
		base := BuiltinAgentTypes[t] // may be zero-value for pure custom agents
		return mergeConfig(base, yamlCfg)
	}
	if cfg, ok := BuiltinAgentTypes[t]; ok {
		return cfg
	}
	return BuiltinAgentTypes[AgentTypeGeneral]
}
