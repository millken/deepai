package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/millken/deepai/pkg/models"
)

// AnthropicProvider implements LLMProvider using the official anthropic-sdk-go.
type AnthropicProvider struct {
	provider string
	client   anthropic.Client
}

// NewAnthropicProvider creates a new Anthropic-backed LLMProvider.
func NewAnthropicProvider(apiKey, baseURL string, httpClient *http.Client) (*AnthropicProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: api key is required")
	}
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(3),
		option.WithHTTPClient(httpClient),
		option.WithMiddleware(payloadMiddleware("anthropic")),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	client := anthropic.NewClient(opts...)
	return &AnthropicProvider{provider: "anthropic", client: client}, nil
}

func (p *AnthropicProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, err
	}
	params := p.buildParams(req)
	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return ChatResponse{}, err
	}
	return p.mapResponse(msg, req.Model), nil
}

func (p *AnthropicProvider) Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	params := p.buildParams(req)
	stream := p.client.Messages.NewStreaming(ctx, params, option.WithMaxRetries(0))

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
			stream = p.client.Messages.NewStreaming(ctx, params, option.WithMaxRetries(0))
		}
	}()
	return ch, nil
}

func (p *AnthropicProvider) buildParams(req ChatRequest) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model: req.Model,
	}
	if req.MaxTokens != nil {
		params.MaxTokens = int64(*req.MaxTokens)
	} else {
		params.MaxTokens = 8192
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}
	if req.ReasoningEffort != "" {
		params.Thinking = mapThinkingConfig(req.ReasoningEffort)
	}
	if len(req.Tools) > 0 {
		params.Tools = mapToolsToAnthropic(req.Tools)
	}
	params.Messages = mapMessagesToAnthropic(req.Messages)
	return params
}

func (p *AnthropicProvider) consumeStream(
	ctx context.Context,
	ch chan<- StreamChunk,
	stream *ssestream.Stream[anthropic.MessageStreamEventUnion],
	model string,
	retryable bool,
) (retry bool) {
	var contentBuf strings.Builder
	var toolCalls []models.ToolCall
	toolCallBuilders := make(map[int64]*toolCallBuilder)
	var lastUsage *Usage
	var stopReason string
	var emitted bool

	for stream.Next() {
		event := stream.Current()
		switch variant := event.AsAny().(type) {
		case anthropic.ContentBlockStartEvent:
			if cb, ok := variant.ContentBlock.AsAny().(*anthropic.ToolUseBlock); ok {
				toolCallBuilders[variant.Index] = &toolCallBuilder{
					id:   cb.ID,
					name: cb.Name,
				}
			}
		case anthropic.ContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case *anthropic.TextDelta:
				if delta.Text != "" {
					emitted = true
					ch <- StreamChunk{Model: model, Delta: delta.Text}
					contentBuf.WriteString(delta.Text)
				}
			case *anthropic.InputJSONDelta:
				if b, ok := toolCallBuilders[variant.Index]; ok {
					b.args += delta.PartialJSON
					emitted = true
				}
			}
		case anthropic.ContentBlockStopEvent:
			if b, ok := toolCallBuilders[variant.Index]; ok {
				tc := models.ToolCall{ID: b.id, Name: b.name}
				if strings.TrimSpace(b.args) != "" {
					var args map[string]any
					if err := json.Unmarshal([]byte(b.args), &args); err == nil {
						tc.Arguments = args
					}
				}
				toolCalls = append(toolCalls, tc)
				ch <- StreamChunk{Model: model, ToolCalls: []models.ToolCall{tc}}
				delete(toolCallBuilders, variant.Index)
			}
		case anthropic.MessageStartEvent:
			if variant.Message.Usage.InputTokens > 0 {
				lastUsage = &Usage{
					InputTokens: int(variant.Message.Usage.InputTokens),
				}
			}
		case anthropic.MessageDeltaEvent:
			stopReason = string(variant.Delta.StopReason)
			if variant.Usage.OutputTokens > 0 {
				if lastUsage == nil {
					lastUsage = &Usage{}
				}
				lastUsage.OutputTokens = int(variant.Usage.OutputTokens)
			}
		}
	}
	if err := stream.Err(); err != nil {
		if !emitted && retryable && isRetryableAnthropicStreamErr(err) {
			slog.Debug("stream failed before emitting content, will retry", "provider", p.provider, "err", err)
			return true
		}
		ch <- StreamChunk{Err: err, Done: true}
		return false
	}
	msg := models.Message{Role: models.RoleAI, Content: contentBuf.String(), ToolCalls: toolCalls}
	ch <- StreamChunk{Model: model, Message: &msg, ToolCalls: toolCalls, Usage: lastUsage, Stop: stopReason, Done: true}
	return false
}

