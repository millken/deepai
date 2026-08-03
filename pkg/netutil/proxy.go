// Package netutil holds small HTTP networking helpers shared by the LLM
// clients and the web tools — chiefly outbound proxy resolution.
package netutil

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// EnvProxyFunc resolves the outbound proxy for req from the standard
// HTTP_PROXY / HTTPS_PROXY / ALL_PROXY / NO_PROXY environment variables (both
// upper- and lowercase), for use as http.Transport.Proxy.
//
// It exists instead of http.ProxyFromEnvironment because the stdlib version
// snapshots the environment on its first call and reuses that snapshot for the
// rest of the process. That makes a proxy exported later invisible, and makes
// the behavior untestable (t.Setenv has no effect once any other test has
// triggered the snapshot). httpproxy.FromEnvironment re-reads the environment on
// every call and implements the same NO_PROXY matching rules.
func EnvProxyFunc(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return httpproxy.FromEnvironment().ProxyFunc()(req.URL)
}

// IsEnvProxyAddr reports whether addr ("host:port", as handed to
// Transport.DialContext) is one of the proxy endpoints configured in the
// environment.
//
// A DialContext that filters private/loopback destinations (the web tools'
// SSRF guard) needs this: once a proxy is in play, the address it is asked to
// dial is the PROXY, which is typically 127.0.0.1. Without this check the guard
// would reject the user's own proxy and no fetch could succeed; with it, the
// guard still applies to every dial that goes straight to a target host.
func IsEnvProxyAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	cfg := httpproxy.FromEnvironment()
	for _, raw := range []string{cfg.HTTPProxy, cfg.HTTPSProxy, envAny("ALL_PROXY", "all_proxy")} {
		if endpoint := proxyEndpoint(raw); endpoint != "" && endpoint == addr {
			return true
		}
	}
	return false
}

// envAny returns the first non-empty value among names. httpproxy.Config covers
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY but not ALL_PROXY, which Go's own transport
// also consults for non-http schemes and which users commonly set alone.
func envAny(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// proxyEndpoint normalizes a proxy setting to the "host:port" form DialContext
// receives. It accepts both a full URL ("http://127.0.0.1:7890") and the
// scheme-less form ("127.0.0.1:7890") that Go's transport also tolerates, and
// fills in the scheme's default port when none is given.
func proxyEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// Scheme-less value: url.Parse puts "127.0.0.1:7890" in Scheme+Opaque,
		// so retry with a scheme prefix to get a Host.
		parsed, err = url.Parse("http://" + raw)
		if err != nil || parsed.Host == "" {
			return ""
		}
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}
