package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/tools"
)

// ServerConfig is a single MCP server entry, parsed from .mcp.json with ${VAR}
// references expanded.
type ServerConfig struct {
	Type    string            // "stdio" | "sse" | "http"; "" → stdio
	Command string            // stdio
	Args    []string          // stdio
	Env     map[string]string // stdio
	URL     string            // sse | http
	Headers map[string]string // sse | http
}

// configFile mirrors the .mcp.json shape used by Claude Code / Cursor.
type configFile struct {
	MCPServers map[string]serverEntry `json:"mcpServers"`
}

type serverEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// mcpListTimeout bounds the startup tools/list call. It is safe to bound
// because tools/list is a per-request call whose ctx is not stored by the
// transport — unlike Start, which must receive the long-lived session ctx.
// Prevents a server that hangs on tools/list from blocking REPL startup.
const mcpListTimeout = 30 * time.Second

// configPaths returns the discovery paths in priority order (global first,
// project overrides). A path is "best-effort" — missing files are silently
// skipped, so callers always get all candidates.
func configPaths(workdir string) []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".deepai", "mcp.json"))
	}
	if strings.TrimSpace(workdir) != "" {
		paths = append(paths, filepath.Join(workdir, ".mcp.json"))
	}
	return paths
}

// Discover reads the config paths and merges servers (project entries override
// global ones with the same name). A missing file is skipped silently; an
// unreadable or unparseable file is logged as a warning, skipped, and its path
// returned in badFiles so the caller can surface it (one bad source never
// blocks the others). Returns an empty map when nothing loaded.
func Discover(workdir string) (servers map[string]ServerConfig, badFiles []string) {
	servers = make(map[string]ServerConfig)
	for _, p := range configPaths(workdir) {
		if loadConfigFile(p, servers) {
			badFiles = append(badFiles, p)
		}
	}
	return servers, badFiles
}

// loadConfigFile loads one config file into servers. It reports whether the
// file was present on disk but could not be read or parsed (true = bad); a
// missing file or a clean load both return false. The detailed error is logged
// via slog; only the path is surfaced to callers.
func loadConfigFile(path string, servers map[string]ServerConfig) (bad bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		slog.Warn("mcp config unreadable; skipping", "path", path, "err", err)
		return true
	}
	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		slog.Warn("mcp config parse failed; skipping", "path", path, "err", err)
		return true
	}
	for name, e := range cf.MCPServers {
		servers[name] = ServerConfig{
			Type:    e.Type,
			Command: expandVars(e.Command),
			URL:     expandVars(e.URL),
			Args:    expandSlice(e.Args),
			Env:     expandMap(e.Env),
			Headers: expandMap(e.Headers),
		}
	}
	return false
}

// Load discovers MCP servers, connects each, lists its tools, and registers
// them into registry. ctx must be the long-lived session ctx — it is bound to
// each server's subprocess/listener lifetime by Start. The Initialize handshake
// and the tools/list call are per-request (their ctx is not stored by the
// transport), so they are bounded internally (defaultHandshakeTimeout,
// mcpListTimeout) without affecting the connection. closers close every
// connected client (defer at session end). report is a one-line human summary,
// "" when no servers were discovered or all sources are missing/unparseable.
// Per-server and per-file failures are warned and folded into report; Load
// itself never returns an error.
func Load(ctx context.Context, registry *tools.Registry, workdir string) (closers []func(), report string) {
	servers, badFiles := Discover(workdir)
	// Silent only when there is genuinely nothing to report: no servers
	// discovered and no config file was present-but-bad. A corrupt .mcp.json
	// must surface so the user can tell it apart from "not configured".
	if len(servers) == 0 && len(badFiles) == 0 {
		return nil, ""
	}

	var loaded, failed []string
	for name, sc := range servers {
		c, connectErr := connectServer(ctx, name, sc)
		if connectErr != nil {
			slog.Warn("mcp server connect failed", "server", name, "err", connectErr)
			failed = append(failed, fmt.Sprintf("%s: %v", name, connectErr))
			continue
		}
		// tools/list is a per-request call (its ctx is not stored by the
		// transport), so bounding it is safe and cannot affect the connection
		// lifetime — unlike Start, which must keep the long-lived session ctx.
		listCtx, cancel := context.WithTimeout(ctx, mcpListTimeout)
		serverTools, listErr := c.Tools(listCtx)
		cancel()
		if listErr != nil {
			_ = c.Close()
			slog.Warn("mcp server list tools failed", "server", name, "err", listErr)
			failed = append(failed, fmt.Sprintf("%s: list tools: %v", name, listErr))
			continue
		}
		client := c // capture for closer
		closers = append(closers, func() { _ = client.Close() })
		for _, t := range serverTools {
			if regErr := registry.Register(t); regErr != nil {
				slog.Warn("mcp tool register failed", "server", name, "tool", t.Name, "err", regErr)
			}
		}
		loaded = append(loaded, name)
	}

	return closers, buildReport(loaded, failed, badFiles)
}

func connectServer(ctx context.Context, name string, sc ServerConfig) (*Client, error) {
	switch strings.ToLower(strings.TrimSpace(sc.Type)) {
	case "", "stdio":
		if strings.TrimSpace(sc.Command) == "" {
			return nil, fmt.Errorf("stdio server missing \"command\"")
		}
		return ConnectStdio(ctx, name, sc.Command, envSlice(sc.Env), sc.Args...)
	case "sse":
		if strings.TrimSpace(sc.URL) == "" {
			return nil, fmt.Errorf("sse server missing \"url\"")
		}
		return ConnectSSE(ctx, name, sc.URL, sc.Headers, nil)
	case "http":
		if strings.TrimSpace(sc.URL) == "" {
			return nil, fmt.Errorf("http server missing \"url\"")
		}
		return ConnectHTTP(ctx, name, sc.URL, sc.Headers, nil)
	default:
		return nil, fmt.Errorf("unknown server type %q", sc.Type)
	}
}

// buildReport renders the one-line MCP summary. badFiles are config sources
// that were present but unreadable/unparseable; they are surfaced so a broken
// .mcp.json is distinguishable from "not configured". Returns "" only when
// there is nothing to report.
func buildReport(loaded, failed, badFiles []string) string {
	if len(loaded) == 0 && len(failed) == 0 && len(badFiles) == 0 {
		return ""
	}
	sort.Strings(loaded)
	sort.Strings(failed)
	sort.Strings(badFiles)
	var parts []string
	if len(loaded) > 0 {
		parts = append(parts, fmt.Sprintf("MCP: %d loaded (%s)", len(loaded), strings.Join(loaded, ", ")))
	} else {
		parts = append(parts, "MCP: 0 loaded")
	}
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("%d failed (%s)", len(failed), strings.Join(failed, ", ")))
	}
	for _, p := range badFiles {
		parts = append(parts, "config error in "+tidyPath(p))
	}
	return strings.Join(parts, ", ")
}

// tidyPath abbreviates the user's home directory to "~" for readable reports.
func tidyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func envSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// envVarRE matches ${NAME} references in config strings.
var envVarRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandVars(s string) string {
	return envVarRE.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

func expandSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expandVars(s)
	}
	return out
}

func expandMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expandVars(v)
	}
	return out
}
