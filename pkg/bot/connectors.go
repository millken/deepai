package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/netutil"
)

const (
	defaultHTTPRequestTimeout  = 30 * time.Second
	defaultHTTPRequestMaxChars = 16000
	maxHTTPRequestMaxChars     = 100000
	maxHTTPResponseReadBytes   = 2 << 20 // 2 MiB raw read cap before truncation
)

var secretPlaceholderRE = regexp.MustCompile(`\{\{secret:([A-Za-z_][A-Za-z0-9_]{0,63})\}\}`)

type botTurnEnvKey struct{}

type botTurnEnv struct {
	botID            string
	extensions       BotExtensions
	disableSelfTrain bool
	secrets          SecretStore
	bots             BotRegistry
	mutateBot        func(ctx context.Context, botID string, mutate func(*BotProfile) ([]string, error)) (BotProfile, error)
	botDataDir       string
	audit            *AuditLogger
	onBotSaved       func(BotProfile)
	allowLocal       bool
}

func withBotTurnEnv(ctx context.Context, env botTurnEnv) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, botTurnEnvKey{}, env)
}

func botTurnEnvFromContext(ctx context.Context) (botTurnEnv, bool) {
	if ctx == nil {
		return botTurnEnv{}, false
	}
	env, ok := ctx.Value(botTurnEnvKey{}).(botTurnEnv)
	return env, ok
}

// HTTPRequestTool builds the controlled `http_request` tool for calling
// third-party REST/GraphQL APIs without shell access, supporting per-bot
// APIConnector profiles, host-side `{{secret:NAME}}` credential injection,
// response secret redaction, and private-network SSRF protection.
func HTTPRequestTool() models.Tool {
	return models.Tool{
		Name:         "http_request",
		Description:  "Make an HTTP request to a third-party REST or GraphQL API without using bash. Supports named bot connectors and host-injected '{{secret:NAME}}' credential placeholders (only allowed when a named 'connector' is specified so credentials stay bound to its base_url/allowed_hosts).",
		Groups:       []string{"builtin", "web", "api"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connector": map[string]any{
					"type":        "string",
					"description": "Name of a configured APIConnector on this bot (inherits base_url, default headers/query_params, and allowed_hosts; required whenever '{{secret:NAME}}' is referenced).",
				},
				"method": map[string]any{
					"type":        "string",
					"description": "HTTP method: GET, POST, PUT, PATCH, DELETE, or HEAD (default: GET).",
				},
				"url": map[string]any{
					"type":        "string",
					"description": "Full https:// URL, or a relative path (e.g. '/v1/pages') when 'connector' is specified.",
				},
				"headers": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional request headers (may reference '{{secret:NAME}}' when 'connector' is set).",
				},
				"query": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional URL query parameters (may reference '{{secret:NAME}}' when 'connector' is set).",
				},
				"json_body": map[string]any{
					"description": "Optional JSON payload (object, array, or primitive). Automatically sets Content-Type: application/json.",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional raw text request body (ignored if json_body is provided).",
				},
				"max_chars": map[string]any{
					"type":        "integer",
					"description": "Maximum response characters to return (default 16000, max 100000).",
				},
			},
			"required": []any{"url"},
		},
		Handler: handleHTTPRequestTool,
	}
}

func valueContainsSecretPlaceholder(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "{{secret:")
	case map[string]any:
		for _, val := range x {
			if valueContainsSecretPlaceholder(val) {
				return true
			}
		}
	case map[string]string:
		for _, val := range x {
			if strings.Contains(val, "{{secret:") {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if valueContainsSecretPlaceholder(item) {
				return true
			}
		}
	}
	return false
}

func resolveTurnConnectors(ctx context.Context, env botTurnEnv, botID string) []APIConnector {
	if env.bots != nil {
		if liveBot, err := env.bots.GetBot(ctx, botID); err == nil {
			return liveBot.Extensions.Connectors
		}
	}
	return env.extensions.Connectors
}

