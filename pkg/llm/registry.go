package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dnsoa/go/env"
	"github.com/voocel/litellm"
)

// ProviderConfig holds provider initialization parameters.
// It decouples callers from litellm-specific types.
type ProviderConfig struct {
	APIKey  string
	BaseURL string
}

// defaultResilience is the baseline resilience config applied to all providers.
var defaultResilience = litellm.ResilienceConfig{
	MaxRetries:     3,
	InitialDelay:   1 * time.Second,
	MaxDelay:       30 * time.Second,
	Multiplier:     2.0,
	Jitter:         true,
	RequestTimeout: 5 * time.Minute,
	ConnectTimeout: 10 * time.Second,
}

// providerDef holds per-provider static config and env var mappings.
var providerDef = map[string]struct {
	apiKeyVar  string
	baseURLVar string
	resilience litellm.ResilienceConfig
}{
	"openai":        {"OPENAI_API_KEY", "OPENAI_BASE_URL", defaultResilience},
	"anthropic":     {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", defaultResilience},
	"qwen":          {"QWEN_API_KEY", "", defaultResilience},
	"gemini":        {"GEMINI_API_KEY", "", defaultResilience},
	"groq":          {"GROQ_API_KEY", "", defaultResilience},
	"ollama":        {"OLLAMA_API_KEY", "", defaultResilience},
	"glm":           {"GLM_API_KEY", "", defaultResilience},
	"bedrock":       {"BEDROCK_API_KEY", "", defaultResilience},
	"deepseek":      {"DEEPSEEK_API_KEY", "", defaultResilience},
	"openai-compat": {"OPENAI_API_KEY", "OPENAI_BASE_URL", defaultResilience},
}

// newHTTP2Client creates an HTTP client with HTTP/2, connection pooling, and retry.
func newHTTP2Client(cfg litellm.ResilienceConfig) *retryClient {
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &retryClient{
		client: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		config: cfg,
	}
}

// retryClient wraps http.Client with retry, exponential backoff, and jitter.
// Implements the HTTPDoer interface expected by litellm providers.
type retryClient struct {
	client *http.Client
	config litellm.ResilienceConfig
}

func (c *retryClient) Do(req *http.Request) (*http.Response, error) {
	var lastErr error
	var originalBody []byte

	if req.Body != nil && req.GetBody == nil {
		var err error
		originalBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}
		req.Body.Close()
	}

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("failed to get request body: %w", err)
			}
			req.Body = body
		} else if originalBody != nil {
			req.Body = io.NopCloser(bytes.NewReader(originalBody))
		}

		resp, err := c.client.Do(req)
		if err == nil && !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		if err != nil {
			lastErr = err
		} else {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || len(bodyBytes) == 0 {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			} else {
				lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			}
		}

		if attempt == c.config.MaxRetries {
			break
		}
		if err == nil && !isRetryableStatus(resp.StatusCode) {
			break
		}
		if err != nil && !isRetryableErr(err) {
			break
		}

		delay := c.calculateDelay(attempt)
		slog.Debug("retry request", "attempt", attempt+1, "delay", delay, "err", lastErr)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		}
	}

	return nil, lastErr
}

func (c *retryClient) calculateDelay(attempt int) time.Duration {
	delay := float64(c.config.InitialDelay) * math.Pow(c.config.Multiplier, float64(attempt))
	if delay > float64(c.config.MaxDelay) {
		delay = float64(c.config.MaxDelay)
	}
	if c.config.Jitter {
		delay += delay * 0.25 * (2*rand.Float64() - 1)
		if delay < 0 {
			delay = float64(c.config.InitialDelay)
		}
	}
	return time.Duration(delay)
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, 529,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func isRetryableErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	return false
}

// buildConfig resolves API key and base URL from env vars at call time,
// then merges with optional explicit overrides and static resilience settings.
func buildConfig(name string, overrides ProviderConfig) (litellm.ProviderConfig, error) {
	def, ok := providerDef[name]
	if !ok {
		return litellm.ProviderConfig{}, fmt.Errorf("unsupported provider %q", name)
	}

	apiKey := overrides.APIKey
	if apiKey == "" && def.apiKeyVar != "" {
		apiKey = env.Get(def.apiKeyVar, "")
	}
	baseURL := overrides.BaseURL
	if baseURL == "" && def.baseURLVar != "" {
		baseURL = strings.TrimSpace(env.Get(def.baseURLVar, ""))
	}

	return litellm.ProviderConfig{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		Resilience: def.resilience,
		HTTPClient: newHTTP2Client(def.resilience),
	}, nil
}

// NewProvider creates a provider using env vars read at call time.
func NewProvider(name string) LLMProvider {
	name = strings.ToLower(strings.TrimSpace(name))
	cfg, err := buildConfig(name, ProviderConfig{})
	if err != nil {
		return &UnavailableProvider{err: err}
	}
	p, err := NewLitellmProvider(name, cfg)
	if err != nil {
		return &UnavailableProvider{err: fmt.Errorf("init provider %q: %w", name, err)}
	}
	return p
}

// NewProviderWithOptions creates a provider like NewProvider but allows passing
// extra litellm client options (e.g. WithOnPayload) that will be applied when
// constructing the underlying litellm client.
func NewProviderWithOptions(name string, opts ...litellm.ClientOption) LLMProvider {
	name = strings.ToLower(strings.TrimSpace(name))
	cfg, err := buildConfig(name, ProviderConfig{})
	if err != nil {
		return &UnavailableProvider{err: err}
	}
	p, err := NewLitellmProvider(name, cfg, opts...)
	if err != nil {
		return &UnavailableProvider{err: fmt.Errorf("init provider %q: %w", name, err)}
	}
	return p
}

// NewProviderFromConfig creates a provider using explicit config values.
// Falls back to env vars for any field not provided.
func NewProviderFromConfig(name string, cfg ProviderConfig) (LLMProvider, error) {
	name = strings.ToLower(strings.TrimSpace(name))

	litCfg, err := buildConfig(name, cfg)
	if err != nil {
		return nil, err
	}

	slog.Debug("provider config resolved",
		"provider", name,
		"api_key", maskKey(litCfg.APIKey),
		"base_url", litCfg.BaseURL,
	)

	return NewLitellmProvider(name, litCfg)
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

type UnavailableProvider struct {
	err error
}

func (p *UnavailableProvider) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, p.err
}

func (p *UnavailableProvider) Stream(_ context.Context, _ ChatRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Err: p.err, Done: true}
	close(ch)
	return ch, nil
}
