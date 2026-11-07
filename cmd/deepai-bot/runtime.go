package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"runtime/debug"
	"time"

	"github.com/dnsoa/go/env"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/claudeplugin"
	"github.com/millken/deepai/pkg/commands"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/mcp"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
	_ "modernc.org/sqlite"
)

// botOptions selects what newBotRuntime builds.
type botOptions struct {
	// WorkDir is the directory the bot's tools operate in (default: cwd).
	WorkDir string
	// Model overrides the configured default model alias (like -m).
	Model string
	// MaxToolCalls caps executed tool calls per turn (0 = unlimited).
	MaxToolCalls int
	// Ephemeral keeps sessions in memory instead of bot.db, and disables
	// long-term memory.
	Ephemeral bool
}

// botRuntime is the agent side of deepai-bot, configured from
// ~/.deepai/config.yaml like the interactive REPL: same models, tools,
// skills, plugins, MCP servers and DEEPAI.md instructions. Sessions and
// memory live in botDB, not in the CLI's database.
type botRuntime struct {
	Runtime *bot.AgentRuntime
	Store   bot.Store
	WorkDir string
	// DB is the database path, empty when ephemeral.
	DB string
	// Close releases MCP servers and the database. Call it after the
	// bot.Manager has been closed.
	Close func()
}

// botHome is the directory deepai-bot keeps its database, pid file, log and
// token in.
func botHome() string {
	return filepath.Join(commands.Home(), "bot")
}

// botDB is deepai-bot's own SQLite database (sessions and long-term memory).
// It is separate from the CLI's commands.DBFile.
func botDB() string {
	return filepath.Join(botHome(), "bot.db")
}

// loadEnvFile loads ~/.deepai/.env so providers can read API keys, as the
// deepai root command does before every subcommand.
func loadEnvFile() {
	if err := env.Load(commands.EnvFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to load .env", "path", commands.EnvFile(), "err", err)
	}
}

