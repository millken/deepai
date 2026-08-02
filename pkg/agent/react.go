package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// defaultStreamIdleTimeout bounds the max silence BETWEEN chunks of a single
// streaming request (not the total request time — see Agent.streamIdleTimeout).
// Generous relative to any real provider's chunk cadence, so it only trips on
// a genuinely stalled request.
const defaultStreamIdleTimeout = 2 * time.Minute

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
	// streamIdleTimeout bounds the max silence BETWEEN chunks of a single
	// streaming request — NOT the total request time (that's requestTimeout).
	// The HTTP client deliberately has no per-request timeout (pkg/llm/http.go),
	// so without this a stream that emits nothing can hang until the outer
	// deadline (or forever, if there is none). Defaults to
	// defaultStreamIdleTimeout; not exposed via AgentConfig — tests in this
	// package set the field directly.
	streamIdleTimeout time.Duration
	events            chan AgentEvent
	runMu             sync.Mutex
	eventsMu          sync.RWMutex
	eventsClosed      bool
	started           bool

	// Context compaction
	contextWindow       int
	compactionThreshold float64
	compactionKeepTail  int
	// aging derives a per-request compressed prompt view (T1/T4). nil = disabled.
	aging *AgingConfig
	// metrics is the Phase 0 measurement sink. nil = disabled (zero overhead).
	metrics MetricsSink
	// lastInputTokens is the provider's own reported input-token count from the
	// most recent response — authoritative for the model's real tokenizer, which
	// the byte heuristic underestimates for CJK/multi-byte text. lastTokenCount-
	// Msgs records how many messages that count covered, so growth since then can
	// be added without re-counting from scratch.
	lastInputTokens    int
	lastTokenCountMsgs int
	// compactionStalled is set when a compaction pass fails to bring the
	// ratio back under compactionThreshold. This is NOT necessarily a
	// permanent floor: the escalating retry loop inside the compaction
	// branch only tries to get the estimate under the raw context window
	// (afterEstimated <= a.contextWindow), so landing in the
	// [compactionThreshold, 1.0) band after a real, productive compaction is
	// routine, not a sign nothing more can ever be compacted. While
	// compactionStalled is set, the proactive branch is skipped so a session
	// genuinely stuck at its floor doesn't re-evaluate and re-compact
	// (including the synchronous, potentially 30s-timeout memory flush) on
	// every single subsequent turn for no benefit — but compactionStalledAt
	// (the canonical message count at the moment of stalling) lets the guard
	// clear itself once enough new messages have been appended that the
	// messages protected as "tail" at stall time have slid into the
	// compactable head region, so a long-running, still-growing session
	// keeps getting compacted every ~compactionKeepTail turns instead of
	// never again after the first inconclusive attempt.
	compactionStalled   bool
	compactionStalledAt int

	// Memory integration
	memoryService   *memory.Service
	memoryExtractor memory.Extractor
	memoryUserID    string

	// Skill tracking for memory source tagging
	activeSkill atomic.Value // stores string

	// User interaction
	userInteraction tools.UserInteraction

	// Plan mode: restrict agent to read-only tools until user approves
	planMode  atomic.Bool
	fullTools *tools.Registry // saved full tool set, restored on exit
	workDir   string          // working directory for plan files
	planFile  string          // path to the current plan file

	// offloadDir is where tool results exceeding offloadThresholdBytes are
	// written to disk. Empty = offload disabled.
	offloadDir string

	// imageDetail controls vision detail level ("low"/"high") for image
	// attachments in ChatRequest.
	imageDetail string

	// nonInteractive marks sub-agents (no user to interact with).
	nonInteractive bool

	// agentCatalog lists available sub-agent types for delegation guidance.
	agentCatalog []AgentInfo

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
	// Opt-in token-efficiency features (metrics/aging) from env, unless the
	// caller set them explicitly. No-op by default.
	applyTokenEfficiencyDefaults(&cfg)
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
	// Auto-derive offload directory so all entry points (REPL, gateway,
	// subagent) get offload without each passing it explicitly.
	offloadDir := cfg.OffloadDir
	if offloadDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			offloadDir = filepath.Join(home, ".deepai", "offload")
		}
	}
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
		streamIdleTimeout:   defaultStreamIdleTimeout,
		events:              make(chan AgentEvent, 128),
		contextWindow:       cfg.ContextWindow,
		compactionThreshold: resolveCompactionThreshold(cfg.CompactionThreshold),
		compactionKeepTail:  resolveCompactionKeepTail(cfg.CompactionKeepTail),
		aging:               cfg.Aging,
		metrics:             cfg.Metrics,
		memoryService:       cfg.MemoryService,
		memoryExtractor:     cfg.MemoryExtractor,
		memoryUserID:        cfg.MemoryUserID,
		userInteraction:     cfg.UserInteraction,
		workDir:             cfg.WorkDir,
		offloadDir:          offloadDir,
		imageDetail:         cfg.ImageDetail,
		nonInteractive:      cfg.NonInteractive,
		agentCatalog:        cfg.AgentCatalog,
	}

	// Register plan mode tools (agent self-references via closures). Skipped for
	// non-interactive agents (subagents): plan mode needs a user to approve the
	// plan, and without one the agent stalls in read-only exploration.
	if !cfg.NonInteractive {
		// Clone before registering: cfg.Tools may be a registry shared across
		// agents (e.g. the REPL reuses one process-wide registry every turn).
		// Registering into it directly would leak this agent's plan tools
		// (bound via closure to this agent) into every other agent sharing it.
		a.tools = a.tools.Clone()
		a.registerPlanTools()

		// Start in plan mode if requested (e.g. user typed /plan).
		if cfg.PlanMode {
			a.enterPlanMode()
		}
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

	emit := func(evt AgentEvent) {
		evt.RequestID = requestID
		if evt.SessionID == "" {
			evt.SessionID = sessionID
		}
		a.emit(evt)
	}

	runMessages := append([]models.Message(nil), messages...)
	usage := &Usage{}
	// breaker holds all circuit-breaker state (validation-failure loop and
	// repeat-call loop). Both the serial and parallel tool-execution paths
	// below feed every (call, result) pair through breaker.observe in batch
	// order, so the two paths enforce identical limits from one implementation.
	breaker := newToolCallBreaker()
	// taskCallCount is the per-Run fan-out cap counter (M2-2 12c): persists
	// across turns (NOT reset per turn), incremented only for "task" tool
	// calls, and checked before execution in both the serial and parallel
	// dispatch paths below. Both paths run on the single goroutine driving
	// this loop (the parallel path only fans out the actual tool.Execute
	// calls, deciding admission up front on this goroutine), so no atomic is
	// needed.
	taskCallCount := 0

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

		// Assemble the system prompt ONCE per turn, before the compaction check,
		// so the check measures the same prompt the request below actually
		// sends (BuildSystemPrompt layers memory injections, the file-op rule,
		// tool recommendations, the delegation prompt + catalog, and plan-mode
		// text on top of the base a.systemPrompt — all of that was previously
		// invisible to the compaction trigger). Built from PRE-compaction
		// runMessages: BuildSystemPrompt's memory relevance context is a
		// heuristic (a recency-based excerpt), so it doesn't need the
		// post-compaction view to stay correct, and rebuilding it a second time
		// after compaction would cost another memory-service round trip for a
		// once-per-turn compaction check. Not recomputed even if compaction
		// fires below — a behavior consequence worth noting: on a turn where
		// compaction fires, the synchronous memory flush below runs (and
		// writes its extracted facts to storage) AFTER this BuildSystemPrompt
		// call, so the injected memory this turn still reflects PRE-flush
		// state; facts the flush just extracted only become visible to
		// InjectWithContext/InjectScopeWithContext starting next turn.
		systemPrompt := a.BuildSystemPrompt(ctx, sessionID, runMessages)

		// T1/T4: derive a per-request compressed prompt view from the canonical
		// runMessages. When aging is disabled this is a zero-copy pass-through.
		// The canonical runMessages (persisted, replayed, mined for memory) are
		// never modified — only the view sent to the provider is compressed.
		// Computed before the compaction check (and recomputed after, if
		// compaction fires) so the check measures what is actually sent, not
		// canonical history that aging may have shrunk considerably.
		promptView := buildPromptView(runMessages, a.aging, a.contextWindow)

		// Clear a previous stall once new material has slid into the
		// compactable region: enough messages have been appended since the
		// stall point that the messages protected as the tail back then are
		// no longer the tail now, so a fresh compaction attempt has new
		// ground to work with instead of just re-deriving the same
		// inconclusive result.
		if a.compactionStalled && len(runMessages) >= a.compactionStalledAt+a.compactionKeepTail {
			a.compactionStalled = false
		}

		// Context compaction: compress old messages when approaching context window.
		// Skipped entirely while compactionStalled is set: a previous turn
		// already discovered that compacting doesn't bring the ratio back
		// under threshold, and re-evaluating and re-compacting every
		// subsequent turn before enough new material has accumulated (see
		// the unstall check above) would only thrash (repeated synchronous
		// memory flushes and re-deriving the same inconclusive compaction)
		// for no benefit.
		if a.contextWindow > 0 && !a.compactionStalled {
			// Measure the assembled prompt + aged view (what the request below
			// actually sends), not canonical messages or the base system prompt —
			// otherwise compaction can fire too late (assembled prompt bigger than
			// base) or too early (aged view much smaller than canonical, so
			// compacting on the canonical estimate destroys history the provider
			// was never even going to see). Tool schemas are sent on every request
			// too; estimateContextTokens adds them internally.
			estimated := a.estimateContextTokens(promptView, systemPrompt)
			ratio := float64(estimated) / float64(a.contextWindow)
			if ratio >= a.compactionThreshold {
				before := len(runMessages)
				compacted, didCompact := compactMessages(runMessages, a.compactionKeepTail)
				if didCompact {
					// Flush memory synchronously before compaction to guarantee no
					// data loss. This blocks while the LLM extracts, but compaction
					// is infrequent and losing information is worse than the
					// latency cost. Moved inside didCompact (rather than gated only
					// on ratio >= threshold) so a trigger turn where
					// compactMessages finds nothing left to compact — already at
					// the head/tail floor — doesn't still pay a 30s-timeout flush
					// for a compaction that's not going to happen.
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

					runMessages = compacted
					a.lastInputTokens = 0
					a.lastTokenCountMsgs = 0
					// Canonical messages changed; the view sent to the provider
					// must be re-derived from them.
					promptView = buildPromptView(runMessages, a.aging, a.contextWindow)

					afterEstimated := a.estimateContextTokens(promptView, systemPrompt)
					if afterEstimated > a.contextWindow {
						for tail := a.compactionKeepTail - 1; tail >= 2; tail-- {
							c2, ok := compactMessages(runMessages, tail)
							if ok {
								runMessages = c2
							}
							promptView = buildPromptView(runMessages, a.aging, a.contextWindow)
							afterEstimated = a.estimateContextTokens(promptView, systemPrompt)
							if afterEstimated <= a.contextWindow {
								break
							}
						}
					}

					afterRatio := float64(afterEstimated) / float64(a.contextWindow)
					a.logger.Debug("context compaction", "turn", turn, "before_msgs", before, "after_msgs", len(runMessages), "before_tokens", estimated, "after_tokens", afterEstimated, "before_ratio", fmt.Sprintf("%.2f", ratio), "after_ratio", fmt.Sprintf("%.2f", afterRatio))
					emit(AgentEvent{
						Type: AgentEventCompact,
						CompactStats: &CompactStats{
							MessagesBefore: before,
							MessagesAfter:  len(runMessages),
							InputTokens:    estimated,
							AfterTokens:    afterEstimated,
							ContextWindow:  a.contextWindow,
							Ratio:          ratio,
							AfterRatio:     afterRatio,
						},
					})
					if afterRatio >= a.compactionThreshold {
						a.compactionStalled = true
						a.compactionStalledAt = len(runMessages)
						a.logger.Warn("context compaction did not drop the ratio under threshold; suppressing "+
							"further compaction attempts until enough new messages accumulate to make the "+
							"current tail compactable again", "turn", turn, "after_tokens", afterEstimated,
							"context_window", a.contextWindow, "threshold", a.compactionThreshold)
					}
				}
			}
		}

		// Phase 0 auxiliary metric: byte breakdown of the outgoing prompt.
		// Captured before the request; combined with the provider's real token
		// counts into one per-turn record once the response arrives.
		var pendingContext ContextBytes
		if a.metrics != nil {
			pendingContext = computeContextBytes(promptView, systemPrompt, a.toolSchemaBytes())
		}

		req := llm.ChatRequest{
			Model:           a.model,
			Messages:        promptView,
			Tools:           a.tools.List(),
			ReasoningEffort: a.reasoningEffort,
			Temperature:     a.temperature,
			MaxTokens:       a.maxTokens,
			SystemPrompt:    systemPrompt,
			ImageDetail:     a.imageDetail,
		}

		aiMessageID := newMessageID("ai")

		// Per-request cancellable ctx (stream idle watchdog, plan #8): a
		// stream that goes silent must not be allowed to hang until the
		// outer deadline (or forever, if there is none — the HTTP client
		// deliberately has no per-request timeout, see pkg/llm/http.go).
		// cancel is handed to consumeStream below, which defers it so the
		// per-request ctx is released on every exit path from this turn's
		// stream consumption; on the early Stream()-error return here it is
		// called explicitly instead, since consumeStream is never reached.
		reqCtx, cancel := context.WithCancel(ctx)
		stream, err := a.llm.Stream(reqCtx, req)
		if err != nil {
			cancel()
			if isContextOverflowError(err) {
				if compacted, ok := a.compactOnOverflow(runMessages, systemPrompt, turn, "stream"); ok {
					runMessages = compacted
					continue
				}
			}
			err = normalizeRunError(ctx, err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return &RunResult{Messages: runMessages, Usage: usage}, err
		}

		streamRes := a.consumeStream(stream, cancel, emit, aiMessageID)
		text := streamRes.text
		toolCalls := streamRes.toolCalls
		streamUsage := streamRes.usage
		stopReason := streamRes.stopReason

		if streamRes.err != nil {
			// Covers both a provider-surfaced chunk error (context overflow
			// or otherwise) and the stream idle watchdog's *TimeoutError —
			// both are handled identically to how the pre-existing chunk-
			// error branch always has: check for context overflow first
			// (compact-and-retry), otherwise normalize and fail the turn.
			if isContextOverflowError(streamRes.err) {
				if compacted, ok := a.compactOnOverflow(runMessages, systemPrompt, turn, "chunk"); ok {
					runMessages = compacted
					continue
				}
			}
			err := normalizeRunError(ctx, streamRes.err, a.requestTimeout)
			emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
			return &RunResult{Messages: runMessages, Usage: usage}, err
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

		// Phase 0 primary metric: provider-reported tokens for this turn, joined
		// with the pre-request byte breakdown. Emitted even when the provider
		// omits usage (tokens = 0) so the byte buckets are still captured.
		if a.metrics != nil {
			m := TurnMetrics{Turn: turn, Context: pendingContext}
			if streamUsage != nil {
				m.InputTokens = streamUsage.InputTokens
				m.OutputTokens = streamUsage.OutputTokens
			}
			// If provider doesn't return input_tokens, estimate from bytes
			if m.InputTokens == 0 && pendingContext.TotalBytes > 0 {
				m.InputTokens = estimateInputTokens(pendingContext)
			}
			a.metrics.RecordTurn(m)
		}

		a.logger.Debug("llm response", "turn", turn, "text_len", len(text), "tool_calls", len(toolCalls), "stop", stopReason)
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
			Content:   text,
			ToolCalls: toolCalls,
			Metadata:  assistantMetadata,
			CreatedAt: time.Now().UTC(),
		}
		if assistantMessage.Content != "" || len(assistantMessage.ToolCalls) > 0 {
			runMessages = append(runMessages, assistantMessage)
		}

		if len(toolCalls) == 0 {
			// Only a genuine INPUT-overflow stop reason (the provider
			// rejected the request itself, no tool calls, no text) is worth
			// a compact-and-retry: compactMessages rewrites content but
			// preserves message *count*, so the previous gate here
			// (didCompact && len(compacted) < len(runMessages)) was
			// provably always false — the same defect compactOnOverflow's
			// comment documents for the chunk.Err path. Delegate to that
			// already-fixed helper (it does its own token-size comparison
			// and provider-anchor invalidation) and retry on success,
			// mirroring the chunk.Err overflow path above.
			if isInputContextOverflow(stopReason) {
				if compacted, ok := a.compactOnOverflow(runMessages, systemPrompt, turn, "no-tool-calls"); ok {
					runMessages = compacted
					continue
				}
			} else if isOutputTruncation(stopReason) {
				// The request fit fine; the provider truncated its
				// *response*. Compacting conversation history cannot fix
				// output truncation, so just warn and end the run below.
				a.logger.Warn("output truncated (max_tokens/length) and no tool calls; compaction cannot help output truncation", "turn", turn, "stop_reason", stopReason)
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

		// Parent-budget passthrough (plan §M2.2 carry-forward): compute the
		// REMAINING token budget once per batch, here on the Run goroutine —
		// usage is loop state and must not be read from inside the parallel
		// path's goroutines below. Injected into the tool ctx so a subagent
		// spawned via the task tool can't be handed an unlimited budget
		// underneath a parent that itself is running under MaxTokensBudget
		// (pkg/tools/subagent.go folds this into SubagentConfig.TokenBudget).
		// A parent without a budget (maxTokensBudget<=0) leaves ctx
		// untouched, so downstream behavior is unchanged.
		//
		// Staleness consequence (accepted): remaining is a snapshot of
		// usage as of the TOP of this batch, computed once and reused for
		// every call in it — a batch of N task calls (e.g. a ParallelSafe
		// fan-out) each independently sees that SAME pre-batch remaining,
		// not remaining-minus-what-earlier-calls-in-this-batch-already-
		// spent. Since none of those calls' own usage is known until they
		// complete, N concurrent subagents could jointly draw up to N times
		// the parent's actual remaining budget before anything notices.
		// This is bounded, not unbounded: the parent's own turn-top budget
		// check (`usage.TotalTokens >= a.maxTokensBudget`, above) rolls up
		// every subagent's usage via addSubagentUsage and will catch the
		// overage at the START of the NEXT turn, ending the run. Tightening
		// this further (e.g. reserving remaining/N per call, or serializing
		// budget decisions across a batch) is out of scope here.
		dispatchCtx := ctx
		if a.maxTokensBudget > 0 {
			remaining := a.maxTokensBudget - usage.TotalTokens
			if remaining < 0 {
				remaining = 0
			}
			dispatchCtx = tools.WithRemainingTokenBudget(ctx, remaining)
		}

		// Tool calls execution.
		// When ALL tool calls in this batch are declared ParallelSafe, run
		// them concurrently and only serialize the surrounding event/message
		// bookkeeping. A single non-parallel-safe call (bash, edit_file,
		// skill, ...) forces the whole batch to run sequentially. ParallelSafe
		// is a handler-level thread-safety promise, not a side-effect-freedom
		// promise: a ParallelSafe tool's Go code has no shared mutable state
		// that concurrent invocations could race on, but its effects (e.g.
		// task spawning subagents that write files or run git) can still
		// collide with each other across goroutines. That cross-goroutine
		// side-effect discipline is governed by prompt-level constraints
		// (see delegationStrategy's "Parallel delegation" section), not by
		// this loop.
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
			// Decide fan-out cap admission for the WHOLE batch up front, on
			// this single goroutine, before any tool actually executes. A
			// batch straddling the cap runs the calls under it and refuses
			// the calls over it (M2-2 12c) — admission order follows batch
			// order, not completion order.
			overCap := make([]bool, len(toolCalls))
			for i, call := range toolCalls {
				overCap[i] = taskCallOverCap(&taskCallCount, call)
			}
			results := make([]models.ToolResult, len(toolCalls))
			var wg sync.WaitGroup
			for i, call := range toolCalls {
				i, call := i, call
				if overCap[i] {
					results[i] = synthesizeTaskCapResult(call)
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = a.runOneTool(dispatchCtx, sessionID, call)
				}()
			}
			wg.Wait()
			batchClean := true
			// pendingHints accumulates breaker hint messages during the batch;
			// they are appended to runMessages only after the batch's last
			// tool result so a hint (RoleHuman) never lands between an
			// assistant tool_calls message and any of its tool results (M1-7).
			var pendingHints []models.Message
			for i, call := range toolCalls {
				result := results[i]
				addSubagentUsage(usage, result)
				offloaded := a.offloadIfNeeded(&result, a.offloadDir)
				runMessages = appendToolResultMessage(runMessages, sessionID, result)
				if a.metrics != nil {
					// M1.2: Enhanced metrics collection
					argsHash := computeArgsHash(call.Arguments)
					filePath := extractPathFromArgs(result.ToolName, call.Arguments)
					durationMs := result.Duration.Milliseconds()

					a.metrics.RecordToolResult(ToolResultMetric{
						Turn:        turn,
						ToolName:    result.ToolName,
						ResultBytes: len(result.Content),
						ArgsHash:    argsHash,
						Path:        filePath,
						Offloaded:   offloaded,
						DurationMs:  durationMs,
					})
				}
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
				// Circuit-breaker bookkeeping — identical logic to the serial
				// path below, run through the shared helper in batch order.
				obs := breaker.observe(sessionID, call, result)
				if len(obs.hintMessages) > 0 {
					pendingHints = append(pendingHints, obs.hintMessages...)
				}
				if obs.validationFailure {
					batchClean = false
				}
				if obs.fatalErr != nil {
					// Invariant that matters here: every tool_use ID on the
					// assistant message that started this batch MUST have a
					// matching tool_result in runMessages, or the next
					// Anthropic request is malformed — and the REPL persists
					// runMessages even on error, so a dropped result would
					// permanently poison the session. results[i+1:] were
					// already computed by the goroutines above (the whole
					// batch runs concurrently before this observation loop
					// starts), so append them now even though the breaker
					// already decided to stop; skip metrics/events for them
					// to keep this simple, since only the tool_result
					// pairing invariant is required for correctness. Usage
					// roll-up (M1) is NOT skipped, though — these results
					// still represent real subagent token consumption that
					// already happened and must not be silently dropped from
					// RunResult.Usage just because the breaker tripped on an
					// earlier result in the same batch.
					for j := i + 1; j < len(toolCalls); j++ {
						addSubagentUsage(usage, results[j])
						// Mirror the normal per-index loop above: a large
						// trailing result must still be shrunk to its
						// offload stub before being persisted, or it lands
						// in runMessages at full size (M1 gap — the normal
						// path applies offload before appending, this tail
						// loop did not).
						a.offloadIfNeeded(&results[j], a.offloadDir)
						runMessages = appendToolResultMessage(runMessages, sessionID, results[j])
					}
					// Append any pending hints only AFTER every tool result of
					// this batch (including the tail append above) — a hint
					// (RoleHuman) must never land between the assistant
					// tool_calls message that started this batch and any of
					// its tool results (M1-7), and flushing it before the
					// tail append violated that invariant on the fatal path.
					if len(pendingHints) > 0 {
						runMessages = append(runMessages, pendingHints...)
					}
					emit(AgentEvent{Type: AgentEventError, Err: obs.fatalErr.Error(), Error: obs.fatalAgentErr})
					return &RunResult{Messages: runMessages, Usage: usage}, obs.fatalErr
				}
			}
			if len(pendingHints) > 0 {
				runMessages = append(runMessages, pendingHints...)
			}
			if batchClean {
				breaker.resetOnCleanBatch()
			}
			if err := ctx.Err(); err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
			}
			continue
		}

		batchClean := true
		// pendingHints accumulates breaker hint messages during the batch;
		// they are appended to runMessages only after the batch's last tool
		// result so a hint (RoleHuman) never lands between an assistant
		// tool_calls message and any of its tool results (M1-7).
		var pendingHints []models.Message
		for idx, call := range toolCalls {
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

			var result models.ToolResult
			if taskCallOverCap(&taskCallCount, call) {
				// Fan-out cap (M2-2 12c): refuse without executing — never
				// reaches the subagent pool's StartTask.
				result = synthesizeTaskCapResult(call)
			} else {
				result = a.runOneTool(dispatchCtx, sessionID, call)
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
				// Track active skill for memory source tagging.
				if skillName, ok := result.Data["skill_name"]; ok {
					if name, _ := skillName.(string); name != "" {
						a.activeSkill.Store(name)
					}
				}
			}

			addSubagentUsage(usage, result)
			offloaded := a.offloadIfNeeded(&result, a.offloadDir)
			runMessages = appendToolResultMessage(runMessages, sessionID, result)
			if a.metrics != nil {
				// M1.2: Enhanced metrics collection
				argsHash := computeArgsHash(call.Arguments)
				filePath := extractPathFromArgs(result.ToolName, call.Arguments)
				durationMs := result.Duration.Milliseconds()

				a.metrics.RecordToolResult(ToolResultMetric{
					Turn:        turn,
					ToolName:    result.ToolName,
					ResultBytes: len(result.Content),
					ArgsHash:    argsHash,
					Path:        filePath,
					Offloaded:   offloaded,
					DurationMs:  durationMs,
				})
			}
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

			// Circuit-breaker bookkeeping (repeat-call loop, then
			// validation-failure loop) via the shared helper — see
			// toolCallBreaker.observe for the combined logic.
			obs := breaker.observe(sessionID, call, result)
			if len(obs.hintMessages) > 0 {
				pendingHints = append(pendingHints, obs.hintMessages...)
			}
			if obs.validationFailure {
				batchClean = false
			}
			if obs.fatalErr != nil {
				// Invariant that matters here: every tool_use ID on the
				// assistant message that started this batch MUST have a
				// matching tool_result in runMessages, or the next Anthropic
				// request is malformed. Unlike the parallel path, the serial
				// loop never executed the remaining calls in this batch —
				// there are no computed results to append, so synthesize a
				// failed placeholder for each one instead.
				for _, remaining := range toolCalls[idx+1:] {
					synthetic := models.ToolResult{
						CallID:      remaining.ID,
						ToolName:    remaining.Name,
						Status:      models.CallStatusFailed,
						Error:       "not executed: batch aborted by circuit breaker",
						CompletedAt: time.Now().UTC(),
					}
					runMessages = appendToolResultMessage(runMessages, sessionID, synthetic)
				}
				// Append any pending hints only AFTER the synthesized tail
				// results above — a hint (RoleHuman) must never land between
				// the assistant tool_calls message that started this batch
				// and any of its tool results (M1-7); flushing it before the
				// synthesize loop violated that invariant.
				if len(pendingHints) > 0 {
					runMessages = append(runMessages, pendingHints...)
				}
				emit(AgentEvent{Type: AgentEventError, Err: obs.fatalErr.Error(), Error: obs.fatalAgentErr})
				return &RunResult{Messages: runMessages, Usage: usage}, obs.fatalErr
			}

			if err := ctx.Err(); err != nil {
				if len(pendingHints) > 0 {
					runMessages = append(runMessages, pendingHints...)
				}
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
			}
		}
		if len(pendingHints) > 0 {
			runMessages = append(runMessages, pendingHints...)
		}
		if batchClean {
			breaker.resetOnCleanBatch()
		}
	}
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
