package claudeplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/mcp"
)

func writePluginFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// makePlugin creates a plugin named `name` under root with the given
// plugin.json body. Returns the plugin directory.
func makePlugin(t *testing.T, root, name, manifestBody string) string {
	dir := filepath.Join(root, name)
	writePluginFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), manifestBody)
	return dir
}

func TestDiscover_RecognizesAndOverrides(t *testing.T) {
	globalRoot := t.TempDir()
	t.Setenv("HOME", globalRoot)
	// Global plugin "shared" + "gonly".
	makePlugin(t, filepath.Join(globalRoot, ".deepai", "plugins"), "gonly", `{"name":"gonly"}`)
	makePlugin(t, filepath.Join(globalRoot, ".deepai", "plugins"), "shared", `{"name":"shared"}`)

	// Project plugin "shared" (overrides global) + "ponly".
	projRoot := t.TempDir()
	makePlugin(t, filepath.Join(projRoot, ".deepai", "plugins"), "shared", `{"name":"shared"}`)
	makePlugin(t, filepath.Join(projRoot, ".deepai", "plugins"), "ponly", `{"name":"ponly"}`)

	plugins, problems := Discover(projRoot)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Name)
	}
	// gonly, ponly, shared (3 — not 4; project overrode global "shared").
	want := map[string]bool{"gonly": true, "ponly": true, "shared": true}
	if len(plugins) != 3 {
		t.Fatalf("want 3 plugins, got %d: %v", len(plugins), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected plugin %q in %v", n, names)
		}
	}
	// The "shared" plugin must resolve to the PROJECT dir (override).
	var shared *Plugin
	for i := range plugins {
		if plugins[i].Name == "shared" {
			shared = &plugins[i]
		}
	}
	if shared == nil || !strings.Contains(shared.Dir, projRoot) {
		t.Fatalf("shared should resolve to project dir, got %q", shared.Dir)
	}
}

func TestDiscover_BadManifestIsProblem(t *testing.T) {
	globalRoot := t.TempDir()
	t.Setenv("HOME", globalRoot)
	// Bad JSON → problem.
	makePlugin(t, filepath.Join(globalRoot, ".deepai", "plugins"), "bad", `{not json`)
	// Missing name → problem.
	makePlugin(t, filepath.Join(globalRoot, ".deepai", "plugins"), "noname", `{"version":"1"}`)
	// No manifest at all → silently skipped (NOT a problem).
	nomanifest := filepath.Join(globalRoot, ".deepai", "plugins", "stray")
	if err := os.MkdirAll(nomanifest, 0o755); err != nil {
		t.Fatal(err)
	}

	plugins, problems := Discover(t.TempDir()) // separate empty workdir → only global root scanned
	if len(plugins) != 0 {
		t.Fatalf("expected no valid plugins, got %v", plugins)
	}
	if len(problems) != 2 {
		t.Fatalf("expected 2 problems (bad, noname), got %v", problems)
	}
}

func TestSkillRoot_ReturnsPluginRoot(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	p, _ := loadPlugin(dir)
	if p.SkillRoot() != dir {
		t.Fatalf("SkillRoot should be plugin root %q, got %q", dir, p.SkillRoot())
	}
	if strings.HasSuffix(p.SkillRoot(), "/skills") {
		t.Fatalf("SkillRoot must NOT include /skills (LoadAll appends it): %q", p.SkillRoot())
	}
}

func TestMCPServers_DefaultDotMCPJSON(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	writePluginFile(t, filepath.Join(dir, ".mcp.json"),
		`{"mcpServers":{"db":{"command":"pg","args":["-p","5432"]}}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("unexpected problem: %s", problem)
	}
	if sc, ok := servers["db"]; !ok || sc.Command != "pg" || len(sc.Args) != 2 {
		t.Fatalf("db server not parsed correctly: %+v", servers)
	}
}

func TestMCPServers_DefaultDotMCPJSON_JunkIsProblem(t *testing.T) {
	// A malformed .mcp.json must surface as a problem, not be silently treated
	// as "no MCP".
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	writePluginFile(t, filepath.Join(dir, ".mcp.json"), `{"foo":"bar"}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem == "" {
		t.Fatalf("junk .mcp.json must be a problem, got servers=%+v", servers)
	}
	if servers != nil {
		t.Fatalf("junk should yield nil servers, got %+v", servers)
	}
}

func TestMCPServers_DefaultDotMCPJSON_EmptyWrappedOK(t *testing.T) {
	// {"mcpServers": {}} is a legitimate "no servers": no problem, and must NOT
	// fabricate a phantom "mcpServers" server via bare-map fallback.
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	writePluginFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("empty wrapped mcpServers should not be a problem: %s", problem)
	}
	if len(servers) != 0 {
		t.Fatalf("expected no servers, got %+v", servers)
	}
}