// buildVersion reports the build version. Release builds stamp
// commands.Version for both binaries.
func buildVersion() string {
	if commands.Version != "" {
		return commands.Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}

// newBotRuntime builds the bot's agent runtime. ctx bounds MCP server
// lifetimes and must outlive the server.
func newBotRuntime(ctx context.Context, opts botOptions) (_ *botRuntime, err error) {
	cfg, err := commands.LoadConfig(commands.ConfigFile())
	if err != nil {
		return nil, err
	}
	applyTokenEfficiencyEnv(cfg)
	if cfg.Provider == "" && len(cfg.Models) == 0 {
		return nil, fmt.Errorf("no provider configured; run `deepai setup` first")
	}

	modelRegistry, err := buildModelRegistry(cfg, opts.Model)
	if err != nil {
		return nil, err
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = 192000
	}
	defaultProvider, defaultModelName, err := modelRegistry.ProviderFor(modelRegistry.DefaultName())
	if err != nil {
		return nil, fmt.Errorf("resolve default model: %w", err)
	}

	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		if workDir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	if workDir, err = filepath.Abs(workDir); err != nil {
		return nil, err
	}
	// The session store records cwd symlink-resolved; scoping compares
	// against this exact string.
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}

	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()

	registry := tools.NewRegistry()
	plugins, pluginProblems := claudeplugin.Discover(workDir)
	var pluginRoots, pluginAgentDirs []string
	pluginServers := map[string]mcp.ServerConfig{}
	for _, p := range plugins {
		pluginRoots = append(pluginRoots, p.SkillRoot())
		pluginAgentDirs = append(pluginAgentDirs, p.AgentDir())
		servers, mcpProblem := p.MCPServers()
		if mcpProblem != "" {
			pluginProblems = append(pluginProblems, fmt.Sprintf("%s: %s", p.Name, mcpProblem))
			continue
		}
		for name, sc := range servers {
			pluginServers[name] = sc
		}
	}
	for _, p := range pluginProblems {
		slog.Warn("plugin load issue", "err", p)
	}

	agentCatalog, agentProblems := agent.EnumerateAgentsReported(workDir, pluginAgentDirs)
	for _, p := range agentProblems {
		slog.Warn("agent config load issue", "err", p)
	}
	agentOpts := make([]tools.AgentOption, 0, len(agentCatalog))
	for _, a := range agentCatalog {
		agentOpts = append(agentOpts, tools.AgentOption{Type: string(a.Type), Description: a.Description})
	}

	skillReg := skill.NewRegistry()
	for _, w := range skillReg.LoadAllReported(workDir, pluginRoots) {
		slog.Warn("skill load issue", "source", w.Source, "dir", w.Dir, "err", w.Msg)
	}

	var instructions string
	for _, p := range []string{commands.GlobalInstructions(), commands.ProjectInstructions(workDir)} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += content
		}
	}

	if err := registerTools(registry, modelRegistry, defaultProvider, cfg, workDir, pluginAgentDirs, agentOpts, skillReg, instructions); err != nil {
		return nil, err
	}
	if skillReg.Count() > 0 {
		if err := registry.Register(skill.SkillToolWithRegistry(skillReg)); err != nil {
			slog.Warn("register skill tool failed", "err", err)
		}
	}

	mcpClosers, mcpReport := mcp.LoadWithServers(ctx, registry, workDir, pluginServers)
	closers = append(closers, func() {
		for _, c := range mcpClosers {
			c()
		}
	})
	if mcpReport != "" {
		slog.Info("mcp", "report", mcpReport)
	}

	systemPrompt := skillReg.Descriptions()
	if instructions != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += instructions
	}

	rtCfg := bot.AgentRuntimeConfig{
		Models:                 modelRegistry,
		Tools:                  registry,
		SystemPrompt:           systemPrompt,
		WorkDir:                workDir,
		SandboxBaseDir:         filepath.Join(botHome(), "sandbox"),
		ContextWindow:          cfg.ContextWindow,
		ReasoningEffort:        cfg.ReasoningEffort,
		Temperature:            cfg.Temperature,
		MaxToolCalls:           opts.MaxToolCalls,
		MaxTokens:              maxOutputTokens(),
		RequestTimeout:         time.Duration(max(cfg.RequestTimeout, 0)) * time.Minute,
		AgentCatalog:           agentCatalog,
		KeepFullTurns:          agent.ResolveKeepFullTurns(cfg.SessionKeepTurns),
		SessionSummaryDisabled: cfg.SessionSummaryDisabled,
		SessionSummaryModel:    strings.TrimSpace(cfg.SessionSummaryModel),
		ToolOffloadBytes:       cfg.ToolOffloadBytes,
		Version:                buildVersion(),
	}

	var store bot.Store
	var dbPath string
	if opts.Ephemeral {
		store = bot.NewMemoryStore()
	} else {
		if err := commands.EnsureHome(); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(botHome(), 0o700); err != nil {
			return nil, err
		}
		// Every deepai-bot process (one per workdir) shares bot.db, so
		// _txlock=immediate and busy_timeout matter as they do for the CLI.
		dbPath = botDB()
		dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		closers = append(closers, func() { _ = db.Close() })
		sessStore := newBotSessionStore(db)
		if err := sessStore.Migrate(); err != nil {
			return nil, fmt.Errorf("migrate session schema: %w", err)
		}
		store = sessStore

		memService := memory.NewService(slog.Default(), memory.NewSQLiteStoreFromDB(db), nil)
		if err := memService.AutoMigrate(ctx); err != nil {
			slog.Warn("memory auto-migrate failed", "err", err)
		}
		memClient := memory.NewLLMClient(defaultProvider, defaultModelName)
		memService.WithReviewer(memClient)
		rtCfg.MemoryService = memService
		rtCfg.MemoryExtractor = memClient
	}

	rt, err := bot.NewAgentRuntime(rtCfg)
	if err != nil {
		return nil, err
	}
	return &botRuntime{Runtime: rt, Store: store, WorkDir: workDir, DB: dbPath, Close: closeAll}, nil
}

// The helpers below mirror unexported counterparts in pkg/commands (the
// REPL's setup), so deepai-bot gets the same models and tool set without
// that package exporting its internals. Keep them in step with
// applyTokenEfficiencyEnv, buildModelRegistry/resolveDefaultAlias and
// registerChatTools in pkg/commands.

