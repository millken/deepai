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
// model would actually get if it invoked them. It collects candidate type stems
// (project yaml/md, then plugin md in pluginAgentDirs order, then builtins),
// then resolves each via resolveAgentTypeConfigWithPlugins — the SAME resolver
// execution uses — so the advertised description is always the one execution
// will apply (e.g. a project foo.yaml wins over a coexisting foo.md for both).
// pluginAgentDirs MUST be the claudeplugin.Discover result order.
func EnumerateAgents(workDir string, pluginAgentDirs []string) []AgentInfo {
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
	for _, t := range stems {
		cfg := resolveAgentTypeConfigWithPlugins(t, workDir, pluginAgentDirs)
		out = append(out, AgentInfo{Type: t, Description: cfg.Description})
	}
	return out
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
