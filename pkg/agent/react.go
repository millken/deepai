package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/tools"
)

const defaultMaxTurns = 0 // 0 = unlimited, rely on token budget and context cancellation
const defaultRequestTimeout = 10 * time.Minute
const maxToolResultInHistory = 8192 // truncate tool results in history to 8KB

var messageSeq uint64
var agentRequestSeq uint64

// Agent runs our custom ReAct loop while delegating model streaming and tool schemas to the LLM provider abstraction.
type Agent struct {
	llm             llm.LLMProvider
	tools           *tools.Registry
	sandbox         *sandbox.Sandbox
	agentType       AgentType
	model           string
	reasoningEffort string
	systemPrompt    string
	temperature     *float64
	maxTokens       *int
	maxTurns        int
	maxTokensBudget int
	requestTimeout  time.Duration
	events          chan AgentEvent
	requests        sync.Map
	runMu           sync.Mutex
	eventsMu        sync.RWMutex
	eventsClosed    bool
	started         bool

	// Context compaction
	contextWindow       int
	compactionThreshold float64
	compactionKeepTail  int
	lastInputTokens     int
}

func New(cfg AgentConfig) *Agent {
	if err := ApplyAgentType(&cfg, cfg.AgentType); err != nil {
		cfg.AgentType = AgentTypeGeneral
		_ = ApplyAgentType(&cfg, AgentTypeGeneral)
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	registry := cfg.Tools
	if registry == nil {
		registry = tools.NewRegistry()
	}
	if cfg.PresentFiles != nil {
		registry = cloneRegistryWithPresentFileTool(registry, cfg.PresentFiles)
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	return &Agent{
		llm:             cfg.LLMProvider,
		tools:           registry,
		sandbox:         cfg.Sandbox,
		agentType:       cfg.AgentType,
		model:           resolveModel(cfg.Model),
		reasoningEffort: strings.TrimSpace(cfg.ReasoningEffort),
		systemPrompt:    strings.TrimSpace(cfg.SystemPrompt),
		temperature:     cfg.Temperature,
		maxTokens:       cfg.MaxTokens,
		maxTurns:        maxTurns,
		maxTokensBudget: cfg.MaxTokensBudget,
		requestTimeout:  requestTimeout,
		events:          make(chan AgentEvent, 128),
		contextWindow:       cfg.ContextWindow,
		compactionThreshold: resolveCompactionThreshold(cfg.CompactionThreshold),
		compactionKeepTail:  resolveCompactionKeepTail(cfg.CompactionKeepTail),
	}
}

// AppendSystemPrompt appends extra text to the agent's system prompt.
// Must be called before Run.
func (a *Agent) AppendSystemPrompt(extra string) {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return
	}
	if a.systemPrompt == "" {
		a.systemPrompt = extra
	} else {
		a.systemPrompt = a.systemPrompt + "\n\n" + extra
	}
}

// removeSkillDescriptions strips the "Available skills" section from system prompt.
// Called after a skill is loaded since the descriptions are no longer needed.
func (a *Agent) removeSkillDescriptions() {
	const marker = "Available skills (use the matching skill when the user request fits):"
	idx := strings.Index(a.systemPrompt, marker)
	if idx > 0 {
		a.systemPrompt = strings.TrimSpace(a.systemPrompt[:idx])
	}
}

func cloneRegistryWithPresentFileTool(base *tools.Registry, presentFiles *tools.PresentFileRegistry) *tools.Registry {
	cloned := tools.NewRegistry()
	if base != nil {
		for _, tool := range base.List() {
			if tool.Name == "present_file" {
				continue
			}
			_ = cloned.Register(tool)
		}
	}
	_ = cloned.Register(tools.PresentFileTool(presentFiles))
	return cloned
}

func (a *Agent) Events() <-chan AgentEvent {
	return a.events
}

func (a *Agent) Run(ctx context.Context, sessionID string, messages []models.Message) (*RunResult, error) {
	if a == nil {
		return nil, fmt.Errorf("agent is nil")
	}
	a.runMu.Lock()
	if a.started {
		a.runMu.Unlock()
		return nil, errors.New("agent instances are single-use")
	}
	a.started = true
	a.runMu.Unlock()
	defer a.closeEvents()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.llm == nil {
		return nil, fmt.Errorf("agent llm provider is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.requestTimeout)
		defer cancel()
	}

	requestID := newAgentRequestID()
	a.requests.Store(requestID, sessionID)
	defer a.requests.Delete(requestID)

	emit := func(evt AgentEvent) {
		evt.RequestID = requestID
		if evt.SessionID == "" {
			evt.SessionID = sessionID
		}
		a.emit(evt)
	}

	runMessages := append([]models.Message(nil), messages...)
	usage := &Usage{}

	for turn := 0; ; turn++ {
		slog.Debug("turn start", "turn", turn, "model", a.model, "messages", len(runMessages))
		// Safety valve: hard turn cap (0 = unlimited)
		if a.maxTurns > 0 && turn >= a.maxTurns {
			err := fmt.Errorf("agent exceeded max turns (%d)", a.maxTurns)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return nil, err
		}

		// Token budget check
		if a.maxTokensBudget > 0 && usage.TotalTokens >= a.maxTokensBudget {
			err := fmt.Errorf("agent exceeded token budget (%d/%d)", usage.TotalTokens, a.maxTokensBudget)
			agentErr := &AgentError{
				Code:       "token_budget_exceeded",
				Message:    err.Error(),
				Suggestion: "Increase token budget or simplify the request.",
			}
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: agentErr})
			return nil, err
		}

		// Context compaction: compress old messages when approaching context window.
		if a.contextWindow > 0 {
			estimated := estimateTokens(runMessages, a.systemPrompt, a.lastInputTokens)
			ratio := float64(estimated) / float64(a.contextWindow)
			if ratio >= a.compactionThreshold {
				before := len(runMessages)
				compacted, didCompact := compactMessages(runMessages, a.compactionKeepTail)
				if didCompact {
					slog.Debug("context compaction", "turn", turn, "before", before, "after", len(compacted), "estimated_tokens", estimated, "ratio", fmt.Sprintf("%.2f", ratio))
					runMessages = compacted
					emit(AgentEvent{
						Type: AgentEventCompact,
						CompactStats: &CompactStats{
							MessagesBefore: before,
							MessagesAfter:  len(compacted),
							InputTokens:    estimated,
							ContextWindow:  a.contextWindow,
							Ratio:          ratio,
						},
					})
					a.lastInputTokens = 0
				}
			}
		}

		req := llm.ChatRequest{
			Model:           a.model,
			Messages:        runMessages,
			Tools:           a.tools.List(),
			ReasoningEffort: a.reasoningEffort,
			Temperature:     a.temperature,
			MaxTokens:       a.maxTokens,
			SystemPrompt:    a.BuildSystemPrompt(ctx, sessionID),
		}

		stream, err := a.llm.Stream(ctx, req)
		if err != nil {
			err = normalizeRunError(ctx, err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return nil, err
		}

		var (
			aiMessageID = newMessageID("ai")
			textBuilder strings.Builder
			toolCalls   []models.ToolCall
			streamUsage *llm.Usage
			stopReason  string
		)

		for chunk := range stream {
			if chunk.Err != nil {
				err := normalizeRunError(ctx, chunk.Err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return nil, err
			}
			if chunk.Delta != "" {
				textBuilder.WriteString(chunk.Delta)
				emit(AgentEvent{Type: AgentEventChunk, MessageID: aiMessageID, Text: chunk.Delta})
				emit(AgentEvent{Type: AgentEventTextChunk, MessageID: aiMessageID, Text: chunk.Delta})
			}
			if len(chunk.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, chunk.ToolCalls)
			}
			if chunk.Usage != nil {
				streamUsage = chunk.Usage
			}
			if chunk.Done {
				stopReason = chunk.Stop
				if chunk.Message != nil {
					if textBuilder.Len() == 0 && chunk.Message.Content != "" {
						textBuilder.WriteString(chunk.Message.Content)
					}
					if len(toolCalls) == 0 && len(chunk.Message.ToolCalls) > 0 {
						toolCalls = append(toolCalls, chunk.Message.ToolCalls...)
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			err = normalizeRunError(ctx, err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return nil, err
		}

		if streamUsage != nil {
			accumulateUsage(usage, streamUsage)
			if streamUsage.InputTokens > 0 {
				a.lastInputTokens = streamUsage.InputTokens
			}
		}

		slog.Debug("llm response", "turn", turn, "text_len", textBuilder.Len(), "tool_calls", len(toolCalls), "stop", stopReason)
		assistantMetadata := map[string]string{"stop_reason": stopReason}
		if streamUsage != nil {
			if raw, err := json.Marshal(streamUsage); err == nil {
				assistantMetadata["usage_metadata"] = string(raw)
			}
		}
		assistantMessage := models.Message{
			ID:        aiMessageID,
			SessionID: sessionID,
			Role:      models.RoleAI,
			Content:   textBuilder.String(),
			ToolCalls: toolCalls,
			Metadata:  assistantMetadata,
			CreatedAt: time.Now().UTC(),
		}
		if assistantMessage.Content != "" || len(assistantMessage.ToolCalls) > 0 {
			runMessages = append(runMessages, assistantMessage)
		}

		if len(toolCalls) == 0 {
			emit(AgentEvent{
				Type:      AgentEventEnd,
				MessageID: aiMessageID,
				Text:      assistantMessage.Content,
				Usage:     cloneUsage(usage),
			})
			return &RunResult{
				Messages:    runMessages,
				FinalOutput: assistantMessage.Content,
				Usage:       usage,
			}, nil
		}

		for _, call := range toolCalls {
			emit(AgentEvent{
				Type:      AgentEventToolCall,
				MessageID: aiMessageID,
				ToolCall:  &call,
				ToolEvent: newToolCallEvent(call, nil),
			})
			startedAt := time.Now().UTC()
			runningCall := call
			runningCall.Status = models.CallStatusRunning
			runningCall.StartedAt = startedAt
			emit(AgentEvent{
				Type:      AgentEventToolCallStart,
				ToolCall:  &runningCall,
				ToolEvent: newToolCallEvent(runningCall, nil),
			})

			toolStarted := time.Now().UTC()
			toolCtx := tools.WithSandbox(ctx, a.sandbox)
			toolCtx = tools.WithThreadID(toolCtx, sessionID)
			result, err := a.tools.Execute(toolCtx, call)
			if err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				result = models.ToolResult{
					CallID:      call.ID,
					ToolName:    call.Name,
					Status:      models.CallStatusFailed,
					Error:       err.Error(),
					CompletedAt: time.Now().UTC(),
				}
			}
			result.Duration = time.Since(toolStarted)
			if result.CompletedAt.IsZero() {
				result.CompletedAt = time.Now().UTC()
			}

			// If a skill was loaded, inject its body into the system prompt
			// so it doesn't need to be repeated in every turn's history.
			if result.ToolName == "skill" {
				if m, ok := result.Data["system_prompt"]; ok {
					if sp, _ := m.(string); sp != "" {
						a.removeSkillDescriptions()
						a.AppendSystemPrompt(sp)
					}
				}
			}

			runMessages = append(runMessages, models.Message{
				ID:         newMessageID("tool"),
				SessionID:  sessionID,
				Role:       models.RoleTool,
				Content:    toolMessageContent(result),
				ToolResult: &result,
				CreatedAt:  time.Now().UTC(),
			})
			toolMessage := runMessages[len(runMessages)-1]
			emit(AgentEvent{
				Type:      AgentEventToolResult,
				MessageID: toolMessage.ID,
				Result:    &result,
				ToolEvent: newToolEventFromResult(call, result),
			})
			completedCall := runningCall
			completedCall.Status = result.Status
			completedCall.CompletedAt = result.CompletedAt
			emit(AgentEvent{
				Type:      AgentEventToolCallEnd,
				MessageID: toolMessage.ID,
				ToolCall:  &completedCall,
				Result:    &result,
				ToolEvent: newToolEventFromResult(completedCall, result),
			})

			if err := ctx.Err(); err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return nil, err
			}
		}
	}
}

func (a *Agent) BuildSystemPrompt(_ context.Context, _ string) string {
	sections := []string{
		strings.TrimSpace(a.systemPrompt),
		"Tool preference: use dedicated tools over bash for file operations \xe2\x80\x94 read_file (not cat/head/tail), edit_file (not sed/awk), write_file (not echo/cat>), list_dir (not ls), find (not find), grep (not grep/rg). Reserve bash for building, testing, git, and operations not covered by dedicated tools.",
	}
	return strings.Join(sections, "\n\n")
}

func (a *Agent) emit(evt AgentEvent) {
	a.eventsMu.RLock()
	defer a.eventsMu.RUnlock()
	if a.eventsClosed {
		return
	}
	select {
	case a.events <- evt:
	default:
	}
}

func (a *Agent) closeEvents() {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	if a.eventsClosed {
		return
	}
	close(a.events)
	a.eventsClosed = true
}

func resolveModel(model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	if model := strings.TrimSpace(os.Getenv("DEFAULT_LLM_MODEL")); model != "" {
		return model
	}
	return "gpt-4.1-mini"
}

func newMessageID(prefix string) string {
	seq := atomic.AddUint64(&messageSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UTC().UnixNano(), seq)
}

func newAgentRequestID() string {
	seq := atomic.AddUint64(&agentRequestSeq, 1)
	return fmt.Sprintf("req_%d_%d", time.Now().UTC().UnixNano(), seq)
}

func normalizeRunError(ctx context.Context, err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &TimeoutError{
			Duration: timeout,
			Message:  "agent request timed out",
		}
	}
	return err
}

func mergeToolCalls(existing, incoming []models.ToolCall) []models.ToolCall {
	if len(existing) == 0 {
		return append([]models.ToolCall(nil), incoming...)
	}

	indexByID := make(map[string]int, len(existing))
	for i, call := range existing {
		indexByID[call.ID] = i
	}

	for _, call := range incoming {
		if idx, ok := indexByID[call.ID]; ok {
			if existing[idx].Name == "" {
				existing[idx].Name = call.Name
			}
			if len(call.Arguments) > 0 {
				existing[idx].Arguments = call.Arguments
			}
			if call.Status != "" {
				existing[idx].Status = call.Status
			}
			continue
		}
		indexByID[call.ID] = len(existing)
		existing = append(existing, call)
	}

	return existing
}

func accumulateUsage(dst *Usage, src *llm.Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
}

func cloneUsage(src *Usage) *Usage {
	if src == nil {
		return nil
	}
	out := *src
	return &out
}

func toolMessageContent(result models.ToolResult) string {
	if result.Error != "" {
		return result.Error
	}
	return truncateForHistory(result.Content)
}

func truncateForHistory(s string) string {
	if len(s) <= maxToolResultInHistory {
		return s
	}
	return s[:maxToolResultInHistory] + fmt.Sprintf("\n... [truncated: showing %d of %d bytes]", maxToolResultInHistory, len(s))
}

func newToolCallEvent(call models.ToolCall, result *models.ToolResult) *ToolCallEvent {
	event := &ToolCallEvent{
		ID:            call.ID,
		Name:          call.Name,
		Arguments:     cloneArguments(call.Arguments),
		ArgumentsText: formatToolArguments(call.Arguments),
		Status:        call.Status,
		RequestedAt:   formatEventTime(call.RequestedAt),
		StartedAt:     formatEventTime(call.StartedAt),
		CompletedAt:   formatEventTime(call.CompletedAt),
	}
	if result != nil {
		event.Result = cloneToolResult(result)
		event.ResultPreview = toolResultPreview(*result)
		event.Error = result.Error
		event.DurationMS = result.Duration.Milliseconds()
		if event.Status == "" {
			event.Status = result.Status
		}
		if event.CompletedAt == "" {
			event.CompletedAt = formatEventTime(result.CompletedAt)
		}
	}
	return event
}

func newToolEventFromResult(call models.ToolCall, result models.ToolResult) *ToolCallEvent {
	return newToolCallEvent(call, &result)
}

func cloneArguments(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	return out
}

func cloneToolResult(result *models.ToolResult) *models.ToolResult {
	if result == nil {
		return nil
	}
	copyResult := *result
	if len(result.Data) > 0 {
		copyResult.Data = make(map[string]any, len(result.Data))
		for k, v := range result.Data {
			copyResult.Data[k] = v
		}
	}
	return &copyResult
}

func formatToolArguments(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func toolResultPreview(result models.ToolResult) string {
	content := strings.TrimSpace(result.Content)
	if content == "" {
		content = strings.TrimSpace(result.Error)
	}
	if content == "" && len(result.Data) > 0 {
		raw, err := json.Marshal(result.Data)
		if err == nil {
			content = string(raw)
		}
	}
	content = strings.ReplaceAll(content, "\n", " ")
	if len(content) > 240 {
		return content[:240] + "..."
	}
	return content
}

func formatEventTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func newAgentError(err error) *AgentError {
	if err == nil {
		return nil
	}
	agentErr := &AgentError{
		Message: err.Error(),
	}
	switch {
	case errors.Is(err, context.Canceled):
		agentErr.Code = "context_canceled"
		agentErr.Suggestion = "Retry the run if the cancellation was unintended."
		agentErr.Retryable = true
	case errors.Is(err, context.DeadlineExceeded):
		agentErr.Code = "deadline_exceeded"
		agentErr.Suggestion = "Retry with a longer timeout or lower max_tokens."
		agentErr.Retryable = true
	case strings.Contains(strings.ToLower(err.Error()), "max turns"):
		agentErr.Code = "max_turns_exceeded"
		agentErr.Suggestion = "Increase max turns or simplify the request."
	case strings.Contains(strings.ToLower(err.Error()), "token budget"):
		agentErr.Code = "token_budget_exceeded"
		agentErr.Suggestion = "Increase token budget or simplify the request."
	case strings.Contains(strings.ToLower(err.Error()), "api key"):
		agentErr.Code = "provider_auth"
		agentErr.Suggestion = "Verify the provider credentials and base URL."
	default:
		agentErr.Code = "run_error"
		agentErr.Suggestion = "Retry the run or inspect the previous tool and model events."
		agentErr.Retryable = true
	}
	return agentErr
}