func TestMCPServers_InlineObject(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p",
		`{"name":"p","mcpServers":{"api":{"type":"http","url":"https://x/mcp"}}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("unexpected problem: %s", problem)
	}
	if sc, ok := servers["api"]; !ok || sc.Type != "http" || sc.URL != "https://x/mcp" {
		t.Fatalf("api server not parsed: %+v", servers)
	}
}

func TestMCPServers_StringPath(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p","mcpServers":"./cfg/mcp.json"}`)
	writePluginFile(t, filepath.Join(dir, "cfg", "mcp.json"),
		`{"mcpServers":{"fs":{"command":"fs-server"}}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("unexpected problem: %s", problem)
	}
	if sc, ok := servers["fs"]; !ok || sc.Command != "fs-server" {
		t.Fatalf("fs server not parsed from path: %+v", servers)
	}
}

func TestMCPServers_StringPathBareMap(t *testing.T) {
	// A path file may also be a bare server map (no "mcpServers" wrapper).
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p","mcpServers":"./servers.json"}`)
	writePluginFile(t, filepath.Join(dir, "servers.json"),
		`{"bare":{"command":"bare-server"}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("unexpected problem: %s", problem)
	}
	if sc, ok := servers["bare"]; !ok || sc.Command != "bare-server" {
		t.Fatalf("bare-map server not parsed: %+v", servers)
	}
}

func TestMCPServers_StringPathEscapeRejected(t *testing.T) {
	// A "../" mcpServers path must not read outside the plugin dir.
	root := t.TempDir()
	secret := filepath.Join(root, "secret.json") // outside the plugin dir (root/p)
	writePluginFile(t, secret, `{"mcpServers":{"leaked":{"command":"x"}}}`)
	dir := makePlugin(t, root, "p", `{"name":"p","mcpServers":"../secret.json"}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem == "" {
		t.Fatalf("escaping ../ path must be rejected, got servers=%+v", servers)
	}
	if servers != nil {
		t.Fatalf("escaping path must yield no servers, got %+v", servers)
	}
}

func TestMCPServers_ArrayShapeIsProblem(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p","mcpServers":["./a.json","./b.json"]}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem == "" {
		t.Fatalf("array mcpServers should produce a problem, got servers=%+v", servers)
	}
	if servers != nil {
		t.Fatalf("on problem, servers should be nil, got %+v", servers)
	}
}

func TestMCPServers_ExpandsCLAUDE_PLUGIN_ROOT(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p",
		`{"name":"p","mcpServers":{"srv":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/srv","args":["${CLAUDE_PLUGIN_ROOT}/conf.json"],"env":{"DATA":"${CLAUDE_PLUGIN_ROOT}/data"}}}}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if problem != "" {
		t.Fatalf("unexpected problem: %s", problem)
	}
	sc := servers["srv"]
	if sc.Command != filepath.Join(dir, "bin", "srv") {
		t.Fatalf("command not expanded: %q", sc.Command)
	}
	if len(sc.Args) != 1 || sc.Args[0] != filepath.Join(dir, "conf.json") {
		t.Fatalf("args not expanded: %+v", sc.Args)
	}
	if sc.Env["DATA"] != filepath.Join(dir, "data") {
		t.Fatalf("env not expanded: %+v", sc.Env)
	}
	// ${VAR} (non-CLAUDE_PLUGIN_ROOT) must be left for the MCP loader.
	dir2 := makePlugin(t, t.TempDir(), "p2",
		`{"name":"p2","mcpServers":{"s":{"command":"x","env":{"TOK":"${MY_TOKEN}"}}}}`)
	p2, _ := loadPlugin(dir2)
	s2, _ := p2.MCPServers()
	if s2["s"].Env["TOK"] != "${MY_TOKEN}" {
		t.Fatalf("${VAR} must be preserved for the MCP loader, got %q", s2["s"].Env["TOK"])
	}
}

func TestMCPServers_NoneWhenAbsent(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	p, _ := loadPlugin(dir)
	servers, problem := p.MCPServers()
	if servers != nil || problem != "" {
		t.Fatalf("plugin with no MCP should return nil,\"\", got %+v / %q", servers, problem)
	}
}

// ensure mcp.ServerConfig is referenced (import sanity).
var _ mcp.ServerConfig

func TestAgentDir(t *testing.T) {
	dir := makePlugin(t, t.TempDir(), "p", `{"name":"p"}`)
	p, _ := loadPlugin(dir)
	want := filepath.Join(dir, "agents")
	if got := p.AgentDir(); got != want {
		t.Fatalf("AgentDir = %q, want %q", got, want)
	}
}
