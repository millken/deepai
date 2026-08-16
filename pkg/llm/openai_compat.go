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
			// See the matching comment in the Anthropic provider: the backoff
			// and the re-issued request both block on a channel the agent is
			// already watching for idleness.
			var cancelled bool
			heartbeatDuring(ch, req.Model, func() {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					cancelled = true
					return
				}
				stream = p.client.Chat.Completions.NewStreaming(ctx, params, option.WithMaxRetries(0))
			})
			if cancelled {
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			}
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

// reasoningDeltaFields are the delta keys that reasoning models use for their
// thinking stream on OpenAI-compatible endpoints. None is part of the official
// OpenAI schema, so the SDK leaves them in ExtraFields rather than exposing a
// typed field. "reasoning_content" is DeepSeek's name, adopted by Qwen, GLM and
// Kimi; "reasoning" is what OpenRouter and several gateways emit.
var reasoningDeltaFields = []string{"reasoning_content", "reasoning"}

// reasoningDelta returns the thinking text carried by an untyped reasoning
// field, or "" when the delta carries none.
//
// Note respjson.Field.Valid() is deliberately NOT consulted: the SDK records
// every extra field as status `invalid` ("couldn't be marshalled into an
// expected type") precisely because it has no typed counterpart, so Valid() is
// false for every reasoning delta ever sent. Raw() still returns the correct
// JSON value, so decoding it is the real test — an omitted field yields "" and
// fails to decode, and a null or non-string value decodes to the empty string.
func reasoningDelta(delta openai.ChatCompletionChunkChoiceDelta) string {
	for _, name := range reasoningDeltaFields {
		field, ok := delta.JSON.ExtraFields[name]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal([]byte(field.Raw()), &text); err == nil && text != "" {
			return text
		}
	}
	return ""
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
			// Reasoning models (DeepSeek, Qwen, GLM — all OpenAI-compat, see
			// registry.go) stream their entire thinking phase as
			// reasoning_content deltas carrying no content and no tool calls.
			// Without a signal here, pkg/agent's stream idle watchdog sees total
			// silence for that whole phase and cancels a perfectly healthy
			// request ("stream idle timeout: no data received after 2m0s") the
			// moment a hard problem thinks for longer than the idle window. The
			// Anthropic provider does the same for thinking_delta.
			//
			// emitted is set too, deliberately: a reasoning stream that dies
			// mid-thought has already produced (and billed) output, so it must
			// not be classified as "nothing was emitted" and transparently
			// re-run. The text itself is NOT forwarded as a Delta — reasoning is
			// a liveness signal, not assistant content.
			if reasoningDelta(choice.Delta) != "" {
				emitted = true
				ch <- StreamChunk{Model: model, Progress: true}
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
				// See StreamChunk.Progress's doc comment: a large tool-call
				// argument can stream for minutes as nothing but these
				// fragments, and without a signal here pkg/agent's stream
				// idle watchdog would see total silence for that whole span
				// and cancel a perfectly healthy request. Gated on a
				// non-empty fragment so the id/name-only initial delta chunk
				// (which OpenAI-compat providers send with an empty
				// arguments string) doesn't manufacture busywork.
				if tc.Function.Arguments != "" {
					ch <- StreamChunk{Model: model, Progress: true}
				}
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
		ch <- StreamChunk{Err: newToolArgsJSONError(badTool, assembleErr), Done: true}
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
			var next openai.ChatCompletionMessageParamUnion
			if len(m.Images) > 0 {
				next = mapHumanWithImages(m, imageDetail)
			} else {
				content := ensureContent(m.Content, "user")
				next = openai.UserMessage(content)
			}
			result = appendOrMergeOpenAIUser(result, next)
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

// appendOrMergeOpenAIUser merges consecutive user-role messages into one,
// mirroring anthropic.go's appendOrMergeUser. Unlike the Anthropic mapper —
// where RoleTool ALSO becomes "user" role, so tool_result-then-hint merging
// matters — RoleTool maps to its own distinct "tool" role here
// (mapMessagesToOpenAI's RoleTool case, via openai.ToolMessage), so the only
// adjacency this needs to fix is two RoleHuman messages in a row. That
// happens on the FIRST request of every Run: canonical [humanMsg] plus
// M4-2's trailing turn injection (also RoleHuman) both map to "user" role —
// two adjacent same-role messages, which some OpenAI-compat providers
// (deepseek-reasoner has historically 400'd on this) reject. It can also
// happen anywhere a RoleHuman breaker-hint or plan-mode nudge lands right
// after another RoleHuman message.
//
// Merges by content: if either side already uses the array-of-parts form
// (an image message), both are normalized to that form and concatenated,
// existing parts first then incoming (same order-preserving discipline as
// the Anthropic mapper); otherwise plain string content is concatenated with
// a blank-line separator.
func appendOrMergeOpenAIUser(msgs []openai.ChatCompletionMessageParamUnion, next openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(msgs) == 0 || msgs[len(msgs)-1].OfUser == nil || next.OfUser == nil {
		return append(msgs, next)
	}
	last := msgs[len(msgs)-1].OfUser
	incoming := next.OfUser

	if len(last.Content.OfArrayOfContentParts) == 0 && len(incoming.Content.OfArrayOfContentParts) == 0 {
		merged := last.Content.OfString.Value
		if incomingText := incoming.Content.OfString.Value; incomingText != "" {
			if merged != "" {
				merged += "\n\n"
			}
			merged += incomingText
		}
		last.Content = openai.ChatCompletionUserMessageParamContentUnion{OfString: param.NewOpt(merged)}
		return msgs
	}

	// At least one side carries content parts (e.g. an image) — normalize
	// both to the array-of-parts form and concatenate, preserving order.
	parts := append(userContentParts(last), userContentParts(incoming)...)
	last.Content = openai.ChatCompletionUserMessageParamContentUnion{OfArrayOfContentParts: parts}
	return msgs
}

// userContentParts normalizes a user message's content into the
// array-of-parts form, wrapping plain string content in a single text part
// (nil/empty when there's nothing to wrap).
func userContentParts(u *openai.ChatCompletionUserMessageParam) []openai.ChatCompletionContentPartUnionParam {
	if len(u.Content.OfArrayOfContentParts) > 0 {
		return u.Content.OfArrayOfContentParts
	}
	if text := u.Content.OfString.Value; text != "" {
		return []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(text)}
	}
	return nil
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
