package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/tools"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscover_MergesGlobalAndProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".deepai", "mcp.json"),
		`{"mcpServers":{"global-only":{"command":"g"},"shared":{"command":"from-global","args":["a"]}}}`)

	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"),
		`{"mcpServers":{"proj-only":{"command":"p"},"shared":{"command":"from-proj"}}}`)

	servers, _ := Discover(proj)
	if len(servers) != 3 {
		t.Fatalf("want 3 servers, got %d: %+v", len(servers), servers)
	}
	if servers["shared"].Command != "from-proj" {
		t.Fatalf("project should override global for shared: %+v", servers["shared"])
	}
	if _, ok := servers["global-only"]; !ok {
		t.Fatal("missing global-only")
	}
	if _, ok := servers["proj-only"]; !ok {
		t.Fatal("missing proj-only")
	}
}

func TestDiscover_NoConfigIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	servers, _ := Discover(t.TempDir())
	if len(servers) != 0 {
		t.Fatalf("want empty, got %+v", servers)
	}
}

func TestDiscover_CorruptFileSkippedNotFatal(t *testing.T) {
	// A corrupt global config must not abort a valid project config: the bad
	// source is warned and skipped, the good source still loads.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".deepai", "mcp.json"), `{not valid json`)
	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"ok":{"command":"c"}}}`)

	servers, badFiles := Discover(proj)
	if _, ok := servers["ok"]; !ok {
		t.Fatalf("valid project server should still load despite corrupt global; got %+v", servers)
	}
	if len(badFiles) != 1 {
		t.Fatalf("corrupt global should be reported as a bad file, got %v", badFiles)
	}
}

func TestDiscover_DefaultTypeIsEmpty(t *testing.T) {
	// Omitted "type" parses as "" → connectServer treats it as stdio.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".deepai", "mcp.json"),
		`{"mcpServers":{"s":{"command":"c","args":["x"]}}}`)
	servers, _ := Discover(home)
	if servers["s"].Type != "" {
		t.Fatalf("omitted type should parse as empty (→ stdio), got %q", servers["s"].Type)
	}
	if servers["s"].Command != "c" || len(servers["s"].Args) != 1 || servers["s"].Args[0] != "x" {
		t.Fatalf("parsed fields wrong: %+v", servers["s"])
	}
}

func TestExpandVars(t *testing.T) {
	t.Setenv("MCP_TOKEN", "sekrit")
	if got := expandVars("Bearer ${MCP_TOKEN}"); got != "Bearer sekrit" {
		t.Fatalf("expand set var: %q", got)
	}
	if got := expandVars("x${DEFINITELY_UNSET_VAR_Z}y"); got != "xy" {
		t.Fatalf("expand unset var should become empty: %q", got)
	}
	if got := expandVars("plain"); got != "plain" {
		t.Fatalf("no-var string changed: %q", got)
	}
}

func TestBuildReport(t *testing.T) {
	if s := buildReport(nil, nil, nil); s != "" {
		t.Fatalf("empty should be %q, got %q", "", s)
	}
	if s := buildReport([]string{"b", "a"}, nil, nil); s != "MCP: 2 loaded (a, b)" {
		t.Fatalf("loaded-only: %q", s)
	}
	if s := buildReport([]string{"a"}, []string{"z"}, nil); s != "MCP: 1 loaded (a), 1 failed (z)" {
		t.Fatalf("loaded+failed: %q", s)
	}
	if s := buildReport(nil, []string{"z"}, nil); s != "MCP: 0 loaded, 1 failed (z)" {
		t.Fatalf("failed-only should prefix MCP: 0 loaded: %q", s)
	}
	// Present-but-corrupt config files surface, abbreviated under ~.
	home := t.TempDir()
	t.Setenv("HOME", home)
	bad := filepath.Join(home, ".deepai", "mcp.json")
	if s := buildReport(nil, nil, []string{bad}); s != "MCP: 0 loaded, config error in ~/.deepai/mcp.json" {
		t.Fatalf("bad-files: %q", s)
	}
}

func TestLoad_NoConfigSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := tools.NewRegistry()
	closers, report := Load(context.Background(), reg, t.TempDir())
	if report != "" {
		t.Fatalf("no servers → empty report, got %q", report)
	}
	if len(closers) != 0 {
		t.Fatalf("no servers → no closers, got %d", len(closers))
	}
}

func TestLoad_RegistersToolsAndReports(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real global config
	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"),
		`{"mcpServers":{"mock":{"command":"go","args":["run","../../cmd/mcp-example/main.go"]}}}`)

	reg := tools.NewRegistry()
	closers, report := Load(context.Background(), reg, proj)
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	if !strings.Contains(report, "1 loaded") {
		t.Fatalf("report should show 1 loaded: %q", report)
	}
	if strings.Contains(report, "failed") {
		t.Fatalf("report should have no failures: %q", report)
	}
	if reg.Get("mock.test-tool") == nil {
		var names []string
		for _, tl := range reg.List() {
			names = append(names, tl.Name)
		}
		t.Fatalf("mock.test-tool not registered; have %v", names)
	}
}

func TestLoad_CorruptConfigSurfacesReport(t *testing.T) {
	// A present-but-corrupt .mcp.json must surface in the report (not be
	// silent), so the TUI can tell the user their config is broken instead of
	// looking like "nothing configured".
	t.Setenv("HOME", t.TempDir())
	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"), `{bad json`)
	reg := tools.NewRegistry()
	closers, report := Load(context.Background(), reg, proj)
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	if report == "" {
		t.Fatalf("corrupt config should produce a non-empty report, got empty")
	}
	if !strings.Contains(report, "config error") {
		t.Fatalf("report should mention config error: %q", report)
	}
}
