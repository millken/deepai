package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/imageproc"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// ReplConfig holds configuration for the chat REPL.
type ReplConfig struct {
	Provider            string
	ModelRegistry       *llm.ModelRegistry
	DatabaseURL         string
	ContextWindow       int
	MaxToolCalls        int
	RequestTimeout      time.Duration
	Query               string // non-interactive single query
	ResumeSession       string // session ID or title to resume
	ContinueLast        bool   // resume most recent session
	SystemPrompt        string
	WorkDir             string
	ToolRegistry        *tools.Registry
	SkillRegistry       *skill.Registry
	MemoryService       *memory.Service
	MemoryExtractor     memory.Extractor
	PreferenceExtractor memory.Extractor
	SessionRepo         models.SessionRepository // injected from outside
	InputHistoryFile    string                   // path for persisting input history (optional)
	// TaskCanceller stops a single running subagent by ID. Supplied by the
	// composition root, which owns the pool. nil disables per-task
	// cancellation (Ctrl+C still cancels the whole turn).
	TaskCanceller  TaskCanceller
	SandboxBaseDir string // root for sandbox session dirs; must NOT be the user's workdir
	MCPReport      string // one-line MCP load summary; printed after banner when non-empty
	AgentCatalog   []agent.AgentInfo
	Commands       map[string]Command // file-based slash commands; body injected as a user turn

	// MemoryAutoRefine enables the auto-refine review gate. When false the REPL
	// falls back to unconditional extraction rather than skipping memory work.
	MemoryAutoRefine bool
	// MemoryRefineInterval is the gate cadence in turns, already resolved by the
	// caller (see commands.resolveRefineInterval); 0 means "no gate".
	MemoryRefineInterval int
}

// fallbackExtractInterval is the turn cadence for unconditional async memory
// extraction — the behaviour used when the auto-refine gate is switched off.
// Set to 5: covers a typical short exchange in one batch while keeping LLM
// extraction cost bounded. Compaction always flushes synchronously, so facts
// are never lost across the context boundary.
const fallbackExtractInterval = 5

// memoryScheduleMode is what a finished turn should queue for memory.
type memoryScheduleMode int

const (
	memoryScheduleNone memoryScheduleMode = iota
	// memoryScheduleRefine runs the review gate, which decides whether to pay
	// for an extraction.
	memoryScheduleRefine
	// memoryScheduleUnconditional extracts without asking, which is what the
	// REPL did before the gate existed.
	memoryScheduleUnconditional
)

// memoryScheduleFor decides what a finished turn should queue.
//
// Any interval that is not a usable cadence means "no gate", never "no memory":
// the fallback branch keeps extraction running at the pre-gate cadence. Without
// it, a config that never mentions the key would stop memory extraction outright
// rather than merely turning off an optimisation.
func memoryScheduleFor(turn, refineInterval int, autoRefine bool) memoryScheduleMode {
	if autoRefine && refineInterval > 0 {
		if turn%refineInterval == 0 {
			return memoryScheduleRefine
		}
		return memoryScheduleNone
	}
	if turn%fallbackExtractInterval == 0 {
		return memoryScheduleUnconditional
	}
	return memoryScheduleNone
}

// ReplUI is the subset of TUI methods the REPL needs. *TUI satisfies it
// implicitly. Defining it as an interface lets tests inject a mock.
type ReplUI interface {
	Info(msg string)
	SetStatus(model string, planMode bool)
	Banner(info BannerInfo)
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
	ReadPrompt(ctx context.Context) (string, []models.MessageImage, error)
	TurnStart(turn int, userInput string)
	TurnEnd(usage *agent.Usage)
	RenderEvent(evt agent.AgentEvent)
	RenderSubagentEvent(evt subagent.TaskEvent)
	RenderInterrupted()
	InterruptCh() <-chan struct{}
	CancelTaskCh() <-chan string
	LoadHistory(path string)
	SaveHistory()
	Close()
}

// TaskCanceller is the narrow slice of the subagent pool the REPL needs: stop
// one task by ID. Kept minimal so the REPL does not depend on the pool type.
type TaskCanceller interface {
	CancelTask(taskID string) bool
}

// ChatRepl is the interactive chat REPL.
type ChatRepl struct {
	cfg               ReplConfig
	ui                ReplUI
	historyFile       string
	sess              *models.Session
	sessMgr           models.SessionRepository
	sb                *sandbox.Sandbox
	turn              int
	prefSched         *memory.PreferenceScheduler
	consecCorrections int
	planMode          bool   // restrict to read-only tools until user approves plan
	currentModel      string // selected model alias (from ModelRegistry)
	currentEffort     string // reasoning effort: "low", "medium", "high", "disabled", or "" (provider default)
	imageDetail       string // vision detail: "low" (default), "high", or "" (use "low")
	refineOverride    *bool  // session-level /refine on|off; nil defers to config

	// carry holds cross-turn Agent state (circuit breaker, active skill,
	// compaction anchors — see agent.SessionCarry's doc comment) that would
	// otherwise silently reset every turn, since runTurn builds a fresh,
	// single-use Agent per turn (M4-3, task-23-brief.md). Passed unchanged
	// into every turn's AgentConfig.Session; reset to a fresh
	// agent.NewSessionCarry() by clearSession (/clear), startNewSession
	// (/new), and undoLastTurn (/undo), alongside the state each of those
	// already invalidates. The REPL drives turns serially (one runTurn
	// completes before the next begins) EXCEPT on the orphan path (see
	// orphanWaitOrDefault) — do not share this pointer with anything that
	// could run concurrently with a turn (e.g. a subagent).
	carry *agent.SessionCarry

	// orphanWait overrides the orphan-turn wait (see orphanWaitOrDefault)
	// for tests. Zero (the field's default in every real ChatRepl, since
	// NewRepl never sets it) means "use the production default" — this
	// mirrors the Agent.streamIdleTimeout pattern (pkg/agent/react.go): a
	// field only tests reach into directly, never exposed via ReplConfig.
	orphanWait time.Duration
}

// defaultOrphanWait bounds how long runTurn waits for an orphaned Run
// goroutine (one that hasn't returned by the time ctx is cancelled — e.g. a
// tool ignoring ctx) before giving up and returning anyway, so a stuck tool
// can never hang the whole REPL.
const defaultOrphanWait = 10 * time.Second

// orphanWaitOrDefault returns r.orphanWait if a test has set it, else
// defaultOrphanWait.
func (r *ChatRepl) orphanWaitOrDefault() time.Duration {
	if r.orphanWait > 0 {
		return r.orphanWait
	}
	return defaultOrphanWait
}

// NewRepl creates a new chat REPL instance.
func NewRepl(cfg ReplConfig) (*ChatRepl, error) {
	// The sandbox session directory must live outside the user's working
	// directory; otherwise cleanup on exit (incl. ctrl+c) could delete project
	// files — e.g. a pre-existing ./cli folder. Fall back to a temp location
	// when no isolated base is provided.
	sandboxBase := strings.TrimSpace(cfg.SandboxBaseDir)
	if sandboxBase == "" {
		sandboxBase = filepath.Join(os.TempDir(), "deepai-sandbox")
	}
	sb, err := sandbox.NewSession(sandboxBase, sandbox.Config{})
	if err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	repl := &ChatRepl{
		cfg:          cfg,
		historyFile:  cfg.InputHistoryFile,
		sessMgr:      cfg.SessionRepo,
		sb:           sb,
		prefSched:    memory.NewPreferenceScheduler(),
		currentModel: cfg.ModelRegistry.DefaultName(),
		carry:        agent.NewSessionCarry(),
	}
	return repl, nil
}

