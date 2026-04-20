package llm

import "context"

// UnavailableProvider is returned when a provider cannot be initialized.
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
