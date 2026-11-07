package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dnsoa/go/env"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/claudeplugin"
	"github.com/millken/deepai/pkg/mcp"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
)

// BotOptions selects what NewBotRuntime builds.
type BotOptions struct {
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

// BotRuntime is the agent side of deepai-bot, configured from
// ~/.deepai/config.yaml exactly like the interactive REPL: same models,
// tools, skills, plugins, MCP servers and DEEPAI.md instructions. Sessions
// and memory live in BotDB, not in the CLI's database.
type BotRuntime struct {
	Runtime *bot.AgentRuntime
	Store   bot.Store
	WorkDir string
	// DB is the database path, empty when ephemeral.
	DB string
	// Close releases MCP servers and the database. Call it after the
	// bot.Manager has been closed.
	Close func()
}

// BotHome is the directory deepai-bot keeps its database, pid file, log and
// token in.
func BotHome() string {
	return filepath.Join(Home(), "bot")
}

// BotDB is deepai-bot's own SQLite database (sessions and long-term memory).
// It is separate from the CLI's DBFile.
func BotDB() string {
	return filepath.Join(BotHome(), "bot.db")
}

// LoadEnvFile loads ~/.deepai/.env so providers can read API keys, as the
// deepai root command does before every subcommand.
func LoadEnvFile() {
	if err := env.Load(EnvFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to load .env", "path", EnvFile(), "err", err)
	}
}

// BuildVersion reports the build version shared by deepai and deepai-bot.
func BuildVersion() string { return version() }

// NewBotRuntime builds the bot's agent runtime. ctx bounds MCP server
// lifetimes and must outlive the server.
func NewBotRuntime(ctx context.Context, opts BotOptions) (_ *BotRuntime, err error) {
	cfg, err := LoadConfig(ConfigFile())
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
	for _, p := range []string{GlobalInstructions(), ProjectInstructions(workDir)} {
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

	registerChatTools(registry, modelRegistry, defaultProvider, cfg.IsAutonomous(), workDir, cfg.ContextWindow, cfg.Temperature, pluginAgentDirs, agentOpts, skillReg, instructions, resolveSubagentTimeout(cfg.SubagentTimeoutMinutes))
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
		SandboxBaseDir:         filepath.Join(BotHome(), "sandbox"),
		ContextWindow:          cfg.ContextWindow,
		ReasoningEffort:        cfg.ReasoningEffort,
		Temperature:            cfg.Temperature,
		MaxToolCalls:           opts.MaxToolCalls,
		MaxTokens:              subagentMaxTokens(),
		RequestTimeout:         resolveRequestTimeout(cfg.RequestTimeout),
		AgentCatalog:           agentCatalog,
		KeepFullTurns:          agent.ResolveKeepFullTurns(cfg.SessionKeepTurns),
		SessionSummaryDisabled: cfg.SessionSummaryDisabled,
		SessionSummaryModel:    strings.TrimSpace(cfg.SessionSummaryModel),
		ToolOffloadBytes:       cfg.ToolOffloadBytes,
		Version:                version(),
	}

	var store bot.Store
	var dbPath string
	if opts.Ephemeral {
		store = bot.NewMemoryStore()
	} else {
		if err := EnsureHome(); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(BotHome(), 0o700); err != nil {
			return nil, err
		}
		// Every deepai-bot process (one per workdir) shares bot.db, so
		// _txlock=immediate and busy_timeout matter as they do for the CLI.
		dbPath = BotDB()
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
	return &BotRuntime{Runtime: rt, Store: store, WorkDir: workDir, DB: dbPath, Close: closeAll}, nil
}
