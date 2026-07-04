// Package claudeplugin discovers Claude Code plugin bundles (directories with a
// .claude-plugin/plugin.json manifest) and exposes their components for the
// chat runtime to load. It owns ONLY discovery + parsing + ${CLAUDE_PLUGIN_ROOT}
// expansion — it does not call the skill or MCP loaders itself (no side effects),
// so the chat layer remains the single aggregation point.
package claudeplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/mcp"
)

// Manifest is the subset of .claude-plugin/plugin.json that this package uses.
type Manifest struct {
	Name        string          `json:"name"` // required, kebab-case
	Version     string          `json:"version,omitempty"`
	Description string          `json:"description,omitempty"`
	MCPServers  json.RawMessage `json:"mcpServers,omitempty"` // object | string path
}

// Plugin is a discovered plugin directory with a parsed manifest.
type Plugin struct {
	Dir  string
	Name string
	man  Manifest
}

// SkillRoot returns the plugin root directory. Callers pass it to
// skill.Registry.LoadAllReported's pluginDirs (which appends "/skills" itself,
// so this must NOT already include /skills).
func (p *Plugin) SkillRoot() string { return p.Dir }

// pluginRoots returns the discovery roots in priority order (global first,
// project overrides).
func pluginRoots(workdir string) []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".deepai", "plugins"))
	}
	if strings.TrimSpace(workdir) != "" {
		roots = append(roots, filepath.Join(workdir, ".deepai", "plugins"))
	}
	return roots
}

// Discover scans the plugin roots. Each immediate subdirectory with a valid
// .claude-plugin/plugin.json is a plugin; a project root overrides a global one
// for the same plugin name. problems holds human-readable per-directory issues
// (unreadable/invalid manifest, missing name) for the caller to surface.
// Directories without a manifest are silently skipped (they are not plugins).
func Discover(workdir string) (plugins []Plugin, problems []string) {
	byName := make(map[string]Plugin)
	for _, root := range pluginRoots(workdir) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // root missing/unreadable → skip silently
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p, problem := loadPlugin(filepath.Join(root, e.Name()))
			if problem != "" {
				problems = append(problems, fmt.Sprintf("%s: %s", e.Name(), problem))
				continue
			}
			if p.Name == "" {
				continue // no manifest → not a plugin, silent
			}
			byName[p.Name] = p // project root scanned later → overrides global
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	plugins = make([]Plugin, 0, len(names))
	for _, n := range names {
		plugins = append(plugins, byName[n])
	}
	return plugins, problems
}

// loadPlugin reads and validates one candidate plugin directory. Returns a
// non-empty problem when the manifest exists but is unreadable/invalid/unnamed;
// returns an empty Plugin AND empty problem when no manifest exists (not a plugin).
func loadPlugin(dir string) (Plugin, string) {
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Plugin{}, ""
		}
		return Plugin{}, "read plugin.json: " + err.Error()
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Plugin{}, "parse plugin.json: " + err.Error()
	}
	if strings.TrimSpace(m.Name) == "" {
		return Plugin{}, "plugin.json missing \"name\""
	}
	return Plugin{Dir: dir, Name: m.Name, man: m}, ""
}

// mcpServerEntry mirrors the per-server object in .mcp.json / plugin.json.
type mcpServerEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (e mcpServerEntry) toConfig() mcp.ServerConfig {
	return mcp.ServerConfig{
		Type:    e.Type,
		Command: e.Command,
		Args:    e.Args,
		Env:     e.Env,
		URL:     e.URL,
		Headers: e.Headers,
	}
}

// entriesToConfigs converts a name→entry map into name→ServerConfig.
func entriesToConfigs(entries map[string]mcpServerEntry) map[string]mcp.ServerConfig {
	out := make(map[string]mcp.ServerConfig, len(entries))
	for n, e := range entries {
		out[n] = e.toConfig()
	}
	return out
}

// bareEntries interprets a top-level JSON object (with no "mcpServers" key) as a
// bare server map: each value must unmarshal as an mcpServerEntry.
func bareEntries(top map[string]json.RawMessage) (map[string]mcpServerEntry, error) {
	out := make(map[string]mcpServerEntry, len(top))
	for k, raw := range top {
		var e mcpServerEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("server %q: %w", k, err)
		}
		out[k] = e
	}
	return out, nil
}