// Run starts the interactive REPL loop. It requires an interactive terminal;
// single-query (-q) and non-TTY modes are not supported.
func (r *ChatRepl) Run(parentCtx context.Context) error {
	defer r.sb.Close()
	defer func() {
		if r.cfg.MemoryService != nil {
			r.cfg.MemoryService.CleanupStale(time.Hour)
		}
	}()

	// Resolve session.
	if err := r.resolveSession(); err != nil {
		return err
	}

	// The REPL is TUI-only: it requires an interactive terminal. Single-query
	// (-q) and non-TTY (pipe/CI) modes are not supported.
	if r.cfg.Query != "" {
		return errors.New("single-query (-q) mode is no longer supported; run deepai interactively")
	}
	if !isInteractiveTTY() {
		return errors.New("deepai requires an interactive terminal (stdin and stderr must be a TTY)")
	}

	bannerInfo := r.bannerInfo()

	// Start the persistent Bubble Tea TUI for the whole session.
	tui := NewTUI(os.Stdin, os.Stderr, bannerInfo)
	tui.Start()
	r.ui = tui
	defer r.ui.Close()
	if r.historyFile != "" {
		r.ui.LoadHistory(r.historyFile)
	}
	defer r.ui.SaveHistory()

	// Show banner.
	r.ui.Banner(bannerInfo)
	if r.cfg.MCPReport != "" {
		r.ui.Info("  " + r.cfg.MCPReport)
	}
	r.ui.SetStatus(r.currentModel, r.planMode)

	// Interactive loop. Ctrl+C during a turn cancels only that turn (delivered
	// via the TUI interrupt channel); Ctrl+C at the prompt exits the REPL.

	// Auto-continue: if the resumed session was interrupted mid-task,
	// start the agent immediately without waiting for user input.
	autoContinue := (r.cfg.ResumeSession != "" || r.cfg.ContinueLast) && isSessionInterrupted(r.sess.Messages)

	for {
		// Auto-continue: on first iteration of an interrupted session,
		// run the agent immediately without waiting for user input.
		if autoContinue {
			autoContinue = false
			r.ui.Info("  Resuming interrupted session...")
			r.turn++
			if err := r.runTurnWithSignal(parentCtx, r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				if err.cancelled {
					r.ui.RenderInterrupted()
					continue
				}
				r.ui.Info(fmt.Sprintf("  Error: %v", err))
			}
		}

		// Wait for user input.
		line, images, err := r.ui.ReadPrompt(parentCtx)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				// Ctrl+C at prompt — exit REPL.
				r.ui.Info("  Interrupted.")
				break
			}
			// io.EOF (Ctrl+D) or context cancellation: exit quietly.
			break
		}
		if line == "" && len(images) == 0 {
			continue
		}

		// Handle slash commands.
		if cmd, ok := ParseSlashCommand(line); ok {
			// File-based command: inject its expanded body as a user turn.
			if c, ok := r.cfg.Commands[cmd.Name]; ok {
				r.turn++
				body := Expand(c.Body, cmd.Args)
				turnErr := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
					return r.runTurn(ctx, body, nil, false)
				})
				if turnErr != nil {
					if turnErr.cancelled {
						r.ui.RenderInterrupted()
						continue
					}
					r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
				}
				continue
			}
			if r.handleSlashCommand(parentCtx, cmd) {
				break
			}
			continue
		}

		// Continuation input ("继续", "continue", etc.): resume agent
		// without adding a new human message.
		if isContinuationInput(line) && len(r.sess.Messages) > 0 {
			r.turn++
			if err := r.runTurnWithSignal(parentCtx, r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				if err.cancelled {
					r.ui.RenderInterrupted()
					continue
				}
				r.ui.Info(fmt.Sprintf("  Error: %v", err))
			}
			continue
		}

		r.turn++

		// Capture images for this turn (may be nil).
		turnImages := images

		turnErr := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
			return r.runTurn(ctx, line, turnImages, false)
		})
		if turnErr != nil {
			if turnErr.cancelled {
				r.ui.RenderInterrupted()
				continue
			}
			r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
		}
	}

	// Save session metadata on exit.
	r.saveSession()
	slog.Info("session ended", "session_id", r.sess.ID, "turns", r.turn)
	return nil
}

// bannerInfo gathers the data shown in the startup banner and footer.
func (r *ChatRepl) bannerInfo() BannerInfo {
	toolCount := 0
	if r.cfg.ToolRegistry != nil {
		toolCount = len(r.cfg.ToolRegistry.List())
	}
	skillCount := 0
	var skillNames []string
	if r.cfg.SkillRegistry != nil {
		skillCount = r.cfg.SkillRegistry.Count()
		skillNames = r.cfg.SkillRegistry.AvailableNames()
	}
	// Resolve provider/model from the current model alias for display.
	provider, model := r.cfg.Provider, r.currentModel
	if def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel); ok {
		provider = def.Provider
		model = def.Model
	}
	return BannerInfo{
		Provider:      provider,
		Model:         model,
		ModelAlias:    r.currentModel,
		ToolCount:     toolCount,
		SkillCount:    skillCount,
		SkillNames:    skillNames,
		SessionID:     r.sess.ID,
		ContextWindow: r.currentContextWindow(),
	}
}

func (r *ChatRepl) resolveSession() error {
	// Resume by ID or title.
	if r.cfg.ResumeSession != "" {
		sess, err := r.sessMgr.Resolve(r.cfg.ResumeSession)
		if err != nil {
			return fmt.Errorf("resume session %q: %w", r.cfg.ResumeSession, err)
		}
		r.sess = sess
		// Load messages from DB into memory for the agent.
		msgs, err := r.sessMgr.LoadMessages(sess.ID)
		if err != nil {
			slog.Warn("load messages for resumed session", "err", err)
		}
		r.sess.Messages = msgs
		slog.Info("resumed session", "id", sess.ID, "messages", len(msgs))
		// Clean up incomplete tool calls from interrupted turn.
		r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
		r.restoreModelFromSession()
		return nil
	}

	// Continue most recent.
	if r.cfg.ContinueLast {
		sess, err := r.sessMgr.Latest()
		if err != nil {
			return fmt.Errorf("load latest session: %w", err)
		}
		if sess != nil {
			r.sess = sess
			msgs, err := r.sessMgr.LoadMessages(sess.ID)
			if err != nil {
				slog.Warn("load messages for continued session", "err", err)
			}
			r.sess.Messages = msgs
			slog.Info("continued session", "id", sess.ID, "messages", len(msgs))
			r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
			r.restoreModelFromSession()
			return nil
		}
	}

	// New session.
	sess, err := r.sessMgr.Create(models.CreateOpts{
		Model: r.currentModel,
		CWD:   r.cfg.WorkDir,
	})
	if err != nil {
		return err
	}
	r.sess = sess
	r.persistModel()
	return nil
}

// filterUnresolvedToolUses removes assistant messages where ALL tool calls
// have no corresponding tool results. Keeps messages with at least one
// resolved tool call so the model has context of completed work.
func filterUnresolvedToolUses(messages []models.Message) []models.Message {
	// Collect all tool result call IDs.
	resolvedResults := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			resolvedResults[msg.ToolResult.CallID] = true
		}
	}

	filtered := make([]models.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != models.RoleAI || len(msg.ToolCalls) == 0 {
			filtered = append(filtered, msg)
			continue
		}
		// Keep assistant message if at least one tool call has a result.
		hasResolved := false
		for _, tc := range msg.ToolCalls {
			if resolvedResults[tc.ID] {
				hasResolved = true
				break
			}
		}
		if hasResolved {
			// Strip unresolved tool calls, keep the rest.
			kept := make([]models.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				if resolvedResults[tc.ID] {
					kept = append(kept, tc)
				}
			}
			msg.ToolCalls = kept
			filtered = append(filtered, msg)
		}
		// Drop assistant messages where ALL tool calls are unresolved.
	}
	return filtered
}

// isContinuationInput checks if the user input is a continuation request.
func isContinuationInput(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "继续" || s == "continue" || s == "go on" || s == "keep going"
}

