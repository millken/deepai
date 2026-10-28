package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Tool schemas fetched from an MCP server at startup are cached here so the
// next session can register them without connecting first — the connection
// (a subprocess launch for stdio, a TLS handshake for remote servers) is what
// makes startup slow, while the model only needs the schema list to see the
// tools. The live connection is then established in the background and reused
// (or lazily dialed on first call) by the lazy handler in lazyclient.go.
//
// One JSON file per server under ~/.deepai/mcp-cache/, keyed by server name
// and invalidated by a fingerprint of the expanded ServerConfig.

const cacheFormatVersion = 1

// cachedTool is the persistable subset of models.Tool. Remote keeps the
// server-side tool name, which differs from the registered (prefixed) name
// and cannot be reconstructed from the cache alone when the prefix rule
// itself changes.
type cachedTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Remote      string         `json:"remote"`
}

type toolCacheFile struct {
	Version     int          `json:"version"`
	Fingerprint string       `json:"fingerprint"`
	Tools       []cachedTool `json:"tools"`
}

func cacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".deepai", "mcp-cache")
}

// serverFingerprint hashes the expanded server config so any edit to the
// command/args/env/url/headers invalidates the cache. Hashed, never stored
// verbatim: env/headers carry tokens after ${VAR} expansion.
func serverFingerprint(name string, sc ServerConfig) string {
	wire, err := json.Marshal(struct {
		Name    string            `json:"name"`
		Type    string            `json:"type"`
		Command string            `json:"command,omitempty"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		URL     string            `json:"url,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}{Name: name, Type: sc.Type, Command: sc.Command, Args: sc.Args, Env: sc.Env, URL: sc.URL, Headers: sc.Headers})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(wire)
	return hex.EncodeToString(sum[:8])
}

func cachePath(name string) string {
	d := cacheDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, toolNameSanitizer.ReplaceAllString(name, "_")+".json")
}

// loadCachedTools returns the cached tool set only when the file is parseable,
// format-current, fingerprint-matching, and every entry carries a remote name;
// anything else is a miss (the caller falls back to connecting synchronously).
func loadCachedTools(name, fingerprint string) ([]cachedTool, bool) {
	p := cachePath(name)
	if p == "" || fingerprint == "" {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var cf toolCacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		slog.Warn("mcp tool cache unreadable; ignoring", "path", p, "err", err)
		return nil, false
	}
	if cf.Version != cacheFormatVersion || cf.Fingerprint != fingerprint || len(cf.Tools) == 0 {
		return nil, false
	}
	for _, t := range cf.Tools {
		if strings.TrimSpace(t.Remote) == "" || strings.TrimSpace(t.Name) == "" {
			return nil, false
		}
	}
	return cf.Tools, true
}

func saveCachedTools(name, fingerprint string, ts []cachedTool) {
	p := cachePath(name)
	if p == "" || fingerprint == "" || len(ts) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		slog.Warn("mcp tool cache mkdir failed", "server", name, "err", err)
		return
	}
	data, err := json.MarshalIndent(toolCacheFile{Version: cacheFormatVersion, Fingerprint: fingerprint, Tools: ts}, "", "  ")
	if err != nil {
		return
	}
	// temp+rename so a crash mid-write can never leave a truncated cache file
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("mcp tool cache write failed", "server", name, "err", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		slog.Warn("mcp tool cache rename failed", "server", name, "err", err)
	}
}
