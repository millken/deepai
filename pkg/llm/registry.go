package llm

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dnsoa/go/env"
)

// ProviderConfig holds provider initialization parameters.
type ProviderConfig struct {
	APIKey  string
	BaseURL string
}

// providerDef holds per-provider static config.
type providerDef struct {
	apiKeyVar  string
	baseURLVar string
	kind       string // "anthropic" or "openai"
}

var providerDefs = map[string]providerDef{
	"openai":        {"OPENAI_API_KEY", "OPENAI_BASE_URL", "openai"},
	"anthropic":     {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "anthropic"},
	"qwen":          {"QWEN_API_KEY", "", "openai"},
	"gemini":        {"GEMINI_API_KEY", "", "openai"},
	"groq":          {"GROQ_API_KEY", "", "openai"},
	"ollama":        {"OLLAMA_API_KEY", "", "openai"},
	"glm":           {"GLM_API_KEY", "", "openai"},
	"bedrock":       {"BEDROCK_API_KEY", "", "openai"},
	"deepseek":      {"DEEPSEEK_API_KEY", "", "openai"},
	"openai-compat": {"OPENAI_API_KEY", "OPENAI_BASE_URL", "openai"},
}

// resolvedConfig holds the resolved provider configuration.
type resolvedConfig struct {
	apiKey     string
	baseURL    string
	kind       string
	httpClient *http.Client
}

func resolveConfig(name string, overrides ProviderConfig) (resolvedConfig, error) {
	def, ok := providerDefs[name]
	if !ok {
		return resolvedConfig{}, fmt.Errorf("unsupported provider %q", name)
	}
	apiKey := overrides.APIKey
	if apiKey == "" && def.apiKeyVar != "" {
		apiKey = env.Get(def.apiKeyVar, "")
	}
	baseURL := overrides.BaseURL
	if baseURL == "" && def.baseURLVar != "" {
		baseURL = strings.TrimSpace(env.Get(def.baseURLVar, ""))
	}
	return resolvedConfig{
		apiKey:     apiKey,
		baseURL:    baseURL,
		kind:       def.kind,
		httpClient: newHTTPClient(),
	}, nil
}

// NewProvider creates a provider using env vars read at call time.
func NewProvider(name string) LLMProvider {
	name = strings.ToLower(strings.TrimSpace(name))
	cfg, err := resolveConfig(name, ProviderConfig{})
	if err != nil {
		return &UnavailableProvider{err: err}
	}
	p, err := buildProvider(name, cfg)
	if err != nil {
		return &UnavailableProvider{err: fmt.Errorf("init provider %q: %w", name, err)}
	}
	return p
}

// NewProviderFromConfig creates a provider using explicit config values.
// Falls back to env vars for any field not provided.
func NewProviderFromConfig(name string, cfg ProviderConfig) (LLMProvider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	rc, err := resolveConfig(name, cfg)
	if err != nil {
		return nil, err
	}
	slog.Debug("provider config resolved",
		"provider", name,
		"api_key", maskKey(rc.apiKey),
		"base_url", rc.baseURL,
	)
	return buildProvider(name, rc)
}

func buildProvider(name string, cfg resolvedConfig) (LLMProvider, error) {
	switch cfg.kind {
	case "anthropic":
		return NewAnthropicProvider(cfg.apiKey, cfg.baseURL, cfg.httpClient)
	case "openai":
		return NewOpenAICompatProvider(name, cfg.apiKey, cfg.baseURL, cfg.httpClient)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", cfg.kind)
	}
}
