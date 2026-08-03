package llm

import (
	"net/http"
	"testing"
)

// TestNewHTTPClient_HonorsEnvProxy: the LLM client builds its own
// http.Transport, and a hand-built Transport ignores HTTP_PROXY/HTTPS_PROXY
// unless Proxy is set explicitly — so every model API call bypassed the user's
// proxy even when web tools went through it.
func TestNewHTTPClient_HonorsEnvProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport, ok := newHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", newHTTPClient().Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("Transport.Proxy is nil; model API calls ignore the proxy environment")
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Transport.Proxy() error = %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:7890" {
		t.Fatalf("Transport.Proxy() = %v, want 127.0.0.1:7890", proxyURL)
	}
}

// TestNewHTTPClient_NoProxyConfigured: with nothing set, requests must go direct
// rather than to some stale snapshot of the environment.
func TestNewHTTPClient_NoProxyConfigured(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	transport := newHTTPClient().Transport.(*http.Transport)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Transport.Proxy() error = %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("Transport.Proxy() = %v, want nil with no proxy configured", proxyURL)
	}
}
