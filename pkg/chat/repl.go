package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	renderer          *Renderer
	input             *InputHandler
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
		cfg:       cfg,
		renderer:  NewRenderer(os.Stderr),
		input:     NewInputHandler(),
		sessMgr:   cfg.SessionRepo,
		sb:        sb,
		prefSched: memory.NewPreferenceScheduler(),
	}
	if cfg.InputHistoryFile != "" {
		repl.input.LoadHistoryFile(cfg.InputHistoryFile)
	}
	return repl, nil
}

// Run starts the REPL loop. It handles both interactive and single-query modes.
func (r *ChatRepl) Run(parentCtx context.Context) error {
	defer r.sb.Close()
	defer r.input.SaveHistoryFile()
	defer func() {
		if r.cfg.MemoryService != nil {
			r.cfg.MemoryService.CleanupStale(time.Hour)
		}
	}()

	// Resolve session.
	if err := r.resolveSession(); err != nil {
		return err
	}

	// Single query mode.
	if r.cfg.Query != "" {
		return r.runSingleQuery(parentCtx)
	}

	// Show banner.
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
	RenderBanner(os.Stderr, BannerInfo{
		Provider:   r.cfg.Provider,
		Model:      r.cfg.Model,
		ToolCount:  toolCount,
		SkillCount: skillCount,
		SkillNames: skillNames,
		SessionID:  r.sess.ID,
	})

	// Interactive loop.
	// SIGINT (Ctrl+C) during a turn cancels only that turn.
	// SIGINT at the prompt exits the REPL.
	sigCh := make(chan os.Signal, 1)
	defer signal.Stop(sigCh)

	// Auto-continue: if the resumed session was interrupted mid-task,
	// start the agent immediately without waiting for user input.
	autoContinue := (r.cfg.ResumeSession != "" || r.cfg.ContinueLast) && isSessionInterrupted(r.sess.Messages)

	for {
		// Auto-continue: on first iteration of an interrupted session,
		// run the agent immediately without waiting for user input.
		if autoContinue {
			autoContinue = false
			fmt.Fprintln(os.Stderr, "  Resuming interrupted session...")
			r.turn++
			if err := r.runTurnWithSignal(parentCtx, sigCh, r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				if err.cancelled {
					r.renderer.RenderInterrupted()
					continue
				}
				fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			}
		}

		// Wait for user input.
		line, err := r.input.ReadPrompt(parentCtx)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				// Ctrl+C at prompt — exit REPL.
				fmt.Fprintln(os.Stderr, "\n  Interrupted.")
				break
			}
			if parentCtx.Err() != nil {
				fmt.Fprintln(os.Stderr, "\n  Interrupted.")
			}
			break
		}
		if line == "" {
			continue
		}

		// Handle slash commands.
		if cmd, ok := ParseSlashCommand(line); ok {
			if r.handleSlashCommand(cmd) {
				break
			}
			continue
		}

		// Continuation input ("继续", "continue", etc.): resume agent
		// without adding a new human message.
		if isContinuationInput(line) && len(r.sess.Messages) > 0 {
			r.turn++
			if err := r.runTurnWithSignal(parentCtx, sigCh, r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			}
			continue
		}

		r.turn++

		turnErr := r.runTurnWithSignal(parentCtx, sigCh, func(ctx context.Context) error {
			return r.runTurn(ctx, line)
		})
		if turnErr != nil {
			if turnErr.cancelled {
				r.renderer.RenderInterrupted()
				continue
			}
			fmt.Fprintf(os.Stderr, "  Error: %v\n", turnErr)
		}
	}

	// Save session metadata on exit.
	r.saveSession()
	slog.Info("session ended", "session_id", r.sess.ID, "turns", r.turn)
	return nil
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

