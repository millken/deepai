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

	// turnInjection is the per-Run trailing message carrying the volatile
	// content (date + memory injections) that used to be baked into the
	// system prompt (M4-2). Computed once at the top of Run (see the call
	// site there) and appended — never mutated in place — to every
	// (re)built provider view via appendTurnInjection, so the request prefix
	// (system prompt + tool schemas + canonical history) stays byte-stable
	// across an entire Run for automatic prefix caching. Zero value before
	// Run computes it is never observed by a request: buildTurnInjection
	// always returns a non-empty date line, and it is set before the turn
	// loop's first iteration. Exception: re-derived (still via
	// buildTurnInjection, not mutated directly) when a "skill" tool call
	// changes ActiveSkill() mid-Run, so the memory fence's activeSource
	// tracks the actual active skill — see the skill-result handling below.
	// "Once per Run" is therefore "once per activeSource segment": cheap,
	// since a skill load is rare, not a per-request event.
	turnInjection models.Message

	// Memory integration
	memoryService   *memory.Service
	memoryExtractor memory.Extractor
	memoryUserID    string

	// Skill tracking for memory source tagging
	activeSkill atomic.Value // stores string

	// appliedSkillPrompt is the exact skill-body string last appended to
	// a.systemPrompt via AppendSystemPrompt (set alongside every such
	// append — Run()'s start-of-Run reapply from a carried session, and
	// the mid-Run skill-result handling below). Review M4-final F-M4-7:
	// used for an EXACT-EQUALITY check (bodyAlreadyApplied) instead of
	// re-deriving "is this body already applied?" via
	// strings.Contains(a.systemPrompt, loadedSkillPrompt), which could
	// false-positive on a short/degenerate body that happens to already be
	// a substring of the base prompt, the file-op rule, or other assembled
	// text — reporting "already applied" for a body that was never
	// actually appended, and silently dropping a genuine reload.
	appliedSkillPrompt string

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

	// session carries state across successive single-use Agent Runs within
	// one conversation (M4-3: breaker, active skill, compaction anchors —
	// see SessionCarry's doc comment). nil unless AgentConfig.Session was
	// set (the REPL's normal case); every other caller, including every
	// subagent, leaves this nil and gets today's per-Run-only behavior.
	session *SessionCarry
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
		systemPrompt:        strings.TrimSpace(cfg.SystemPrompt),
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
		session:             cfg.Session,
	}

	// M4-3: prime the compaction anchor/stall bookkeeping from the carried
	// session, if any, so this fresh Run's first estimate isn't blind to the
	// previous Run's real provider-reported count (see estimateContextTokens
	// and maybeCompact's doc comments). The active-skill/breaker carriage
	// happens in Run() instead (see the comments there) since a skill's
	// system-prompt append must happen AFTER the caller's own post-New()
	// AppendSystemPrompt calls (e.g. the REPL appends the skill catalog
	// after New() returns) for removeSkillDescriptions to find anything to
	// strip.
	if cfg.Session != nil {
		a.lastInputTokens = cfg.Session.lastInputTokens
		a.lastTokenCountMsgs = cfg.Session.lastTokenCountMsgs
		a.compactionStalled = cfg.Session.compactionStalled
		a.compactionStalledAt = cfg.Session.compactionStalledAt
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
	if idx <= 0 {
		return
	}
	before := strings.TrimSpace(a.systemPrompt[:idx])
	// Review r1 F2: this used to truncate EVERYTHING after the marker
	// (a.systemPrompt[:idx] and nothing else), which silently and
	// permanently discarded whatever the caller appended AFTER the catalog
	// — e.g. the REPL appends the skill catalog and THEN the CLI/DEEPAI.md
	// system prompt (repl.go's runTurn, two AppendSystemPrompt calls right
	// after New()). Since M4-3 carries a loaded skill across Runs (this
	// function now runs at the top of every subsequent Run, not just once
	// per turn), that loss became permanent for the rest of the
	// conversation instead of lasting only the remainder of one turn.
	// Excise ONLY the catalog block itself: AppendSystemPrompt always joins
	// sections with exactly "\n\n", so the first such boundary after the
	// marker is where the catalog ends and whatever was appended after it
	// begins — preserve that tail. If no such boundary exists, the catalog
	// was the last thing appended and there is nothing to preserve, which
	// reduces to the original behavior for that case.
	rest := a.systemPrompt[idx:]
	after := ""
	if sep := strings.Index(rest, "\n\n"); sep >= 0 {
		after = strings.TrimSpace(rest[sep+2:])
	}
	switch {
	case before == "":
		a.systemPrompt = after
	case after == "":
		a.systemPrompt = before
	default:
		a.systemPrompt = before + "\n\n" + after
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

	// M4-3: initialize skill tracking for this Run from the carried session
	// instead of unconditionally resetting to "" — a skill loaded by a
	// previous Run sharing this session (a different, single-use Agent
	// instance) must stay active here too, or the "Available skills"
	// catalog gets re-shown and M4-2's memory fence loses its cross-turn
	// activeSource. Reapplying session.skillPrompt here (rather than in
	// New()) is deliberate: it must run AFTER the caller's own post-New()
	// AppendSystemPrompt calls (e.g. the REPL appends the skill catalog and
	// its own system prompt after New() returns, before calling Run) so
	// removeSkillDescriptions below actually finds the catalog marker to
	// strip — calling this in New() would run before that marker exists.
	initialSkill := ""
	if a.session != nil {
		initialSkill = a.session.activeSkill
		if a.session.skillPrompt != "" {
			a.removeSkillDescriptions()
			a.AppendSystemPrompt(a.session.skillPrompt)
			a.appliedSkillPrompt = a.session.skillPrompt
		}
	}
	a.activeSkill.Store(initialSkill)

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

	// M4-2: compute the per-Run turn injection (date + memory) exactly ONCE,
	// from the runMessages present at Run start — see buildTurnInjection's
	// doc comment for the full rationale (prefix stability + the async
	// memory-extraction cadence making intra-run recomputation pointless).
	// Every (re)build of the provider view below appends this via
	// appendTurnInjection instead of BuildSystemPrompt baking volatile
	// content into the request prefix.
	a.turnInjection = a.buildTurnInjection(ctx, sessionID, runMessages)

	usage := &Usage{}
	// breaker holds all circuit-breaker state (validation-failure loop and
	// repeat-call loop). Both the serial and parallel tool-execution paths
	// below feed every (call, result) pair through breaker.observe in batch
	// order, so the two paths enforce identical limits from one implementation.
	//
	// M4-3: when a session is carried, reuse (and lazily create, once) its
	// breaker instead of a fresh one — the SAME *toolCallBreaker is then
	// mutated in place by this Run and read again, unchanged, by the next
	// Run sharing this session (see SessionCarry's doc comment for the
	// single-goroutine access contract this relies on). A subagent's
	// AgentConfig never sets Session (see subagent.go), so this is a no-op
	// there: every subagent Run still gets its own fresh breaker.
	var breaker *toolCallBreaker
	if a.session != nil {
		if a.session.breaker == nil {
			a.session.breaker = newToolCallBreaker()
		}
		breaker = a.session.breaker
	} else {
		breaker = newToolCallBreaker()
	}
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
		// sends (BuildSystemPrompt layers the file-op rule, tool
		// recommendations, the delegation prompt + catalog, and plan-mode text
		// on top of the base a.systemPrompt — all of that was previously
		// invisible to the compaction trigger). M4-2: BuildSystemPrompt no
		// longer layers memory injections — those, plus the date, now live in
		// a.turnInjection (computed once per Run, above) and are appended to
		// the provider view below via appendTurnInjection instead of being
		// baked into this prefix, so this prefix stays byte-stable across an
		// entire Run (automatic prefix caching on OpenAI-compat providers).
		systemPrompt := a.BuildSystemPrompt()

		// T1/T4: derive a per-request compressed prompt view from the canonical
		// runMessages. When aging is disabled this is a zero-copy pass-through.
		// The canonical runMessages (persisted, replayed, mined for memory) are
		// never modified — only the view sent to the provider is compressed.
		// Computed before the compaction check (and recomputed after, if
		// compaction fires) so the check measures what is actually sent, not
		// canonical history that aging may have shrunk considerably. Does NOT
		// yet include a.turnInjection — maybeCompact appends it (via
		// appendTurnInjection) to both its internal estimate and its returned
		// view, exactly once, whether or not compaction actually fires.
		promptView := buildPromptView(runMessages, a.aging, a.contextWindow)

		// Unstall check + threshold-triggered compaction (memory flush,
		// escalating retry, stall bookkeeping) — see maybeCompact.
		runMessages, promptView = a.maybeCompact(ctx, sessionID, turn, runMessages, promptView, systemPrompt, emit)

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
				// len(runMessages) is exactly the CANONICAL message set the
				// provider just counted. lastInputTokens, however, is the
				// provider's count for the VIEW actually sent — which (via
				// maybeCompact) had a.turnInjection appended at its tail, one
				// message beyond len(runMessages). estimateContextTokens's
				// anchor path accounts for that offset explicitly (see its
				// doc comment): it must not re-add the injection's bytes via
				// the delta, since lastInputTokens already priced in that
				// exact (Run-constant) content once.
				a.setTokenAnchor(streamUsage.InputTokens, len(runMessages))
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

			// Per-result bookkeeping (usage rollup, offload, message
			// append, metrics, events, breaker observation) is identical to
			// the serial path below — run through the shared helper, in
			// batch order, so both paths enforce identical limits and
			// invariants from one implementation. See toolBatchState.
			batch := newToolBatchState(a, sessionID, turn, breaker, usage, emit, runMessages)
			for i, call := range toolCalls {
				obs := batch.handleResult(call, results[i], runningCalls[i])
				if obs.fatalErr != nil {
					// Invariant that matters here: every tool_use ID on the
					// assistant message that started this batch MUST have a
					// matching tool_result in runMessages, or the next
					// Anthropic request is malformed — and the REPL persists
					// runMessages even on error, so a dropped result would
					// permanently poison the session. results[i+1:] were
					// already computed by the goroutines above (the whole
					// batch runs concurrently before this observation loop
					// starts), so append them now (usage + offload only,
					// matching the original tail loop's scope) even though
					// the breaker already decided to stop, then flush any
					// pending hints only AFTER every tool result of this
					// batch — a hint (RoleHuman) must never land between the
					// assistant tool_calls message that started this batch
					// and any of its tool results (M1-7).
					batch.appendRemaining(results[i+1:])
					batch.flushPendingHints()
					emit(AgentEvent{Type: AgentEventError, Err: obs.fatalErr.Error(), Error: obs.fatalAgentErr})
					return &RunResult{Messages: batch.runMessages, Usage: usage}, obs.fatalErr
				}
			}
			batch.finishBatch()
			runMessages = batch.runMessages
			if err := ctx.Err(); err != nil {
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: runMessages, Usage: usage}, err
			}
			continue
		}

		// Per-result bookkeeping mirrors the parallel path above — see
		// toolBatchState.
		batch := newToolBatchState(a, sessionID, turn, breaker, usage, emit, runMessages)
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
				skillName, _ := result.Data["skill_name"].(string)
				loadedSkillPrompt, _ := result.Data["system_prompt"].(string)

				// Review r1 F7 (dedup) + review r2 F2-b (guard property
				// fixed) + review M4-final F-M4-7 (exact-equality, not
				// substring): if the model reloads a skill whose body is
				// ALREADY present in the system prompt, reapplying would
				// duplicate it verbatim — doubling its token cost for the
				// rest of the turn. r1's guard keyed this off the skill
				// NAME alone (skillName == a.ActiveSkill()), which diverges
				// from the actual property ("this body is already in
				// a.systemPrompt") in exactly the state the r2 F11 fix
				// makes durable: activeSkill set but skillPrompt=="" (an
				// empty-bodied load carried across a Run boundary) — the
				// name matches on a later real-body reload of the SAME
				// skill, but nothing was ever actually applied. r2 fixed
				// that by additionally requiring strings.Contains(a.
				// systemPrompt, loadedSkillPrompt) — but a substring check
				// can itself false-positive: a short/degenerate body that
				// happens to already appear as a substring of the base
				// prompt (or the file-op rule, or anything else assembled
				// into a.systemPrompt) would report "already applied" for a
				// body that was never actually appended, silently dropping
				// a genuine reload. Compare against a.appliedSkillPrompt
				// (the EXACT string this Agent itself last appended, set
				// alongside every AppendSystemPrompt call for a skill body)
				// instead: same name AND byte-for-byte the same body that
				// was actually applied, so a later reload with a
				// DIFFERENT body — even one that happens to collide with
				// unrelated prompt text — still gets applied exactly once.
				bodyAlreadyApplied := loadedSkillPrompt != "" && skillName != "" && skillName == a.ActiveSkill() &&
					loadedSkillPrompt == a.appliedSkillPrompt
				if loadedSkillPrompt != "" && !bodyAlreadyApplied {
					a.removeSkillDescriptions()
					a.AppendSystemPrompt(loadedSkillPrompt)
					a.appliedSkillPrompt = loadedSkillPrompt
				}
				// Track active skill for memory source tagging.
				if skillName != "" {
					// Review r2 F7-a: capture whether this load actually
					// changes activeSource (the turn injection's memory
					// fence key, "skill:"+ActiveSkill()) BEFORE overwriting
					// a.activeSkill below — a same-skill reload (whether its
					// body is empty, a duplicate, or newly real per the F2-b
					// fix above) leaves activeSource unchanged, so the
					// recompute a few lines down would be pure wasted work
					// (an extra memory-service round trip) with nothing to
					// show for it.
					activeSourceChanged := skillName != a.ActiveSkill()
					a.activeSkill.Store(skillName)
					// M4-3: write the loaded skill back onto the carried
					// session (if any) so the NEXT Run sharing this
					// session (a fresh, single-use Agent) starts with it
					// already active instead of forgetting it — see
					// react.go's Run() start, which reapplies
					// session.skillPrompt/activeSkill the same way.
					if a.session != nil {
						a.session.activeSkill = skillName
						// Review r1 F11: only overwrite the carried body
						// when a NEW, non-empty one was actually returned.
						// A skill result with skill_name set but an empty
						// system_prompt (e.g. a skill whose rendered body is
						// genuinely empty) must not wipe out a previously
						// carried body while leaving activeSkill pointed at
						// this name — that would leave the next Run
						// reporting ActiveSkill()==name with no body and an
						// un-stripped catalog.
						if loadedSkillPrompt != "" {
							a.session.skillPrompt = loadedSkillPrompt
						}
					}
					// M4-2 fix: the once-per-Run turn injection (computed at
					// Run start — see buildTurnInjection's doc comment) is
					// built from a.ActiveSkill() as of that moment: "" on a
					// session-less Run, or the carried skill from a previous
					// Run when one is carried (M4-3). Recompute only when
					// activeSourceChanged (this load switched ActiveSkill()
					// to a genuinely different skill), so the injection's
					// memory content is re-fenced to it. Review r2 F7-a:
					// skipping the recompute on a same-skill reload (the
					// common alternative to activeSourceChanged) fixes two
					// things at once — it avoids the extra memory-service
					// round trip when nothing about activeSource changed,
					// and it retires the old comment's now-conditionally-
					// false claim that "AppendSystemPrompt just above
					// already grew the system prompt this turn" (true when
					// the F2-b reapply fired, but the reapply is skipped
					// entirely when the body was already present — this
					// branch no longer runs in that case at all, so the
					// claim never needs to hold here). "Once per Run" really
					// means "once per activeSource segment" — a skill load
					// changing to a genuinely different active skill is the
					// only thing that changes activeSource mid-Run, and
					// it's a rare event, not a per-request one.
					if activeSourceChanged {
						a.turnInjection = a.buildTurnInjection(ctx, sessionID, runMessages)
					}
				}
			}

			obs := batch.handleResult(call, result, runningCall)
			if obs.fatalErr != nil {
				// Invariant that matters here: every tool_use ID on the
				// assistant message that started this batch MUST have a
				// matching tool_result in runMessages, or the next Anthropic
				// request is malformed. Unlike the parallel path, the serial
				// loop never executed the remaining calls in this batch —
				// there are no computed results to append, so synthesize a
				// failed placeholder for each one instead, then flush any
				// pending hints only AFTER those synthesized results — a
				// hint (RoleHuman) must never land between the assistant
				// tool_calls message that started this batch and any of its
				// tool results (M1-7).
				batch.appendSynthesizedFailures(toolCalls[idx+1:])
				batch.flushPendingHints()
				emit(AgentEvent{Type: AgentEventError, Err: obs.fatalErr.Error(), Error: obs.fatalAgentErr})
				return &RunResult{Messages: batch.runMessages, Usage: usage}, obs.fatalErr
			}

			if err := ctx.Err(); err != nil {
				batch.flushPendingHints()
				err = normalizeRunError(ctx, err, a.requestTimeout)
				emit(AgentEvent{Type: AgentEventError, Err: err.Error(), Error: newAgentError(err)})
				return &RunResult{Messages: batch.runMessages, Usage: usage}, err
			}
		}
		batch.finishBatch()
		runMessages = batch.runMessages
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