// isSessionInterrupted checks if the session ended mid-task (last message
// indicates an incomplete turn: tool result without follow-up, or error state).
func isSessionInterrupted(messages []models.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	// Last message is a tool result → agent didn't get to respond
	if last.Role == models.RoleTool {
		return true
	}
	// Last assistant message is empty or has tool calls with no following results
	if last.Role == models.RoleAI {
		if strings.TrimSpace(last.Content) == "" && len(last.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// continueTurn resumes the agent from an interrupted session by injecting
// a continuation prompt instead of a real user message.
func (r *ChatRepl) continueTurn(ctx context.Context) error {
	// A turn interrupted mid tool-batch can leave an assistant message whose
	// tool calls never received results. Strip those unresolved calls so the
	// resumed request is well-formed for the provider API (the reload path does
	// this too, but an in-session "continue" never reloads from the DB).
	r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
	return r.runTurn(ctx, "Continue from where you left off.", nil, true)
}

// turnError wraps errors from runTurn to distinguish cancellation from real errors.
type turnError struct {
	err       error
	cancelled bool
}

func (e *turnError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "cancelled"
}

// runTurnWithSignal creates a cancellable turn context, cancels it when the user
// presses Ctrl+C (delivered on the TUI interrupt channel, since raw mode means
// Ctrl+C never raises SIGINT), runs the given turn function, and returns a
// turnError that distinguishes cancellation from real errors. Returns nil on
// clean success.
func (r *ChatRepl) runTurnWithSignal(parentCtx context.Context, fn func(context.Context) error) *turnError {
	turnCtx, turnCancel := context.WithCancel(parentCtx)
	uiInterrupt := r.ui.InterruptCh()

	// sigFired is written by the watcher goroutine before turnCancel(); reading
	// it after fn returns (which implies the context is done) is race-free.
	sigFired := make(chan struct{}, 1)
	cancelTasks := r.ui.CancelTaskCh()
	go func() {
		for {
			select {
			case <-uiInterrupt:
				sigFired <- struct{}{}
				turnCancel()
				return
			case taskID := <-cancelTasks:
				// Per-task cancellation does NOT end the turn: the point is to
				// drop one stuck subagent and let the rest finish.
				if r.cfg.TaskCanceller != nil {
					r.cfg.TaskCanceller.CancelTask(taskID)
				}
			case <-turnCtx.Done():
				return
			}
		}
	}()

	err := fn(turnCtx)
	turnCancel()

	interrupted := false
	select {
	case <-sigFired:
		interrupted = true
	default:
	}

	if err == nil && !interrupted {
		return nil
	}
	return &turnError{err: err, cancelled: interrupted}
}

// mainAgentMaxTokens returns a fresh pointer to agent.ResolveMaxOutputTokens()
// for AgentConfig.MaxTokens, which takes *int. It exists only so this value
// is read from the one shared resolver rather than a local literal — see
// pkg/commands/chat.go's subagentMaxTokens, which the subagent wiring must
// keep in step with. ResolveMaxOutputTokens honors an explicit
// DEEPAI_MAX_OUTPUT_TOKENS setting and otherwise falls back to
// agent.DefaultMaxOutputTokens; it never returns 0.
func mainAgentMaxTokens() *int {
	n := agent.ResolveMaxOutputTokens()
	return &n
}

func (r *ChatRepl) runTurn(ctx context.Context, userInput string, images []models.MessageImage, continuation bool) error {
	ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		r.ui.RenderSubagentEvent(evt)
	})

	// Evaluate fact feedback from previous turn (consume-once).
	r.evaluateFactFeedback(r.sess.ID, r.turn, userInput)

	// Parse @path image references from the input text.
	cleanedInput, pathImages := parseImageReferences(userInput, r.cfg.WorkDir)
	if len(pathImages) > 0 {
		images = append(images, pathImages...)
		userInput = cleanedInput
	}

	// Append user message to session history. A continuation is a synthetic
	// nudge (e.g. resume after interrupt), not a real user turn: hand it to the
	// agent in-memory but never persist it, so it doesn't pollute the saved
	// transcript or the FTS index.
	userMsg := models.Message{
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   userInput,
		Images:    images,
	}
	if !continuation {
		if err := r.sessMgr.AppendMessage(r.sess.ID, userMsg); err != nil {
			slog.Warn("append user message", "err", err)
		}
	}
	r.sess.Messages = append(r.sess.Messages, userMsg)

	// Resolve the current model's provider + model name.
	provider, modelName, err := r.cfg.ModelRegistry.ProviderFor(r.currentModel)
	if err != nil {
		return fmt.Errorf("resolve model %q: %w", r.currentModel, err)
	}

	// Create a fresh agent for this turn.
	agentCfg := agent.AgentConfig{
		LLMProvider:     provider,
		Tools:           r.cfg.ToolRegistry,
		Sandbox:         r.sb,
		Model:           modelName,
		ContextWindow:   r.currentContextWindow(),
		ReasoningEffort: r.currentReasoningEffort(),
		MaxToolCalls:    r.cfg.MaxToolCalls,
		// MaxTokens: without this the provider default applies (8192 for
		// Anthropic), the same limit pkg/commands/chat.go raises for
		// subagents to avoid truncating a large tool-call argument
		// mid-stream — this is the agent the user actually talks to, so it
		// needs the same headroom. See agent.ResolveMaxOutputTokens and
		// mainAgentMaxTokens below.
		MaxTokens:       mainAgentMaxTokens(),
		RequestTimeout:  r.cfg.RequestTimeout,
		UserInteraction: r.ui,
		PlanMode:        r.planMode,
		WorkDir:         r.cfg.WorkDir,
		MemoryService:   r.cfg.MemoryService,
		MemoryExtractor: r.cfg.MemoryExtractor,
		MemoryUserID:    r.cfg.WorkDir,
		ImageDetail:     r.currentImageDetail(),
		AgentCatalog:    r.cfg.AgentCatalog,
		Session:         r.carry,
	}

	runAgent := agent.New(agentCfg)

	// Append skill descriptions and system prompt.
	if r.cfg.SkillRegistry != nil {
		if desc := r.cfg.SkillRegistry.Descriptions(); desc != "" {
			runAgent.AppendSystemPrompt(desc)
		}
	}
	if r.cfg.SystemPrompt != "" {
		runAgent.AppendSystemPrompt(r.cfg.SystemPrompt)
	}

	r.ui.TurnStart(r.turn, userInput)

	// Remember message count before agent run to only persist new messages.
	prevMsgCount := len(r.sess.Messages)

	// Start event draining goroutine BEFORE Run().
	events := make(chan agent.AgentEvent, 128)
	go func() {
		for evt := range runAgent.Events() {
			events <- evt
		}
		close(events)
	}()

	// Run the agent.
	type outcome struct {
		result *agent.RunResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := runAgent.Run(ctx, r.sess.ID, r.sess.Messages)
		outcomes <- outcome{result: result, err: err}
	}()

	// Process events as they arrive. The TUI shows a live spinner + elapsed
	// timer while the agent runs, so no separate idle heartbeat is needed.
	var lastUsage *agent.Usage
	var turnErr error
	var turnToolCalls []memory.ToolCallInfo
EventLoop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if evt.Usage != nil {
				lastUsage = evt.Usage
			}
			r.ui.RenderEvent(evt)
			// Collect tool call names for distribution tracking.
			if evt.Type == agent.AgentEventToolCallStart {
				name := ""
				if evt.ToolEvent != nil {
					name = evt.ToolEvent.Name
				} else if evt.ToolCall != nil {
					name = evt.ToolCall.Name
				}
				if name != "" {
					turnToolCalls = append(turnToolCalls, memory.ToolCallInfo{Name: name})
				}
			}
		case out := <-outcomes:
			// Drain remaining events.
			if events != nil {
				for evt := range events {
					if evt.Usage != nil {
						lastUsage = evt.Usage
					}
					r.ui.RenderEvent(evt)
				}
			}
			if out.result != nil {
				r.sess.Messages = out.result.Messages
				if out.result.Usage != nil {
					lastUsage = out.result.Usage
				}
			}
			turnErr = out.err
			break EventLoop
		case <-ctx.Done():
			// The user interrupted (ctrl+c) or the turn timed out. The agent's
			// Run goroutine returns promptly once ctx is cancelled, carrying the
			// messages accumulated so far. Capture them so the partial progress
			// is persisted and an in-session "continue" resumes with full
			// context instead of restarting the turn from scratch. emit() never
			// blocks (it drops on a full buffer), so Run cannot deadlock here.
			turnErr = ctx.Err()
			select {
			case out := <-outcomes:
				if out.result != nil {
					r.sess.Messages = out.result.Messages
					if out.result.Usage != nil {
						lastUsage = out.result.Usage
					}
				}
			case <-time.After(r.orphanWaitOrDefault()):
				// Defensive: a tool ignoring ctx could delay Run's return.
				// Persist what we have rather than hanging the REPL.
				//
				// M4-3 (review r1 F3): the abandoned runAgent.Run goroutine
				// above is still alive and will keep mutating whatever
				// *agent.SessionCarry it was handed (breaker maps,
				// activeSkill/skillPrompt, token anchors) for as long as it
				// runs — SessionCarry has no locking (see its doc comment),
				// and the very next turn is about to build a NEW Agent
				// sharing r.carry, which would then race with the orphan on
				// every one of those fields (and can panic on the map writes
				// inside breaker.observe). Detach: give the next turn a fresh
				// carry and leave the orphaned goroutine as the sole (if now
				// pointless) owner of the old one. This preserves the "never
				// hand one SessionCarry to two concurrently-running Agents"
				// contract instead of violating it, at the cost of losing
				// this orphaned turn's carried state — acceptable, since the
				// turn itself was already abandoned.
				r.carry = agent.NewSessionCarry()
			}
			break EventLoop
		}
	}

	r.ui.TurnEnd(lastUsage)

	// Always persist new messages, even on timeout/cancellation.
	for _, msg := range r.sess.Messages[prevMsgCount:] {
		_ = r.sessMgr.AppendMessage(r.sess.ID, msg)
	}

	// Sync plan mode both directions (M4-3): the agent may have exited plan
	// mode this turn (e.g. user confirmed the plan via exit_plan_mode) or
	// ENTERED it mid-turn (e.g. enter_plan_mode, deciding a complex request
	// needs a plan first) — either way, the next turn's AgentConfig.PlanMode
	// must reflect what actually happened, not just the exit direction.
	// Unconditional assignment (not the old if-guarded clear-only form)
	// keeps both directions symmetric with a single readback. Review r1
	// F10/item 6: this now runs BEFORE the turnErr early-return below so an
	// errored/interrupted turn still gets the readback (e.g. the agent
	// called enter_plan_mode and was then Ctrl+C'd) — runAgent.IsPlanMode()
	// is an atomic.Bool read, safe to call even if the Run goroutine that
	// owns it hasn't fully returned yet (the orphan path above).
	r.planMode = runAgent.IsPlanMode()
	// M4 final-phase review F-M4-4: every other write to r.planMode pairs
	// it with a SetStatus call (Run()'s startup, /plan, /run, model
	// switch) so the TUI footer stays in sync — this symmetric readback
	// must too, or a mid-turn enter_plan_mode leaves the footer showing
	// full tool access while the agent has actually restricted itself to
	// read-only tools. Safe to call here: runTurn executes on the REPL's
	// own goroutine, same as every other SetStatus call site.
	r.ui.SetStatus(r.currentModel, r.planMode)

	if turnErr != nil {
		r.saveSession()
		return turnErr
	}

	// Auto-title generation after first turn. Run asynchronously so the user
	// can keep typing while the title LLM call is in flight; a missed title
	// (e.g. REPL exits before goroutine returns) is acceptable since the user
	// can always rename via /title.
	if r.turn == 1 && r.sess.Title == "" {
		sessionID := r.sess.ID
		var firstUserMsg string
		for _, m := range r.sess.Messages {
			if m.Role == models.RoleHuman {
				firstUserMsg = m.Content
				break
			}
		}
		go r.generateTitle(sessionID, firstUserMsg)
	}

	// Record tool call distribution for preference extraction triggers.
	if r.prefSched != nil && len(turnToolCalls) > 0 {
		r.prefSched.RecordToolCalls(turnToolCalls)
	}

	// Schedule memory work for this turn.
	//
	// The throttle keeps LLM extraction cost bounded; compaction performs a
	// synchronous flush ([pkg/agent/react.go] CancelPendingUpdates+UpdateWith),
	// so nothing is lost between checkpoints. With auto-refine on, the gate
	// decides whether a checkpoint is worth extracting at all; with it off, the
	// unconditional extraction below is exactly the pre-gate behaviour.
	if r.cfg.MemoryService != nil && r.cfg.MemoryExtractor != nil {
		userScopeKey := ""
		if uid := strings.TrimSpace(r.cfg.WorkDir); uid != "" {
			userScopeKey = memory.UserScope(uid).Key()
		}
		switch memoryScheduleFor(r.turn, r.cfg.MemoryRefineInterval, r.autoRefineEnabled()) {
		case memoryScheduleRefine:
			// One gate call decides for both scopes; ScheduleRefine queues a job
			// per scope so dedup, cancellation and flush versioning stay sharded
			// by storage key.
			r.cfg.MemoryService.ScheduleRefine(r.sess.ID, userScopeKey, r.sess.Messages, r.cfg.MemoryExtractor)
		case memoryScheduleUnconditional:
			r.cfg.MemoryService.ScheduleUpdateWith(r.sess.ID, r.sess.Messages, r.cfg.MemoryExtractor)
			if userScopeKey != "" {
				r.cfg.MemoryService.ScheduleUpdateWith(userScopeKey, r.sess.Messages, r.cfg.MemoryExtractor)
			}
		}
	}

	// Schedule preference extraction (throttle is handled internally).
	if r.cfg.MemoryService != nil && r.cfg.PreferenceExtractor != nil {
		r.cfg.MemoryService.SchedulePreferenceUpdate(
			r.sess.ID, r.sess.Messages, r.cfg.PreferenceExtractor, r.prefSched,
		)
	}

	r.saveSession()
	return nil
}

