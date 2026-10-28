package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// TestMain pre-builds the example MCP server while HOME is still the real
// one, so `go build` hits the warm GOCACHE. Tests below t.Setenv("HOME",
// tmp), which redirects GOCACHE to an empty dir; a `go run` server config
// would then cold-compile the whole dependency tree and blow the 30s MCP
// handshake timeout (the pre-existing TestLoad_* flakes on a clean tree).
func TestMain(m *testing.M) {
	mcpExamplePath()
	os.Exit(m.Run())
}

var mcpExamplePath = sync.OnceValue(func() string {
	bin := filepath.Join(os.TempDir(), "deepai-mcp-example-test")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/mcp-example").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("pre-build cmd/mcp-example: %v: %s", err, out))
	}
	return bin
})

func runClosers(closers []func()) {
	for _, c := range closers {
		c()
	}
}

func TestServerFingerprint_ConfigSensitivity(t *testing.T) {
	base := ServerConfig{Command: "npx", Args: []string{"-y", "pkg@1"}}
	a := serverFingerprint("s", base)
	b := serverFingerprint("s", ServerConfig{Command: "npx", Args: []string{"-y", "pkg@2"}})
	c := serverFingerprint("other", base)
	if a == "" || a == b || a == c {
		t.Fatalf("fingerprints must differ across args/name: %q %q %q", a, b, c)
	}
	if a != serverFingerprint("s", base) {
		t.Fatal("fingerprint must be deterministic")
	}
}

func TestCachedTools_RoundTripAndInvalidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fp := "fp123"
	ts := []cachedTool{{Name: "x__t", Description: "d", InputSchema: map[string]any{"type": "object"}, Remote: "t"}}
	saveCachedTools("x", fp, ts)

	got, ok := loadCachedTools("x", fp)
	if !ok || len(got) != 1 || got[0].Remote != "t" || got[0].Name != "x__t" {
		t.Fatalf("round trip: ok=%v got=%+v", ok, got)
	}
	if _, ok := loadCachedTools("x", "different"); ok {
		t.Fatal("fingerprint mismatch must be a miss")
	}

	if err := os.WriteFile(cachePath("x"), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCachedTools("x", fp); ok {
		t.Fatal("corrupt cache file must be a miss")
	}

	saveCachedTools("x", fp, []cachedTool{{Name: "x__t"}})
	if _, ok := loadCachedTools("x", fp); ok {
		t.Fatal("entry without remote name must be a miss")
	}
}

func TestLoad_ColdPopulatesCacheThenWarmServesIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"),
		fmt.Sprintf(`{"mcpServers":{"mock":{"command":%q}}}`, mcpExamplePath()))

	// Cold: synchronous connect + tools/list, cache written for next time.
	reg := tools.NewRegistry()
	closers, report := Load(context.Background(), reg, proj)
	defer runClosers(closers)
	if !strings.Contains(report, "1 loaded (mock)") || strings.Contains(report, "cached") {
		t.Fatalf("cold report should load synchronously without cache: %q", report)
	}
	if reg.Get("mock__test-tool") == nil {
		t.Fatal("cold path must register mock__test-tool")
	}

	// Warm: same config — tools come from cache, connection dials in background.
	reg2 := tools.NewRegistry()
	closers2, report2 := Load(context.Background(), reg2, proj)
	defer runClosers(closers2)
	if !strings.Contains(report2, "1 loaded (mock (1 cached))") {
		t.Fatalf("warm report should mark cache use: %q", report2)
	}
	tool := reg2.Get("mock__test-tool")
	if tool == nil {
		t.Fatal("warm path must register mock__test-tool from cache")
	}

	// The lazy handler must transparently reach the background-dialed server.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := tool.Handler(ctx, models.ToolCall{ID: "1", Name: tool.Name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("warm tool call failed: %v", err)
	}
	if !strings.Contains(res.Content, "tool result") {
		t.Fatalf("unexpected tool result: %s", res.Content)
	}
}

func TestLoad_WarmCacheDeadServerRegistersButFailsCalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	writeConfig(t, filepath.Join(proj, ".mcp.json"),
		`{"mcpServers":{"dead":{"command":"/nonexistent-deepai-test"}}}`)

	sc := ServerConfig{Command: "/nonexistent-deepai-test"}
	saveCachedTools("dead", serverFingerprint("dead", sc), []cachedTool{{
		Name: "dead__noop", Description: "d", InputSchema: map[string]any{"type": "object"}, Remote: "noop",
	}})

	reg := tools.NewRegistry()
	closers, report := Load(context.Background(), reg, proj)
	defer runClosers(closers)
	if !strings.Contains(report, "(1 cached)") {
		t.Fatalf("dead server with warm cache should still register from cache: %q", report)
	}
	tool := reg.Get("dead__noop")
	if tool == nil {
		t.Fatal("cached tool must be registered")
	}

	// Calling it must fail with a bounded error, not hang: background dial
	// failed, one synchronous redial fails fast, get() gives up.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := tool.Handler(ctx, models.ToolCall{ID: "1", Name: "dead__noop", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("expected a dial failure error from a dead server")
	}
}

// Review C3: an empty tool list is a legal cache state — a server that
// removed all its tools must overwrite the stale cache, and the next
// session must see zero tools instead of ghosts.
func TestCachedTools_EmptyListRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fp := "fp-empty"
	saveCachedTools("e", fp, []cachedTool{})
	got, ok := loadCachedTools("e", fp)
	if !ok || len(got) != 0 {
		t.Fatalf("empty cache must be a hit with zero tools: ok=%v got=%+v", ok, got)
	}
}

// Review C3: tools/list order is not part of the contract; equal sets in a
// different order must not be reported as drift (which would rewrite the
// cache and warn on every warm start).
func TestCachedEqual_OrderInsensitive(t *testing.T) {
	a := []cachedTool{{Name: "x__a", Remote: "a"}, {Name: "x__b", Remote: "b"}}
	b := []cachedTool{{Name: "x__b", Remote: "b"}, {Name: "x__a", Remote: "a"}}
	if !cachedEqual(a, b) {
		t.Fatal("same tools in different order must compare equal")
	}
	b[0].Description = "changed"
	if cachedEqual(a, b) {
		t.Fatal("a real field change must still be detected")
	}
}
