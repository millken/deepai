package llm

import (
	"context"
	"fmt"
	"log/slog"
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

// providerDef holds per-provider static config and env var mappings.
var providerDef = map[string]struct {
	apiKeyVar  string
	baseURLVar string
	resilience litellm.ResilienceConfig
}{
	"openai": {"OPENAI_API_KEY", "OPENAI_BASE_URL", litellm.ResilienceConfig{
		RequestTimeout: 5 * time.Minute,
		ConnectTimeout: 10 * time.Second,
		MaxRetries:     1,
	}},
	"anthropic": {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", litellm.ResilienceConfig{}},
	"qwen":      {"QWEN_API_KEY", "", litellm.ResilienceConfig{}},
	"gemini":    {"GEMINI_API_KEY", "", litellm.ResilienceConfig{}},
	"groq":      {"GROQ_API_KEY", "", litellm.ResilienceConfig{}},
	"ollama":    {"OLLAMA_API_KEY", "", litellm.ResilienceConfig{}},
	"glm":       {"GLM_API_KEY", "", litellm.ResilienceConfig{}},
	"bedrock":   {"BEDROCK_API_KEY", "", litellm.ResilienceConfig{}},
	"deepseek":  {"DEEPSEEK_API_KEY", "", litellm.ResilienceConfig{}},
	"openai-compat": {"OPENAI_API_KEY", "OPENAI_BASE_URL", litellm.ResilienceConfig{
		RequestTimeout: 5 * time.Minute,
		ConnectTimeout: 10 * time.Second,
		MaxRetries:     1,
	}},
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