func (r *ChatRepl) runSingleQuery(ctx context.Context) error {
	r.turn = 1
	return r.runTurn(ctx, r.cfg.Query)
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
	return r.runTurn(ctx, "Continue from where you left off.")
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

// runTurnWithSignal creates a cancellable turn context, forwards SIGINT to it,
// runs the given turn function, and returns a turnError that distinguishes
// cancellation (Ctrl+C) from real errors. Returns nil on clean success.
func (r *ChatRepl) runTurnWithSignal(parentCtx context.Context, sigCh chan os.Signal, fn func(context.Context) error) *turnError {
	turnCtx, turnCancel := context.WithCancel(parentCtx)
	signal.Notify(sigCh, os.Interrupt)

	// sigFired is written by the signal goroutine before turnCancel(); reading
	// it after fn returns (which implies the context is done) is race-free.
	sigFired := make(chan struct{}, 1)
	go func() {
		select {
		case <-sigCh:
			sigFired <- struct{}{}
			turnCancel()
		case <-turnCtx.Done():
		}
	}()

	err := fn(turnCtx)
	turnCancel()
	signal.Stop(sigCh)

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

func (r *ChatRepl) runTurn(ctx context.Context, userInput string) error {
	ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		r.renderer.RenderSubagentEvent(evt)
	})

	// Evaluate fact feedback from previous turn (consume-once).
	r.evaluateFactFeedback(r.sess.ID, r.turn, userInput)

	// Append user message to session history and persist.
	userMsg := models.Message{
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   userInput,
	}
	if err := r.sessMgr.AppendMessage(r.sess.ID, userMsg); err != nil {
		slog.Warn("append user message", "err", err)
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
		UserInteraction: r.input,
		PlanMode:        r.planMode,
		WorkDir:         r.cfg.WorkDir,
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

	r.renderer.TurnStart(r.turn, userInput)

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

	// Process events as they arrive.
	var lastUsage *agent.Usage
	var turnErr error
	var turnToolCalls []memory.ToolCallInfo
	turnStart := time.Now()
	lastProgress := turnStart
	var lastHeartbeat time.Time
	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()
EventLoop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			lastProgress = time.Now()
			if evt.Usage != nil {
				lastUsage = evt.Usage
			}
			r.renderer.RenderEvent(evt)
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
			lastProgress = time.Now()
			// Drain remaining events.
			if events != nil {
				for evt := range events {
					lastProgress = time.Now()
					if evt.Usage != nil {
						lastUsage = evt.Usage
					}
					r.renderer.RenderEvent(evt)
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
			turnErr = ctx.Err()
			break EventLoop
		case <-heartbeatTicker.C:
			idle := time.Since(lastProgress)
			if idle < 20*time.Second {
				continue
			}
			if !lastHeartbeat.IsZero() && time.Since(lastHeartbeat) < 30*time.Second {
				continue
			}
			r.renderer.RenderHeartbeat(time.Since(turnStart), idle)
			lastHeartbeat = time.Now()
		}
	}

	r.renderer.TurnEnd(lastUsage)

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
func (r *ChatRepl) handleSlashCommand(cmd SlashCommand) bool {
	switch cmd.Name {
	case "exit", "quit", "q":
		return true
	case "help", "h":
		printSlashHelp()
	case "clear":
		r.sess.Messages = nil
		fmt.Fprintln(os.Stderr, "  Session history cleared.")
	case "history":
		PrintHistory(os.Stderr, r.sess.Messages)
	case "compact":
		fmt.Fprintln(os.Stderr, "  Compaction is automatic when context fills up.")
	case "new":
		r.startNewSession()
	case "title":
		if cmd.Args == "" {
			fmt.Fprintln(os.Stderr, "  Usage: /title <name>")
			return false
		}
		_ = r.sessMgr.SetTitle(r.sess.ID, cmd.Args)
		r.sess.Title = cmd.Args
		fmt.Fprintf(os.Stderr, "  Title set to: %s\n", cmd.Args)
	case "save":
		r.saveSession()
		fmt.Fprintln(os.Stderr, "  Session saved.")
	case "sessions":
		r.printSessionList()
	case "undo":
		r.undoLastTurn()
	case "plan":
		r.planMode = true
		fmt.Fprintln(os.Stderr, "  Plan mode enabled. Agent will explore and plan before writing code.")
		fmt.Fprintln(os.Stderr, "  Use /run to disable, or the agent will ask you to approve the plan.")
	case "run", "code":
		r.planMode = false
		fmt.Fprintln(os.Stderr, "  Plan mode disabled. Agent has full tool access.")
	case "model":
		if cmd.Args == "" {
			fmt.Fprintf(os.Stderr, "  Current model: %s\n", r.cfg.Model)
			return false
		}
		r.cfg.Model = cmd.Args
		fmt.Fprintf(os.Stderr, "  Model changed to: %s\n  (takes effect on next turn)\n", r.cfg.Model)
	default:
		fmt.Fprintf(os.Stderr, "  Unknown command: /%s\n", cmd.Name)
		printSlashHelp()
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
		fmt.Fprintf(os.Stderr, "  Error creating new session: %v\n", err)
		return
	}
	r.sess = sess
	r.turn = 0
	fmt.Fprintf(os.Stderr, "  New session started: %s\n", sess.ID)
}

func (r *ChatRepl) undoLastTurn() {
	// Find the last human message and remove it and everything after it.
	lastHuman := -1
	for i := len(r.sess.Messages) - 1; i >= 0; i-- {
		if r.sess.Messages[i].Role == models.RoleHuman {
			lastHuman = i
			break
		}
	}
	if lastHuman < 0 {
		fmt.Fprintln(os.Stderr, "  Nothing to undo.")
		return
	}
	removed := len(r.sess.Messages) - lastHuman
	r.sess.Messages = r.sess.Messages[:lastHuman]

	// Delete persisted messages after the kept boundary.
	// LoadMessages returns messages ordered by seq ASC, so index maps to seq = index+1.
	// We keep messages 0..lastHuman-1 (seq 1..lastHuman), delete seq > lastHuman.
	if err := r.sessMgr.DeleteMessagesAfterSeq(r.sess.ID, lastHuman); err != nil {
		slog.Warn("undo: delete messages from DB failed", "err", err)
	}

	fmt.Fprintf(os.Stderr, "  Undone %d messages.\n", removed)
}

func printSlashHelp() {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Commands:")
	fmt.Fprintln(os.Stderr, "    /help      Show this help")
	fmt.Fprintln(os.Stderr, "    /clear     Clear session history")
	fmt.Fprintln(os.Stderr, "    /history   Show conversation history")
	fmt.Fprintln(os.Stderr, "    /sessions  List recent sessions")
	fmt.Fprintln(os.Stderr, "    /new       Start a new session")
	fmt.Fprintln(os.Stderr, "    /title     Set session title")
	fmt.Fprintln(os.Stderr, "    /save      Save session metadata")
	fmt.Fprintln(os.Stderr, "    /undo      Undo last turn")
	fmt.Fprintln(os.Stderr, "    /plan      Enter plan mode (read-only, explore before coding)")
	fmt.Fprintln(os.Stderr, "    /run       Exit plan mode (full tool access)")
	fmt.Fprintln(os.Stderr, "    /model     Show current model (/model <name> to switch)")
	fmt.Fprintln(os.Stderr, "    /exit      Exit the REPL")
	fmt.Fprintln(os.Stderr)
}

func printHistory(messages []models.Message) {
	styles := DefaultStyles()
	for _, msg := range messages {
		switch msg.Role {
		case models.RoleHuman:
			fmt.Fprintln(os.Stderr, styles.UserPrompt.Render("  You: ")+msg.Content)
		case models.RoleAI:
			content := msg.Content
			if len(content) > 100 {
				content = content[:100] + "..."
			}
			fmt.Fprintln(os.Stderr, styles.Assistant.Render("  AI: ")+content)
		}
	}
}

// printSessionList lists recent sessions in the current REPL.
func (r *ChatRepl) printSessionList() {
	if r.sessMgr == nil {
		fmt.Fprintln(os.Stderr, "  No session repository configured.")
		return
	}
	metas, err := r.sessMgr.ListRecent(20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error listing sessions: %v\n", err)
		return
	}
	if len(metas) == 0 {
		fmt.Fprintln(os.Stderr, "  No sessions found.")
		return
	}
	styles := DefaultStyles()
	header := fmt.Sprintf("  %-24s %-40s %5s %s", "ID", "TITLE", "MSGS", "CREATED")
	fmt.Fprintln(os.Stderr, styles.Dim.Render(header))
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
		fmt.Fprintf(os.Stderr, "%s %-24s %-40s %5d %s\n", marker, m.ID, title, m.MsgCount, created)
	}
	fmt.Fprintf(os.Stderr, "  Use 'deepai -r <ID>' to resume a session.\n")
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
