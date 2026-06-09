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
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/tools"
)

const defaultMaxTurns = 0 // 0 = unlimited, rely on token budget and context cancellation
const defaultRequestTimeout = 10 * time.Minute

var messageSeq uint64
var agentRequestSeq uint64

// Agent runs our custom ReAct loop while delegating model streaming and tool schemas to the LLM provider abstraction.
type Agent struct {
	llm             llm.LLMProvider
	tools           *tools.Registry
	sandbox         *sandbox.Sandbox
	logger          *slog.Logger
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
	// lastInputTokens is the provider's own reported input-token count from the
	// most recent response — authoritative for the model's real tokenizer, which
	// the byte heuristic underestimates for CJK/multi-byte text. lastTokenCount-
	// Msgs records how many messages that count covered, so growth since then can
	// be added without re-counting from scratch.
	lastInputTokens    int
	lastTokenCountMsgs int

	// Memory integration
	memoryService   *memory.Service
	memoryExtractor memory.Extractor
	memoryUserID    string

	// Skill tracking for memory source tagging
	activeSkill atomic.Value // stores string

	// User interaction
	userInteraction tools.UserInteraction

	// Plan mode: restrict to read-only tools until user approves
	planMode  atomic.Bool
	fullTools *tools.Registry // saved full tool set, restored on exit
	workDir   string          // working directory for plan files
	planFile  string          // path to the current plan file

	// Diagnostic: warn at most once when the events channel overflows so the
	// silent drop is visible in logs without flooding when the slow consumer
	// stays slow for many events in a row.
	eventDropWarned atomic.Bool
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
	// 0 means unlimited — the caller (e.g. interactive REPL) governs
	// lifetime via context cancellation (Ctrl+C). A positive value applies a
	// hard deadline per Run() invocation.
	requestTimeout := cfg.RequestTimeout
	a := &Agent{
		llm:                 cfg.LLMProvider,
		tools:               registry,
		sandbox:             cfg.Sandbox,
		logger:              slog.Default().With("component", "agent"),
		agentType:           cfg.AgentType,
		model:               resolveModel(cfg.Model),
		reasoningEffort:     strings.TrimSpace(cfg.ReasoningEffort),
		systemPrompt:        buildSystemPrompt(strings.TrimSpace(cfg.SystemPrompt), time.Now().Format("2006-01-02")),
		temperature:         cfg.Temperature,
		maxTokens:           cfg.MaxTokens,
		maxTurns:            maxTurns,
		maxTokensBudget:     cfg.MaxTokensBudget,
		requestTimeout:      requestTimeout,
		events:              make(chan AgentEvent, 128),
		contextWindow:       cfg.ContextWindow,
		compactionThreshold: resolveCompactionThreshold(cfg.CompactionThreshold),
		compactionKeepTail:  resolveCompactionKeepTail(cfg.CompactionKeepTail),
		memoryService:       cfg.MemoryService,
		memoryExtractor:     cfg.MemoryExtractor,
		memoryUserID:        cfg.MemoryUserID,
		userInteraction:     cfg.UserInteraction,
		workDir:             cfg.WorkDir,
	}

	// Register plan mode tools (agent self-references via closures).
	a.registerPlanTools()

	// Start in plan mode if requested (e.g. user typed /plan).
	if cfg.PlanMode {
		a.enterPlanMode()
	}

	return a
}

