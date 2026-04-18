package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

type mockMemoryLLMProvider struct {
	chatResp llm.ChatResponse
	chatErr  error
	lastReq  llm.ChatRequest
}

func (m *mockMemoryLLMProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	m.lastReq = req
	if m.chatErr != nil {
		return llm.ChatResponse{}, m.chatErr
	}
	return m.chatResp, nil
}

func (m *mockMemoryLLMProvider) Stream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain json",
			raw:  `{"facts":[]}`,
			want: `{"facts":[]}`,
		},
		{
			name: "fenced json",
			raw:  "```json\n{\"facts\":[]}\n```",
			want: `{"facts":[]}`,
		},
		{
			name: "prefixed reasoning then json",
			raw:  "First, I will analyze the conversation.\n{\"facts\":[]}",
			want: `{"facts":[]}`,
		},
		{
			name: "no json",
			raw:  "First, I will analyze the conversation.",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.raw)
			if got != tt.want {
				t.Fatalf("extractJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLLMClientExtractUpdate_DisableReasoningEffort(t *testing.T) {
	provider := &mockMemoryLLMProvider{
		chatResp: llm.ChatResponse{Message: models.Message{Role: models.RoleAI, Content: `{"facts":[]}`}},
	}
	client := NewLLMClient(provider, "test-model")

	_, err := client.ExtractUpdate(context.Background(), Document{SessionID: "s1"}, []models.Message{{Role: models.RoleHuman, Content: "hello"}})
	if err != nil {
		t.Fatalf("ExtractUpdate() error = %v", err)
	}
	if provider.lastReq.ReasoningEffort != "disabled" {
		t.Fatalf("ReasoningEffort = %q, want %q", provider.lastReq.ReasoningEffort, "disabled")
	}
}

func TestPreviewForLog(t *testing.T) {
	got := previewForLog("First line\nSecond line", 200)
	if got != "First line\\nSecond line" {
		t.Fatalf("previewForLog() = %q", got)
	}

	truncated := previewForLog("abcdef", 3)
	if truncated != "abc..." {
		t.Fatalf("previewForLog() truncated = %q", truncated)
	}
}
