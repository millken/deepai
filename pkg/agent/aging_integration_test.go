package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// captureProvider records the Messages of the first Stream request, then ends
// the run with a plain AI reply.
type captureProvider struct {
	mu       sync.Mutex
	captured []models.Message
}

func (p *captureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *captureProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	if p.captured == nil {
		// Deep-copy Content so a later canonical read can't mask a shared-buffer bug.
		p.captured = make([]models.Message, len(req.Messages))
		copy(p.captured, req.Messages)
	}
	p.mu.Unlock()
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *captureProvider) seen() []models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.captured
}

// TestAging_EndToEnd proves the provider receives an aged (compressed) view
// while the canonical run messages returned to the caller stay full-fidelity.
func TestAging_EndToEnd(t *testing.T) {
	big := strings.Repeat("x", 10000)
	history := []models.Message{
		{Role: models.RoleHuman, Content: "explore the repo"},
		aiTools(""),                 // aiTurnIndex 0
		toolMsg("read_file", big),   // owner 0 -> age 1 in the view -> compressed
		aiTools(""),                 // aiTurnIndex 1 (latest)
		toolMsg("grep", "small hit"), // owner 1 -> age 0 -> untouched
	}

	p := &captureProvider{}
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         tools.NewRegistry(),
		Model:         "m",
		ContextWindow: 100000,
		Aging: &AgingConfig{
			Enabled:             true,
			MinContextPressure:  0, // gate off so aging is deterministic here
			ConversationBudgets: map[int]int{}, // T1 only
		},
	})

	result, err := a.Run(context.Background(), "sess-1", history)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := p.seen()
	if seen == nil {
		t.Fatal("provider never received a request")
	}
	// The view the provider saw: the age-1 read_file result is compressed.
	viewTool := seen[2]
	if len(viewTool.Content) >= 10000 {
		t.Errorf("provider should see compressed read_file result, got %d bytes", len(viewTool.Content))
	}
	if !strings.Contains(viewTool.Content, "re-call read_file") {
		t.Errorf("compressed view should carry re-call hint, got: %q", viewTool.Content)
	}

	// The canonical messages returned to the caller are NOT compressed.
	if got := len(result.Messages[2].Content); got != 10000 {
		t.Errorf("canonical read_file result must stay full (%d bytes), got %d", 10000, got)
	}
}

// TestAging_DisabledByDefault confirms that without an Aging config the provider
// sees the untouched history (backward compatibility).
func TestAging_DisabledByDefault(t *testing.T) {
	clearTokenEnv(t) // isolate from ambient DEEPAI_TOKEN_* exports
	big := strings.Repeat("x", 10000)
	history := []models.Message{
		{Role: models.RoleHuman, Content: "q"},
		aiTools(""),
		toolMsg("read_file", big),
		aiTools(""),
		toolMsg("grep", "s"),
	}

	p := &captureProvider{}
	a := New(AgentConfig{LLMProvider: p, Tools: tools.NewRegistry(), Model: "m", ContextWindow: 100000})

	if _, err := a.Run(context.Background(), "sess-2", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(p.seen()[2].Content); got != 10000 {
		t.Errorf("aging disabled: provider should see full result, got %d bytes", got)
	}
}
