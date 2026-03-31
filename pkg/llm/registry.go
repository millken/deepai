package llm

import (
	"context"
	"os"
	"strings"
)

func NewProvider(name string) LLMProvider {
	provider := strings.TrimSpace(name)
	if provider == "" {
		provider = strings.TrimSpace(os.Getenv("DEFAULT_LLM_PROVIDER"))
	}

	p, err := NewEinoProvider(provider)
	if err != nil {
		return &UnavailableProvider{err: err}
	}
	return p
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
