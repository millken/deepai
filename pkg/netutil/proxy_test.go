package netutil

import (
	"net/http"
	"testing"
)

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request %q: %v", rawURL, err)
	}
	return req
}

// TestEnvProxyFunc_ReadsEnvironmentPerCall is the reason this helper exists at
// all instead of http.ProxyFromEnvironment: the stdlib version snapshots the
// environment on first use for the whole process, so a proxy exported after the
// first outbound request — and any test that sets one with t.Setenv — is
// silently ignored.
func TestEnvProxyFunc_ReadsEnvironmentPerCall(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")

	got, err := EnvProxyFunc(mustRequest(t, "https://example.com/x"))
	if err != nil {
		t.Fatalf("EnvProxyFunc() error = %v", err)
	}
	if got != nil {
		t.Fatalf("proxy = %v, want nil with no proxy configured", got)
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	got, err = EnvProxyFunc(mustRequest(t, "https://example.com/x"))
	if err != nil {
		t.Fatalf("EnvProxyFunc() error = %v", err)
	}
	if got == nil || got.Host != "127.0.0.1:7890" {
		t.Fatalf("proxy = %v, want 127.0.0.1:7890 — the env must be re-read, not snapshotted", got)
	}
}

func TestEnvProxyFunc_HonorsNoProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "internal.example.com")

	got, err := EnvProxyFunc(mustRequest(t, "https://internal.example.com/x"))
	if err != nil {
		t.Fatalf("EnvProxyFunc() error = %v", err)
	}
	if got != nil {
		t.Fatalf("proxy = %v, want nil for a NO_PROXY host", got)
	}

	got, err = EnvProxyFunc(mustRequest(t, "https://other.example.com/x"))
	if err != nil {
		t.Fatalf("EnvProxyFunc() error = %v", err)
	}
	if got == nil {
		t.Fatal("proxy = nil for a host outside NO_PROXY, want the configured proxy")
	}
}

// TestIsEnvProxyAddr matters for SSRF filtering: a DialContext that rejects
// private/loopback addresses must recognize a dial aimed at the user's own proxy
// (typically 127.0.0.1) and let it through, while still guarding every dial that
// goes straight to a target host.
func TestIsEnvProxyAddr(t *testing.T) {
	tests := []struct {
		name       string
		httpProxy  string
		httpsProxy string
		allProxy   string
		addr       string
		want       bool
	}{
		{"loopback proxy with port", "", "http://127.0.0.1:7890", "", "127.0.0.1:7890", true},
		{"different port is not the proxy", "", "http://127.0.0.1:7890", "", "127.0.0.1:9999", false},
		{"http proxy endpoint", "http://127.0.0.1:3128", "", "", "127.0.0.1:3128", true},
		{"all_proxy endpoint", "", "", "socks5://127.0.0.1:1080", "127.0.0.1:1080", true},
		{"scheme-less proxy value", "", "127.0.0.1:8080", "", "127.0.0.1:8080", true},
		{"proxy without port defaults to 80", "http://proxy.internal", "", "", "proxy.internal:80", true},
		{"no proxy configured", "", "", "", "127.0.0.1:7890", false},
		{"unrelated target", "", "http://127.0.0.1:7890", "", "192.168.1.5:80", false},
		{"empty addr", "", "http://127.0.0.1:7890", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HTTP_PROXY", tt.httpProxy)
			t.Setenv("HTTPS_PROXY", tt.httpsProxy)
			t.Setenv("ALL_PROXY", tt.allProxy)
			// Lowercase forms must not leak in from the developer's shell.
			t.Setenv("http_proxy", "")
			t.Setenv("https_proxy", "")
			t.Setenv("all_proxy", "")
			if got := IsEnvProxyAddr(tt.addr); got != tt.want {
				t.Fatalf("IsEnvProxyAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}