func handleHTTPRequestTool(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusFailed,
			Error:    err.Error(),
		}, err
	}

	env, _ := botTurnEnvFromContext(ctx)
	botID := effectiveBotID(env.botID)

	rawURL, _ := call.Arguments["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fail(errors.New("url is required"))
	}

	method := "GET"
	if m, _ := call.Arguments["method"].(string); strings.TrimSpace(m) != "" {
		method = strings.ToUpper(strings.TrimSpace(m))
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		return fail(fmt.Errorf("unsupported HTTP method %q", method))
	}

	connectorName, _ := call.Arguments["connector"].(string)
	connectorName = strings.TrimSpace(connectorName)

	var conn *APIConnector
	if connectorName != "" {
		connectors := resolveTurnConnectors(ctx, env, botID)
		for i := range connectors {
			if strings.EqualFold(connectors[i].Name, connectorName) {
				cp := connectors[i].Clone()
				conn = &cp
				break
			}
		}
		if conn == nil {
			return fail(fmt.Errorf("unknown connector %q on bot %q", connectorName, botID))
		}
	} else if valueContainsSecretPlaceholder(call.Arguments) {
		return fail(errors.New("referencing '{{secret:NAME}}' requires specifying a configured 'connector' so credentials stay restricted to its base_url and allowed_hosts"))
	}

	// Secret expansion table collected during this call so any echoed secret is
	// scrubbed from responses and error strings before reaching the model.
	resolvedSecrets := map[string]string{}
	expand := func(s string) (string, error) {
		return expandSecrets(ctx, env, conn, nil, s, resolvedSecrets)
	}
	redact := func(s string) string {
		if len(resolvedSecrets) == 0 || s == "" {
			return s
		}
		// Redact longer secrets first so prefix overlaps never leak suffixes.
		names := make([]string, 0, len(resolvedSecrets))
		for k := range resolvedSecrets {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool {
			return len(resolvedSecrets[names[i]]) > len(resolvedSecrets[names[j]])
		})
		for _, k := range names {
			if v := resolvedSecrets[k]; v != "" {
				s = strings.ReplaceAll(s, v, "[REDACTED:"+k+"]")
			}
		}
		return s
	}

	// Build target URL (resolving against connector BaseURL when relative).
	expandedURL, err := expand(rawURL)
	if err != nil {
		return fail(err)
	}
	targetURL, allowedHosts, err := buildConnectorTargetURL(expandedURL, conn)
	if err != nil {
		return fail(errors.New(redact(err.Error())))
	}

	// Merge query params (connector defaults overridden by call-level query).
	mergedQuery := map[string]string{}
	if conn != nil {
		for k, v := range conn.QueryParams {
			mergedQuery[k] = v
		}
	}
	if rawQ, ok := call.Arguments["query"].(map[string]any); ok {
		for k, v := range rawQ {
			mergedQuery[k] = fmt.Sprint(v)
		}
	}
	if len(mergedQuery) > 0 {
		q := targetURL.Query()
		for k, rawV := range mergedQuery {
			ev, err := expand(rawV)
			if err != nil {
				return fail(err)
			}
			q.Set(k, ev)
		}
		targetURL.RawQuery = q.Encode()
	}

	// Merge headers (connector defaults overridden by call-level headers).
	mergedHeaders := map[string]string{}
	if conn != nil {
		for k, v := range conn.Headers {
			mergedHeaders[k] = v
		}
	}
	if rawH, ok := call.Arguments["headers"].(map[string]any); ok {
		for k, v := range rawH {
			mergedHeaders[k] = fmt.Sprint(v)
		}
	}
	finalHeaders := http.Header{}
	finalHeaders.Set("User-Agent", "deepai-bot/1.0")
	for k, rawV := range mergedHeaders {
		ev, err := expand(rawV)
		if err != nil {
			return fail(err)
		}
		finalHeaders.Set(k, ev)
	}

	// Build body (json_body takes precedence over raw body).
	var bodyBytes []byte
	if jb, exists := call.Arguments["json_body"]; exists && jb != nil {
		rawJSON, err := json.Marshal(jb)
		if err != nil {
			return fail(fmt.Errorf("marshal json_body: %w", err))
		}
		expandedBody, err := expand(string(rawJSON))
		if err != nil {
			return fail(err)
		}
		bodyBytes = []byte(expandedBody)
		if finalHeaders.Get("Content-Type") == "" {
			finalHeaders.Set("Content-Type", "application/json")
		}
	} else if rb, ok := call.Arguments["body"].(string); ok && rb != "" {
		expandedBody, err := expand(rb)
		if err != nil {
			return fail(err)
		}
		bodyBytes = []byte(expandedBody)
	}

	hasSecrets := len(resolvedSecrets) > 0
	if hasSecrets && (conn == nil || len(allowedHosts) == 0) {
		return fail(errors.New("refusing to send secret credentials without a named connector host allowlist"))
	}
	// Enforce HTTPS whenever secrets are attached unless running in a local test harness.
	if hasSecrets && targetURL.Scheme != "https" && !env.allowLocal {
		return fail(errors.New("refusing to send secret credentials over non-HTTPS URL"))
	}
	if err := validateOutboundHostWithSecrets(targetURL.Hostname(), allowedHosts, hasSecrets, env.allowLocal); err != nil {
		return fail(errors.New(redact(err.Error())))
	}

	timeout := defaultHTTPRequestTimeout
	if conn != nil && conn.TimeoutSec > 0 {
		timeout = time.Duration(conn.TimeoutSec) * time.Second
	}
	client := newSafeConnectorHTTPClient(timeout, allowedHosts, hasSecrets, env.allowLocal)

	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL.String(), bodyReader)
	if err != nil {
		return fail(errors.New(redact(err.Error())))
	}
	req.Header = finalHeaders

	resp, err := client.Do(req)
	if err != nil {
		return fail(errors.New(redact(err.Error())))
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseReadBytes))
	if err != nil {
		return fail(errors.New(redact(err.Error())))
	}

	maxChars := defaultHTTPRequestMaxChars
	if rawMax, ok := call.Arguments["max_chars"].(float64); ok && rawMax > 0 {
		maxChars = int(rawMax)
	} else if rawMaxInt, ok := call.Arguments["max_chars"].(int); ok && rawMaxInt > 0 {
		maxChars = rawMaxInt
	}
	if maxChars > maxHTTPRequestMaxChars {
		maxChars = maxHTTPRequestMaxChars
	}

	bodyText := redact(string(respBytes))
	truncated := false
	if runes := []rune(bodyText); len(runes) > maxChars {
		bodyText = string(runes[:maxChars])
		truncated = true
	}

	outPayload := map[string]any{
		"status":       resp.StatusCode,
		"status_text":  resp.Status,
		"content_type": resp.Header.Get("Content-Type"),
		"truncated":    truncated,
		"body":         bodyText,
	}
	encoded, _ := json.Marshal(outPayload)
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  string(encoded),
	}, nil
}

