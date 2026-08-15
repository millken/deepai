package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlAgentConfig represents the YAML file structure for agent definitions.
// MaxToolCalls and Temperature are pointers so an explicit zero
// (`max_tool_calls: 0`, `temperature: 0`) is distinguishable from the key
// being absent — see loadAgentYAML's use of maxToolCallsSet/temperatureSet on
// the resulting AgentTypeConfig. LegacyMaxTurns reads the deprecated
// `max_turns` key so pre-rename configs keep working; max_tool_calls wins
// when both are present.
type yamlAgentConfig struct {
	Type             string   `yaml:"type"`
	Name             string   `yaml:"name"`
	Description      string   `yaml:"description"`
	SystemPrompt     string   `yaml:"system_prompt"`
	SystemPromptFile string   `yaml:"system_prompt_file"`
	DefaultTools     []string `yaml:"tools"`
	MaxToolCalls     *int     `yaml:"max_tool_calls"`
	LegacyMaxTurns   *int     `yaml:"max_turns"`
	Temperature      *float64 `yaml:"temperature"`
	Model            string   `yaml:"model"`
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
		Model:        yc.Model,
	}
	if yc.MaxToolCalls != nil {
		cfg.MaxToolCalls = *yc.MaxToolCalls
		cfg.maxToolCallsSet = true
	} else if yc.LegacyMaxTurns != nil {
		cfg.MaxToolCalls = *yc.LegacyMaxTurns
		cfg.maxToolCallsSet = true
	}
	if yc.Temperature != nil {
		cfg.Temperature = *yc.Temperature
		cfg.temperatureSet = true
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

// mergeConfig overlays YAML/MD config onto base — the resolved builtin
// profile for the type, or the zero AgentTypeConfig when the type has no
// builtin entry (a custom/unknown type). baseIsBuiltin must be an explicit
// signal from the caller (a BuiltinAgentTypes lookup, e.g. `_, ok :=
// BuiltinAgentTypes[t]`) — never inferred from base's field emptiness —
// because a real builtin's nil DefaultTools legitimately means
// "unrestricted" (e.g. general-purpose), while a custom type's absent
// DefaultTools should fall back to a conservative read-only set instead.
func mergeConfig(base AgentTypeConfig, override *AgentTypeConfig, baseIsBuiltin bool) AgentTypeConfig {
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
	// override.MaxToolCalls/Temperature > 0 catches a positive explicit value;
	// maxToolCallsSet/temperatureSet additionally catches an explicit zero,
	// which is otherwise indistinguishable from "absent" and would leave the
	// base's value stuck. The flags are set on result too (not just checked on
	// override) so a further merge layer that reuses this result as ITS
	// override still sees the value as explicit, instead of resurrecting a
	// stale base value.
	if override.MaxToolCalls > 0 || override.maxToolCallsSet {
		result.MaxToolCalls = override.MaxToolCalls
		result.maxToolCallsSet = true
	}
	if override.Temperature > 0 || override.temperatureSet {
		result.Temperature = override.Temperature
		result.temperatureSet = true
	}
	if strings.TrimSpace(override.Model) != "" {
		result.Model = override.Model
	}

	// A real builtin's nil/absent DefaultTools means "unrestricted" and must
	// be respected as-is (ApplyAgentType skips Restrict when empty; the
	// subagent executor's empty-selectors path keeps all tools). A
	// custom/unknown type has no builtin profile to default to unrestricted —
	// an absent tools: key there falls back to a conservative read-only set.
	if !baseIsBuiltin && len(result.DefaultTools) == 0 {
		result.DefaultTools = []string{"read_file", "list_dir", "glob", "grep", "find"}
	}
	return result
}

// resolveAgentTypeConfig is the unified agent type resolver for the main agent
// (no plugin agents). Priority: project YAML/MD > builtin > fallback to general.
func resolveAgentTypeConfig(t AgentType, workDir string) AgentTypeConfig {
	return resolveAgentTypeConfigWithPlugins(t, workDir, nil)
}

// resolveAgentTypeConfigWithPlugins resolves an agent type for subagent
// execution, including plugin-bundled agents. Priority: project YAML > project
// MD > plugin MD (in pluginAgentDirs order, MUST be the same slice
// EnumerateAgents uses) > builtin > general. pluginAgentDirs elements are agent
// directories (<plugin>/agents). Per-source parse errors are skipped silently
// here — use resolveAgentTypeConfigWithPluginsReported (EnumerateAgentsReported
// wires it up) to observe them.
func resolveAgentTypeConfigWithPlugins(t AgentType, workDir string, pluginAgentDirs []string) AgentTypeConfig {
	cfg, _ := resolveAgentTypeConfigWithPluginsReported(t, workDir, pluginAgentDirs)
	return cfg
}

// resolveAgentTypeConfigWithPluginsReported is resolveAgentTypeConfigWithPlugins
// plus the human-readable problems (file + error) hit while trying each
// candidate source. A source that fails to parse does not abort resolution:
// it is recorded and resolution falls through to the next source in priority
// order, ending at builtin/general — execution-path behavior is identical to
// resolveAgentTypeConfigWithPlugins, only the reporting differs.
func resolveAgentTypeConfigWithPluginsReported(t AgentType, workDir string, pluginAgentDirs []string) (AgentTypeConfig, []string) {
	cfg, problems, _ := resolveAgentTypeConfigResolved(t, workDir, pluginAgentDirs)
	return cfg, problems
}

// resolveAgentTypeConfigResolved is resolveAgentTypeConfigWithPluginsReported
// plus a third value reporting whether t was actually BACKED by a source: a
// project YAML/MD, a plugin MD, or a builtin profile. false means the returned
// config is the general-purpose fallback for a type nothing defines — a
// hallucinated or typo'd agent_type, or a name rejected by validateSafeName.
//
// The distinction exists because the two consumers want opposite behavior:
// enumeration and the main agent's ApplyAgentType want the lenient fallback,
// while the subagent executor must REJECT an unbacked type. Silently running it
// as general-purpose used to hand the subagent an unrestricted tool set (more
// privilege than an explicit general-purpose), the exact widening
// selectSubagentTools refuses for an unmatched tools selector.
func resolveAgentTypeConfigResolved(t AgentType, workDir string, pluginAgentDirs []string) (AgentTypeConfig, []string, bool) {
	var problems []string
	// Explicit builtin-ness check, passed to every mergeConfig call below —
	// mergeConfig must never infer this from base's field emptiness.
	_, isBuiltin := BuiltinAgentTypes[t]
	if err := validateSafeName(string(t)); err != nil {
		// Rejected stem (e.g. contains ".." or a path separator): resolution
		// still falls through to builtin/general below (safe), but the
		// rejection must not be silently swallowed — record it so it surfaces
		// as a startup warning identifying the offending stem.
		problems = append(problems, fmt.Sprintf("%s: %v", string(t), err))
	} else {
		// loadAgentYAML/loadAgentMDFileReported errors USUALLY already embed
		// the full path they came from ("parse agent yaml <path>: ...",
		// "parse agent md <path>: ..."), so appending err.Error() directly
		// avoids prepending the SAME path a second time (M1-final Minor #5).
		// The one exception: loadAgentYAML's system_prompt_file error family
		// ("prompt file escapes agents directory", "resolve prompt file
		// path", "resolve agents dir", "read prompt file") names the prompt
		// file, not the yaml that declared it — so for THOSE errors the
		// declaring yaml's own path would otherwise vanish from the warning
		// entirely (security-relevant: an operator needs to know which file
		// to fix). Only prefix when the path isn't already present.
		yamlPath := filepath.Join(workDir, ".deepai", "agents", string(t)+".yaml")
		if cfg, err := loadAgentYAML(t, workDir); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, yamlPath) {
				msg = fmt.Sprintf("%s: %s", yamlPath, msg)
			}
			problems = append(problems, msg)
		} else if cfg != nil {
			return mergeConfig(BuiltinAgentTypes[t], cfg, isBuiltin), problems, true
		}

		mdPath := filepath.Join(workDir, ".deepai", "agents", string(t)+".md")
		if cfg, err := loadAgentMDFileReported(mdPath); err != nil {
			problems = append(problems, err.Error())
		} else if cfg != nil {
			return mergeConfig(BuiltinAgentTypes[t], cfg, isBuiltin), problems, true
		}

		for _, dir := range pluginAgentDirs {
			pluginPath := filepath.Join(dir, string(t)+".md")
			if cfg, err := loadAgentMDFileReported(pluginPath); err != nil {
				problems = append(problems, err.Error())
			} else if cfg != nil {
				return mergeConfig(BuiltinAgentTypes[t], cfg, isBuiltin), problems, true
			}
		}
	}
	if cfg, ok := BuiltinAgentTypes[t]; ok {
		return cfg, problems, true
	}
	return BuiltinAgentTypes[AgentTypeGeneral], problems, false
}

// loadAgentMDFile parses an agent .md if it exists; returns nil for missing or
// unparseable. Use loadAgentMDFileReported to distinguish "missing" from
// "unparseable".
func loadAgentMDFile(path string) *AgentTypeConfig {
	cfg, _ := loadAgentMDFileReported(path)
	return cfg
}

// loadAgentMDFileReported parses an agent .md if it exists. Returns (nil, nil)
// when the file does not exist (not an error — a source simply isn't present);
// returns (nil, err) when the file exists but fails to parse.
func loadAgentMDFileReported(path string) (*AgentTypeConfig, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	cfg, err := ParseAgentMarkdown(path)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
