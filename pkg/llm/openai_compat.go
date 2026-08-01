package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
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
	if err != nil && isReasoningEffortError(err) && params.ReasoningEffort != "" {
		slog.Debug("model does not support reasoning_effort, retrying without it", "provider", p.provider, "model", req.Model)
		params.ReasoningEffort = ""
		resp, err = p.client.Chat.Completions.New(ctx, params)
	}
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
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	stream := p.client.Chat.Completions.NewStreaming(ctx, params, option.WithMaxRetries(0))

	ch := make(chan StreamChunk, 128)
	go func() {
		defer close(ch)
		const maxRetries = 3
		reasoningStripped := false
		for attempt := 0; attempt <= maxRetries; attempt++ {
			retry, stripRE := p.consumeStream(ctx, ch, stream, req.Model, attempt < maxRetries)
			stream.Close()
			if !retry {
				return
			}
			if stripRE && !reasoningStripped && params.ReasoningEffort != "" {
				params.ReasoningEffort = ""
				reasoningStripped = true
				slog.Debug("stripped reasoning_effort for retry", "provider", p.provider, "model", req.Model)
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
		switch strings.ToLower(strings.TrimSpace(req.ReasoningEffort)) {
		case "low", "medium", "high":
			params.ReasoningEffort = openai.ReasoningEffort(strings.ToLower(strings.TrimSpace(req.ReasoningEffort)))
			// "disabled", "none", "off", "auto" etc. → do not set the field;
			// the server default applies. Prevents 400 errors from providers that
			// only accept low/medium/high.
		}
	}
	if len(req.Tools) > 0 {
		params.Tools = mapToolsToOpenAI(req.Tools)
	}
	params.Messages = mapMessagesToOpenAI(req.SystemPrompt, req.Messages, req.ImageDetail)
	return params
}

func (p *OpenAICompatProvider) consumeStream(
	ctx context.Context,
	ch chan<- StreamChunk,
	stream *ssestream.Stream[openai.ChatCompletionChunk],
	model string,
	retryable bool,
) (retry bool, stripReasoningEffort bool) {
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
		// Debug: log all usage fields to diagnose provider behavior
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.TotalTokens > 0 {
			slog.Debug("provider usage chunk",
				"provider", p.provider,
				"model", model,
				"prompt_tokens", chunk.Usage.PromptTokens,
				"completion_tokens", chunk.Usage.CompletionTokens,
				"total_tokens", chunk.Usage.TotalTokens,
			)
		}
	}
	if err := stream.Err(); err != nil {
		// reasoning_effort rejection: signal caller to strip the field and retry.
		if !emitted && retryable && isReasoningEffortError(err) {
			slog.Debug("model rejected reasoning_effort, will retry without it", "provider", p.provider, "model", model, "err", err)
			return true, true
		}
		if !emitted && retryable && isRetryableOpenAIStreamErr(err) {
			slog.Debug("stream failed before emitting content, will retry", "provider", p.provider, "err", err)
			return true, false
		}
		ch <- StreamChunk{Err: err, Done: true}
		return false, false
	}
	// Finalize tool call builders.
	assembled, badTool, assembleErr := assembleToolCalls(toolCallBuilders)
	if assembleErr != nil {
		if retryable {
			slog.Debug("tool call arguments JSON invalid, will retry", "tool", badTool, "err", assembleErr)
			return true, false
		}
		ch <- StreamChunk{Err: fmt.Errorf("tool %q: invalid arguments JSON: %w", badTool, assembleErr), Done: true}
		return false, false
	}
	toolCalls = append(toolCalls, assembled...)
	// If no intermediate tool call chunks were sent, include in final chunk.
	if len(toolCalls) > 0 && !emittedToolCall {
		ch <- StreamChunk{Model: model, ToolCalls: toolCalls}
	}
	msg := models.Message{Role: models.RoleAI, Content: contentBuf.String(), ToolCalls: toolCalls}
	ch <- StreamChunk{Model: model, Message: &msg, ToolCalls: toolCalls, Usage: lastUsage, Stop: stopReason, Done: true}
	return false, false
}

