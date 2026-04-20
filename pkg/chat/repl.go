package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	"github.com/millken/deepai/pkg/workflow"
)

// ReplConfig holds configuration for the chat REPL.
type ReplConfig struct {
	Provider            string
	Model               string
	LLMProvider         llm.LLMProvider
	DatabaseURL         string
	ContextWindow       int
	MaxTurns            int
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
}

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
	sb, err := sandbox.New("cli", cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	return &ChatRepl{
		cfg:       cfg,
		renderer:  NewRenderer(os.Stderr),
		input:     NewInputHandler(),
		sessMgr:   cfg.SessionRepo,
		sb:        sb,
		prefSched: memory.NewPreferenceScheduler(),
	}, nil
}

// Run starts the REPL loop. It handles both interactive and single-query modes.
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
	for {
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
				break // exit requested
			}
			continue
		}

		r.turn++

		// Create a cancellable context for this turn.
		// Forward SIGINT to cancel the turn context, not the parent.
		turnCtx, turnCancel := context.WithCancel(parentCtx)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			select {
			case <-sigCh:
				turnCancel()
			case <-turnCtx.Done():
			}
		}()

		err = r.runTurn(turnCtx, line)
		turnCancel()
		signal.Stop(sigCh) // stop receiving on sigCh until next turn

		if err != nil {
			if turnCtx.Err() != nil {
				r.renderer.RenderInterrupted()
				continue // back to prompt
			}
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
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

func (r *ChatRepl) runTurn(ctx context.Context, userInput string) error {
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

	r.renderer.TurnStart(r.turn)

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
			// Drain remaining events.
			if events != nil {
				for evt := range events {
					if evt.Usage != nil {
						lastUsage = evt.Usage
					}
					r.renderer.RenderEvent(evt)
				}
			}
			if out.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return out.err
			}
			if out.result != nil {
				r.sess.Messages = out.result.Messages
				if out.result.Usage != nil {
					lastUsage = out.result.Usage
				}
			}
			break EventLoop
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	r.renderer.TurnEnd(lastUsage)

	// Sync plan mode: if the agent exited plan mode during this turn
	// (e.g. user confirmed the plan via exit_plan_mode tool), clear the
	// REPL flag so the next turn won't re-enter plan mode.
	if r.planMode && !runAgent.IsPlanMode() {
		r.planMode = false
	}

	// Persist only new messages produced by the agent.
	for _, msg := range r.sess.Messages[prevMsgCount:] {
		_ = r.sessMgr.AppendMessage(r.sess.ID, msg)
	}

	// Auto-title generation after first turn (synchronous to guarantee completion).
	if r.turn == 1 && r.sess.Title == "" {
		sessionID := r.sess.ID
		var firstUserMsg string
		for _, m := range r.sess.Messages {
			if m.Role == models.RoleHuman {
				firstUserMsg = m.Content
				break
			}
		}
		r.generateTitle(sessionID, firstUserMsg)
	}

	// Record tool call distribution for preference extraction triggers.
	if r.prefSched != nil && len(turnToolCalls) > 0 {
		r.prefSched.RecordToolCalls(turnToolCalls)
	}

	// Schedule memory update.
	if r.cfg.MemoryService != nil && r.cfg.MemoryExtractor != nil {
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
		slog.Warn("auto-title skipped", "provider_nil", r.cfg.LLMProvider == nil, "msg_empty", firstUserMsg == "")
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
	resp, err := r.cfg.LLMProvider.Chat(context.Background(), llm.ChatRequest{
		Model:           r.cfg.Model,
		Messages:        []models.Message{{Role: models.RoleHuman, Content: prompt}},
		MaxTokens:       &maxTokens,
		ReasoningEffort: "disabled",
	})
	if err != nil {
		slog.Warn("auto-title LLM failed", "err", err)
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		if err := r.sessMgr.SetTitle(sessionID, fallback); err != nil {
			slog.Warn("auto-title fallback SetTitle failed", "err", err)
		} else {
			slog.Info("auto-title set via fallback", "id", sessionID, "title", fallback)
		}
		return
	}

	title := strings.TrimSpace(resp.Message.Content)
	slog.Info("auto-title LLM response", "id", sessionID, "raw_title", resp.Message.Content, "title", title)
	if len([]rune(title)) > 30 {
		title = string([]rune(title)[:30])
	}
	if title == "" {
		slog.Warn("auto-title: LLM returned empty content, using fallback")
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		_ = r.sessMgr.SetTitle(sessionID, fallback)
		return
	}

	if err := r.sessMgr.SetTitle(sessionID, title); err != nil {
		slog.Warn("auto-title SetTitle failed", "err", err)
	}
	slog.Info("session title set", "id", sessionID, "title", title)
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
		printHistory(r.sess.Messages)
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
	case "undo":
		r.undoLastTurn()
	case "plan":
		r.planMode = true
		fmt.Fprintln(os.Stderr, "  Plan mode enabled. Agent will explore and plan before writing code.")
		fmt.Fprintln(os.Stderr, "  Use /run to disable, or the agent will ask you to approve the plan.")
	case "run", "code":
		r.planMode = false
		fmt.Fprintln(os.Stderr, "  Plan mode disabled. Agent has full tool access.")
	case "pipeline":
		r.handlePipeline(cmd.Args)
	case "workflow":
		r.handleWorkflow(cmd.Args)
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
	fmt.Fprintln(os.Stderr, "    /new       Start a new session")
	fmt.Fprintln(os.Stderr, "    /title     Set session title")
	fmt.Fprintln(os.Stderr, "    /save      Save session metadata")
	fmt.Fprintln(os.Stderr, "    /undo      Undo last turn")
	fmt.Fprintln(os.Stderr, "    /plan      Enter plan mode (read-only, explore before coding)")
	fmt.Fprintln(os.Stderr, "    /run       Exit plan mode (full tool access)")
	fmt.Fprintln(os.Stderr, "    /pipeline  Manage pipelines (list, run)")
	fmt.Fprintln(os.Stderr, "    /workflow  Manage workflows (list, run)")
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
	if len(factIDs) == 0 || result.Classification != memory.FeedbackPositive {
		return
	}
	r.cfg.MemoryService.ScheduleHelpfulIncrement(sessionID, turn, factIDs)
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

func (r *ChatRepl) handlePipeline(args string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Listen for Ctrl+C to cancel the pipeline
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	parts := strings.Fields(args)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "  Usage: /pipeline <list|run> [name] [prompt]")
		fmt.Fprintln(os.Stderr, "    /pipeline list              List available pipelines")
		fmt.Fprintln(os.Stderr, "    /pipeline run <name> <prompt>  Run a pipeline")
		return
	}

	switch parts[0] {
	case "list":
		names := agent.ListPipelines(r.cfg.WorkDir)
		if len(names) == 0 {
			fmt.Fprintln(os.Stderr, "  No pipelines available.")
			return
		}
		fmt.Fprintln(os.Stderr, "  Available pipelines:")
		for _, name := range names {
			p, err := agent.ResolvePipeline(name, r.cfg.WorkDir)
			if err != nil {
				continue
			}
			reviewerCount := len(p.Reviewers)
			desc := fmt.Sprintf("actor=%s, reviewers=%d, on_issues=%s", p.Actor.AgentType, reviewerCount, p.OnIssues)
			fmt.Fprintf(os.Stderr, "    %-20s %s\n", name, desc)
		}
	case "run":
		if len(parts) < 3 {
			fmt.Fprintln(os.Stderr, "  Usage: /pipeline run <name> <prompt>")
			return
		}
		name := parts[1]
		prompt := strings.Join(parts[2:], " ")

		p, err := agent.ResolvePipeline(name, r.cfg.WorkDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "  Running pipeline %q...\n", name)

		executor := agent.NewSubagentExecutor(r.cfg.LLMProvider, r.cfg.ToolRegistry, r.sb, r.cfg.Model).
			WithWorkDir(r.cfg.WorkDir).
			WithContextWindow(r.cfg.ContextWindow)
		pool := agent.NewSubagentPool(executor, 3, 2*time.Minute)
		orch := agent.NewOrchestrator(executor, pool, r.cfg.WorkDir).
			WithEventSink(func(evt agent.OrchestratorEvent) {
				switch evt.Type {
				case "actor_started":
					round := ""
					if evt.Round > 0 {
						round = fmt.Sprintf(" (retry round %d)", evt.Round+1)
					}
					fmt.Fprintf(os.Stderr, "  [%s] running%s...\n", evt.Name, round)
				case "actor_completed":
					fmt.Fprintf(os.Stderr, "  [%s] done\n", evt.Name)
				case "reviewer_started":
					fmt.Fprintf(os.Stderr, "  [%s] running...\n", evt.Name)
				case "reviewer_completed":
					fmt.Fprintf(os.Stderr, "  [%s] done\n", evt.Name)
				case "reviewer_failed":
					msg := evt.Message
					if len(msg) > 80 {
						msg = msg[:80] + "..."
					}
					fmt.Fprintf(os.Stderr, "  [%s] failed: %s\n", evt.Name, msg)
				}
			})

		result, err := orch.Run(ctx, p, agent.OrchestratorInput{
			UserInput: prompt,
			WorkDir:   r.cfg.WorkDir,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Pipeline error: %v\n", err)
			return
		}

		fmt.Fprintf(os.Stderr, "\n  Pipeline result: %s (rounds=%d)\n", result.Verdict, result.Rounds)
		if len(result.Reviews) > 0 {
			fmt.Fprintln(os.Stderr, "  Reviews:")
			for key, review := range result.Reviews {
				fmt.Fprintf(os.Stderr, "    [%s] %s: %s\n", key, review.Verdict, review.Summary)
			}
		}
		if result.ActorOutput != "" {
			preview := result.ActorOutput
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			fmt.Fprintf(os.Stderr, "\n  Output:\n%s\n", preview)
		}
	default:
		fmt.Fprintf(os.Stderr, "  Unknown pipeline subcommand: %s\n", parts[0])
		fmt.Fprintln(os.Stderr, "  Use: list or run")
	}
}

func (r *ChatRepl) handleWorkflow(args string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Listen for Ctrl+C to cancel the workflow
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	parts := strings.Fields(args)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "  Usage: /workflow <list|run> [name] [prompt]")
		fmt.Fprintln(os.Stderr, "    /workflow list              List available workflows")
		fmt.Fprintln(os.Stderr, "    /workflow run <name> <prompt>  Run a workflow")
		return
	}

	switch parts[0] {
	case "list":
		names := workflow.ListWorkflows(r.cfg.WorkDir)
		if len(names) == 0 {
			fmt.Fprintln(os.Stderr, "  No workflows available.")
			return
		}
		fmt.Fprintln(os.Stderr, "  Available workflows:")
		for _, name := range names {
			wf, err := workflow.ResolveWorkflow(name, r.cfg.WorkDir)
			if err != nil {
				continue
			}
			desc := fmt.Sprintf("stages=%d", len(wf.Stages))
			if wf.Description != "" {
				desc = wf.Description
			}
			fmt.Fprintf(os.Stderr, "    %-20s %s\n", name, DefaultStyles().Dim.Render(desc))
		}
	case "run":
		if len(parts) < 3 {
			fmt.Fprintln(os.Stderr, "  Usage: /workflow run <name> <prompt>")
			return
		}
		name := parts[1]
		prompt := strings.Join(parts[2:], " ")

		wf, err := workflow.ResolveWorkflow(name, r.cfg.WorkDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "  Running workflow %q (%d stages)...\n", name, len(wf.Stages))

		executor := agent.NewSubagentExecutor(r.cfg.LLMProvider, r.cfg.ToolRegistry, r.sb, r.cfg.Model).
			WithWorkDir(r.cfg.WorkDir).
			WithContextWindow(r.cfg.ContextWindow)
		pool := agent.NewSubagentPool(executor, 3, 2*time.Minute)

		env := workflow.NewEnvironment()
		defer env.Close()

		engine := workflow.NewEngine(executor, pool, r.cfg.WorkDir).WithEnvironment(env)

		// Inject event sink for real-time stage progress
		wfCtx := subagent.WithEventSink(ctx, subagent.EventSink(func(evt subagent.TaskEvent) {
			switch evt.Type {
			case "stage_started":
				fmt.Fprintf(os.Stderr, "  [%s] running...\n", evt.Description)
			case "stage_completed":
				fmt.Fprintf(os.Stderr, "  [%s] done\n", evt.Description)
			case "stage_skipped":
				fmt.Fprintf(os.Stderr, "  [%s] skipped\n", evt.Description)
			case "stage_failed":
				fmt.Fprintf(os.Stderr, "  [%s] failed\n", evt.Description)
			}
		}))

		result, err := engine.Run(wfCtx, wf, prompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Workflow error: %v\n", err)
			return
		}

		r.renderer.RenderWorkflowResult(result)
	default:
		fmt.Fprintf(os.Stderr, "  Unknown workflow subcommand: %s\n", parts[0])
		fmt.Fprintln(os.Stderr, "  Use: list or run")
	}
}
