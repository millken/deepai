package bot

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestHTTPRequestConnectorRedactionAndHostGuards(t *testing.T) {
	const headerVal = "hdr_val_alpha_123"
	const queryVal = "qry_val_beta_456"

	var gotKeyHeader, gotQueryKey string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/echo":
			gotKeyHeader = r.Header.Get("X-Service-Key")
			gotQueryKey = r.URL.Query().Get("app_key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"echo_h":"` + headerVal + `","echo_q":"` + queryVal + `"}`))
		case "/v1/jump":
			http.Redirect(w, r, "http://"+strings.Replace(r.Host, "127.0.0.1", "localhost", 1)+"/v1/echo", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()

	secrets := NewMemorySecretStore()
	ctx := context.Background()
	_, _ = secrets.SetSecret(ctx, "finbot", "FIN_HDR", headerVal)
	_, _ = secrets.SetSecret(ctx, "finbot", "FIN_QRY", queryVal)

	ext := BotExtensions{
		Connectors: []APIConnector{{
			Name:         "fin_api",
			BaseURL:      apiSrv.URL + "/v1",
			AllowedHosts: []string{"localhost"},
			Headers: map[string]string{
				"X-Service-Key": "{{secret:FIN_HDR}}",
			},
			QueryParams: map[string]string{
				"app_key": "{{secret:FIN_QRY}}",
			},
		}},
	}

	tool := HTTPRequestTool()
	turnCtx := withBotTurnEnv(ctx, botTurnEnv{
		botID:      "finbot",
		extensions: ext,
		secrets:    secrets,
		allowLocal: true,
	})

	res, err := tool.Handler(turnCtx, models.ToolCall{
		ID:        "c1",
		Name:      "http_request",
		Arguments: map[string]any{"connector": "fin_api", "url": "/echo"},
	})
	if err != nil || res.Status != models.CallStatusCompleted {
		t.Fatalf("http_request failed: res=%+v err=%v", res, err)
	}
	if gotKeyHeader != headerVal || gotQueryKey != queryVal {
		t.Fatalf("upstream received header=%q query=%q", gotKeyHeader, gotQueryKey)
	}
	if strings.Contains(res.Content, headerVal) || strings.Contains(res.Content, queryVal) {
		t.Fatalf("response leaked secret: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED:FIN_HDR]") || !strings.Contains(res.Content, "[REDACTED:FIN_QRY]") {
		t.Fatalf("expected redaction markers in response: %s", res.Content)
	}

	// Cross-host redirect is refused when secrets are attached.
	redirRes, err := tool.Handler(turnCtx, models.ToolCall{
		ID:        "c2",
		Name:      "http_request",
		Arguments: map[string]any{"connector": "fin_api", "url": "/jump"},
	})
	if err == nil || !strings.Contains(redirRes.Error, "cross-host redirect") {
		t.Fatalf("expected cross-host redirect block, got res=%+v err=%v", redirRes, err)
	}

	// Host allowlist blocks unlisted domains.
	offHostRes, err := tool.Handler(turnCtx, models.ToolCall{
		ID:        "c3",
		Name:      "http_request",
		Arguments: map[string]any{"connector": "fin_api", "url": "https://api.example.com/v1"},
	})
	if err == nil || !strings.Contains(offHostRes.Error, "allowed_hosts") {
		t.Fatalf("expected allowed_hosts block, got res=%+v err=%v", offHostRes, err)
	}

	// Production guards (allowLocal=false): non-HTTPS with secrets + non-public IP/host checks.
	prodCtx := withBotTurnEnv(ctx, botTurnEnv{botID: "finbot", secrets: secrets})
	if _, err := tool.Handler(prodCtx, models.ToolCall{
		ID:   "c4",
		Name: "http_request",
		Arguments: map[string]any{
			"url":     "http://api.example.com/v1",
			"headers": map[string]any{"X-Service-Key": "{{secret:FIN_HDR}}"},
		},
	}); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("expected non-HTTPS block, got err=%v", err)
	}
	if err := validateOutboundHost("localhost", nil, false); err == nil {
		t.Fatal("expected localhost to be blocked when allowLocal=false")
	}
	for _, ip := range []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv4(10, 1, 2, 3),
		net.IPv4(192, 168, 1, 1),
		net.ParseIP("fe80::1"),
	} {
		if err := rejectNonPublicIP(ip); err == nil {
			t.Fatalf("expected non-public IP %s to be rejected", ip)
		}
	}
}