func assembleToolCalls(builders map[int64]*toolCallBuilder) ([]models.ToolCall, string, error) {
	indices := make([]int64, 0, len(builders))
	for idx := range builders {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })

	calls := make([]models.ToolCall, 0, len(indices))
	for _, idx := range indices {
		b := builders[idx]
		tc := models.ToolCall{ID: b.id, Name: b.name}
		if strings.TrimSpace(b.args) != "" {
			var args map[string]any
			if err := json.Unmarshal([]byte(b.args), &args); err != nil {
				return nil, b.name, err
			}
			tc.Arguments = args
		}
		calls = append(calls, tc)
	}
	return calls, "", nil
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
	// 优先使用服务端返回的模型名，如果为空则回退到请求时的模型名
	actualModel := resp.Model
	if actualModel == "" {
		actualModel = model
	}
	return ChatResponse{Model: actualModel, Message: m, Usage: usage, Stop: string(choice.FinishReason)}
}

// --- message mapping ---

func mapMessagesToOpenAI(systemPrompt string, msgs []models.Message, imageDetail string) []openai.ChatCompletionMessageParamUnion {
	if imageDetail == "" {
		imageDetail = "low"
	}
	var result []openai.ChatCompletionMessageParamUnion
	if systemPrompt != "" {
		result = append(result, openai.SystemMessage(systemPrompt))
	}
	for _, m := range msgs {
		switch m.Role {
		case models.RoleHuman:
			if len(m.Images) > 0 {
				result = append(result, mapHumanWithImages(m, imageDetail))
			} else {
				content := ensureContent(m.Content, "user")
				result = append(result, openai.UserMessage(content))
			}
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

// mapHumanWithImages constructs a multimodal user message with text + image
// content parts. The imageDetail parameter ("low"/"auto"/"high") controls
// OpenAI vision token cost.
func mapHumanWithImages(m models.Message, imageDetail string) openai.ChatCompletionMessageParamUnion {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Images)+1)
	if content := strings.TrimSpace(m.Content); content != "" {
		parts = append(parts, openai.TextContentPart(content))
	}
	for _, img := range m.Images {
		parts = append(parts, openai.ImageContentPart(
			openai.ChatCompletionContentPartImageImageURLParam{
				URL:    "data:" + img.MimeType + ";base64," + img.Base64,
				Detail: imageDetail,
			},
		))
	}
	return openai.UserMessage(parts)
}

func mapAssistantWithToolCalls(m models.Message) openai.ChatCompletionMessageParamUnion {
	tcs := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		if tc.Arguments == nil {
			argsJSON = []byte("{}")
		}
		tcs = append(tcs, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: tc.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
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

func mapOpenAIToolCalls(calls []openai.ChatCompletionMessageToolCallUnion) []models.ToolCall {
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

func mapToolsToOpenAI(tools []models.Tool) []openai.ChatCompletionToolUnionParam {
	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		fn := openai.FunctionDefinitionParam{
			Name:       t.Name,
			Parameters: openai.FunctionParameters(t.InputSchema),
		}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		result = append(result, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: fn,
			},
		})
	}
	return result
}

func isRetryableOpenAIStreamErr(err error) bool {
	var apierr *openai.Error
	if errors.As(err, &apierr) {
		if apierr.StatusCode == 429 || apierr.StatusCode >= 500 {
			return true
		}
		if apierr.StatusCode != 0 {
			return false
		}
	}
	return isTransientStreamError(err)
}

// isReasoningEffortError reports whether a 400 error was caused by the
// reasoning_effort field being rejected by the provider (e.g. models that only
// accept low/medium/high, or models that don't support the field at all).
func isReasoningEffortError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "reasoning_effort") ||
		strings.Contains(s, "reasoning effort")
}
