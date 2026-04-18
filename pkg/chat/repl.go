package chat

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
)

// ReplConfig holds configuration for the chat REPL.
type ReplConfig struct {
	Provider       string
	Model          string
	LLMProvider    llm.LLMProvider
	DatabaseURL    string
	ContextWindow  int
	MaxTurns       int
	Query          string // non-interactive single query
	ResumeSession  string // session ID to resume
	ContinueLast   bool   // resume most recent session
	SystemPrompt   string
	WorkDir        string
	ToolRegistry   *tools.Registry
	SkillRegistry  *skill.Registry
	MemoryService  *memory.Service
	MemoryExtractor memory.Extractor
}

// ChatRepl is the interactive chat REPL.
type ChatRepl struct {
	cfg      ReplConfig
	renderer *Renderer
	input    *InputHandler
	sess     *Session
	sessMgr  *SessionStore
	sb       *sandbox.Sandbox
	turn     int
}

// NewRepl creates a new chat REPL instance.
func NewRepl(cfg ReplConfig) (*ChatRepl, error) {
	sb, err := sandbox.New("cli", cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	home, _ := os.UserHomeDir()
	sessDir := home + "/.deepai/sessions"
	sessMgr, err := NewSessionStore(sessDir)
	if err != nil {
		return nil, fmt.Errorf("session store init: %w", err)
	}

	return &ChatRepl{
		cfg:      cfg,
		renderer: NewRenderer(os.Stdout),
		input:    NewInputHandler(os.Stdin),
		sessMgr:  sessMgr,
		sb:       sb,
	}, nil
}

// Run starts the REPL loop. It handles both interactive and single-query modes.
func (r *ChatRepl) Run(ctx context.Context) error {
	defer r.sb.Close()

	// Resolve session.
	if err := r.resolveSession(); err != nil {
		return err
	}

	// Single query mode.
	if r.cfg.Query != "" {
		return r.runSingleQuery(ctx)
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
	for {
		line, err := r.input.ReadPrompt(ctx)
		if err != nil {
			if ctx.Err() != nil {
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
		if err := r.runTurn(ctx, line); err != nil {
			if ctx.Err() != nil {
				r.renderer.RenderInterrupted()
				continue
			}
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		}
	}

	// Save session on exit.
	r.saveSession()
	slog.Info("session ended", "session_id", r.sess.ID, "turns", r.turn)
	return nil
}

func (r *ChatRepl) resolveSession() error {
	// Resume by ID.
	if r.cfg.ResumeSession != "" {
		sess, err := r.sessMgr.Load(r.cfg.ResumeSession)
		if err != nil {
			return fmt.Errorf("resume session %s: %w", r.cfg.ResumeSession, err)
		}
		r.sess = sess
		slog.Info("resumed session", "id", sess.ID, "messages", len(sess.Messages))
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
			slog.Info("continued session", "id", sess.ID, "messages", len(sess.Messages))
			return nil
		}
	}

	// New session.
	sess, err := r.sessMgr.Create()
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

func (r *ChatRepl) runTurn(parentCtx context.Context, userInput string) error {
	// Create a cancellable sub-context for this turn.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// Append user message to session history.
	r.sess.Messages = append(r.sess.Messages, models.Message{
		ID:        fmt.Sprintf("m-%d", r.turn),
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   userInput,
	})

	// Create a fresh agent for this turn.
	agentCfg := agent.AgentConfig{
		LLMProvider:   r.cfg.LLMProvider,
		Tools:         r.cfg.ToolRegistry,
		Sandbox:       r.sb,
		AgentType:     agent.AgentTypeCoder,
		Model:         r.cfg.Model,
		ContextWindow: r.cfg.ContextWindow,
		MaxTurns:      r.cfg.MaxTurns,
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
			cancel()
			return ctx.Err()
		}
	}

	r.renderer.TurnEnd(lastUsage)

	// Schedule memory update.
	if r.cfg.MemoryService != nil && r.cfg.MemoryExtractor != nil {
		r.cfg.MemoryService.ScheduleUpdateWith(r.sess.ID, r.sess.Messages, r.cfg.MemoryExtractor)
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
	default:
		fmt.Fprintf(os.Stderr, "  Unknown command: /%s\n", cmd.Name)
		printSlashHelp()
	}
	return false
}

func printSlashHelp() {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Commands:")
	fmt.Fprintln(os.Stderr, "    /help      Show this help")
	fmt.Fprintln(os.Stderr, "    /clear     Clear session history")
	fmt.Fprintln(os.Stderr, "    /history   Show conversation history")
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