func (r *ChatRepl) saveSession() {
	if r.sess == nil || r.sessMgr == nil {
		return
	}
	if err := r.sessMgr.Save(r.sess); err != nil {
		slog.Warn("save session failed", "err", err)
	}
}

// persistModel saves the current model alias and effort to the session metadata
// so that resuming the session restores the user's model choice and effort setting.
func (r *ChatRepl) persistModel() {
	if r.sess == nil {
		return
	}
	if r.sess.Metadata == nil {
		r.sess.Metadata = make(map[string]string)
	}
	r.sess.Metadata["model"] = r.currentModel
	r.sess.Metadata["effort"] = r.currentEffort
	r.saveSession()
}

// restoreModelFromSession reads the model alias and effort from session metadata
// and applies them to r.currentModel and r.currentEffort if the alias is still
// available in the registry.
func (r *ChatRepl) restoreModelFromSession() {
	if r.sess == nil || r.cfg.ModelRegistry == nil {
		return
	}
	alias := strings.TrimSpace(r.sess.Metadata["model"])
	if alias != "" && r.cfg.ModelRegistry.Has(alias) {
		r.currentModel = strings.ToLower(alias)
	} else if alias != "" {
		slog.Warn("session model alias not in registry, using default", "alias", alias, "default", r.currentModel)
	}
	// Restore effort; empty string means use model/provider default.
	r.currentEffort = strings.TrimSpace(r.sess.Metadata["effort"])
}

