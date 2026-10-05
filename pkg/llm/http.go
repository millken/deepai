package llm

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/millken/deepai/pkg/netutil"
)

// newHTTPClient creates an *http.Client with HTTP/2, connection pooling, and sane timeouts.
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		// Honor HTTP_PROXY / HTTPS_PROXY / NO_PROXY. A hand-built Transport
		// gets no proxy support unless this is set — only http.DefaultTransport
		// comes with it — so without this line every model API call ignores the
		// user's proxy. netutil.EnvProxyFunc rather than
		// http.ProxyFromEnvironment because the stdlib version snapshots the
		// environment process-wide on first use.
		Proxy:             netutil.EnvProxyFunc,
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// ResponseHeaderTimeout bounds only the wait for the FIRST response
		// byte — dial, TLS, and the server thinking before it starts
		// streaming. Past that first byte the stream's own idle window
		// (agent streamIdleTimeout, 2m by default) is the bound, so a
		// legitimately long stream is never cut. Without this, a request
		// that connects but never produces a first chunk can hang until the
		// outer deadline — or forever when there is none: an unattended
		// overnight mission once sat silent for 8h48m on exactly that.
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	return &http.Client{
		// Do not set Client.Timeout for streaming LLM responses.
		// A fixed client timeout aborts long reads with:
		// "context deadline exceeded (Client.Timeout or context cancellation while reading body)".
		// Run lifetime is bounded by the caller context (agent RequestTimeout / Ctrl+C).
		Timeout: 0,
		// newTracingHTTPTransport returns transport completely unwrapped when
		// DEEPAI_STREAM_TRACE_FILE is unset (the default) — see
		// stream_trace.go and TestNewHTTPClient_TraceDisabledByDefault.
		Transport: newTracingHTTPTransport(transport),
	}
}