func (p *AnthropicProvider) mapResponse(msg *anthropic.Message, model string) ChatResponse {
	var content string
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(*anthropic.TextBlock); ok {
			content += tb.Text
		}
	}
	var toolCalls []models.ToolCall
	for _, block := range msg.Content {
		if tu, ok := block.AsAny().(*anthropic.ToolUseBlock); ok {
			tc := models.ToolCall{ID: tu.ID, Name: tu.Name}
			if len(tu.Input) > 0 {
				var args map[string]any
				if err := json.Unmarshal(tu.Input, &args); err == nil {
					tc.Arguments = args
				}
			}
			toolCalls = append(toolCalls, tc)
		}
	}
	m := models.Message{Role: models.RoleAI, Content: content, ToolCalls: toolCalls}
	var usage Usage
	if msg.Usage.InputTokens > 0 || msg.Usage.OutputTokens > 0 {
		usage = Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		}
	}
	return ChatResponse{Model: model, Message: m, Usage: usage, Stop: string(msg.StopReason)}
}

// --- message mapping ---

func mapMessagesToAnthropic(msgs []models.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, m := range msgs {
		switch m.Role {
		case models.RoleHuman:
			result = append(result, anthropic.NewUserMessage(
				anthropic.NewTextBlock(ensureContent(m.Content, "user")),
			))
		case models.RoleAI:
			blocks := mapAssistantToBlocks(m)
			result = append(result, anthropic.NewAssistantMessage(blocks...))
		case models.RoleTool:
			userBlocks := mapToolResultToUserBlocks(m)
			result = appendOrMergeUser(result, userBlocks)
		}
	}
	return result
}

func mapAssistantToBlocks(m models.Message) []anthropic.ContentBlockParamUnion {
	var blocks []anthropic.ContentBlockParamUnion
	if strings.TrimSpace(m.Content) != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, tc := range m.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		if tc.Arguments == nil {
			argsJSON = []byte("{}")
		}
		blocks = append(blocks, anthropic.ContentBlockParamUnion{
			OfToolUse: &anthropic.ToolUseBlockParam{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: json.RawMessage(argsJSON),
			},
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropic.NewTextBlock("(no response text)"))
	}
	return blocks
}

func mapToolResultToUserBlocks(m models.Message) []anthropic.ContentBlockParamUnion {
	content := ensureContent(m.Content, "tool")
	if m.ToolResult != nil {
		return []anthropic.ContentBlockParamUnion{
			{
				OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: m.ToolResult.CallID,
					IsError:   param.NewOpt(m.ToolResult.Status == models.CallStatusFailed),
					Content: []anthropic.ToolResultBlockParamContentUnion{
						{OfText: &anthropic.TextBlockParam{Text: content}},
					},
				},
			},
		}
	}
	return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(content)}
}

// appendOrMergeUser merges tool result blocks into the last user message if it's also user role.
func appendOrMergeUser(msgs []anthropic.MessageParam, blocks []anthropic.ContentBlockParamUnion) []anthropic.MessageParam {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == anthropic.MessageParamRoleUser {
		msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, blocks...)
		return msgs
	}
	return append(msgs, anthropic.NewUserMessage(blocks...))
}

func mapToolsToAnthropic(tools []models.Tool) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{Type: "object"}
		if props, ok := t.InputSchema["properties"]; ok {
			schema.Properties = props
		}
		if req, ok := t.InputSchema["required"]; ok {
			if reqSlice, ok := toStrSlice(req); ok {
				schema.Required = reqSlice
			}
		}
		p := anthropic.ToolParam{
			Name:        t.Name,
			InputSchema: schema,
		}
		if t.Description != "" {
			p.Description = anthropic.String(t.Description)
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &p})
	}
	return result
}

func mapThinkingConfig(effort string) anthropic.ThinkingConfigParamUnion {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "disabled", "disable", "none", "off", "false", "0":
		return anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
	default:
		return anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{
				BudgetTokens: 10000,
				Type:         "enabled",
			},
		}
	}
}

func ensureContent(content, role string) string {
	if strings.TrimSpace(content) != "" {
		return content
	}
	if role == "tool" {
		return "(tool returned empty output)"
	}
	return "(no response text)"
}

func toStrSlice(v any) ([]string, bool) {
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		result := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result, true
	}
	return nil, false
}

func isRetryableAnthropicStreamErr(err error) bool {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return apierr.StatusCode == 429 || apierr.StatusCode == 529 || apierr.StatusCode >= 500
	}
	return false
}

// --- shared helpers ---

type toolCallBuilder struct {
	id   string
	name string
	args string
}

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

func payloadMiddleware(provider string) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if req.Body != nil {
			if body, err := io.ReadAll(req.Body); err == nil {
				slog.Debug("provider payload", "provider", provider, "payload", string(body))
				req.Body = io.NopCloser(bytes.NewBuffer(body))
			}
		}
		return next(req)
	}
}
