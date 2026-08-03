package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeProxy starts an HTTP proxy on loopback. A client configured to use it
// sends plain-http requests in absolute-URI form ("GET http://host/path"), so the
// handler can answer directly without forwarding — receiving the request at all
// is the proof that traffic went through the proxy.
func newFakeProxy(t *testing.T, body string) (addr string, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxied request", http.StatusBadRequest)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), hits
}

func setProxyEnv(t *testing.T, addr string) {
	t.Helper()
	value := ""
	if addr != "" {
		value = "http://" + addr
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, value)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
}

// TestWebFetch_ThroughLoopbackProxy is the RED test for proxy support in the web
// tools: safeTransport is a hand-built http.Transport with no Proxy field, so
// HTTP_PROXY/HTTPS_PROXY were ignored entirely for web_fetch. It also pins the
// interaction with the SSRF guard — the proxy lives on 127.0.0.1, exactly what
// safeTransport.DialContext rejects as a private address, so naive proxy support
// fails with "resolved to private address" instead of working.
func TestWebFetch_ThroughLoopbackProxy(t *testing.T) {
	addr, hits := newFakeProxy(t, "<html><head><title>Proxied</title></head><body><p>via proxy</p></body></html>")
	setProxyEnv(t, addr)

	// example.invalid never resolves; reaching it can only work through the
	// proxy, which makes this assertion unambiguous and DNS-free.
	got, err := fetchWebPage(context.Background(), "http://example.invalid/page", 4096, false)
	if err != nil {
		t.Fatalf("fetchWebPage() error = %v, want success through the configured proxy", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("proxy saw %d requests, want 1 — the request did not go through the proxy", hits.Load())
	}
	if !strings.Contains(got, "via proxy") {
		t.Fatalf("content = %q, want the proxy's response body", got)
	}
}

// TestWebFetch_RejectsLiteralPrivateTargetWithProxy: configuring a proxy must not
// become an SSRF bypass. With a proxy in play the transport dials only the proxy,
// so DialContext never sees the target — the target has to be screened before the
// request is issued.
func TestWebFetch_RejectsLiteralPrivateTargetWithProxy(t *testing.T) {
	addr, hits := newFakeProxy(t, "<html><body>secret</body></html>")
	setProxyEnv(t, addr)

	for _, target := range []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/admin",
		"http://192.168.1.1/",
		"http://10.0.0.5/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://localhost:9000/",
	} {
		t.Run(target, func(t *testing.T) {
			before := hits.Load()
			_, err := fetchWebPage(context.Background(), target, 4096, false)
			if err == nil {
				t.Fatalf("fetchWebPage(%q) error = nil, want rejection", target)
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("fetchWebPage(%q) error = %v, want a not-allowed rejection", target, err)
			}
			if hits.Load() != before {
				t.Fatalf("fetchWebPage(%q) reached the proxy; the target must be screened before the request", target)
			}
		})
	}
}

// TestWebFetch_RejectsLiteralPrivateTargetWithoutProxy is the same guard with no
// proxy configured — the pre-existing protection must not regress.
func TestWebFetch_RejectsLiteralPrivateTargetWithoutProxy(t *testing.T) {
	setProxyEnv(t, "")
	for _, target := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://localhost/"} {
		if _, err := fetchWebPage(context.Background(), target, 4096, false); err == nil {
			t.Fatalf("fetchWebPage(%q) error = nil, want rejection", target)
		}
	}
}

// TestWebClientsHonorEnvProxy covers the search-side clients too: webClient has
// no custom Transport so it already proxied via http.DefaultTransport, but it
// must keep doing so through the same env-reading helper the fetch client uses,
// rather than the stdlib's process-wide snapshot.
func TestWebClientsHonorEnvProxy(t *testing.T) {
	setProxyEnv(t, "127.0.0.1:7890")

	req, err := http.NewRequest(http.MethodGet, "https://html.duckduckgo.com/html/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, transport := range map[string]*http.Transport{
		"safeTransport": safeTransport,
		"webTransport":  webTransport,
	} {
		if transport.Proxy == nil {
			t.Fatalf("%s.Proxy is nil; env proxy settings are ignored", name)
		}
		proxyURL, err := transport.Proxy(req)
		if err != nil {
			t.Fatalf("%s.Proxy() error = %v", name, err)
		}
		if proxyURL == nil || proxyURL.Host != "127.0.0.1:7890" {
			t.Fatalf("%s.Proxy() = %v, want 127.0.0.1:7890", name, proxyURL)
		}
	}
}

// TestEnvDuration pins the timeout override parsing. The web clients used fixed
// 20s/10s deadlines, which is the reported symptom: through a proxy the round
// trip is longer and the fixed deadline expires.
func TestEnvDuration(t *testing.T) {
	const def = 20 * time.Second
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset falls back", "", def},
		{"duration string", "45s", 45 * time.Second},
		{"minutes", "2m", 2 * time.Minute},
		{"bare number means seconds", "60", 60 * time.Second},
		{"whitespace tolerated", "  30s  ", 30 * time.Second},
		{"invalid falls back", "soon", def},
		{"zero falls back", "0", def},
		{"negative falls back", "-5s", def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DEEPAI_WEB_TIMEOUT_TEST", tt.raw)
			if got := envDuration("DEEPAI_WEB_TIMEOUT_TEST", def); got != tt.want {
				t.Fatalf("envDuration(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
