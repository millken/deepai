package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

type LLMClient struct {
	provider    llm.LLMProvider
	model       string
	temperature *float64
	maxTokens   *int
}

func NewLLMClient(provider llm.LLMProvider, model string) *LLMClient {
	return &LLMClient{
		provider: provider,
		model:    strings.TrimSpace(model),
	}
}

func (c *LLMClient) WithTemperature(v float64) *LLMClient {
	if c != nil {
		c.temperature = &v
	}
	return c
}

func (c *LLMClient) WithMaxTokens(v int) *LLMClient {
	if c != nil {
		c.maxTokens = &v
	}
	return c
}

func (c *LLMClient) ExtractUpdate(ctx context.Context, current Document, messages []models.Message) (Update, error) {
	if c == nil || c.provider == nil {
		return Update{}, errors.New("memory llm provider is not configured")
	}
	if len(messages) == 0 {
		return Update{}, nil
	}

	resp, err := c.provider.Chat(ctx, llm.ChatRequest{
		Model:        c.model,
		SystemPrompt: MemoryUpdateSystemPrompt,
		Messages: []models.Message{
			{
				ID:        "memory-update",
				SessionID: current.SessionID,
				Role:      models.RoleHuman,
				Content:   BuildMemoryUpdatePrompt(messages, current),
				CreatedAt: time.Now().UTC(),
			},
		},
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	})
	if err != nil {
		return Update{}, fmt.Errorf("memory llm call failed: %w", err)
	}

	content := extractJSON(resp.Message.Content)
	if content == "" {
		slog.Debug("memory extraction returned empty content, skipping update", "session", current.SessionID)
		return Update{}, nil
	}
	var update Update
	if err := json.Unmarshal([]byte(content), &update); err != nil {
		return Update{}, fmt.Errorf("decode memory llm response: %w", err)
	}
	return update, nil
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}