func (r *ChatRepl) generateTitle(sessionID, firstUserMsg string) {
	if r.cfg.ModelRegistry == nil || firstUserMsg == "" {
		slog.Debug("auto-title skipped", "registry_nil", r.cfg.ModelRegistry == nil, "msg_empty", firstUserMsg == "")
		return
	}
	provider, modelName, err := r.cfg.ModelRegistry.ProviderFor(r.currentModel)
	if err != nil {
		slog.Debug("auto-title skipped: model resolve failed", "err", err)
		return
	}
	if len(firstUserMsg) > 500 {
		firstUserMsg = firstUserMsg[:500]
	}

	prompt := fmt.Sprintf(
		"Generate a concise title (no more than 30 chars) in the same language as the user's message. Return only the title text, no quotes or formatting.\nUser: %s",
		firstUserMsg,
	)
	maxTokens := 60
	// Bounded timeout so a stuck LLM never leaks a goroutine for the lifetime
	// of the REPL.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:           modelName,
		Messages:        []models.Message{{Role: models.RoleHuman, Content: prompt}},
		MaxTokens:       &maxTokens,
		ReasoningEffort: "disabled",
	})
	if err != nil {
		slog.Debug("auto-title LLM failed", "err", err)
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		if err := r.sessMgr.SetTitle(sessionID, fallback); err != nil {
			slog.Debug("auto-title fallback SetTitle failed", "err", err)
		} else {
			slog.Debug("auto-title set via fallback", "id", sessionID, "title", fallback)
		}
		return
	}

	title := strings.TrimSpace(resp.Message.Content)
	slog.Debug("auto-title LLM response", "id", sessionID, "raw_title", resp.Message.Content, "title", title)
	if len([]rune(title)) > 30 {
		title = string([]rune(title)[:30])
	}
	if title == "" {
		slog.Debug("auto-title: LLM returned empty content, using fallback")
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		_ = r.sessMgr.SetTitle(sessionID, fallback)
		return
	}

	if err := r.sessMgr.SetTitle(sessionID, title); err != nil {
		slog.Debug("auto-title SetTitle failed", "err", err)
	}
	slog.Debug("session title set", "id", sessionID, "title", title)
}

// handleSlashCommand processes a slash command. Returns true if the REPL should exit.
func (r *ChatRepl) handleSlashCommand(parentCtx context.Context, cmd SlashCommand) bool {
	switch cmd.Name {
	case "exit", "quit", "q":
		return true
	case "help", "h":
		r.ui.Info(slashHelpText(SortedCommands(r.cfg.Commands)))
	case "clear":
		r.clearSession()
		r.ui.Info("  Session history cleared.")
	case "history":
		var sb strings.Builder
		PrintHistory(&sb, r.sess.Messages)
		r.ui.Info(strings.TrimRight(sb.String(), "\n"))
	case "compact":
		r.ui.Info("  Compaction is automatic when context fills up.")
	case "new":
		r.startNewSession()
	case "title":
		if cmd.Args == "" {
			r.ui.Info("  Usage: /title <name>")
			return false
		}
		_ = r.sessMgr.SetTitle(r.sess.ID, cmd.Args)
		r.sess.Title = cmd.Args
		r.ui.Info(fmt.Sprintf("  Title set to: %s", cmd.Args))
	case "save":
		r.saveSession()
		r.ui.Info("  Session saved.")
	case "sessions":
		r.ui.Info(r.sessionListText())
	case "undo":
		r.undoLastTurn()
	case "plan":
		r.planMode = true
		r.ui.SetStatus(r.currentModel, r.planMode)
		r.ui.Info("  Plan mode enabled. Agent will explore and plan before writing code.\n  Use /run to disable, or the agent will ask you to approve the plan.")
	case "run", "code":
		r.planMode = false
		r.ui.SetStatus(r.currentModel, r.planMode)
		r.ui.Info("  Plan mode disabled. Agent has full tool access.")
	case "model":
		r.handleModelCommand(parentCtx, cmd.Args)
	case "effort":
		r.handleEffortCommand(parentCtx, cmd.Args)
	case "image":
		r.handleImageCommand(parentCtx, cmd.Args)
	case "imagedetail", "imagequality":
		r.handleImageDetailCommand(cmd.Args)
	case "refine":
		r.handleRefineCommand(parentCtx, cmd.Args)
	case "doctor":
		r.ui.Info(r.doctorText(parentCtx))
	case "status", "st":
		r.ui.Info(r.statusText())
	default:
		r.ui.Info(fmt.Sprintf("  Unknown command: /%s", cmd.Name))
		r.ui.Info(slashHelpText(SortedCommands(r.cfg.Commands)))
	}
	return false
}

// handleModelCommand implements /model: show current, list available, switch by
// alias, or launch an interactive picker via the persistent TUI input box.
func (r *ChatRepl) handleModelCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)
	reg := r.cfg.ModelRegistry

	// /model (no args) or /model list — show current + available.
	if args == "" || args == "list" {
		var sb strings.Builder
		def, _ := reg.Resolve(r.currentModel)
		fmt.Fprintf(&sb, "  Current model: %s (%s/%s)", r.currentModel, def.Provider, def.Model)
		models := reg.List()
		if len(models) > 1 {
			fmt.Fprintf(&sb, "\n  Available models:")
			for _, m := range models {
				marker := "  "
				if strings.EqualFold(m.Name, r.currentModel) {
					marker = "→ "
				}
				fmt.Fprintf(&sb, "\n    %s%-12s — %s/%s", marker, m.Name, m.Provider, m.Model)
			}
		}
		r.ui.Info(sb.String())
		return
	}

	// /model ? — interactive picker through the TUI's own input box.
	if args == "?" {
		r.pickModel(ctx)
		return
	}

	// /model <alias> — switch by name.
	if !reg.Has(args) {
		r.ui.Info(fmt.Sprintf("  Unknown model %q. Available: %s", args, r.availableModelNames()))
		return
	}
	r.applyModel(ctx, strings.ToLower(args))
}

// pickModel presents an interactive model picker through the persistent TUI
// input box (AskQuestion), avoiding a nested huh/tea.Program that would
// conflict with the running Bubble Tea program.
func (r *ChatRepl) pickModel(ctx context.Context) {
	reg := r.cfg.ModelRegistry
	models := reg.List()
	if len(models) <= 1 {
		r.ui.Info("  Only one model configured.")
		return
	}
	var options []string
	for _, m := range models {
		label := fmt.Sprintf("%s (%s/%s)", m.Name, m.Provider, m.Model)
		if strings.EqualFold(m.Name, r.currentModel) {
			label += " *"
		}
		options = append(options, label)
	}
	header := "Select model:\n"
	for i, opt := range options {
		header += fmt.Sprintf("  %d. %s\n", i+1, opt)
	}
	header += "Enter number or alias (empty to cancel):"
	answer, err := r.ui.AskQuestion(ctx, header, options)
	if err != nil || strings.TrimSpace(answer) == "" {
		return
	}
	answer = strings.TrimSpace(answer)
	// AskQuestion returns the option text when a number is entered; extract alias.
	// Otherwise the user may have typed an alias directly.
	for _, m := range models {
		if strings.EqualFold(answer, m.Name) {
			r.applyModel(ctx, strings.ToLower(m.Name))
			return
		}
		// Match "alias (provider/model)" or "alias (provider/model) *"
		prefix := m.Name + " ("
		if strings.HasPrefix(strings.ToLower(answer), strings.ToLower(prefix)) {
			r.applyModel(ctx, strings.ToLower(m.Name))
			return
		}
	}
	r.ui.Info(fmt.Sprintf("  Unknown selection %q.", answer))
}

// applyModel switches to the named alias, updates UI, and persists to session.
func (r *ChatRepl) applyModel(_ context.Context, alias string) {
	r.currentModel = alias
	r.ui.SetStatus(r.currentModel, r.planMode)
	def, _ := r.cfg.ModelRegistry.Resolve(r.currentModel)
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  Model changed to: %s (%s/%s)\n  (takes effect on next turn)", r.currentModel, def.Provider, def.Model))
}

// availableModelNames returns a comma-separated list of model aliases.
func (r *ChatRepl) availableModelNames() string {
	var names []string
	for _, m := range r.cfg.ModelRegistry.List() {
		names = append(names, m.Name)
	}
	return strings.Join(names, ", ")
}

// currentReasoningEffort returns the effective reasoning effort for the current
// model: model-level override if set, otherwise REPL runtime setting (r.currentEffort),
// finally empty string (provider default).
func (r *ChatRepl) currentReasoningEffort() string {
	if r.cfg.ModelRegistry == nil {
		return r.currentEffort
	}
	def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel)
	if !ok || def.Effort == "" {
		return r.currentEffort
	}
	return def.Effort
}

