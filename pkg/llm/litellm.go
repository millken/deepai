// Package llm provides the provider-agnostic LLM abstraction used by the agent runtime.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/providers"
)

func init() {
	litellm.RegisterProviderWithDescriptor(litellm.ProviderDescriptor{
		Name:       "openai-compat",
		DefaultURL: "https://api.openai.com",
		Factory: func(config litellm.ProviderConfig) litellm.Provider {
			return providers.NewOpenAICompat(config, providers.Compat{
				ProviderName: "openai-compat",
			})
		},
	})
}

// LitellmProvider is a thin wrapper around litellm.Client.
type LitellmProvider struct {
	provider string
	client   *litellm.Client
}

func NewLitellmProvider(provider string, cfg litellm.ProviderConfig, opts ...litellm.ClientOption) (*LitellmProvider, error) {
	client, err := litellm.NewWithProvider(provider, cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("init litellm client: %w", err)
	}

	return &LitellmProvider{provider: provider, client: client}, nil
}

func (p *LitellmProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, err
	}

	// map messages
	msgs := make([]litellm.Message, 0, len(req.Messages))
	if strings.TrimSpace(req.SystemPrompt) != "" {
		msgs = append(msgs, litellm.Message{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, mapMessageToLitellm(m))
	}

	litReq := mapChatReqToLitellmRequest(req, msgs)
	resp, err := p.client.Chat(ctx, litReq)
	if err != nil {
		return ChatResponse{}, err
	}

	msg := models.Message{Role: models.RoleAI, Content: extractResponseContent(resp), ToolCalls: convertLitellmToolCalls(resp.ToolCalls)}
	var usage Usage
	if resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 || resp.Usage.TotalTokens != 0 {
		usage = Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens}
	}
	return ChatResponse{Model: req.Model, Message: msg, Usage: usage, Stop: resp.FinishReason}, nil
}

func (p *LitellmProvider) Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	msgs := make([]litellm.Message, 0, len(req.Messages))
	if strings.TrimSpace(req.SystemPrompt) != "" {
		msgs = append(msgs, litellm.Message{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, mapMessageToLitellm(m))
	}

	litReq := mapChatReqToLitellmRequest(req, msgs)
	stream, err := p.client.Stream(ctx, litReq)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk, 128)
	go func() {
		defer close(ch)

		const maxStreamRetries = 3
		var lastRetryErr error
		for attempt := 0; attempt <= maxStreamRetries; attempt++ {
			// consumeStream returns true if we should retry.
			retry := p.consumeStream(ctx, ch, stream, req.Model, attempt < maxStreamRetries)
			stream.Close()
			if !retry {
				return
			}
			// Exponential backoff with jitter before retry.
			delay := streamRetryDelay(attempt)
			slog.Debug("retrying stream", "provider", p.provider, "attempt", attempt+1, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			}
			var err error
			stream, err = p.client.Stream(ctx, litReq)
			if err != nil {
				lastRetryErr = err
				if !isRetryableStreamErr(err) || attempt == maxStreamRetries {
					ch <- StreamChunk{Err: lastRetryErr, Done: true}
					return
				}
				// Reconnect failed but still retryable — continue loop to backoff and retry again.
				continue
			}
		}
		// All retries exhausted during reconnect.
		if lastRetryErr != nil {
			ch <- StreamChunk{Err: lastRetryErr, Done: true}
		}
	}()

	return ch, nil
}

// streamRetryDelay returns exponential backoff with jitter for stream retries.
func streamRetryDelay(attempt int) time.Duration {
	delay := float64(time.Second) * math.Pow(2, float64(attempt))
	if delay > float64(30*time.Second) {
		delay = float64(30 * time.Second)
	}
	delay += delay * 0.25 * (2*rand.Float64() - 1)
	if delay < 0 {
		delay = float64(time.Second)
	}
	return time.Duration(delay)
}

// isRetryableStreamErr checks if a stream error is worth retrying.
// Only retries on timeout, network, rate limit, overloaded, and 5xx provider errors.
func isRetryableStreamErr(err error) bool {
	var litErr *providers.LiteLLMError
	if errors.As(err, &litErr) {
		switch litErr.Type {
		case providers.ErrorTypeNetwork, providers.ErrorTypeTimeout,
			providers.ErrorTypeRateLimit, providers.ErrorTypeOverloaded:
			return true
		case providers.ErrorTypeProvider:
			// Only retry 5xx provider errors, not 4xx (validation, auth, etc.)
			return litErr.StatusCode >= 500 || litErr.StatusCode == 0
		}
		return false
	}
	// Non-LiteLLM errors are not retried by default.
	return false
}

