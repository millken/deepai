package llm

import (
	"context"
	"errors"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// LLMProvider describes the minimal contract implemented by each model backend.
type LLMProvider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// ChatRequest is the provider-agnostic request payload.
type ChatRequest struct {
	Model           string            `json:"model"`
	Messages        []models.Message  `json:"messages"`
	Tools           []models.Tool     `json:"tools,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       *int              `json:"max_tokens,omitempty"`
	SystemPrompt    string            `json:"system_prompt,omitempty"`
	OnChunk         func(StreamChunk) `json:"-"`
	OnPayload       func(provider string, payload []byte) `json:"-"`
}

// ChatResponse is the normalized provider response.
type ChatResponse struct {
	Model   string         `json:"model,omitempty"`
	Message models.Message `json:"message"`
	Usage   Usage          `json:"usage,omitempty"`
	Stop    string         `json:"stop,omitempty"`
}

// StreamChunk is a normalized streaming delta.
type StreamChunk struct {
	Model     string            `json:"model,omitempty"`
	Delta     string            `json:"delta,omitempty"`
	ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
	Message   *models.Message   `json:"message,omitempty"`
	Usage     *Usage            `json:"usage,omitempty"`
	Stop      string            `json:"stop,omitempty"`
	Done      bool              `json:"done,omitempty"`
	Err       error             `json:"-"`
}

// Usage tracks token counts when a provider returns them.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

func (r ChatRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages are required")
	}
	return nil
}
