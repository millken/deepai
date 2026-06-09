package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
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
	Model               string
	LLMProvider         llm.LLMProvider
	DatabaseURL         string
	ContextWindow       int
	MaxTurns            int
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
	SandboxBaseDir      string                   // root for sandbox session dirs; must NOT be the user's workdir
}

// memoryExtractInterval is the turn cadence for async memory extraction in CLI.
// Set to 5: covers a typical short exchange in one batch while keeping LLM
// extraction cost bounded. Compaction always flushes synchronously, so facts
// are never lost across the context boundary.
const memoryExtractInterval = 5

// ChatRepl is the interactive chat REPL.
type ChatRepl struct {
	cfg               ReplConfig
	ui                *TUI
	historyFile       string
	sess              *models.Session
	sessMgr           models.SessionRepository
	sb                *sandbox.Sandbox
	turn              int
	prefSched         *memory.PreferenceScheduler
	consecCorrections int
	planMode          bool // restrict to read-only tools until user approves plan
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
		cfg:         cfg,
		historyFile: cfg.InputHistoryFile,
		sessMgr:     cfg.SessionRepo,
		sb:          sb,
		prefSched:   memory.NewPreferenceScheduler(),
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
	r.ui.SetStatus(r.cfg.Model, r.planMode)

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
		line, err := r.ui.ReadPrompt(parentCtx)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				// Ctrl+C at prompt — exit REPL.
				r.ui.Info("  Interrupted.")
				break
			}
			// io.EOF (Ctrl+D) or context cancellation: exit quietly.
			break
		}
		if line == "" {
			continue
		}

		// Handle slash commands.
		if cmd, ok := ParseSlashCommand(line); ok {
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

		turnErr := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
			return r.runTurn(ctx, line, false)
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
	return BannerInfo{
		Provider:      r.cfg.Provider,
		Model:         r.cfg.Model,
		ToolCount:     toolCount,
		SkillCount:    skillCount,
		SkillNames:    skillNames,
		SessionID:     r.sess.ID,
		ContextWindow: r.cfg.ContextWindow,
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
			return nil
		}
	}

	// New session.
	sess, err := r.sessMgr.Create(models.CreateOpts{
		Model: r.cfg.Model,
		CWD:   r.cfg.WorkDir,
	})
	if err != nil {
		return err
	}
	r.sess = sess
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
	return r.runTurn(ctx, "Continue from where you left off.", true)
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
	go func() {
		select {
		case <-uiInterrupt:
			sigFired <- struct{}{}
			turnCancel()
		case <-turnCtx.Done():
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

func (r *ChatRepl) runTurn(ctx context.Context, userInput string, continuation bool) error {
	ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		r.ui.RenderSubagentEvent(evt)
	})

	// Evaluate fact feedback from previous turn (consume-once).
	r.evaluateFactFeedback(r.sess.ID, r.turn, userInput)

	// Append user message to session history. A continuation is a synthetic
	// nudge (e.g. resume after interrupt), not a real user turn: hand it to the
	// agent in-memory but never persist it, so it doesn't pollute the saved
	// transcript or the FTS index.
	userMsg := models.Message{
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   userInput,
	}
	if !continuation {
		if err := r.sessMgr.AppendMessage(r.sess.ID, userMsg); err != nil {
			slog.Warn("append user message", "err", err)
		}
	}
	r.sess.Messages = append(r.sess.Messages, userMsg)

	// Create a fresh agent for this turn.
	agentCfg := agent.AgentConfig{
		LLMProvider:     r.cfg.LLMProvider,
		Tools:           r.cfg.ToolRegistry,
		Sandbox:         r.sb,
		Model:           r.cfg.Model,
		ContextWindow:   r.cfg.ContextWindow,
		MaxTurns:        r.cfg.MaxTurns,
		RequestTimeout:  r.cfg.RequestTimeout,
		UserInteraction: r.ui,
		PlanMode:        r.planMode,
		WorkDir:         r.cfg.WorkDir,
		MemoryService:   r.cfg.MemoryService,
		MemoryExtractor: r.cfg.MemoryExtractor,
		MemoryUserID:    r.cfg.WorkDir,
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
			case <-time.After(10 * time.Second):
				// Defensive: a tool ignoring ctx could delay Run's return.
				// Persist what we have rather than hanging the REPL.
			}
			break EventLoop
		}
	}

	r.ui.TurnEnd(lastUsage)

	// Always persist new messages, even on timeout/cancellation.
	for _, msg := range r.sess.Messages[prevMsgCount:] {
		_ = r.sessMgr.AppendMessage(r.sess.ID, msg)
	}

	if turnErr != nil {
		r.saveSession()
		return turnErr
	}

	// Sync plan mode: if the agent exited plan mode during this turn
	// (e.g. user confirmed the plan via exit_plan_mode tool), clear the
	// REPL flag so the next turn won't re-enter plan mode.
	if r.planMode && !runAgent.IsPlanMode() {
		r.planMode = false
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

	// Schedule memory update.
	// Throttle: extract every memoryExtractInterval turns. Compaction performs a
	// synchronous flush ([pkg/agent/react.go] CancelPendingUpdates+UpdateWith) so
	// nothing is lost; this guard avoids paying for an LLM extraction call on every
	// single turn while still capturing facts before context is compacted.
	if r.cfg.MemoryService != nil && r.cfg.MemoryExtractor != nil && r.turn%memoryExtractInterval == 0 {
		r.cfg.MemoryService.ScheduleUpdateWith(r.sess.ID, r.sess.Messages, r.cfg.MemoryExtractor)
		if uid := strings.TrimSpace(r.cfg.WorkDir); uid != "" {
			r.cfg.MemoryService.ScheduleUpdateWith(memory.UserScope(uid).Key(), r.sess.Messages, r.cfg.MemoryExtractor)
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

func (r *ChatRepl) generateTitle(sessionID, firstUserMsg string) {
	if r.cfg.LLMProvider == nil || firstUserMsg == "" {
		slog.Debug("auto-title skipped", "provider_nil", r.cfg.LLMProvider == nil, "msg_empty", firstUserMsg == "")
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
	resp, err := r.cfg.LLMProvider.Chat(ctx, llm.ChatRequest{
		Model:           r.cfg.Model,
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
		r.ui.Info(slashHelpText())
	case "clear":
		r.sess.Messages = nil
		r.turn = 0
		if r.sessMgr != nil && r.sess != nil && r.sess.ID != "" {
			if err := r.sessMgr.DeleteMessagesAfterSeq(r.sess.ID, 0); err != nil {
				slog.Warn("clear persisted messages failed", "err", err)
			}
		}
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
		r.ui.SetStatus(r.cfg.Model, r.planMode)
		r.ui.Info("  Plan mode enabled. Agent will explore and plan before writing code.\n  Use /run to disable, or the agent will ask you to approve the plan.")
	case "run", "code":
		r.planMode = false
		r.ui.SetStatus(r.cfg.Model, r.planMode)
		r.ui.Info("  Plan mode disabled. Agent has full tool access.")
	case "model":
		if cmd.Args == "" {
			r.ui.Info(fmt.Sprintf("  Current model: %s", r.cfg.Model))
			return false
		}
		r.cfg.Model = cmd.Args
		r.ui.SetStatus(r.cfg.Model, r.planMode)
		r.ui.Info(fmt.Sprintf("  Model changed to: %s\n  (takes effect on next turn)", r.cfg.Model))
	default:
		r.ui.Info(fmt.Sprintf("  Unknown command: /%s", cmd.Name))
		r.ui.Info(slashHelpText())
	}
	return false
}

func (r *ChatRepl) startNewSession() {
	// Mark current session as completed.
	r.sess.State = models.SessionStateCompleted
	r.saveSession()

	// Create new session.
	sess, err := r.sessMgr.Create(models.CreateOpts{
		Model: r.cfg.Model,
		CWD:   r.cfg.WorkDir,
	})
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error creating new session: %v", err))
		return
	}
	r.sess = sess
	r.turn = 0
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
