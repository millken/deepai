package commands

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

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
	_ "modernc.org/sqlite"
)

// Chat flags shared between root and chat subcommand.
var chatFlags struct {
	Query    string
	Resume   string
	Continue bool
	Model    string
	MaxTurns int
}

// resumePickerSentinel is the value assigned to -r when used without an argument.
const resumePickerSentinel = "__picker__"

func registerResumeFlag(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&chatFlags.Resume, "resume", "r", "", "Resume a session by ID or title (no arg: interactive picker)")
	if f := cmd.Flags().Lookup("resume"); f != nil {
		f.NoOptDefVal = resumePickerSentinel
	}
}

func addChat(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:    "chat",
		Short:  "Start an interactive chat session",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChat(cmd.Context(), chatFlags.Query, chatFlags.Resume, chatFlags.Continue, chatFlags.Model, chatFlags.MaxTurns)
		},
	}

	cmd.Flags().StringVarP(&chatFlags.Query, "query", "q", "", "Single query (non-interactive mode)")
	registerResumeFlag(cmd)
	cmd.Flags().BoolVarP(&chatFlags.Continue, "continue", "c", false, "Continue most recent session")
	cmd.Flags().StringVarP(&chatFlags.Model, "model", "m", "", "Override model from config")
	cmd.Flags().IntVar(&chatFlags.MaxTurns, "max-turns", 0, "Max agent turns per run (0=unlimited)")

	topLevel.AddCommand(cmd)
}

// RegisterChatFlags adds chat flags to a command (used by root to support bare `deepai -q`).
func RegisterChatFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&chatFlags.Query, "query", "q", "", "Single query (non-interactive mode)")
	registerResumeFlag(cmd)
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

	// Safety floor: when the user has not configured a context window, the
	// agent's proactive compaction is disabled (react.go gates on >0) and a
	// long session can grow runMessages until the model itself rejects the
	// request. 192k just enables the 75%-threshold compaction loop for the
	// common large-context tier; smaller-window providers are still covered
	// because the estimate now anchors to the provider's reported token count
	// and the reactive overflow backstop retries with compaction. Users who
	// know their model's exact window can override via config.yaml.
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = 192000
	}

	slog.Debug("chat config",
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
	registerChatTools(registry, provider, cfg.IsAutonomous(), workDir)
	if cfg.IsAutonomous() {
		slog.Info("autonomous mode enabled: ask_clarification will not block")
	}

	// Load skills.
	skillReg := skill.NewRegistry()
	if err := skillReg.LoadAll(workDir, nil); err != nil {
		slog.Warn("skill load failed", "err", err)
	}
	if skillReg.Count() > 0 {
		if err := registry.Register(skill.SkillToolWithRegistry(skillReg)); err != nil {
			slog.Warn("register skill tool failed", "err", err)
		}
		slog.Debug("loaded skills", "count", skillReg.Count(), "names", strings.Join(skillReg.AvailableNames(), ", "))
	}

	// Open unified SQLite database.
	dbPath := DBFile()
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	sessStore := chat.NewSQLiteSessionStoreFromDB(db)
	if err := sessStore.Migrate(); err != nil {
		return fmt.Errorf("migrate session schema: %w", err)
	}

	// Memory service: reuse the same DB.
	var memService *memory.Service
	var memExtractor memory.Extractor
	var prefExtractor memory.Extractor
	memStore := memory.NewSQLiteStoreFromDB(db)
	memService = memory.NewService(slog.Default(), memStore, nil)
	if err := memService.AutoMigrate(ctx); err != nil {
		slog.Warn("memory auto-migrate failed", "err", err)
	}
	memExtractor = memory.NewLLMClient(provider, modelName)
	prefExtractor = memory.NewPreferenceExtractor(provider, modelName)
	slog.Debug("memory service enabled", "store", dbPath)

	// Handle -r with no argument: interactive picker.
	if resume == resumePickerSentinel {
		sess, err := chat.PickSession(sessStore)
		if err != nil {
			return err
		}
		resume = sess.ID
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
		RequestTimeout:      resolveRequestTimeout(cfg.RequestTimeout),
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
		SessionRepo:         sessStore,
		InputHistoryFile:    InputHistoryFile(),
		SandboxBaseDir:      SandboxDir(),
	}

	repl, err := chat.NewRepl(replCfg)
	if err != nil {
		return err
	}

	return repl.Run(ctx)
}

func registerChatTools(registry *tools.Registry, provider llm.LLMProvider, autonomous bool, workDir string) {
	mustRegisterTool(registry, builtin.BashTool())
	mustRegisterTool(registry, clarification.AskClarificationToolWithMode(nil, autonomous))

	// Subagent tools.
	subExecutor := agent.NewSubagentExecutor(provider, registry, nil)
	subPool := agent.NewSubagentPool(subExecutor, 4, 0)
	mustRegisterTool(registry, tools.TaskTool(subPool))
	mustRegisterTool(registry, tools.ImplementTaskTool(subPool, workDir))
	mustRegisterTool(registry, tools.DesignTaskTool(subPool))
	mustRegisterTool(registry, tools.BuildTaskTool(subPool, workDir))
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

// resolveRequestTimeout converts a config value (in minutes) to a duration.
// 0 means unlimited — the agent run is bounded only by context cancellation.
func resolveRequestTimeout(minutes int) time.Duration {
	if minutes > 0 {
		return time.Duration(minutes) * time.Minute
	}
	return 0
}

func mustRegisterTool(registry *tools.Registry, tool models.Tool) {
	if err := registry.Register(tool); err != nil {
		panic(fmt.Sprintf("register tool %s: %v", tool.Name, err))
	}
}
