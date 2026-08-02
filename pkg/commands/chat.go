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
	"github.com/millken/deepai/pkg/claudeplugin"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/mcp"
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

	// Bridge config.yaml token-efficiency settings (token_metrics/token_aging)
	// to the env vars agent.New() reads, so they persist without shell exports.
	applyTokenEfficiencyEnv(cfg)

	if cfg.Provider == "" && len(cfg.Models) == 0 {
		fmt.Fprintln(os.Stderr, "  No provider configured. Run `deepai setup` first.")
		return fmt.Errorf("no provider configured")
	}

	// Build the model registry: explicit multi-model config takes precedence;
	// otherwise fall back to the top-level provider/model (backward compat).
	var modelRegistry *llm.ModelRegistry
	if len(cfg.Models) > 0 {
		// Resolve the default model alias from -m override or config Model.
		// Accept alias name, bare model name, or provider/model ref.
		defaultName := resolveDefaultAlias(cfg.Models, modelOverride, cfg.Model)
		modelRegistry, err = llm.NewModelRegistry(cfg.Models, defaultName)
		if err != nil {
			return fmt.Errorf("build model registry: %w", err)
		}

		// If -m specified a bare model name not in the registry, inject it as a
		// temporary entry under the top-level provider so the session can use it.
		if modelOverride != "" && !modelRegistry.Has(modelOverride) {
			def, _ := modelRegistry.Resolve(defaultName)
			provider := def.Provider
			if provider == "" {
				provider = cfg.Provider
			}
			tmpReg, err2 := llm.NewModelRegistry(append(cfg.Models, llm.ModelDef{
				Name: modelOverride, Provider: provider, Model: modelOverride, BaseURL: cfg.BaseURL,
			}), modelOverride)
			if err2 != nil {
				return fmt.Errorf("build model registry with -m override: %w", err2)
			}
			modelRegistry = tmpReg
		}
	} else {
		// Backward compat: single provider/model from top-level config.
		modelName := cfg.Model
		if modelOverride != "" {
			modelName = modelOverride
		}
		if modelName == "" {
			modelName = "default"
		}
		modelRegistry = llm.NewSingleModelRegistry(cfg.Provider, modelName, cfg.BaseURL)
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
		"models", len(modelRegistry.List()),
		"default_model", modelRegistry.DefaultName(),
		"context_window", cfg.ContextWindow,
		"database_url_set", cfg.DatabaseURL != "",
	)

	// Get the default provider for tools that need a provider at registration
	// time (git_auto_commit) and for memory extractors.
	defaultProvider, defaultModelName, err := modelRegistry.ProviderFor(modelRegistry.DefaultName())
	if err != nil {
		return fmt.Errorf("resolve default model: %w", err)
	}

	// Create sandbox.
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	// Create tool registry.
	registry := tools.NewRegistry()

	// Discover Claude plugins early — the agent catalog feeds the task tool's
	// advertised agent_type list. claudeplugin owns discovery/parsing.
	plugins, pluginProblems := claudeplugin.Discover(workDir)
	var pluginRoots, pluginAgentDirs []string
	var pluginCommandDirs []chat.PluginCommandDir
	pluginServers := map[string]mcp.ServerConfig{}
	for _, p := range plugins {
		pluginRoots = append(pluginRoots, p.SkillRoot())
		pluginAgentDirs = append(pluginAgentDirs, p.AgentDir())
		pluginCommandDirs = append(pluginCommandDirs, chat.PluginCommandDir{Plugin: p.Name, Dir: p.CommandDir()})
		servers, mcpProblem := p.MCPServers()
		if mcpProblem != "" {
			pluginProblems = append(pluginProblems, fmt.Sprintf("%s: %s", p.Name, mcpProblem))
			continue
		}
		for name, sc := range servers {
			pluginServers[name] = sc
		}
	}

	// Load file-based slash commands (user + project + plugin).
	commands, cmdProblems := chat.LoadCommands(workDir, pluginCommandDirs)
	pluginProblems = append(pluginProblems, cmdProblems...)

	// Enumerate advertised agents (project + plugin + builtin). The same
	// pluginAgentDirs slice is handed to the executor so advertising and
	// execution agree on each type's source.
	agentCatalog := agent.EnumerateAgents(workDir, pluginAgentDirs)
	agentOpts := make([]tools.AgentOption, 0, len(agentCatalog))
	for _, a := range agentCatalog {
		agentOpts = append(agentOpts, tools.AgentOption{Type: string(a.Type), Description: a.Description})
	}
	registerChatTools(registry, modelRegistry, defaultProvider, cfg.IsAutonomous(), workDir, cfg.ContextWindow, pluginAgentDirs, agentOpts)
	if cfg.IsAutonomous() {
		slog.Info("autonomous mode enabled: ask_clarification will not block")
	}

	// Load skills (plugin roots included; LoadAllReported appends /skills itself).
	// Warnings are surfaced below for plugin-source dirs.
	skillReg := skill.NewRegistry()
	skillWarnings := skillReg.LoadAllReported(workDir, pluginRoots)
	for _, w := range skillWarnings {
		slog.Warn("skill load issue", "source", w.Source, "dir", w.Dir, "err", w.Msg)
	}
	if skillReg.Count() > 0 {
		if err := registry.Register(skill.SkillToolWithRegistry(skillReg)); err != nil {
			slog.Warn("register skill tool failed", "err", err)
		}
		slog.Debug("loaded skills", "count", skillReg.Count(), "names", strings.Join(skillReg.AvailableNames(), ", "))
	}

	// Load MCP servers: disk config (<workdir>/.mcp.json, ~/.deepai/mcp.json)
	// plus plugin-bundled servers, merged. ctx is the session ctx — it is bound
	// to each server's lifetime, so it must outlive the REPL; closers tear
	// clients down at session end.
	mcpClosers, mcpReport := mcp.LoadWithServers(ctx, registry, workDir, pluginServers)
	defer func() {
		for _, closeFn := range mcpClosers {
			closeFn()
		}
	}()

	// Combine the startup report: plugin/skill problems first, then MCP summary.
	var reportParts []string
	for _, pp := range pluginProblems {
		reportParts = append(reportParts, "plugin "+pp)
	}
	for _, w := range skillWarnings {
		if w.Source == "plugin" {
			reportParts = append(reportParts, "plugin skill "+w.Dir+": "+w.Msg)
		}
	}
	if mcpReport != "" {
		reportParts = append(reportParts, mcpReport)
	}
	startupReport := strings.Join(reportParts, ", ")

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
	memExtractor = memory.NewLLMClient(defaultProvider, defaultModelName)
	prefExtractor = memory.NewPreferenceExtractor(defaultProvider, defaultModelName)
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
		ModelRegistry:       modelRegistry,
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
		MCPReport:           startupReport,
		Commands:            commands,
	}

	repl, err := chat.NewRepl(replCfg)
	if err != nil {
		return err
	}

	return repl.Run(ctx)
}