func expandSecrets(ctx context.Context, env botTurnEnv, conn *APIConnector, allowedHosts []string, s string, resolvedSecrets map[string]string) (string, error) {
	if !strings.Contains(s, "{{secret:") {
		return s, nil
	}
	if conn == nil && len(allowedHosts) == 0 {
		return "", errors.New("referencing '{{secret:NAME}}' requires specifying a configured 'connector'")
	}
	if env.secrets == nil {
		return "", errors.New("secret store is not configured on this runtime")
	}
	botID := effectiveBotID(env.botID)
	var firstErr error
	expanded := secretPlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		sub := secretPlaceholderRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		secName := sub[1]
		if val, cached := resolvedSecrets[secName]; cached {
			return val
		}
		val, err := env.secrets.GetSecret(ctx, botID, secName)
		if err != nil {
			firstErr = err
			return match
		}
		resolvedSecrets[secName] = val
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return expanded, nil
}

func buildConnectorTargetURL(expandedURL string, conn *APIConnector) (*url.URL, []string, error) {
	if conn == nil {
		u, err := url.Parse(expandedURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, nil, errors.New("url must be a valid http(s) URL")
		}
		return u, nil, nil
	}

	base, err := url.Parse(strings.TrimSpace(conn.BaseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return nil, nil, fmt.Errorf("connector %q has invalid base_url", conn.Name)
	}

	allowed := make([]string, 0, len(conn.AllowedHosts)+1)
	allowed = append(allowed, strings.ToLower(base.Hostname()))
	for _, h := range conn.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			allowed = append(allowed, h)
		}
	}

	var target *url.URL
	if strings.HasPrefix(expandedURL, "http://") || strings.HasPrefix(expandedURL, "https://") {
		target, err = url.Parse(expandedURL)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid url: %w", err)
		}
	} else {
		rel, err := url.Parse(expandedURL)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid relative path: %w", err)
		}
		baseCopy := *base
		if strings.HasPrefix(rel.Path, "/") && baseCopy.Path != "" && baseCopy.Path != "/" {
			baseCopy.Path = strings.TrimRight(baseCopy.Path, "/") + rel.Path
			rel.Path = ""
			target = &baseCopy
			target.RawQuery = rel.RawQuery
		} else {
			target = baseCopy.ResolveReference(rel)
		}
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, nil, errors.New("url scheme must be http or https")
	}
	return target, allowed, nil
}

