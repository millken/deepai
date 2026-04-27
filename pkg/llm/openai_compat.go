package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/packages/ssestream"
)

// OpenAICompatProvider implements LLMProvider using the official openai-go SDK.
// It covers all OpenAI-compatible providers (openai, qwen, gemini, groq, ollama, glm, bedrock, deepseek, openai-compat).
type OpenAICompatProvider struct {
	provider string
	client   openai.Client
}

// NewOpenAICompatProvider creates a new OpenAI-compatible LLMProvider.
func NewOpenAICompatProvider(provider, apiKey, baseURL string, httpClient *http.Client) (*OpenAICompatProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%s: api key is required", provider)
	}
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(3),
		option.WithHTTPClient(httpClient),
		option.WithMiddleware(payloadMiddleware(provider)),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	client := openai.NewClient(opts...)
	return &OpenAICompatProvider{provider: provider, client: client}, nil
}

func (p *OpenAICompatProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, err
	}
	params := p.buildParams(req)
	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return ChatResponse{}, err
	}
	return p.mapResponse(resp, req.Model), nil
}

func (p *OpenAICompatProvider) Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	params := p.buildParams(req)
	stream := p.client.Chat.Completions.NewStreaming(ctx, params, option.WithMaxRetries(0))

	ch := make(chan StreamChunk, 128)
	go func() {
		defer close(ch)
		const maxRetries = 3
		for attempt := 0; attempt <= maxRetries; attempt++ {
			retry := p.consumeStream(ctx, ch, stream, req.Model, attempt < maxRetries)
			stream.Close()
			if !retry {
				return
			}
			delay := streamRetryDelay(attempt)
			slog.Debug("retrying stream", "provider", p.provider, "attempt", attempt+1, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			}
			stream = p.client.Chat.Completions.NewStreaming(ctx, params, option.WithMaxRetries(0))
		}
	}()
	return ch, nil
}

func (p *OpenAICompatProvider) buildParams(req ChatRequest) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model: req.Model,
	}
	if req.MaxTokens != nil {
		params.MaxTokens = param.NewOpt(int64(*req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.ReasoningEffort != "" {
		params.ReasoningEffort = openai.ReasoningEffort(req.ReasoningEffort)
	}
	if len(req.Tools) > 0 {
		params.Tools = mapToolsToOpenAI(req.Tools)
	}
	params.Messages = mapMessagesToOpenAI(req.SystemPrompt, req.Messages)
	return params
}

func (p *OpenAICompatProvider) consumeStream(
	ctx context.Context,
	ch chan<- StreamChunk,
	stream *ssestream.Stream[openai.ChatCompletionChunk],
	model string,
	retryable bool,
) (retry bool) {
	var contentBuf strings.Builder
	var toolCalls []models.ToolCall
	toolCallBuilders := make(map[int64]*toolCallBuilder)
	var lastUsage *Usage
	var stopReason string
	var emitted bool
	var emittedToolCall bool

	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				emitted = true
				ch <- StreamChunk{Model: model, Delta: choice.Delta.Content}
				contentBuf.WriteString(choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				emitted = true
				emittedToolCall = true
				b, exists := toolCallBuilders[tc.Index]
				if !exists {
					b = &toolCallBuilder{
						id:   tc.ID,
						name: tc.Function.Name,
					}
					toolCallBuilders[tc.Index] = b
				} else {
					if tc.ID != "" {
						b.id = tc.ID
					}
					if tc.Function.Name != "" {
						b.name = tc.Function.Name
					}
				}
				b.args += tc.Function.Arguments
			}
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
			}
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			lastUsage = &Usage{
				InputTokens:  int(chunk.Usage.PromptTokens),
				OutputTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:  int(chunk.Usage.TotalTokens),
			}
		}
	}
	if err := stream.Err(); err != nil {
		if !emitted && retryable && isRetryableOpenAIStreamErr(err) {
			slog.Debug("stream failed before emitting content, will retry", "provider", p.provider, "err", err)
			return true
		}
		ch <- StreamChunk{Err: err, Done: true}
		return false
	}
	// Finalize tool call builders.
	for i := int64(0); int(i) < len(toolCallBuilders); i++ {
		if b, ok := toolCallBuilders[i]; ok {
			tc := models.ToolCall{ID: b.id, Name: b.name}
			if strings.TrimSpace(b.args) != "" {
				var args map[string]any
				if err := json.Unmarshal([]byte(b.args), &args); err == nil {
					tc.Arguments = args
				}
			}
			toolCalls = append(toolCalls, tc)
		}
	}
	// If no intermediate tool call chunks were sent, include in final chunk.
	if len(toolCalls) > 0 && !emittedToolCall {
		ch <- StreamChunk{Model: model, ToolCalls: toolCalls}
	}
	msg := models.Message{Role: models.RoleAI, Content: contentBuf.String(), ToolCalls: toolCalls}
	ch <- StreamChunk{Model: model, Message: &msg, ToolCalls: toolCalls, Usage: lastUsage, Stop: stopReason, Done: true}
	return false
}