// consumeStream reads from a stream and sends chunks to ch.
// Returns true if the stream failed early (no content emitted) with a retryable error.
func (p *LitellmProvider) consumeStream(ctx context.Context, ch chan<- StreamChunk, stream litellm.StreamReader, model string, retryable bool) (retry bool) {
	toolAcc := litellm.NewToolCallAccumulator()
	var lastUsage *Usage
	var emitted bool

	resp, err := litellm.CollectStreamWithHandler(stream, func(chunk *litellm.StreamChunk) {
		if chunk == nil {
			return
		}

		if chunk.Content != "" {
			emitted = true
			ch <- StreamChunk{Model: model, Delta: chunk.Content}
		}

		if chunk.ToolCallDelta != nil {
			emitted = true
			toolAcc.Apply(chunk.ToolCallDelta)
			if call := toolAcc.Get(chunk.ToolCallDelta.Index); call != nil {
				ch <- StreamChunk{Model: model, ToolCalls: convertLitellmToolCalls([]litellm.ToolCall{*call})}
			}
		}

		if chunk.Usage != nil {
			lastUsage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
	})
	if err != nil {
		if !emitted && retryable && isRetryableStreamErr(err) {
			slog.Debug("stream failed before emitting content, will retry", "provider", p.provider, "err", err)
			return true
		}
		ch <- StreamChunk{Err: err, Done: true}
		return false
	}
	msg := models.Message{Role: models.RoleAI, Content: extractResponseContent(resp), ToolCalls: convertLitellmToolCalls(resp.ToolCalls)}
	usage := lastUsage
	if usage == nil && (resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 || resp.Usage.TotalTokens != 0) {
		usage = &Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens}
	}
	ch <- StreamChunk{Model: model, Message: &msg, ToolCalls: msg.ToolCalls, Usage: usage, Stop: resp.FinishReason, Done: true}
	return false
}

func convertLitellmToolCalls(calls []litellm.ToolCall) []models.ToolCall {
	if len(calls) == 0 {
		return nil
	}

	result := make([]models.ToolCall, 0, len(calls))
	for _, call := range calls {
		toolCall := models.ToolCall{ID: call.ID, Name: call.Function.Name}
		if toolCall.ID == "" && toolCall.Name == "" {
			continue
		}

		if strings.TrimSpace(call.Function.Arguments) != "" {
			var args map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err == nil {
				toolCall.Arguments = args
			}
		}

		result = append(result, toolCall)
	}
	return result
}

func extractResponseContent(resp *litellm.Response) string {
	if resp == nil {
		return ""
	}

	if strings.TrimSpace(resp.Content) != "" {
		return resp.Content
	}

	for _, c := range resp.Contents {
		if strings.TrimSpace(c.Text) != "" {
			return c.Text
		}
	}

	if strings.TrimSpace(resp.ReasoningContent) != "" {
		return resp.ReasoningContent
	}

	return ""
}

// mapMessageToLitellm converts a models.Message into a litellm.Message,
// preserving ToolCalls (assistant) and ToolCallID (tool result).
func mapMessageToLitellm(m models.Message) litellm.Message {
	role := "assistant"
	switch m.Role {
	case models.RoleHuman:
		role = "user"
	case models.RoleSystem:
		role = "system"
	case models.RoleTool:
		role = "tool"
	}

	lm := litellm.Message{Role: role, Content: m.Content}

	// Ensure content is never empty — providers reject empty content for tool/assistant messages.
	if strings.TrimSpace(lm.Content) == "" {
		if role == "tool" {
			lm.Content = "(tool returned empty output)"
		} else if role == "assistant" && len(m.ToolCalls) == 0 {
			lm.Content = "(no response text)"
		}
	}

	if len(m.ToolCalls) > 0 {
		lm.ToolCalls = make([]litellm.ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			if tc.Arguments == nil {
				argsJSON = []byte("{}")
			}
			lm.ToolCalls[i] = litellm.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: litellm.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			}
		}
	}

	if m.ToolResult != nil {
		lm.ToolCallID = m.ToolResult.CallID
	}

	return lm
}

// mapChatReqToLitellmRequest attempts to copy optional fields from our provider-agnostic
// ChatRequest into a litellm.Request using reflection. This keeps the adapter resilient
// to small variations in the litellm.Request shape while still propagating options
// like Tools, ReasoningEffort, Temperature and MaxTokens when present.
func mapChatReqToLitellmRequest(req ChatRequest, msgs []litellm.Message) *litellm.Request {
	r := &litellm.Request{
		Model:    req.Model,
		Messages: msgs,
	}

	// ReasoningEffort
	// Thinking/Reasoning effort: enable thinking with provided level
	if req.ReasoningEffort != "" {
		switch strings.ToLower(strings.TrimSpace(req.ReasoningEffort)) {
		case "disabled", "disable", "none", "off", "false", "0":
			r.Thinking = &litellm.ThinkingConfig{Type: litellm.ThinkingDisabled}
		default:
			r.Thinking = &litellm.ThinkingConfig{Type: litellm.ThinkingEnabled, Level: req.ReasoningEffort}
		}
	}

	// Temperature: litellm.Request expects *float64
	if req.Temperature != nil {
		r.Temperature = req.Temperature
	}

	// MaxTokens
	if req.MaxTokens != nil {
		r.MaxTokens = req.MaxTokens
	}

	// Tools: map our models.Tool -> litellm.Tool (function def)
	if len(req.Tools) > 0 {
		tools := make([]litellm.Tool, 0, len(req.Tools))
		for _, tt := range req.Tools {
			tools = append(tools, litellm.Tool{
				Type: "function",
				Function: litellm.FunctionDef{
					Name:        tt.Name,
					Description: tt.Description,
					Parameters:  tt.InputSchema,
				},
			})
		}
		r.Tools = tools
	}

	// Log request payload via slog.
	r.OnPayload = func(providerName string, payload []byte) {
		slog.Debug("provider payload", "provider", providerName, "payload", string(payload))
	}

	return r
}
