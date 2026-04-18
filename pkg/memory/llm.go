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
		Model:           c.model,
		SystemPrompt:    MemoryUpdateSystemPrompt,
		ReasoningEffort: "disabled",
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
		slog.Debug(
			"memory llm decode failed",
			"session",
			current.SessionID,
			"raw_preview",
			previewForLog(resp.Message.Content, 200),
			"json_preview",
			previewForLog(content, 200),
			"err",
			err,
		)
		return Update{}, fmt.Errorf("decode memory llm response: %w", err)
	}
	return update, nil
}

func previewForLog(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > limit {
		s = string(r[:limit]) + "..."
	}
	// Keep log lines readable in one line.
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// Fast path: already a valid JSON payload.
	if json.Valid([]byte(raw)) {
		return raw
	}

	// Strip common fenced wrappers.
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if json.Valid([]byte(raw)) {
		return raw
	}

	// Recover first valid JSON object/array from mixed text.
	start := -1
	for i := 0; i < len(raw); i++ {
		if raw[i] == '{' || raw[i] == '[' {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}

	for end := len(raw); end > start; end-- {
		candidate := strings.TrimSpace(raw[start:end])
		if json.Valid([]byte(candidate)) {
			return candidate
		}
	}

	return ""
}
