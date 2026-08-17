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
	"regexp"
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
			// Both the backoff and NewStreaming block, and NewStreaming blocks
			// for as long as the server withholds response headers — which is
			// exactly as long as it keeps the request queued. Heartbeat through
			// the whole span or the agent's idle watchdog kills this stream
			// while it is legitimately reconnecting.
			var cancelled bool
			heartbeatDuring(ch, req.Model, func() {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					cancelled = true
					return
				}
				stream = p.client.Messages.NewStreaming(ctx, params, option.WithMaxRetries(0))
			})
			if cancelled {
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			}
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
	if req.Temperature != nil && acceptsTemperature(req.Model) {
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
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				toolCallBuilders[event.Index] = &toolCallBuilder{
					id:   event.ContentBlock.ID,
					name: event.ContentBlock.Name,
				}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					emitted = true
					ch <- StreamChunk{Model: model, Delta: event.Delta.Text}
					contentBuf.WriteString(event.Delta.Text)
				}
			case "input_json_delta":
				if b, ok := toolCallBuilders[event.Index]; ok {
					b.args += event.Delta.PartialJSON
					emitted = true
					// A large tool-call argument (e.g. write_file's markdown
					// body) can stream for minutes as nothing but these
					// fragments — with no Progress signal, the idle watchdog
					// in pkg/agent/streaming.go would see total silence for
					// that whole span and cancel a perfectly healthy request
					// (see StreamChunk.Progress's doc comment). Gated on a
					// non-empty fragment so an empty delta (were the API to
					// ever send one) doesn't manufacture busywork.
					if event.Delta.PartialJSON != "" {
						ch <- StreamChunk{Model: model, Progress: true}
					}
				}
			case "thinking_delta", "signature_delta":
				// Extended thinking (reasoning models, incl. GLM via the
				// anthropic-compat endpoint) streams minutes of thinking_delta
				// events before the first text/tool token. These carry no
				// message content, but they ARE stream activity: without a
				// Progress signal here the idle watchdog in pkg/agent would
				// see total silence for the whole reasoning phase and cancel
				// a perfectly healthy request ("stream idle timeout") the
				// moment a hard problem thinks for longer than the idle
				// window. Gated on a non-empty payload so a keep-alive-shaped
				// empty delta doesn't manufacture busywork. (SSE ping events
				// can't feed the watchdog too — the SDK's ssestream skips
				// them before this loop ever sees them — so thinking deltas
				// are the only activity signal a reasoning stream provides.)
				// Setting emitted too: a thinking-only stream that dies
				// mid-reasoning has already streamed (and billed) minutes of
				// output — it must NOT be classified as "nothing was emitted"
				// and transparently re-run up to 4 attempts.
				if event.Delta.Thinking != "" || event.Delta.Signature != "" {
					emitted = true
					ch <- StreamChunk{Model: model, Progress: true}
				}
			}
		case "content_block_stop":
			if b, ok := toolCallBuilders[event.Index]; ok {
				tc := models.ToolCall{ID: b.id, Name: b.name}
				if strings.TrimSpace(b.args) != "" {
					var args map[string]any
					if err := json.Unmarshal([]byte(b.args), &args); err != nil {
						// Arguments JSON is malformed (e.g. stream was truncated).
						// Surface as a retryable error instead of silently dropping.
						if retryable {
							slog.Debug("tool call arguments JSON invalid, will retry", "tool", b.name, "err", err)
							return true
						}
						ch <- StreamChunk{Err: newToolArgsJSONError(b.name, err), Done: true}
						return false
					}
					tc.Arguments = args
				}
				toolCalls = append(toolCalls, tc)
				ch <- StreamChunk{Model: model, ToolCalls: []models.ToolCall{tc}}
				delete(toolCallBuilders, event.Index)
			}
		case "message_start":
			if event.Message.Usage.InputTokens > 0 {
				lastUsage = &Usage{
					InputTokens: int(event.Message.Usage.InputTokens),
				}
			}
		case "message_delta":
			stopReason = string(event.Delta.StopReason)
			if event.Usage.OutputTokens > 0 {
				if lastUsage == nil {
					lastUsage = &Usage{}
				}
				lastUsage.OutputTokens = int(event.Usage.OutputTokens)
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
	var toolCalls []models.ToolCall
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "tool_use":
			tc := models.ToolCall{ID: block.ID, Name: block.Name}
			if len(block.Input) > 0 {
				var args map[string]any
				if err := json.Unmarshal(block.Input, &args); err == nil {
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
	// 优先使用服务端返回的模型名，如果为空则回退到请求时的模型名
	actualModel := string(msg.Model)
	if actualModel == "" {
		actualModel = model
	}
	return ChatResponse{Model: actualModel, Message: m, Usage: usage, Stop: string(msg.StopReason)}
}

// --- message mapping ---

func mapMessagesToAnthropic(msgs []models.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, m := range msgs {
		switch m.Role {
		case models.RoleHuman:
			if len(m.Images) > 0 {
				blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Images)+1)
				for _, img := range m.Images {
					blocks = append(blocks, anthropic.NewImageBlockBase64(img.MimeType, img.Base64))
				}
				if content := strings.TrimSpace(m.Content); content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(content))
				}
				result = appendOrMergeUser(result, blocks)
			} else {
				result = appendOrMergeUser(result, []anthropic.ContentBlockParamUnion{
					anthropic.NewTextBlock(ensureContent(m.Content, "user")),
				})
			}
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

// appendOrMergeUser merges blocks (tool_result blocks, or a RoleHuman
// message's text/image blocks) into the last user message if it's also user
// role, appending at the tail so tool_result-first ordering within a merged
// turn is preserved; otherwise it starts a new user message.
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

// Claude 4.7+ (and Fable/Mythos at any version) reject temperature with a
// 400; only send it to models that still accept sampling parameters.
var claudeNoSamplingRE = regexp.MustCompile(
	`^claude-(?:opus|sonnet)-(?:[5-9]|4-[7-9])|^claude-(?:fable|mythos)`)

func acceptsTemperature(model string) bool {
	return !claudeNoSamplingRE.MatchString(strings.ToLower(model))
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
		if apierr.StatusCode == 429 || apierr.StatusCode == 529 || apierr.StatusCode >= 500 {
			return true
		}
		if apierr.StatusCode != 0 {
			return false
		}
	}
	return isTransientStreamError(err)
}

func isTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	needles := []string{
		"unexpected end of json input", "unexpected eof", "eof",
		"connection reset", "connection refused", "broken pipe", "no such host",
		"api_error", "overloaded", "internal server error", "service unavailable",
		"bad gateway", "gateway timeout", "upstream",
		"rate limit", "rate_limit",
		"try again", "please retry", "retry later", "temporarily", "temporary",
		"网络错误", "请稍后重试", "请重试", "稍后再试", "稍后重试", "服务繁忙", "系统繁忙",
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
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
				req.Body = io.NopCloser(bytes.NewBuffer(body))
				// Log a compact summary instead of the full payload to avoid
				// bloating debug logs with large tool results.
				var summary struct {
					Model    string `json:"model"`
					Messages []any  `json:"messages"`
					Tools    []any  `json:"tools"`
				}
				if json.Unmarshal(body, &summary) == nil {
					hasImage := bytes.Contains(body, []byte(`"type":"image"`))
					slog.Debug("provider payload",
						"provider", provider,
						"model", summary.Model,
						"messages", len(summary.Messages),
						"tools", len(summary.Tools),
						"body_bytes", len(body),
						"has_image", hasImage)
				} else {
					slog.Debug("provider payload", "provider", provider, "body_bytes", len(body))
				}
			}
		}
		return next(req)
	}
}