// AppendSystemPrompt appends extra text to the agent's system prompt.
// Must be called before Run.
// ActiveSkill returns the name of the currently active skill, or "" if none.
func (a *Agent) ActiveSkill() string {
	if a == nil {
		return ""
	}
	v := a.activeSkill.Load()
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

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

	// Reset skill tracking for this run.
	a.activeSkill.Store("")

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.llm == nil {
		return nil, fmt.Errorf("agent llm provider is required")
	}
	if a.requestTimeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, a.requestTimeout)
			defer cancel()
		}
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
	// validationFailures tracks consecutive tool-validation errors per tool name.
	// When the same tool fails argument validation 3 times in a row the agent
	// injects a synthetic human message to break the loop.
	validationFailures := make(map[string]int)
	consecutiveValidationFailures := 0

	for turn := 0; ; turn++ {
		a.logger.Debug("turn start", "turn", turn, "model", a.model, "messages", len(runMessages))
		// Safety valve: hard turn cap (0 = unlimited)
		if a.maxTurns > 0 && turn >= a.maxTurns {
			err := fmt.Errorf("agent exceeded max turns (%d)", a.maxTurns)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return &RunResult{Messages: runMessages, Usage: usage}, err
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
			return &RunResult{Messages: runMessages, Usage: usage}, err
		}

		// Context compaction: compress old messages when approaching context window.
		if a.contextWindow > 0 {
			// Tool schemas are sent on every request and count against the
			// provider's window, but estimateTokens only sees messages. Add them
			// here so the agent doesn't underestimate the real payload and
			// compact too late (which is how an overflowing session still slips
			// past the threshold and hits the provider's hard limit).
			estimated := a.estimateContextTokens(runMessages)
			ratio := float64(estimated) / float64(a.contextWindow)
			if ratio >= a.compactionThreshold {
				// Flush memory synchronously before compaction to guarantee no data loss.
				// This blocks while the LLM extracts, but compaction is infrequent
				// and losing information is worse than the latency cost.
				if a.memoryService != nil && a.memoryExtractor != nil {
					// Cancel any queued update for this session so the
					// stale async job won't overwrite our sync flush.
					a.memoryService.CancelPendingUpdates(sessionID)
					flushCtx, flushCancel := context.WithTimeout(ctx, 30*time.Second)
					if skillName := a.ActiveSkill(); skillName != "" {
						_ = a.memoryService.UpdateWithFactSource(flushCtx, sessionID, runMessages, a.memoryExtractor, "skill:"+skillName)
					} else {
						_ = a.memoryService.UpdateWith(flushCtx, sessionID, runMessages, a.memoryExtractor)
					}
					flushCancel()
				}
				before := len(runMessages)
				compacted, didCompact := compactMessages(runMessages, a.compactionKeepTail)
				if didCompact {
					a.logger.Debug("context compaction", "turn", turn, "before", before, "after", len(compacted), "estimated_tokens", estimated, "ratio", fmt.Sprintf("%.2f", ratio))
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
					a.lastTokenCountMsgs = 0
				}

				// If still over the hard limit (ratio > 1.0), apply increasingly
				// aggressive compaction with a shrinking tail to prevent sending
				// an oversized payload that would cause the provider to drop the
				// connection mid-stream ("unexpected end of JSON input").
				afterEstimated := a.estimateContextTokens(runMessages)
				if afterRatio := float64(afterEstimated) / float64(a.contextWindow); afterRatio > 1.0 {
					// About to rewrite message content in place; the provider
					// anchor no longer matches those messages, so drop it and let
					// the byte heuristic track the shrinking payload.
					a.lastInputTokens = 0
					a.lastTokenCountMsgs = 0
					for tail := a.compactionKeepTail - 1; tail >= 2; tail-- {
						c2, ok := compactMessages(runMessages, tail)
						if ok {
							runMessages = c2
						}
						e2 := a.estimateContextTokens(runMessages)
						a.logger.Debug("aggressive compaction", "turn", turn, "tail", tail, "estimated", e2, "ratio", fmt.Sprintf("%.2f", float64(e2)/float64(a.contextWindow)))
						if float64(e2)/float64(a.contextWindow) <= 1.0 {
							break
						}
					}
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
			SystemPrompt:    a.BuildSystemPrompt(ctx, sessionID, runMessages),
		}

		stream, err := a.llm.Stream(ctx, req)
		if err != nil {
			if isContextOverflowError(err) {
				if compacted, ok := a.compactOnOverflow(runMessages, turn, "stream"); ok {
					runMessages = compacted
					continue
				}
			}
			err = normalizeRunError(ctx, err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return &RunResult{Messages: runMessages, Usage: usage}, err
		}

		var (
			aiMessageID = newMessageID("ai")
			textBuilder strings.Builder
			toolCalls   []models.ToolCall
			streamUsage *llm.Usage
			stopReason  string
		)

		overflowRetry := false
		for chunk := range stream {
			if chunk.Err != nil {
				if isContextOverflowError(chunk.Err) {
					if compacted, ok := a.compactOnOverflow(runMessages, turn, "chunk"); ok {
						runMessages = compacted
						overflowRetry = true
						// Drain remaining chunks so the upstream goroutine isn't leaked.
						for range stream {
						}
						break
					}
				}
				err := normalizeRunError(ctx, chunk.Err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
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
		if overflowRetry {
			continue
		}
		if err := ctx.Err(); err != nil {
			err = normalizeRunError(ctx, err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return &RunResult{Messages: runMessages, Usage: usage}, err
		}

		if streamUsage != nil {
			accumulateUsage(usage, streamUsage)
			if streamUsage.InputTokens > 0 {
				// The assistant message for this turn isn't appended yet, so
				// len(runMessages) is exactly the message set the provider just
				// counted. Anchor here so later estimates add only the growth.
				a.lastInputTokens = streamUsage.InputTokens
				a.lastTokenCountMsgs = len(runMessages)
			}
		}

		a.logger.Debug("llm response", "turn", turn, "text_len", textBuilder.Len(), "tool_calls", len(toolCalls), "stop", stopReason)
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
			// If the model hit context length limits, compact and retry
			// instead of silently ending with empty output.
			if isContextOverflow(stopReason) {
				compacted, didCompact := compactMessages(runMessages, a.compactionKeepTail)
				if !didCompact {
					compacted, didCompact = compactMessages(runMessages, 4)
				}
				if didCompact && len(compacted) < len(runMessages) {
					a.logger.Debug("compacting after context overflow", "turn", turn, "before", len(runMessages), "after", len(compacted))
					runMessages = compacted
					continue
				}
				a.logger.Warn("context overflow and compaction cannot reduce further", "turn", turn, "messages", len(runMessages))
			}
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

		// Tool calls execution.
		// When ALL tool calls in this batch are declared ParallelSafe, run
		// them concurrently and only serialize the surrounding event/message
		// bookkeeping. A single non-parallel-safe call (bash, edit_file,
		// skill, ...) forces the whole batch to run sequentially so mutating
		// tools observe each other's effects deterministically.
		if a.allParallelSafe(toolCalls) && len(toolCalls) > 1 {
			runningCalls := make([]models.ToolCall, len(toolCalls))
			for i, call := range toolCalls {
				emit(AgentEvent{
					Type:      AgentEventToolCall,
					MessageID: aiMessageID,
					ToolCall:  &call,
					ToolEvent: newToolCallEvent(call, nil),
				})
				running := call
				running.Status = models.CallStatusRunning
				running.StartedAt = time.Now().UTC()
				runningCalls[i] = running
				emit(AgentEvent{
					Type:      AgentEventToolCallStart,
					ToolCall:  &running,
					ToolEvent: newToolCallEvent(running, nil),
				})
			}
			results := make([]models.ToolResult, len(toolCalls))
			var wg sync.WaitGroup
			for i, call := range toolCalls {
				i, call := i, call
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = a.runOneTool(ctx, sessionID, call)
				}()
			}
			wg.Wait()
			batchClean := true
			for i, call := range toolCalls {
				result := results[i]
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
				completed := runningCalls[i]
				completed.Status = result.Status
				completed.CompletedAt = result.CompletedAt
				emit(AgentEvent{
					Type:      AgentEventToolCallEnd,
					MessageID: toolMessage.ID,
					ToolCall:  &completed,
					Result:    &result,
					ToolEvent: newToolEventFromResult(completed, result),
				})
				// Circuit-breaker for parallel path (same logic as serial path).
				if result.Status == models.CallStatusFailed && isValidationError(result.Error) {
					batchClean = false
					consecutiveValidationFailures++
					validationFailures[call.Name]++
					if validationFailures[call.Name] >= maxValidationRetries {
						hint := fmt.Sprintf(
							"You have called %q %d times without providing the required arguments and each attempt failed with: %s. "+
								"Please re-read the tool schema carefully, provide ALL required arguments, or ask the user for the missing information instead of retrying.",
							call.Name, validationFailures[call.Name], result.Error,
						)
						runMessages = append(runMessages, models.Message{
							ID:        newMessageID("human"),
							SessionID: sessionID,
							Role:      models.RoleHuman,
							Content:   hint,
							CreatedAt: time.Now().UTC(),
						})
						validationFailures[call.Name] = 0
					}
					if consecutiveValidationFailures >= 8 {
						err := fmt.Errorf("too many consecutive tool argument validation failures (%d): %s", consecutiveValidationFailures, result.Error)
						agentErr := &AgentError{
							Code:       "tool_validation_loop",
							Message:    err.Error(),
							Suggestion: "Model repeatedly called tools without required arguments. Try a shorter request or explicitly provide missing parameters.",
						}
						emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: agentErr})
						return &RunResult{Messages: runMessages, Usage: usage}, err
					}
				} else {
					validationFailures[call.Name] = 0
				}
			}
			if batchClean {
				consecutiveValidationFailures = 0
			}
			if err := ctx.Err(); err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
			}
			continue
		}

		batchClean := true
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

			result := a.runOneTool(ctx, sessionID, call)

			// If a skill was loaded, inject its body into the system prompt
			// so it doesn't need to be repeated in every turn's history.
			if result.ToolName == "skill" {
				if m, ok := result.Data["system_prompt"]; ok {
					if sp, _ := m.(string); sp != "" {
						a.removeSkillDescriptions()
						a.AppendSystemPrompt(sp)
					}
				}
				// Track active skill for memory source tagging.
				if skillName, ok := result.Data["skill_name"]; ok {
					if name, _ := skillName.(string); name != "" {
						a.activeSkill.Store(name)
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

			// Circuit-breaker: if the same tool fails argument validation 3 times
			// in a row, inject a human hint and reset the counter. This prevents
			// infinite loops where the model keeps omitting required arguments.
			const maxValidationRetries = 3
			if result.Status == models.CallStatusFailed && isValidationError(result.Error) {
				batchClean = false
				consecutiveValidationFailures++
				validationFailures[call.Name]++
				if validationFailures[call.Name] >= maxValidationRetries {
					hint := fmt.Sprintf(
						"You have called %q %d times without providing the required arguments and each attempt failed with: %s. "+
							"Please re-read the tool schema carefully, provide ALL required arguments, or ask the user for the missing information instead of retrying.",
						call.Name, validationFailures[call.Name], result.Error,
					)
					runMessages = append(runMessages, models.Message{
						ID:        newMessageID("human"),
						SessionID: sessionID,
						Role:      models.RoleHuman,
						Content:   hint,
						CreatedAt: time.Now().UTC(),
					})
					validationFailures[call.Name] = 0
				}
				if consecutiveValidationFailures >= 8 {
					err := fmt.Errorf("too many consecutive tool argument validation failures (%d): %s", consecutiveValidationFailures, result.Error)
					agentErr := &AgentError{
						Code:       "tool_validation_loop",
						Message:    err.Error(),
						Suggestion: "Model repeatedly called tools without required arguments. Try a shorter request or explicitly provide missing parameters.",
					}
					emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: agentErr})
					return &RunResult{Messages: runMessages, Usage: usage}, err
				}
			} else {
				validationFailures[call.Name] = 0
			}

			if err := ctx.Err(); err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
			}
		}
		if batchClean {
			consecutiveValidationFailures = 0
		}
	}
}

func recentConversationContext(messages []models.Message) string {
	const maxMessages = 6
	const maxBytes = 4000
	var parts []string
	for i := len(messages) - 1; i >= 0 && len(parts) < maxMessages; i-- {
		m := messages[i]
		if m.Role != models.RoleHuman && m.Role != models.RoleAI {
			continue
		}
		if c := strings.TrimSpace(m.Content); c != "" {
			parts = append(parts, c)
		}
	}
	joined := strings.Join(parts, "\n")
	if len(joined) > maxBytes {
		joined = joined[len(joined)-maxBytes:]
	}
	return joined
}

func (a *Agent) BuildSystemPrompt(ctx context.Context, sessionID string, runMessages []models.Message) string {
	sections := []string{strings.TrimSpace(a.systemPrompt)}

	if a.memoryService != nil {
		activeSource := ""
		if skillName := a.ActiveSkill(); skillName != "" {
			activeSource = "skill:" + skillName
		}
		relevanceContext := recentConversationContext(runMessages)
		if uid := strings.TrimSpace(a.memoryUserID); uid != "" {
			if userMem := a.memoryService.InjectScopeWithContext(ctx, memory.UserScope(uid), relevanceContext, activeSource); userMem != "" {
				sections = append(sections, userMem)
			}
		}
		if strings.TrimSpace(sessionID) != "" {
			if injection := a.memoryService.InjectWithContext(ctx, sessionID, relevanceContext, activeSource); injection != "" {
				sections = append(sections, injection)
			}
		}
	}

	sections = append(sections, "File-operation rule: ALWAYS use the dedicated tools, never bash, to read, edit, write, search, or list files \xe2\x80\x94 read_file (not cat/head/tail/sed), edit_file (not sed/awk/perl), write_file (not echo>/cat>/tee), list_dir (not ls), find (not the find command), grep (not grep/rg/ag). If an edit_file call fails to match, re-read the file with read_file and retry edit_file; do NOT fall back to bash sed/perl. Reserve bash for building, running, testing, package managers, git, and operations no dedicated tool covers.")
	sections = a.appendPlanModePrompt(sections)
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
		// Buffer full — a slow consumer (renderer / HTTP client) is falling
		// behind. We still drop to keep the agent live, but surface it once so
		// the missing chunks aren't completely silent. Final assistant text is
		// always re-emitted in AgentEventEnd.Text, so correctness is preserved.
		if a.eventDropWarned.CompareAndSwap(false, true) {
			a.logger.Warn("agent event channel full, dropping events", "event_type", evt.Type, "buffer_size", cap(a.events))
		}
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

func buildSystemPrompt(base string, date string) string {
	var b strings.Builder
	if base != "" {
		b.WriteString(base)
		b.WriteString("\n\n")
	}
	b.WriteString("# Current date\nToday's date is ")
	b.WriteString(date)
	b.WriteByte('.')
	return b.String()
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
	// Only map to agent timeout when the run context itself hit deadline.
	// Keep inner/tool deadline errors intact so users see the real source.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
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
	s := result.Content
	if result.Error != "" {
		s = result.Error
	}
	if len(s) > maxToolContentBytes {
		s = s[:maxToolContentBytes] + fmt.Sprintf("\n... [truncated: %d bytes total]", len(s))
	}
	return s
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

// toolSchemaTokens estimates the token cost of the tool definitions attached to
// every request (name + description + JSON schema). These are invisible to
// estimateTokens, which only inspects messages, so they must be added to any
// context-window estimate or the agent will compact later than the real payload
// warrants. Uses the same ~3-bytes-per-token heuristic as estimateTokens.
func (a *Agent) toolSchemaTokens() int {
	if a == nil || a.tools == nil {
		return 0
	}
	totalBytes := 0
	for _, t := range a.tools.List() {
		totalBytes += len(t.Name) + len(t.Description) + 10 // per-tool framing
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				totalBytes += len(b)
			}
		}
	}
	return totalBytes / 3
}

// estimateContextTokens returns the best estimate of how many tokens the next
// request will occupy. It prefers the provider's own reported input-token count
// from the previous response — accurate for the model's real tokenizer, which
// the byte heuristic underestimates for CJK/multi-byte text — plus a byte
// estimate of any messages appended since that count was taken. The anchor
// (lastInputTokens/lastTokenCountMsgs) is reset to zero at every compaction
// site, so whenever it is set the first lastTokenCountMsgs messages are exactly
// what the provider counted. Falls back to the pure byte heuristic (plus tool
// schemas) before the first response or right after a compaction reset.
func (a *Agent) estimateContextTokens(runMessages []models.Message) int {
	heuristic := estimateTokens(runMessages, a.systemPrompt, 0) + a.toolSchemaTokens()
	if a.lastInputTokens <= 0 || a.lastTokenCountMsgs <= 0 || a.lastTokenCountMsgs > len(runMessages) {
		return heuristic
	}
	// lastInputTokens already covers the system prompt, tool schemas, and the
	// first lastTokenCountMsgs messages; add only the growth since the anchor.
	delta := estimateTokens(runMessages[a.lastTokenCountMsgs:], "", 0)
	if provider := a.lastInputTokens + delta; provider > heuristic {
		return provider
	}
	return heuristic
}

func isContextOverflow(stopReason string) bool {
	switch stopReason {
	case "max_tokens", "length", "model_context_window_exceeded":
		return true
	}
	return false
}

// isContextOverflowError detects context-window overflow surfaced as a
// transport-level error (typically HTTP 400 from OpenAI-compatible providers
// such as DeepSeek/Qwen/GLM that don't expose stopReason in this case).
// Matched substrings are intentionally specific to avoid false positives on
// generic "too long" complaints.
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	needles := []string{
		"context_length_exceeded",       // openai
		"context length",                // openai variants
		"maximum context length",        // openai
		"context window",                // generic
		"model_context_window_exceeded", // anthropic stop reason surfaced as error
		"prompt is too long",            // anthropic
		"input is too long",             // anthropic / qwen
		"request too large",             // openai
		"reduce the length",             // openai "please reduce the length..."
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// compactOnOverflow tries to shrink runMessages and reports whether the caller
// should continue the outer loop (i.e. retry the LLM request).
//
// compactMessages rewrites message *content* but preserves message *count*, so
// the retry must be gated on the estimated token size dropping — gating on
// len() (as a previous version did) was always false, leaving this reactive
// backstop dead and turning every provider context-overflow into a hard
// failure. Progressively smaller tails are tried so a payload dominated by a
// big tail can still be reduced.
func (a *Agent) compactOnOverflow(runMessages []models.Message, turn int, where string) ([]models.Message, bool) {
	before := estimateTokens(runMessages, a.systemPrompt, 0)
	for _, tail := range []int{a.compactionKeepTail, 4, 2} {
		if tail <= 0 {
			continue
		}
		compacted, didCompact := compactMessages(runMessages, tail)
		if !didCompact {
			continue
		}
		after := estimateTokens(compacted, a.systemPrompt, 0)
		if after < before {
			a.logger.Warn("compacting after "+where+" context overflow", "turn", turn, "tail", tail, "before_tokens", before, "after_tokens", after)
			// Content changed under the provider anchor; invalidate it so the
			// next estimate uses the byte heuristic on the compacted messages.
			a.lastInputTokens = 0
			a.lastTokenCountMsgs = 0
			return compacted, true
		}
	}
	a.logger.Warn("context overflow and compaction cannot reduce further", "where", where, "turn", turn, "messages", len(runMessages))
	return runMessages, false
}

// runOneTool executes a single tool call with the standard sandbox/thread/UI
// context plumbing and normalizes errors into a Failed ToolResult so callers
// can treat success and failure uniformly.
func (a *Agent) runOneTool(ctx context.Context, sessionID string, call models.ToolCall) models.ToolResult {
	toolStarted := time.Now().UTC()
	toolCtx := tools.WithSandbox(ctx, a.sandbox)
	toolCtx = tools.WithThreadID(toolCtx, sessionID)
	if a.userInteraction != nil {
		toolCtx = tools.WithUserInteraction(toolCtx, a.userInteraction)
	}
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
	return result
}

// allParallelSafe reports whether every call in the batch resolves to a
// registered tool that has declared ParallelSafe=true. Unknown tools and any
// false flag short-circuit to false so the safe (sequential) path is taken.
func (a *Agent) allParallelSafe(calls []models.ToolCall) bool {
	if a.tools == nil {
		return false
	}
	for _, c := range calls {
		t := a.tools.Get(c.Name)
		if t == nil || !t.ParallelSafe {
			return false
		}
	}
	return true
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

// isValidationError reports whether a tool error string originates from
// argument validation (missing required args, invalid schema). Used by the
// circuit-breaker to distinguish fixable model mistakes from real failures.
func isValidationError(errMsg string) bool {
	return strings.HasPrefix(errMsg, "missing required argument") ||
		strings.HasPrefix(errMsg, "invalid tool arguments")
}

// maxValidationRetries is the number of consecutive validation failures for the
// same tool before the circuit-breaker injects a corrective human hint.
const maxValidationRetries = 3