// MCPServers resolves a plugin's MCP servers from the official sources (default
// <plugin>/.mcp.json, manifest inline object, manifest string path) and expands
// ${CLAUDE_PLUGIN_ROOT} to the plugin directory. ${VAR} env expansion is left
// to the MCP loader. Returns (nil, "") when the plugin has no MCP. A non-empty
// problem means parsing hit an issue (the plugin's MCP is skipped, not silently
// dropped) so the caller can surface it.
func (p *Plugin) MCPServers() (map[string]mcp.ServerConfig, string) {
	servers := make(map[string]mcp.ServerConfig)
	var problem string

	// 1. default <plugin>/.mcp.json
	if s, prob := readMCPFile(filepath.Join(p.Dir, ".mcp.json")); prob != "" {
		problem = appendMsg(problem, prob)
	} else {
		for n, sc := range s {
			servers[n] = sc
		}
	}

	// 2. manifest mcpServers field (object | string path); array is not in the
	//    official mcpServers spec → treat as unsupported, do not silently drop.
	if len(p.man.MCPServers) > 0 {
		trimmed := strings.TrimSpace(string(p.man.MCPServers))
		switch {
		case trimmed == "" || trimmed == "null":
			// nothing
		case trimmed[0] == '{':
			var obj map[string]mcpServerEntry
			if err := json.Unmarshal(p.man.MCPServers, &obj); err != nil {
				problem = appendMsg(problem, "mcpServers inline: "+err.Error())
			} else {
				for n, e := range obj {
					servers[n] = e.toConfig()
				}
			}
		case trimmed[0] == '[':
			problem = appendMsg(problem, "mcpServers array shape unsupported (expected object or path string)")
		default:
			var pathStr string
			if err := json.Unmarshal(p.man.MCPServers, &pathStr); err != nil {
				problem = appendMsg(problem, "mcpServers path: "+err.Error())
				break
			}
			full, ok := safePluginPath(p.Dir, pathStr)
			if !ok {
				problem = appendMsg(problem, "mcpServers path escapes plugin dir: "+pathStr)
				break
			}
			s, prob := readMCPFile(full)
			if prob != "" {
				problem = appendMsg(problem, prob)
			} else {
				for n, sc := range s {
					servers[n] = sc
				}
			}
		}
	}

	if problem != "" {
		return nil, problem
	}
	for n, sc := range servers {
		servers[n] = expandPluginRoot(sc, p.Dir)
	}
	if len(servers) == 0 {
		return nil, ""
	}
	return servers, ""
}

// readMCPFile parses an MCP config file into configs. It dispatches on the
// top-level shape: {"mcpServers": {...}} (wrapped, standard .mcp.json) or a
// bare {"name": {...}} server map. A genuinely empty file ({}) → no servers,
// no problem; a malformed file (junk that yields no servers under either
// shape) → a parse problem, so it is not silently treated as "no MCP".
// Missing file → nil,"".
func readMCPFile(path string) (map[string]mcp.ServerConfig, string) {
	base := filepath.Base(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, "read " + base + ": " + err.Error()
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, "parse " + base + ": " + err.Error()
	}
	if raw, ok := top["mcpServers"]; ok {
		// Wrapped shape. An empty value ({}) is a legitimate "no servers".
		var entries map[string]mcpServerEntry
		if len(raw) == 0 || string(raw) == "null" {
			return nil, ""
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, "parse " + base + " mcpServers: " + err.Error()
		}
		return entriesToConfigs(entries), ""
	}
	// Bare server map. An empty object ({}) is legitimate; anything that fails
	// to parse as name→entry is a problem.
	entries, err := bareEntries(top)
	if err != nil {
		return nil, "parse " + base + ": " + err.Error()
	}
	return entriesToConfigs(entries), ""
}

func appendMsg(existing, msg string) string {
	if existing == "" {
		return msg
	}
	return existing + "; " + msg
}

// safePluginPath joins rel to base and ensures the result stays within base,
// rejecting ../ escapes. Mirrors the agent prompt-file containment check
// (pkg/agent/yaml_loader.go) so a plugin's mcpServers path can't read outside
// the plugin directory.
func safePluginPath(base, rel string) (string, bool) {
	rel = strings.TrimPrefix(rel, "./")
	full := filepath.Join(base, rel)
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", false
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", false
	}
	r, err := filepath.Rel(absBase, absFull)
	if err != nil || strings.HasPrefix(r, "..") {
		return "", false
	}
	return full, true
}

// expandPluginRoot substitutes ${CLAUDE_PLUGIN_ROOT} with dir on every string
// field. Done before the MCP loader's ${VAR} expansion so the two never collide.
func expandPluginRoot(sc mcp.ServerConfig, dir string) mcp.ServerConfig {
	sub := func(s string) string { return strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", dir) }
	sc.Command = sub(sc.Command)
	sc.URL = sub(sc.URL)
	for i, a := range sc.Args {
		sc.Args[i] = sub(a)
	}
	for k, v := range sc.Env {
		sc.Env[k] = sub(v)
	}
	for k, v := range sc.Headers {
		sc.Headers[k] = sub(v)
	}
	return sc
}
