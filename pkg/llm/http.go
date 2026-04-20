package llm

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// newHTTPClient creates an *http.Client with HTTP/2, connection pooling, and sane timeouts.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
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
		},
	}
}

func maskKey(key string) string {
	if key == "" {
		return "(empty)"
	}
	if len(key) <= 2 {
		return key + "****"
	}
	if len(key) <= 8 {
		return key[:2] + "****"
	}
	return key[:3] + "****" + key[len(key)-4:]
}
