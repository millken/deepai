// Package llm provides the provider-agnostic LLM abstraction used by the agent runtime.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/voocel/litellm"
)

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

	litReq := mapChatReqToLitellmRequest(req, msgs)
	resp, err := p.client.Chat(ctx, litReq)
	if err != nil {
		return ChatResponse{}, err
	}

	msg := models.Message{Role: models.RoleAI, Content: resp.Content, ToolCalls: convertLitellmToolCalls(resp.ToolCalls)}
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

	litReq := mapChatReqToLitellmRequest(req, msgs)
	stream, err := p.client.Stream(ctx, litReq)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		defer stream.Close()

		toolAcc := litellm.NewToolCallAccumulator()
		var lastUsage *Usage

		resp, err := litellm.CollectStreamWithHandler(stream, func(chunk *litellm.StreamChunk) {
			if chunk == nil {
				return
			}

			if chunk.Content != "" {
				ch <- StreamChunk{Model: req.Model, Delta: chunk.Content}
			}

			if chunk.ToolCallDelta != nil {
				toolAcc.Apply(chunk.ToolCallDelta)
				if call := toolAcc.Get(chunk.ToolCallDelta.Index); call != nil {
					ch <- StreamChunk{Model: req.Model, ToolCalls: convertLitellmToolCalls([]litellm.ToolCall{*call})}
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
			ch <- StreamChunk{Err: err, Done: true}
			return
		}
		msg := models.Message{Role: models.RoleAI, Content: resp.Content, ToolCalls: convertLitellmToolCalls(resp.ToolCalls)}
		usage := lastUsage
		if usage == nil && (resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 || resp.Usage.TotalTokens != 0) {
			usage = &Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens}
		}
		ch <- StreamChunk{Model: req.Model, Message: &msg, ToolCalls: msg.ToolCalls, Usage: usage, Stop: resp.FinishReason, Done: true}
	}()

	return ch, nil
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
		r.Thinking = &litellm.ThinkingConfig{Type: litellm.ThinkingEnabled, Level: req.ReasoningEffort}
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

	// allow enabling debug payload logging via DEEPAI_DEBUG
	if strings.TrimSpace(os.Getenv("DEEPAI_DEBUG")) != "" {
		// enable request payload hook inside pkg/llm
		r.OnPayload = func(providerName string, payload []byte) {
			log.Printf("[litellm payload] provider=%s payload=%s\n", providerName, string(payload))
		}
	}

	return r
}
