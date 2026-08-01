package llm

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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

// ---------------------------------------------------------------------------
// ModelRegistry — multi-model support
// ---------------------------------------------------------------------------

// ModelDef defines a single model entry in config.yaml. Each entry binds a
// human-friendly alias (Name) to a concrete provider + model pair, with
// optional BaseURL and APIKeyEnv overrides.
type ModelDef struct {
	Name          string `yaml:"name" json:"name"`
	Provider      string `yaml:"provider" json:"provider"`
	Model         string `yaml:"model" json:"model"`
	BaseURL       string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	ContextWindow int    `yaml:"context_window,omitempty" json:"context_window,omitempty"` // 模型级别上下文覆盖，0 表示使用全局默认
	Effort        string `yaml:"effort,omitempty" json:"effort,omitempty"`                 // 模型级别推理深度覆盖，如 "low", "medium", "high"，空表示使用全局默认
}

// ModelRegistry manages a set of named model definitions and lazily creates
// (and caches) the underlying LLMProvider for each unique provider+baseURL+apiKey
// combination. It is safe for concurrent use.
type ModelRegistry struct {
	defs        map[string]ModelDef // key = lowercased Name
	order       []string            // config-order alias names (lowercased)
	defaultName string              // lowercased default alias
	providers   map[string]LLMProvider
	mu          sync.RWMutex
}

// NewModelRegistry builds a registry from defs. defaultName is the alias to use
// when no model is explicitly selected; if empty, the first def's name is used.
// Returns an error if there are zero defs or duplicate aliases.
func NewModelRegistry(defs []ModelDef, defaultName string) (*ModelRegistry, error) {
	if len(defs) == 0 {
		return nil, fmt.Errorf("model registry requires at least one model definition")
	}
	r := &ModelRegistry{
		defs:      make(map[string]ModelDef, len(defs)),
		providers: make(map[string]LLMProvider),
	}
	for _, d := range defs {
		name := strings.ToLower(strings.TrimSpace(d.Name))
		if name == "" {
			return nil, fmt.Errorf("model definition missing name")
		}
		if strings.TrimSpace(d.Provider) == "" {
			return nil, fmt.Errorf("model %q missing provider", d.Name)
		}
		if strings.TrimSpace(d.Model) == "" {
			return nil, fmt.Errorf("model %q missing model", d.Name)
		}
		if _, exists := r.defs[name]; exists {
			return nil, fmt.Errorf("duplicate model alias %q", d.Name)
		}
		r.defs[name] = d
		r.order = append(r.order, name)
	}
	dn := strings.ToLower(strings.TrimSpace(defaultName))
	if dn == "" {
		dn = r.order[0]
	}
	if _, ok := r.defs[dn]; !ok {
		return nil, fmt.Errorf("default model %q not found in registry", defaultName)
	}
	r.defaultName = dn
	return r, nil
}

// NewSingleModelRegistry is a convenience for backward compatibility: it wraps
// a single provider/model into a one-entry registry so callers that expect
// ModelRegistry work unchanged when no multi-model config is present.
// Panics only on programming errors (empty provider), which are impossible
// from the validated Config path.
func NewSingleModelRegistry(provider, model, baseURL string) *ModelRegistry {
	r, err := NewModelRegistry([]ModelDef{{
		Name:     "default",
		Provider: provider,
		Model:    model,
		BaseURL:  baseURL,
	}}, "default")
	if err != nil {
		panic(fmt.Sprintf("NewSingleModelRegistry: %v", err))
	}
	return r
}

// DefaultName returns the default model alias.
func (r *ModelRegistry) DefaultName() string {
	if r == nil {
		return ""
	}
	return r.defaultName
}