// currentImageDetail returns the vision detail level for image attachments.
// Default is "low" (single tile, ~170 tokens) for token efficiency.
func (r *ChatRepl) currentImageDetail() string {
	if d := strings.TrimSpace(r.imageDetail); d != "" {
		return d
	}
	return "low"
}

// handleEffortCommand implements /effort: show current, list valid values, or set effort.
func (r *ChatRepl) handleEffortCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)

	// /effort (no args) — show current effort
	if args == "" {
		effort := r.currentReasoningEffort()
		if effort == "" {
			effort = "(provider default)"
		}
		r.ui.Info(fmt.Sprintf("  Current reasoning effort: %s", effort))
		r.ui.Info("  Valid values: low, medium, high, disabled, default")
		r.ui.Info("  Usage: /effort <value>")
		return
	}

	// /effort default — 重置为空（使用模型配置或 provider default）
	if args == "default" {
		r.currentEffort = ""
		r.persistModel()
		r.ui.Info("  Reasoning effort reset to model/provider default")
		r.ui.Info("  (takes effect on next turn)")
		return
	}

	// Validate and set effort
	validValues := map[string]bool{"low": true, "medium": true, "high": true, "disabled": true}
	argsLower := strings.ToLower(args)
	if !validValues[argsLower] {
		r.ui.Info(fmt.Sprintf("  Invalid effort %q. Valid: low, medium, high, disabled, default", args))
		return
	}

	r.currentEffort = argsLower
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  Reasoning effort set to: %s", r.currentEffort))
	r.ui.Info("  (takes effect on next turn)")
}

// handleImageCommand implements /image: show clipboard support status or
// attach an image file by path for the next turn.
func (r *ChatRepl) handleImageCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)

	if args == "" {
		// Show clipboard support status.
		r.ui.Info(fmt.Sprintf("  Clipboard image support: %s", imageproc.ClipboardSupport()))
		r.ui.Info("  Usage:")
		r.ui.Info("    Ctrl+V     Paste image from clipboard")
		r.ui.Info("    @path.png  Reference image file in your message")
		r.ui.Info("    /image <path>  Attach image file explicitly")
		return
	}

	// /image <path> — read and optimize the image, then run a turn with it.
	resolved := args
	if !filepath.IsAbs(resolved) && r.cfg.WorkDir != "" {
		resolved = filepath.Join(r.cfg.WorkDir, resolved)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error reading image: %v", err))
		return
	}
	result, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error optimizing image: %v", err))
		return
	}
	images := []models.MessageImage{{MimeType: result.MimeType, Base64: result.Base64}}
	r.ui.Info(fmt.Sprintf("  Attached: %s (%s, %dKB, ~%d tokens)",
		filepath.Base(args), result.MimeType, result.Bytes/1024,
		imageproc.EstimateTilesToTokens(1, "low")))

	r.turn++
	turnErr := r.runTurnWithSignal(ctx, func(ctx context.Context) error {
		return r.runTurn(ctx, fmt.Sprintf("Please analyze this image: %s", filepath.Base(args)), images, false)
	})
	if turnErr != nil {
		if turnErr.cancelled {
			r.ui.RenderInterrupted()
			return
		}
		r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
	}
}

// handleImageDetailCommand implements /imagedetail: set vision detail level.
func (r *ChatRepl) handleImageDetailCommand(args string) {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "" {
		current := r.currentImageDetail()
		r.ui.Info(fmt.Sprintf("  Current image detail: %s", current))
		r.ui.Info("  Valid values: low (default, ~170 tokens), high (multi-tile, finer detail)")
		r.ui.Info("  Usage: /imagedetail <low|high>")
		return
	}
	if args != "low" && args != "high" && args != "auto" {
		r.ui.Info(fmt.Sprintf("  Invalid value %q. Valid: low, high, auto", args))
		return
	}
	r.imageDetail = args
	r.ui.Info(fmt.Sprintf("  Image detail set to: %s (takes effect on next turn)", args))
}

// currentContextWindow returns the effective context window for the current
// model: model-level override if set, otherwise global default.
func (r *ChatRepl) currentContextWindow() int {
	if r.cfg.ModelRegistry == nil {
		return r.cfg.ContextWindow
	}
	def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel)
	if !ok || def.ContextWindow == 0 {
		return r.cfg.ContextWindow
	}
	return def.ContextWindow
}

// clearSession wipes the current session's history — both the in-memory
// messages and the persisted rows — so a later `deepai -c` resumes it empty
// instead of replaying a cleared conversation.
func (r *ChatRepl) clearSession() {
	r.sess.Messages = nil
	r.turn = 0
	// M4-3: the message history is gone, so any carried cross-turn Agent
	// state (breaker counters, active skill, compaction anchors) referring
	// to it must go too — a fresh SessionCarry, not a reset of the old
	// one's fields, matching NewSessionCarry's doc comment.
	r.carry = agent.NewSessionCarry()
	if r.sessMgr != nil {
		if err := r.sessMgr.DeleteMessagesAfterSeq(r.sess.ID, 0); err != nil {
			slog.Warn("clear persisted messages failed", "err", err)
		}
	}
}

func (r *ChatRepl) startNewSession() {
	// Mark current session as completed.
	r.sess.State = models.SessionStateCompleted
	r.saveSession()

	// Create new session.
	sess, err := r.sessMgr.Create(models.CreateOpts{
		Model: r.currentModel,
		CWD:   r.cfg.WorkDir,
	})
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error creating new session: %v", err))
		return
	}
	r.sess = sess
	r.turn = 0
	// M4-3 (review r1 F1): a new session must not inherit the previous
	// conversation's carried Agent state — same reset as clearSession, for
	// the same reason (see SessionCarry's doc comment).
	r.carry = agent.NewSessionCarry()
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  New session started: %s", sess.ID))
}

func (r *ChatRepl) undoLastTurn() {
	removed, err := r.sessMgr.DeleteLastUserTurn(r.sess.ID)
	if err != nil {
		slog.Warn("undo: delete last user turn failed", "err", err)
		r.ui.Info("  Undo failed.")
		return
	}
	if removed == 0 {
		r.ui.Info("  Nothing to undo.")
		return
	}

	msgs, err := r.sessMgr.LoadMessages(r.sess.ID)
	if err != nil {
		slog.Warn("undo: reload messages failed", "err", err)
	}
	r.sess.Messages = filterUnresolvedToolUses(msgs)

	// M4-3 (review r1 F8): the removed messages invalidate the carried
	// compaction anchor (lastInputTokens/lastTokenCountMsgs referred to a
	// message count that no longer exists — estimateContextTokens's stale-
	// anchor guard only catches the larger-shrink case) and any
	// skill/breaker state built up during the undone turn. Reset the whole
	// carry rather than only its anchors, matching /clear and /new.
	r.carry = agent.NewSessionCarry()

	r.ui.Info(fmt.Sprintf("  Undone %d messages.", removed))
}

// sessionListText renders the recent-session list as a string.
func (r *ChatRepl) sessionListText() string {
	if r.sessMgr == nil {
		return "  No session repository configured."
	}
	metas, err := r.sessMgr.ListRecent(20)
	if err != nil {
		return fmt.Sprintf("  Error listing sessions: %v", err)
	}
	if len(metas) == 0 {
		return "  No sessions found."
	}
	styles := DefaultStyles()
	var sb strings.Builder
	header := fmt.Sprintf("  %-24s %-40s %5s %s", "ID", "TITLE", "MSGS", "CREATED")
	sb.WriteString(styles.Dim.Render(header))
	sb.WriteString("\n")
	for _, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:37]) + "..."
		}
		created := m.CreatedAt.Format("2006-01-02 15:04")
		marker := "  "
		if m.ID == r.sess.ID {
			marker = styles.Highlight.Render(" *")
		}
		sb.WriteString(fmt.Sprintf("%s %-24s %-40s %5d %s\n", marker, m.ID, title, m.MsgCount, created))
	}
	sb.WriteString("  Use 'deepai -r <ID>' to resume a session.")
	return sb.String()
}