func registerChatTools(registry *tools.Registry, modelRegistry *llm.ModelRegistry, defaultProvider llm.LLMProvider, autonomous bool, workDir string, contextWindow int, pluginAgentDirs []string, agentOpts []tools.AgentOption) {
	mustRegisterTool(registry, builtin.BashTool())
	mustRegisterTool(registry, clarification.AskClarificationToolWithMode(nil, autonomous))

	// Subagent tools. pluginAgentDirs is the same slice EnumerateAgents used, so
	// advertised agents resolve to the same source at execution time.
	subMaxTokens := 16384 // larger than provider default (8192) to avoid truncating big tool args
	subExecutor := agent.NewSubagentExecutor(modelRegistry, registry, nil).
		WithWorkDir(workDir).
		WithContextWindow(contextWindow).
		WithMaxTokens(&subMaxTokens).
		WithPluginAgentDirs(pluginAgentDirs)
	subPool := agent.NewSubagentPool(subExecutor, 4, 15*time.Minute)
	mustRegisterTool(registry, tools.TaskTool(subPool, agentOpts))
	mustRegisterTool(registry, tools.GitAutoCommitTool(defaultProvider))

	for _, tool := range builtin.FileTools() {
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

// resolveDefaultAlias resolves the default model alias from -m override or
// config top-level Model. Accepts: alias name, bare model name, or
// provider/model ref. Returns "" to let the registry pick the first entry.
func resolveDefaultAlias(defs []llm.ModelDef, modelOverride, configModel string) string {
	candidates := []string{modelOverride, configModel}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// Exact alias match (case-insensitive).
		for _, d := range defs {
			if strings.EqualFold(d.Name, c) {
				return d.Name
			}
		}
		// Bare model name match.
		for _, d := range defs {
			if d.Model == c {
				return d.Name
			}
		}
		// provider/model ref match.
		if parts := strings.SplitN(c, "/", 2); len(parts) == 2 {
			for _, d := range defs {
				if strings.EqualFold(d.Provider, parts[0]) && d.Model == parts[1] {
					return d.Name
				}
			}
		}
	}
	return ""
}

func mustRegisterTool(registry *tools.Registry, tool models.Tool) {
	if err := registry.Register(tool); err != nil {
		panic(fmt.Sprintf("register tool %s: %v", tool.Name, err))
	}
}
