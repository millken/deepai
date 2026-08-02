package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// claudeToolAliases maps Claude Code tool names (used in plugin agent
// frontmatter) to deepai tool names, so an agent's "tools" list selects real
// tools. Unmatched names pass through unchanged (allows already-deepai names).
var claudeToolAliases = map[string]string{
	"Read":  "read_file",
	"Edit":  "edit_file",
	"Write": "write_file",
	"Bash":  "bash",
	"Grep":  "grep",
	"Glob":  "glob",
	"List":  "list_dir",
	"Task":  "task",
}

type agentMDFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Tools       string `yaml:"tools"`
	Model       string `yaml:"model"`
}

// ParseAgentMarkdown parses a Claude-style agent definition: a markdown file
// with YAML frontmatter (name/description/tools/model) and a body that becomes
// the system prompt. A missing/empty name falls back to the filename stem with
// a slog.Warn (compatibility, not rejection). model is ignored (deepai has no
// per-type model).
func ParseAgentMarkdown(path string) (*AgentTypeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent md %s: %w", path, err)
	}
	fm, body, err := splitAgentFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("parse agent md %s: %w", path, err)
	}
	var af agentMDFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &af); err != nil {
		return nil, fmt.Errorf("parse agent md frontmatter %s: %w", path, err)
	}

	name := strings.TrimSpace(af.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
		slog.Warn("agent md missing \"name\"; using filename stem", "path", path, "fallback", name)
	}

	cfg := &AgentTypeConfig{
		Type:         AgentType(name),
		Name:         name,
		Description:  strings.TrimSpace(af.Description),
		SystemPrompt: strings.TrimSpace(body),
		DefaultTools: mapClaudeTools(af.Tools),
	}
	if strings.TrimSpace(af.Model) != "" {
		slog.Debug("agent md model field ignored", "path", path, "model", af.Model)
	}
	return cfg, nil
}

func mapClaudeTools(csv string) []string {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if alias, ok := claudeToolAliases[t]; ok {
			out = append(out, alias)
		} else {
			out = append(out, t)
		}
	}
	return out
}

func splitAgentFrontmatter(data []byte) (fm, body string, err error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return "", strings.TrimSpace(s), nil
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", fmt.Errorf("missing closing frontmatter delimiter")
	}
	fm = rest[:idx]
	body = strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	return fm, strings.TrimSpace(body), nil
}

// AgentInfo is an advertised agent type for the task tool description.
type AgentInfo struct {
	Type        AgentType
	Description string
}

// EnumerateAgents lists all resolvable agent types with the description the
// model would actually get if it invoked them. Load problems (invalid project
// YAML/MD, invalid plugin MD) are discarded; use EnumerateAgentsReported to
// see them.
func EnumerateAgents(workDir string, pluginAgentDirs []string) []AgentInfo {
	agents, _ := EnumerateAgentsReported(workDir, pluginAgentDirs)
	return agents
}

// EnumerateAgentsReported is like EnumerateAgents but also returns
// human-readable load problems (file + error) encountered while resolving
// each candidate type — one per failing source, in priority order. It
// collects candidate type stems (project yaml/md, then plugin md in
// pluginAgentDirs order, then builtins), then resolves each via
// resolveAgentTypeConfigWithPluginsReported — the SAME resolver execution
// uses — so the advertised description is always the one execution will
// apply (e.g. a project foo.yaml wins over a coexisting foo.md for both). A
// source that fails to parse does not stop resolution: it falls through to
// the next source in priority order (project MD, then plugin MD, then
// builtin/general), same as before this warning surfaced. pluginAgentDirs
// MUST be the claudeplugin.Discover result order.
func EnumerateAgentsReported(workDir string, pluginAgentDirs []string) ([]AgentInfo, []string) {
	seen := make(map[AgentType]bool)
	var stems []AgentType
	add := func(t AgentType) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		stems = append(stems, t)
	}

	collectStems(filepath.Join(workDir, ".deepai", "agents"), true, add)
	for _, dir := range pluginAgentDirs {
		collectStems(dir, false, add)
	}
	var bs []AgentType
	for t := range BuiltinAgentTypes {
		bs = append(bs, t)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	for _, t := range bs {
		add(t)
	}

	out := make([]AgentInfo, 0, len(stems))
	var warnings []string
	for _, t := range stems {
		cfg, problems := resolveAgentTypeConfigWithPluginsReported(t, workDir, pluginAgentDirs)
		warnings = append(warnings, problems...)
		warnings = append(warnings, checkShadowedSources(t, workDir, pluginAgentDirs)...)
		out = append(out, AgentInfo{Type: t, Description: cfg.Description})
	}
	return out, warnings
}

