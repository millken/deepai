package commands

import (
	"context"
	"fmt"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/chat"
	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
	"github.com/spf13/cobra"
	"log/slog"
	"os"
	"strings"
)

// Chat flags shared between root and chat subcommand.
var chatFlags struct {
	Query    string
	Resume   string
	Continue bool
	Model    string
	MaxTurns int
}

func addChat(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Start an interactive chat session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChat(cmd.Context(), chatFlags.Query, chatFlags.Resume, chatFlags.Continue, chatFlags.Model, chatFlags.MaxTurns)
		},
	}

	cmd.Flags().StringVarP(&chatFlags.Query, "query", "q", "", "Single query (non-interactive mode)")
	cmd.Flags().StringVarP(&chatFlags.Resume, "resume", "r", "", "Resume a session by ID")
	cmd.Flags().BoolVarP(&chatFlags.Continue, "continue", "c", false, "Continue most recent session")
	cmd.Flags().StringVarP(&chatFlags.Model, "model", "m", "", "Override model from config")
	cmd.Flags().IntVar(&chatFlags.MaxTurns, "max-turns", 0, "Max agent turns per run (0=unlimited)")

	topLevel.AddCommand(cmd)
}

// RegisterChatFlags adds chat flags to a command (used by root to support bare `deepai -q`).
func RegisterChatFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&chatFlags.Query, "query", "q", "", "Single query (non-interactive mode)")
	cmd.Flags().StringVarP(&chatFlags.Resume, "resume", "r", "", "Resume a session by ID")
	cmd.Flags().BoolVarP(&chatFlags.Continue, "continue", "c", false, "Continue most recent session")
	cmd.Flags().StringVarP(&chatFlags.Model, "model", "m", "", "Override model from config")
	cmd.Flags().IntVar(&chatFlags.MaxTurns, "max-turns", 0, "Max agent turns per run (0=unlimited)")
}

func runChat(ctx context.Context, query, resume string, continueLast bool, modelOverride string, maxTurns int) error {
	// Load config.
	cfg, err := LoadConfig(ConfigFile())
	if err != nil {
		return err
	}

	if cfg.Provider == "" {
		fmt.Fprintln(os.Stderr, "  No provider configured. Run `deepai setup` first.")
		return fmt.Errorf("no provider configured")
	}

	// Resolve model.
	modelName := cfg.Model
	if modelOverride != "" {
		modelName = modelOverride
	}
	if modelName == "" {
		modelName = "default"
	}

	slog.Info("chat config",
		"provider", cfg.Provider,
		"model", modelName,
		"base_url", cfg.BaseURL,
		"context_window", cfg.ContextWindow,
		"database_url_set", cfg.DatabaseURL != "",
	)

	// Create LLM provider.
	provider, err := llm.NewProviderFromConfig(cfg.Provider, llm.ProviderConfig{
		BaseURL: cfg.BaseURL,
	})
	if err != nil {
		return fmt.Errorf("create provider %q: %w", cfg.Provider, err)
	}

	// Create sandbox.
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	// Create tool registry.
	registry := tools.NewRegistry()
	registerChatTools(registry, provider)

	// Load skills.
	skillReg := skill.NewRegistry()
	if err := skillReg.LoadAll(workDir, nil); err != nil {
		slog.Warn("skill load failed", "err", err)
	}
	if skillReg.Count() > 0 {
		if err := registry.Register(skill.SkillToolWithRegistry(skillReg)); err != nil {
			slog.Warn("register skill tool failed", "err", err)
		}
		slog.Info("loaded skills", "count", skillReg.Count(), "names", strings.Join(skillReg.AvailableNames(), ", "))
	}

	// Memory service. CLI defaults to SQLite at ~/.deepai/memory.db
	// when no DATABASE_URL is configured.
	var memService *memory.Service
	var memExtractor memory.Extractor
	var prefExtractor memory.Extractor
	memDBURL := cfg.DatabaseURL
	if memDBURL == "" {
		home, _ := os.UserHomeDir()
		memDBURL = home + "/.deepai/memory.db"
	}
	memStore, err := memory.OpenStore(ctx, memDBURL)
	if err != nil {
		slog.Warn("memory store init failed", "path", memDBURL, "err", err)
	} else {
		memService = memory.NewService(slog.Default(), memStore, nil)
		if err := memService.AutoMigrate(ctx); err != nil {
			slog.Warn("memory auto-migrate failed", "err", err)
		}
		memExtractor = memory.NewLLMClient(provider, modelName)
		prefExtractor = memory.NewPreferenceExtractor(provider, modelName)
		slog.Info("memory service enabled", "store", memDBURL)
	}

	// Load DEEPAI.md system prompts.
	var systemPrompt string
	for _, p := range []string{
		GlobalInstructions(),
		ProjectInstructions(workDir),
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			if systemPrompt != "" {
				systemPrompt += "\n\n"
			}
			systemPrompt += content
		}
	}

	// Build REPL config.
	replCfg := chat.ReplConfig{
		Provider:            cfg.Provider,
		Model:               modelName,
		LLMProvider:         provider,
		DatabaseURL:         cfg.DatabaseURL,
		ContextWindow:       cfg.ContextWindow,
		MaxTurns:            maxTurns,
		Query:               query,
		ResumeSession:       resume,
		ContinueLast:        continueLast,
		SystemPrompt:        systemPrompt,
		WorkDir:             workDir,
		ToolRegistry:        registry,
		SkillRegistry:       skillReg,
		MemoryService:       memService,
		MemoryExtractor:     memExtractor,
		PreferenceExtractor: prefExtractor,
	}

	repl, err := chat.NewRepl(replCfg)
	if err != nil {
		return err
	}

	return repl.Run(ctx)
}

func registerChatTools(registry *tools.Registry, provider llm.LLMProvider) {
	mustRegisterTool(registry, builtin.BashTool())
	mustRegisterTool(registry, clarification.AskClarificationTool(nil))

	// Subagent tools.
	subExecutor := agent.NewSubagentExecutor(provider, registry, nil)
	subPool := agent.NewSubagentPool(subExecutor, 1, 0)
	mustRegisterTool(registry, tools.TaskTool(subPool))
	mustRegisterTool(registry, tools.GitAutoCommitTool(provider))

	for _, tool := range builtin.FileTools() {
		mustRegisterTool(registry, tool)
	}
	for _, tool := range builtin.GitTools() {
		mustRegisterTool(registry, tool)
	}
	for _, tool := range builtin.WebTools() {
		mustRegisterTool(registry, tool)
	}
}

func mustRegisterTool(registry *tools.Registry, tool models.Tool) {
	if err := registry.Register(tool); err != nil {
		panic(fmt.Sprintf("register tool %s: %v", tool.Name, err))
	}
}
