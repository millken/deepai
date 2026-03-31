package llm

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/voocel/litellm"
)

var supportedProviders = []struct {
	name   string
	config litellm.ProviderConfig
}{
	{
		name: "openai",
		config: litellm.ProviderConfig{
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			BaseURL: strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")), // optional, for OpenAI-compatible providers
			// other config fields can be added here as needed
		},
	},
	//anthropic bedrock deepseek gemini glm grok ollama openai openrouter qwen
	{
		name: "anthropic",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
		},
	},
	{
		name: "qwen",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("QWEN_API_KEY"),
		},
	},
	{
		name: "gemini",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("GEMINI_API_KEY"),
		},
	},
	{
		name: "groq",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("GROQ_API_KEY"),
		},
	},
	{
		name: "ollama",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("OLLAMA_API_KEY"),
		},
	},
	{
		name: "glm",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("GLM_API_KEY"),
		},
	},
	{
		name: "bedrock",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("BEDROCK_API_KEY"),
		},
	},
	{
		name: "deepseek",
		config: litellm.ProviderConfig{
			APIKey: os.Getenv("DEEPSEEK_API_KEY"),
		},
	},
}

func NewProvider(name string) LLMProvider {
	provider := strings.ToLower(strings.TrimSpace(name))
	var cfg litellm.ProviderConfig
	if slices.ContainsFunc(supportedProviders, func(p struct {
		name   string
		config litellm.ProviderConfig
	}) bool {
		if p.name == provider {
			cfg = p.config
			return true
		}
		return false
	}) {
		p, err := NewLitellmProvider(provider, cfg)
		if err != nil {
			return &UnavailableProvider{err: fmt.Errorf("init provider %q: %w", provider, err)}
		}
		return p
	}
	return &UnavailableProvider{err: fmt.Errorf("unsupported provider %q", provider)}
}

// NewProviderWithOptions creates a provider like NewProvider but allows passing
// extra litellm client options (e.g. WithOnPayload) that will be applied when
// constructing the underlying litellm client.
func NewProviderWithOptions(name string, opts ...litellm.ClientOption) LLMProvider {
	provider := strings.ToLower(strings.TrimSpace(name))
	var cfg litellm.ProviderConfig
	if slices.ContainsFunc(supportedProviders, func(p struct {
		name   string
		config litellm.ProviderConfig
	}) bool {
		if p.name == provider {
			cfg = p.config
			return true
		}
		return false
	}) {
		p, err := NewLitellmProvider(provider, cfg, opts...)
		if err != nil {
			return &UnavailableProvider{err: fmt.Errorf("init provider %q: %w", provider, err)}
		}
		return p
	}
	return &UnavailableProvider{err: fmt.Errorf("unsupported provider %q", provider)}
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