// shadowedSource is one file-backed candidate in agent-type resolution
// priority order (project yaml > project md > plugin md, in pluginAgentDirs
// order), captured with its parse outcome for checkShadowedSources.
type shadowedSource struct {
	path string
	cfg  *AgentTypeConfig
	err  error
}

// checkShadowedSources re-attempts to parse every lower-priority source for
// a type that already resolved from a higher-priority one, purely to surface
// a warning when a file a human placed on disk is silently shadowed AND
// broken (M1-final Minor #6). resolveAgentTypeConfigWithPluginsReported
// (the execution-path resolver, also used for enumeration's description)
// intentionally never reaches these — first source that parses wins, no
// further I/O — so a broken foo.md sitting next to a working foo.yaml would
// otherwise never surface anywhere: not an error (yaml still resolves fine)
// and not a warning (the resolver stopped before ever looking at the md).
// Called ONLY from the Reported enumeration path (EnumerateAgentsReported);
// execution-path resolution stays untouched (still first-win, no extra I/O).
func checkShadowedSources(t AgentType, workDir string, pluginAgentDirs []string) []string {
	if err := validateSafeName(string(t)); err != nil {
		return nil
	}

	var candidates []shadowedSource
	if workDir != "" {
		yamlPath := filepath.Join(workDir, ".deepai", "agents", string(t)+".yaml")
		cfg, err := loadAgentYAML(t, workDir)
		candidates = append(candidates, shadowedSource{path: yamlPath, cfg: cfg, err: err})

		mdPath := filepath.Join(workDir, ".deepai", "agents", string(t)+".md")
		cfg, err = loadAgentMDFileReported(mdPath)
		candidates = append(candidates, shadowedSource{path: mdPath, cfg: cfg, err: err})
	}
	for _, dir := range pluginAgentDirs {
		pluginPath := filepath.Join(dir, string(t)+".md")
		cfg, err := loadAgentMDFileReported(pluginPath)
		candidates = append(candidates, shadowedSource{path: pluginPath, cfg: cfg, err: err})
	}

	// The winner is the first candidate (in priority order) that parsed
	// successfully — the same source resolveAgentTypeConfigWithPluginsReported
	// would have returned. Any candidate BEFORE the winner that failed is
	// already captured in that resolver's own `problems` (it walks the same
	// order and only stops once it finds a winner), so only candidates AFTER
	// the winner are new information here.
	winner := -1
	for i, c := range candidates {
		if c.err == nil && c.cfg != nil {
			winner = i
			break
		}
	}
	if winner < 0 {
		return nil
	}
	var warnings []string
	for i := winner + 1; i < len(candidates); i++ {
		if candidates[i].err != nil {
			warnings = append(warnings, fmt.Sprintf("shadowed by %s and failed to parse: %v",
				filepath.Base(candidates[winner].path), candidates[i].err))
		}
	}
	return warnings
}

// collectStems adds filename stems (without extension) found in dir. yaml stems
// are collected before md so that, for a project dir with both foo.yaml and
// foo.md, the yaml is the canonical stem (resolution already prefers yaml, so
// this is cosmetic for ordering but keeps the stem list tidy).
func collectStems(dir string, includeYAML bool, add func(AgentType)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if includeYAML {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
				add(AgentType(strings.TrimSuffix(e.Name(), ".yaml")))
			}
		}
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			add(AgentType(strings.TrimSuffix(e.Name(), ".md")))
		}
	}
}