func (p *OpenAICompatProvider) mapResponse(resp *openai.ChatCompletion, model string) ChatResponse {
	if len(resp.Choices) == 0 {
		return ChatResponse{Model: model}
	}
	choice := resp.Choices[0]
	m := models.Message{
		Role:      models.RoleAI,
		Content:   choice.Message.Content,
		ToolCalls: mapOpenAIToolCalls(choice.Message.ToolCalls),
	}
	var usage Usage
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		usage = Usage{
			InputTokens:  int(resp.Usage.PromptTokens),
			OutputTokens: int(resp.Usage.CompletionTokens),
			TotalTokens:  int(resp.Usage.TotalTokens),
		}
	}
	return ChatResponse{Model: model, Message: m, Usage: usage, Stop: string(choice.FinishReason)}
}

// --- message mapping ---

func mapMessagesToOpenAI(systemPrompt string, msgs []models.Message) []openai.ChatCompletionMessageParamUnion {
	var result []openai.ChatCompletionMessageParamUnion
	if systemPrompt != "" {
		result = append(result, openai.SystemMessage(systemPrompt))
	}
	for _, m := range msgs {
		switch m.Role {
		case models.RoleHuman:
			content := ensureContent(m.Content, "user")
			result = append(result, openai.UserMessage(content))
		case models.RoleAI:
			if len(m.ToolCalls) > 0 {
				result = append(result, mapAssistantWithToolCalls(m))
			} else {
				content := ensureContent(m.Content, "assistant")
				result = append(result, openai.AssistantMessage(content))
			}
		case models.RoleTool:
			if m.ToolResult != nil {
				content := ensureContent(m.Content, "tool")
				result = append(result, openai.ToolMessage(content, m.ToolResult.CallID))
			}
		}
	}
	return result
}

func mapAssistantWithToolCalls(m models.Message) openai.ChatCompletionMessageParamUnion {
	tcs := make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		if tc.Arguments == nil {
			argsJSON = []byte("{}")
		}
		tcs = append(tcs, openai.ChatCompletionMessageToolCallParam{
			ID: tc.ID,
			Function: openai.ChatCompletionMessageToolCallFunctionParam{
				Name:      tc.Name,
				Arguments: string(argsJSON),
			},
		})
	}
	content := m.Content
	if strings.TrimSpace(content) == "" {
		content = "(no response text)"
	}
	return openai.ChatCompletionMessageParamUnion{
		OfAssistant: &openai.ChatCompletionAssistantMessageParam{
			Content: openai.ChatCompletionAssistantMessageParamContentUnion{
				OfString: param.NewOpt(content),
			},
			ToolCalls: tcs,
		},
	}
}

func mapOpenAIToolCalls(calls []openai.ChatCompletionMessageToolCall) []models.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]models.ToolCall, 0, len(calls))
	for _, call := range calls {
		tc := models.ToolCall{ID: call.ID, Name: call.Function.Name}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			var args map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err == nil {
				tc.Arguments = args
			}
		}
		result = append(result, tc)
	}
	return result
}

func mapToolsToOpenAI(tools []models.Tool) []openai.ChatCompletionToolParam {
	result := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		p := openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:       t.Name,
				Parameters: openai.FunctionParameters(t.InputSchema),
			},
		}
		if t.Description != "" {
			p.Function.Description = openai.String(t.Description)
		}
		result = append(result, p)
	}
	return result
}

func isRetryableOpenAIStreamErr(err error) bool {
	var apierr *openai.Error
	if errors.As(err, &apierr) {
		return apierr.StatusCode == 429 || apierr.StatusCode >= 500
	}
	// Transient network / SSE-parse errors (truncated stream, connection reset)
	// are worth retrying once before surfacing to the caller.
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "unexpected end of JSON input") ||
		strings.Contains(s, "unexpected EOF")
}