// applyTokenEfficiencyEnv bridges config.yaml token-efficiency settings to
// the env vars agent.New reads. Explicit env values win.
func applyTokenEfficiencyEnv(cfg commands.Config) {
	if v := strings.TrimSpace(cfg.TokenMetrics); v != "" && os.Getenv("DEEPAI_TOKEN_METRICS") == "" {
		os.Setenv("DEEPAI_TOKEN_METRICS", v)
	}
	if cfg.TokenAging && os.Getenv("DEEPAI_TOKEN_AGING") == "" {
		os.Setenv("DEEPAI_TOKEN_AGING", "1")
	}
}

// buildModelRegistry: an explicit models list takes precedence; otherwise
// the top-level provider/model. modelOverride is -m.
func buildModelRegistry(cfg commands.Config, modelOverride string) (*llm.ModelRegistry, error) {
	if len(cfg.Models) == 0 {
		name := cfg.Model
		if modelOverride != "" {
			name = modelOverride
		}
		if name == "" {
			name = "default"
		}
		return llm.NewSingleModelRegistry(cfg.Provider, name, cfg.BaseURL), nil
	}
	defaultName := resolveDefaultAlias(cfg.Models, modelOverride, cfg.Model)
	reg, err := llm.NewModelRegistry(cfg.Models, defaultName)
	if err != nil {
		return nil, fmt.Errorf("build model registry: %w", err)
	}
	if modelOverride == "" || reg.Has(modelOverride) {
		return reg, nil
	}
	// A bare -m model name not in the registry runs under the default
	// entry's provider.
	def, _ := reg.Resolve(defaultName)
	provider := def.Provider
	if provider == "" {
		provider = cfg.Provider
	}
	reg, err = llm.NewModelRegistry(append(cfg.Models, llm.ModelDef{
		Name: modelOverride, Provider: provider, Model: modelOverride, BaseURL: cfg.BaseURL,
	}), modelOverride)
	if err != nil {
		return nil, fmt.Errorf("build model registry with -m override: %w", err)
	}
	return reg, nil
}

// resolveDefaultAlias accepts an alias, a bare model name or a
// provider/model ref; "" lets the registry pick its first entry.
func resolveDefaultAlias(defs []llm.ModelDef, modelOverride, configModel string) string {
	for _, c := range []string{modelOverride, configModel} {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		for _, d := range defs {
			if strings.EqualFold(d.Name, c) {
				return d.Name
			}
		}
		for _, d := range defs {
			if d.Model == c {
				return d.Name
			}
		}
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

func maxOutputTokens() *int {
	n := agent.ResolveMaxOutputTokens()
	return &n
}

// registerTools registers the REPL's built-in tool set.
func registerTools(registry *tools.Registry, modelReg *llm.ModelRegistry, defaultProvider llm.LLMProvider, cfg commands.Config, workDir string, pluginAgentDirs []string, agentOpts []tools.AgentOption, skillReg *skill.Registry, instructions string) error {
	subExecutor := agent.NewSubagentExecutor(modelReg, registry, nil).
		WithWorkDir(workDir).
		WithContextWindow(cfg.ContextWindow).
		WithMaxTokens(maxOutputTokens()).
		WithTemperature(cfg.Temperature).
		WithPluginAgentDirs(pluginAgentDirs).
		WithSkillRegistry(skillReg).
		WithInstructions(instructions)
	subagentTimeout := time.Duration(max(cfg.SubagentTimeoutMinutes, 0)) * time.Minute

	all := []models.Tool{
		builtin.BashTool(),
		builtin.CIWaitTool(),
		builtin.FigmaTool(),
		clarification.AskClarificationToolWithMode(cfg.IsAutonomous()),
		tools.TaskTool(agent.NewSubagentPool(subExecutor, subagentTimeout), agentOpts),
		tools.GitAutoCommitTool(defaultProvider),
	}
	all = append(all, builtin.FileTools()...)
	all = append(all, builtin.RecallOutputTool(agent.DefaultOffloadDir()))
	all = append(all, builtin.WebTools()...)
	all = append(all, builtin.DocxTools()...)
	all = append(all, builtin.TodoWriteTool())
	for _, t := range all {
		if err := registry.Register(t); err != nil {
			return fmt.Errorf("register tool %s: %w", t.Name, err)
		}
	}
	return nil
}
