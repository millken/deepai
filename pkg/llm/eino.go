package llm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/voocel/litellm"
)

// EinoProvider is a thin wrapper around litellm.Client to keep the existing
// construction function name `NewEinoProvider` used elsewhere in the codebase.
type EinoProvider struct {
	provider string
	client   *litellm.Client
}

func NewEinoProvider(name string) (*EinoProvider, error) {
	provider := strings.ToLower(strings.TrimSpace(name))
	if provider == "" {
		provider = "openai"
	}

	// Build provider config from environment variables
	cfg := litellm.ProviderConfig{}
	switch provider {
	case "", "openai":
		cfg.APIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("openai api key is not set")
		}
	case "siliconflow":
		cfg.APIKey = strings.TrimSpace(os.Getenv("SILICONFLOW_API_KEY"))
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("siliconflow api key is not set")
		}
	case "anthropic":
		cfg.APIKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("anthropic api key is not set")
		}
	default:
		return nil, fmt.Errorf("unsupported llm provider %q", provider)
	}

	client, err := litellm.NewWithProvider(provider, cfg)
	if err != nil {
		return nil, fmt.Errorf("init litellm client: %w", err)
	}

	return &EinoProvider{provider: provider, client: client}, nil
}

func (p *EinoProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, err
	}

	// map messages
	msgs := make([]litellm.Message, 0, len(req.Messages))
	if strings.TrimSpace(req.SystemPrompt) != "" {
		msgs = append(msgs, litellm.Message{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		role := "assistant"
		switch m.Role {
		case models.RoleHuman:
			role = "user"
		case models.RoleSystem:
			role = "system"
		case models.RoleTool:
			role = "tool"
		}
		msgs = append(msgs, litellm.Message{Role: role, Content: m.Content})
	}

	litReq := &litellm.Request{Model: req.Model, Messages: msgs}
	resp, err := p.client.Chat(ctx, litReq)
	if err != nil {
		return ChatResponse{}, err
	}

	// Map litellm.Response to ChatResponse. We use the content as the AI message.
	msg := models.Message{Role: models.RoleAI, Content: resp.Content}
	return ChatResponse{Model: req.Model, Message: msg}, nil
}

func (p *EinoProvider) Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	msgs := make([]litellm.Message, 0, len(req.Messages))
	if strings.TrimSpace(req.SystemPrompt) != "" {
		msgs = append(msgs, litellm.Message{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		role := "assistant"
		switch m.Role {
		case models.RoleHuman:
			role = "user"
		case models.RoleSystem:
			role = "system"
		case models.RoleTool:
			role = "tool"
		}
		msgs = append(msgs, litellm.Message{Role: role, Content: m.Content})
	}

	litReq := &litellm.Request{Model: req.Model, Messages: msgs}
	stream, err := p.client.Stream(ctx, litReq)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		defer stream.Close()

		// Collect and emit a single final chunk with the combined content.
		resp, err := litellm.CollectStream(stream)
		if err != nil {
			ch <- StreamChunk{Err: err, Done: true}
			return
		}
		msg := models.Message{Role: models.RoleAI, Content: resp.Content}
		ch <- StreamChunk{Model: req.Model, Message: &msg, Done: true}
	}()

	return ch, nil
}

func ptr[T any](v T) *T { return &v }