// Has reports whether a model alias exists (case-insensitive).
func (r *ModelRegistry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.defs[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Resolve looks up a model definition by alias (case-insensitive).
// Returns the def and true if found.
func (r *ModelRegistry) Resolve(name string) (ModelDef, bool) {
	if r == nil {
		return ModelDef{}, false
	}
	d, ok := r.defs[strings.ToLower(strings.TrimSpace(name))]
	return d, ok
}

// List returns all model definitions in config order.
func (r *ModelRegistry) List() []ModelDef {
	if r == nil {
		return nil
	}
	out := make([]ModelDef, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.defs[name])
	}
	return out
}

// ProviderFor returns the LLMProvider and model name for the given alias.
// The provider is created lazily on first use and cached for subsequent calls.
// If name is empty, the default model is used.
func (r *ModelRegistry) ProviderFor(name string) (LLMProvider, string, error) {
	if r == nil {
		return nil, "", fmt.Errorf("model registry is nil")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = r.defaultName
	}
	def, ok := r.defs[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown model alias %q; available: %s", name, strings.Join(r.order, ", "))
	}

	cacheKey := providerCacheKey(def)
	// Fast path: read lock.
	r.mu.RLock()
	if p, ok := r.providers[cacheKey]; ok {
		r.mu.RUnlock()
		return p, def.Model, nil
	}
	r.mu.RUnlock()

	// Slow path: create provider under write lock.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock.
	if p, ok := r.providers[cacheKey]; ok {
		return p, def.Model, nil
	}
	p, err := buildProviderFromDef(def)
	if err != nil {
		return nil, "", fmt.Errorf("init model %q: %w", def.Name, err)
	}
	r.providers[cacheKey] = p
	return p, def.Model, nil
}

// providerCacheKey builds a cache key that uniquely identifies a provider
// configuration so identical configs reuse the same LLMProvider instance.
func providerCacheKey(def ModelDef) string {
	apiKey := resolveAPIKey(def)
	return strings.ToLower(strings.TrimSpace(def.Provider)) + "|" +
		strings.TrimSpace(def.BaseURL) + "|" + apiKey
}

// ResolveBaseURL returns the effective base URL for a ModelDef after applying
// the same env-var fallback as resolveConfig: def.BaseURL → provider's default
// env var → "". Callers can display this to show where requests are sent.
// When the result is "", the provider SDK uses its own hardcoded default
// (e.g. "https://api.anthropic.com", "https://api.openai.com").
func ResolveBaseURL(def ModelDef) string {
	if strings.TrimSpace(def.BaseURL) != "" {
		return def.BaseURL
	}
	pd, ok := providerDefs[strings.ToLower(strings.TrimSpace(def.Provider))]
	if !ok || pd.baseURLVar == "" {
		return ""
	}
	return strings.TrimSpace(env.Get(pd.baseURLVar, ""))
}

// resolveAPIKey determines the API key for a ModelDef: if APIKeyEnv is set,
// read from that env var; otherwise fall back to the provider's default env var.
func resolveAPIKey(def ModelDef) string {
	if envVar := strings.TrimSpace(def.APIKeyEnv); envVar != "" {
		return env.Get(envVar, "")
	}
	pd, ok := providerDefs[strings.ToLower(strings.TrimSpace(def.Provider))]
	if !ok || pd.apiKeyVar == "" {
		return ""
	}
	return env.Get(pd.apiKeyVar, "")
}

// buildProviderFromDef creates a new LLMProvider from a ModelDef, resolving
// API key and base URL with the same env-var fallback as resolveConfig.
func buildProviderFromDef(def ModelDef) (LLMProvider, error) {
	name := strings.ToLower(strings.TrimSpace(def.Provider))
	rc, err := resolveConfig(name, ProviderConfig{
		APIKey:  resolveAPIKey(def),
		BaseURL: def.BaseURL,
	})
	if err != nil {
		return nil, err
	}
	return buildProvider(name, rc)
}

// InjectProvider pre-populates the provider cache for a given provider+baseURL+apiKey
// combination. This is primarily for testing: it lets tests inject mock providers
// without needing real API keys or network access.
func (r *ModelRegistry) InjectProvider(provider, baseURL, apiKey string, p LLMProvider) {
	if r == nil {
		return
	}
	key := strings.ToLower(strings.TrimSpace(provider)) + "|" +
		strings.TrimSpace(baseURL) + "|" + apiKey
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[key] = p
}