func hostMatchesAllowlist(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, pat := range allowed {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if host == pat {
			return true
		}
	}
	return false
}

func validateOutboundHost(hostname string, allowed []string, allowLocal bool) error {
	return validateOutboundHostWithSecrets(hostname, allowed, false, allowLocal)
}

func validateOutboundHostWithSecrets(hostname string, allowed []string, hasSecrets, allowLocal bool) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return errors.New("empty target host")
	}
	if hasSecrets && len(allowed) == 0 {
		return errors.New("secret-bearing requests require an explicit connector host allowlist")
	}
	if !hostMatchesAllowlist(hostname, allowed) {
		return fmt.Errorf("host %q is not in connector allowed_hosts", hostname)
	}
	if allowLocal {
		return nil
	}
	lower := strings.ToLower(hostname)
	if lower == "localhost" || lower == "ip6-localhost" || lower == "ip6-loopback" {
		return fmt.Errorf("loopback host %q is blocked", hostname)
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return rejectNonPublicIP(ip)
	}
	return nil
}

func rejectNonPublicIP(ip net.IP) error {
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("unspecified address %s is blocked", ip)
	case ip.IsLoopback():
		return fmt.Errorf("loopback address %s is blocked", ip)
	case ip.To4() != nil && ip.Equal(net.IPv4(169, 254, 169, 254)):
		return fmt.Errorf("cloud metadata address %s is blocked", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("link-local address %s is blocked", ip)
	case ip.IsPrivate():
		return fmt.Errorf("private network address %s is blocked", ip)
	}
	return nil
}

func newSafeConnectorHTTPClient(timeout time.Duration, allowedHosts []string, hasSecrets, allowLocal bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: netutil.EnvProxyFunc,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !allowLocal && !netutil.IsEnvProxyAddr(addr) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("resolve host %q: %w", host, err)
				}
				for _, ip := range ips {
					if err := rejectNonPublicIP(ip.IP); err != nil {
						return nil, fmt.Errorf("host %q resolved to blocked address: %w", host, err)
					}
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if len(via) > 0 && hasSecrets && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				return errors.New("refusing cross-host redirect when secret credentials are attached")
			}
			if err := validateOutboundHostWithSecrets(req.URL.Hostname(), allowedHosts, hasSecrets, allowLocal); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
}