func (r *ChatRepl) statusText() string {
	var sb strings.Builder
	sessionID := ""
	if r.sess != nil {
		sessionID = r.sess.ID
	}
	tools := []models.Tool(nil)
	if r.cfg.ToolRegistry != nil {
		tools = r.cfg.ToolRegistry.List()
	}

	mcpTools := 0
	mcpServers := map[string]int{}
	for _, t := range tools {
		if !toolHasGroup(t, "mcp") {
			continue
		}
		mcpTools++
		server := mcpServerFromTool(t)
		if server == "" {
			server = "(unknown)"
		}
		mcpServers[server]++
	}

	pluginCmds := make([]string, 0)
	for _, c := range r.cfg.Commands {
		if c.Source == "plugin" {
			pluginCmds = append(pluginCmds, c.Name)
		}
	}
	sort.Strings(pluginCmds)

	type usage struct {
		count  int
		failed int
	}
	usageByTool := map[string]usage{}
	totalCalls := 0
	failedCalls := 0
	var messages []models.Message
	if r.sess != nil {
		messages = r.sess.Messages
	}
	for _, m := range messages {
		if m.Role != models.RoleTool || m.ToolResult == nil {
			continue
		}
		name := strings.TrimSpace(m.ToolResult.ToolName)
		if name == "" {
			continue
		}
		u := usageByTool[name]
		u.count++
		totalCalls++
		if m.ToolResult.Status == models.CallStatusFailed {
			u.failed++
			failedCalls++
		}
		usageByTool[name] = u
	}

	type kv struct {
		name string
		cnt  int
		fail int
	}
	toolUsage := make([]kv, 0, len(usageByTool))
	for name, u := range usageByTool {
		toolUsage = append(toolUsage, kv{name: name, cnt: u.count, fail: u.failed})
	}
	sort.Slice(toolUsage, func(i, j int) bool {
		if toolUsage[i].cnt != toolUsage[j].cnt {
			return toolUsage[i].cnt > toolUsage[j].cnt
		}
		return toolUsage[i].name < toolUsage[j].name
	})

	fmt.Fprintf(&sb, "  Runtime status:\n")
	// Show alias + provider/model for clarity.
	if def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel); ok {
		fmt.Fprintf(&sb, "  Model: %s (%s/%s)\n", r.currentModel, def.Provider, def.Model)
	} else {
		fmt.Fprintf(&sb, "  Model: %s\n", r.currentModel)
	}
	fmt.Fprintf(&sb, "  Session: %s\n", sessionID)
	fmt.Fprintf(&sb, "  Loaded tools: %d (builtin/custom: %d, mcp: %d)\n", len(tools), len(tools)-mcpTools, mcpTools)
	fmt.Fprintf(&sb, "  Image clipboard: %s\n", imageproc.ClipboardSupport())
	fmt.Fprintf(&sb, "  Image detail: %s\n", r.currentImageDetail())

	if len(mcpServers) == 0 {
		sb.WriteString("  MCP servers: none\n")
	} else {
		names := make([]string, 0, len(mcpServers))
		for name := range mcpServers {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s(%d)", name, mcpServers[name]))
		}
		fmt.Fprintf(&sb, "  MCP servers: %s\n", strings.Join(parts, ", "))
	}

	if len(pluginCmds) == 0 {
		sb.WriteString("  Plugin commands: none\n")
	} else {
		fmt.Fprintf(&sb, "  Plugin commands: %d (%s)\n", len(pluginCmds), strings.Join(pluginCmds, ", "))
	}

	if strings.TrimSpace(r.cfg.MCPReport) != "" {
		fmt.Fprintf(&sb, "  Startup report: %s\n", r.cfg.MCPReport)
	}

	fmt.Fprintf(&sb, "  Tool calls this session: %d total, %d failed\n", totalCalls, failedCalls)
	if len(toolUsage) > 0 {
		sb.WriteString("  Most used tools:\n")
		maxItems := 8
		if len(toolUsage) < maxItems {
			maxItems = len(toolUsage)
		}
		for i := 0; i < maxItems; i++ {
			line := fmt.Sprintf("    - %s: %d", toolUsage[i].name, toolUsage[i].cnt)
			if toolUsage[i].fail > 0 {
				line += fmt.Sprintf(" (failed %d)", toolUsage[i].fail)
			}
			sb.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// doctorText runs environment diagnostics across all three extension surfaces
// — models, skills, and MCP servers — and returns a combined report. This
// mirrors the /doctor command in Claude Code for quick environment checks.
func (r *ChatRepl) doctorText(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("  Doctor — environment diagnostics:\n\n")

	// 显示当前 effort 设置
	effort := r.currentEffort
	if effort == "" {
		effort = "(provider default)"
	}
	fmt.Fprintf(&sb, "  Current reasoning effort: %s\n\n", effort)

	sb.WriteString(r.doctorModels(ctx))
	sb.WriteString("\n\n")
	sb.WriteString(r.doctorSkills())
	sb.WriteString("\n\n")
	sb.WriteString(r.doctorMCP())
	return sb.String()
}

// doctorModels probes every configured model with a minimal "hello" request and
// reports per-model reachability, latency, and a summary.
func (r *ChatRepl) doctorModels(ctx context.Context) string {
	reg := r.cfg.ModelRegistry
	if reg == nil {
		return "  Models: no registry configured."
	}
	defs := reg.List()
	if len(defs) == 0 {
		return "  Models: none configured."
	}

	type result struct {
		def         llm.ModelDef
		ok          bool
		latency     time.Duration
		errMsg      string
		reply       string
		actualModel string // 服务端实际返回的模型名
	}

	results := make([]result, len(defs))
	var wg sync.WaitGroup
	// Per-probe timeout: a healthy model should respond to a trivial prompt
	// well under 30s. We bound each probe so one slow/unreachable model does
	// not stall the whole report.
	const probeTimeout = 30 * time.Second

	for i, def := range defs {
		wg.Add(1)
		go func(idx int, d llm.ModelDef) {
			defer wg.Done()
			res := result{def: d}
			provider, modelName, err := reg.ProviderFor(d.Name)
			if err != nil {
				res.errMsg = fmt.Sprintf("init failed: %v", err)
				results[idx] = res
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			maxTokens := 16
			start := time.Now()
			resp, err := provider.Chat(probeCtx, llm.ChatRequest{
				Model: modelName,
				Messages: []models.Message{{
					Role:    models.RoleHuman,
					Content: "Reply with exactly: hello",
				}},
				MaxTokens:       &maxTokens,
				ReasoningEffort: "disabled",
			})
			res.latency = time.Since(start)
			if err != nil {
				res.errMsg = err.Error()
				results[idx] = res
				return
			}
			res.ok = true
			res.reply = strings.TrimSpace(resp.Message.Content)
			res.actualModel = resp.Model
			results[idx] = res
		}(i, def)
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("  Models:\n")
	passed := 0
	for _, res := range results {
		marker := "✗"
		if res.ok {
			marker = "✓"
			passed++
		}
		endpoint := llm.ResolveBaseURL(res.def)
		if endpoint == "" {
			endpoint = "(provider default)"
		}
		// 计算有效上下文窗口
		effectiveCtx := r.cfg.ContextWindow
		if res.def.ContextWindow > 0 {
			effectiveCtx = res.def.ContextWindow
		}
		ctxLabel := fmt.Sprintf("%dK", effectiveCtx/1000)
		if res.def.ContextWindow > 0 {
			ctxLabel = fmt.Sprintf("%dK (model)", effectiveCtx/1000)
		}

		fmt.Fprintf(&sb, "    %s %-12s — %s/%s\n        endpoint: %s  ctx: %s", marker, res.def.Name, res.def.Provider, res.def.Model, endpoint, ctxLabel)
		if res.ok {
			fmt.Fprintf(&sb, "  (%dms)", res.latency.Milliseconds())
			// 显示实际模型名（如果与配置不同）
			if res.actualModel != "" && res.actualModel != res.def.Model {
				fmt.Fprintf(&sb, "  [actual: %s]", res.actualModel)
			}
			if res.reply != "" {
				reply := res.reply
				if len([]rune(reply)) > 40 {
					reply = string([]rune(reply)[:40]) + "..."
				}
				fmt.Fprintf(&sb, " → %q", reply)
			}
			sb.WriteString("\n")
		} else {
			msg := res.errMsg
			if msg == "" {
				msg = "unknown error"
			}
			if len([]rune(msg)) > 80 {
				msg = string([]rune(msg)[:80]) + "..."
			}
			fmt.Fprintf(&sb, "\n        ✗ %s\n", msg)
		}
	}
	if passed == len(results) {
		fmt.Fprintf(&sb, "    All %d model(s) healthy.", len(results))
	} else {
		fmt.Fprintf(&sb, "    %d/%d model(s) healthy. Check API keys, base URLs, and network.", passed, len(results))
	}
	return sb.String()
}

// doctorSkills reports the loaded skill set: count, per-skill source, and
// whether each skill's body is loadable. Skills are local files, so the check
// is configuration integrity (present + parseable), not network reachability.
func (r *ChatRepl) doctorSkills() string {
	reg := r.cfg.SkillRegistry
	if reg == nil || reg.Count() == 0 {
		return "  Skills: none loaded."
	}
	skills := reg.List()
	var sb strings.Builder
	fmt.Fprintf(&sb, "  Skills (%d):\n", len(skills))
	for _, s := range skills {
		source := s.Source
		if source == "" {
			source = "local"
		}
		fmt.Fprintf(&sb, "    ✓ %-20s (%s)\n", s.DisplayName(), source)
	}
	fmt.Fprintf(&sb, "    All %d skill(s) loaded.", len(skills))
	return sb.String()
}

// doctorMCP reports MCP server health from the runtime tool registry. MCP
// servers connect at startup and register their tools into ToolRegistry, so
// the presence of tools grouped under a server name signals a live connection.
// The startup report (MCPReport) surfaces any servers that failed to connect.
func (r *ChatRepl) doctorMCP() string {
	var sb strings.Builder
	sb.WriteString("  MCP servers:\n")

	// Collect MCP tools from the registry, grouped by server name.
	mcpTools := 0
	mcpServers := map[string]int{}
	if r.cfg.ToolRegistry != nil {
		for _, t := range r.cfg.ToolRegistry.List() {
			if !toolHasGroup(t, "mcp") {
				continue
			}
			mcpTools++
			server := mcpServerFromTool(t)
			if server == "" {
				server = "(unknown)"
			}
			mcpServers[server]++
		}
	}

	if len(mcpServers) == 0 {
		sb.WriteString("    none connected")
		// Surface startup failures if the config existed but nothing loaded.
		if msg := strings.TrimSpace(r.cfg.MCPReport); msg != "" {
			fmt.Fprintf(&sb, "\n    Startup report: %s", msg)
		}
		return sb.String()
	}

	// Stable server-name ordering.
	names := make([]string, 0, len(mcpServers))
	for name := range mcpServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&sb, "    ✓ %-20s — %d tool(s)\n", name, mcpServers[name])
	}
	fmt.Fprintf(&sb, "    %d server(s) connected, %d tool(s) registered.", len(mcpServers), mcpTools)
	return sb.String()
}

func toolHasGroup(t models.Tool, group string) bool {
	for _, g := range t.Groups {
		if g == group {
			return true
		}
	}
	return false
}

func mcpServerFromTool(t models.Tool) string {
	for _, g := range t.Groups {
		if g != "" && g != "mcp" {
			return g
		}
	}
	if idx := strings.Index(t.Name, "."); idx > 0 {
		return t.Name[:idx]
	}
	return ""
}

// evaluateFactFeedback classifies the user message for feedback purposes,
// records events for preference extraction triggers, and increments HelpfulCount
// for previously retrieved facts if the signal is positive. Called before the
// agent runs for the current turn.
func (r *ChatRepl) evaluateFactFeedback(sessionID string, turn int, userMessage string) {
	var prevMsg string
	for i := len(r.sess.Messages) - 1; i >= 0; i-- {
		if r.sess.Messages[i].Role == models.RoleHuman {
			prevMsg = r.sess.Messages[i].Content
			break
		}
	}
	similarity := memory.TextCosineSimilarity(userMessage, prevMsg)
	result := memory.ClassifyUserResponse(userMessage, prevMsg, similarity)

	slog.Debug("evaluateFactFeedback",
		"session", sessionID,
		"turn", turn,
		"classification", result.Classification,
		"similarity", similarity,
	)
	r.monitorFalseReward(result.Classification)

	if result.Classification == memory.FeedbackNegative && r.prefSched != nil {
		r.prefSched.RecordNegativeFeedback()
	} else if r.prefSched != nil {
		r.prefSched.RecordNonNegativeFeedback()
	}
	if r.prefSched != nil {
		r.prefSched.CheckLanguageSwitch(userMessage)
	}

	if r.cfg.MemoryService == nil {
		return
	}
	factIDs := r.cfg.MemoryService.LastRetrieved(sessionID)
	if len(factIDs) == 0 {
		return
	}
	switch result.Classification {
	case memory.FeedbackPositive:
		r.cfg.MemoryService.ScheduleHelpfulIncrement(sessionID, turn, factIDs)
	case memory.FeedbackNegative:
		r.cfg.MemoryService.ScheduleSuspectIncrement(sessionID, turn, factIDs)
	}
}

// monitorFalseReward checks if a positive classification was wrong by tracking
// consecutive corrections.
func (r *ChatRepl) monitorFalseReward(classification memory.FeedbackClassification) {
	if classification == memory.FeedbackNegative {
		r.consecCorrections++
		if r.consecCorrections >= 3 {
			slog.Warn("fact feedback: consecutive corrections detected, check for false rewards",
				"consecutive", r.consecCorrections,
			)
		}
	} else if classification == memory.FeedbackPositive {
		if r.consecCorrections >= 3 {
			slog.Warn("fact feedback: positive signal after consecutive corrections — possible false reward",
				"consecutive_before", r.consecCorrections,
			)
		}
		r.consecCorrections = 0
	}
}

var imageFileExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".gif":  true,
}

// parseImageReferences scans input for @path tokens pointing to image files,
// reads and optimizes them, and returns the cleaned text (with image refs
// replaced by placeholders) and the parsed images.
// Non-image @path references are left untouched. Original whitespace is
// preserved.
func parseImageReferences(input string, workDir string) (string, []models.MessageImage) {
	var images []models.MessageImage
	cleaned := input
	slog.Debug("parseImageReferences", "input", input, "workDir", workDir)

	for _, w := range strings.Fields(input) {
		if !strings.HasPrefix(w, "@") {
			continue
		}
		pathStr := strings.TrimPrefix(w, "@")
		// Strip surrounding quotes.
		pathStr = strings.Trim(pathStr, `"'`)
		if pathStr == "" {
			continue
		}

		ext := strings.ToLower(filepath.Ext(pathStr))
		if !imageFileExts[ext] {
			slog.Debug("parseImageReferences: not an image ext", "word", w, "ext", ext)
			continue
		}

		// Resolve relative to workDir.
		resolved := pathStr
		if !filepath.IsAbs(resolved) && workDir != "" {
			resolved = filepath.Join(workDir, resolved)
		}

		raw, err := os.ReadFile(resolved)
		if err != nil {
			slog.Debug("parseImageReferences: read failed", "path", resolved, "err", err)
			continue // not readable → leave the @ref as-is
		}

		result, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
		if err != nil {
			slog.Warn("image optimization failed", "path", resolved, "err", err)
			continue
		}

		slog.Debug("parseImageReferences: image loaded", "path", resolved, "mime", result.MimeType, "bytes", result.Bytes)

		// Replace just this @path token with a placeholder (preserves spacing).
		// Keep the full resolved path so the model can pass it to vision MCP tools.
		idx := len(images) // 1-based after append below
		images = append(images, models.MessageImage{
			MimeType: result.MimeType,
			Base64:   result.Base64,
		})
		cleaned = strings.Replace(cleaned, w, fmt.Sprintf("[image#%d:%s]", idx, resolved), 1)
	}

	return cleaned, images
}
