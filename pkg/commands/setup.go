package commands

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Config represents the deepai configuration stored in ~/.deepai/config.yaml.
// Secrets (API keys) are stored separately in ~/.deepai/.env.
type Config struct {
	Provider      string `yaml:"provider,omitempty"`
	Model         string `yaml:"model,omitempty"`
	DatabaseURL   string `yaml:"database_url,omitempty"`
	ContextWindow int    `yaml:"context_window,omitempty"`
	BaseURL       string `yaml:"base_url,omitempty"`
	RequestTimeout int    `yaml:"request_timeout,omitempty"` // agent request timeout in minutes (default 30)
	// Mode controls whether the agent stops to ask clarifying questions.
	// Empty or "interactive" (default): the agent may use ask_clarification to
	// block on user input. "autonomous": ask_clarification short-circuits to a
	// best-judgment response so unattended runs never block.
	Mode          string `yaml:"mode,omitempty"`
}

// IsAutonomous reports whether the configured mode skips user prompts.
func (c Config) IsAutonomous() bool {
	return strings.EqualFold(strings.TrimSpace(c.Mode), "autonomous")
}

// providerInfo maps provider names to their API key env var and common models.
var providerInfo = map[string]struct {
	envVar string
	models []string
}{
	"anthropic":     {"ANTHROPIC_API_KEY", []string{"claude-sonnet-4-20250514", "claude-haiku-4-5-20251001", "claude-opus-4-20250514"}},
	"openai":        {"OPENAI_API_KEY", []string{"gpt-4o", "gpt-4o-mini", "o3", "o4-mini"}},
	"openrouter":    {"OPENROUTER_API_KEY", []string{"anthropic/claude-sonnet-4-20250514", "openai/gpt-4o"}},
	"gemini":        {"GEMINI_API_KEY", []string{"gemini-2.5-pro", "gemini-2.5-flash"}},
	"deepseek":      {"DEEPSEEK_API_KEY", []string{"deepseek-chat", "deepseek-reasoner"}},
	"qwen":          {"QWEN_API_KEY", []string{"qwen-max", "qwen-plus", "qwen-turbo"}},
	"glm":           {"GLM_API_KEY", []string{"glm-4-plus", "glm-4-flash"}},
	"groq":          {"GROQ_API_KEY", []string{"llama-3.3-70b-versatile", "mixtral-8x7b-32768"}},
	"ollama":        {"OLLAMA_API_KEY", []string{"qwen3:32b", "deepseek-r1:32b"}},
	"bedrock":       {"BEDROCK_API_KEY", []string{"anthropic.claude-sonnet-4-20250514-v1:0"}},
	"openai-compat": {"OPENAI_API_KEY", []string{}},
}

const defaultDeepaiMD = `# DEEPAI.md

Project instructions for deepai agent.

## Guidelines
- Write clean, idiomatic code
- Follow existing project conventions
`

// ConfigPaths returns (deepaiDir, configPath, envPath).
func ConfigPaths() (string, string, string, error) {
	dir := Home()
	if dir == "" {
		return "", "", "", fmt.Errorf("cannot determine home directory")
	}
	return dir, ConfigFile(), EnvFile(), nil
}

func addSetup(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure deepai",
		RunE:  runSetup,
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "provider",
			Short: "Configure LLM provider and API key",
			Long: `Interactive wizard to select an LLM provider (e.g. anthropic, openai, deepseek)
and configure its API key. The key is saved to ~/.deepai/.env, the provider
name is written to ~/.deepai/config.yaml.`,
			Example: "  deepai setup provider",
			RunE: runSetupProvider,
		},
		&cobra.Command{
			Use:   "model",
			Short: "Configure model name",
			Long: `Interactive wizard to pick or enter a model name for the configured provider.
The model name is written to ~/.deepai/config.yaml and used as the default
for every chat session unless overridden with -m.`,
			Example: "  deepai setup model",
			RunE: runSetupModel,
		},
		&cobra.Command{
			Use:   "database",
			Short: "Configure external database URL",
			Long: `Optionally set an external PostgreSQL database URL for session and memory
storage. Leave blank to keep using the default local SQLite database at
~/.deepai/deepai.db.`,
			Example: "  deepai setup database",
			RunE: runSetupDatabase,
		},
	)

	topLevel.AddCommand(cmd)
}

// --- Full wizard ---

func runSetup(cmd *cobra.Command, args []string) error {
	deepaiDir, configPath, envPath, err := ConfigPaths()
	if err != nil {
		return err
	}

	if err := ensureDeepaiHome(deepaiDir); err != nil {
		return err
	}

	cfg, cfgErr := LoadConfig(configPath)
	if cfgErr != nil {
		fmt.Printf("  Warning: %v\n", cfgErr)
		fmt.Println("  Starting with fresh config.")
	}

	fmt.Println("=== deepai setup ===")
	fmt.Println()

	if err := setupProvider(&cfg, envPath); err != nil {
		return err
	}
	if err := setupModel(&cfg); err != nil {
		return err
	}
	if err := setupDatabase(&cfg); err != nil {
		return err
	}

	// Save config.
	if err := saveConfig(configPath, &cfg); err != nil {
		return err
	}

	// Create default DEEPAI.md if not exists.
	createDefaultDeepaiMD(deepaiDir)

	fmt.Println()
	fmt.Println("Setup complete! Config saved to", configPath)
	printSummary(&cfg)
	return nil
}

// --- Subcommand: provider ---

func runSetupProvider(cmd *cobra.Command, args []string) error {
	deepaiDir, configPath, envPath, err := ConfigPaths()
	if err != nil {
		return err
	}

	if err := ensureDeepaiHome(deepaiDir); err != nil {
		return err
	}

	cfg, cfgErr := LoadConfig(configPath)
	if cfgErr != nil {
		return cfgErr
	}

	if err := setupProvider(&cfg, envPath); err != nil {
		return err
	}
	if err := saveConfig(configPath, &cfg); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Provider saved:", cfg.Provider)
	return nil
}

// --- Subcommand: model ---

func runSetupModel(cmd *cobra.Command, args []string) error {
	deepaiDir, configPath, _, err := ConfigPaths()
	if err != nil {
		return err
	}

	if err := ensureDeepaiHome(deepaiDir); err != nil {
		return err
	}

	cfg, cfgErr := LoadConfig(configPath)
	if cfgErr != nil {
		return cfgErr
	}

	if err := setupModel(&cfg); err != nil {
		return err
	}
	if err := saveConfig(configPath, &cfg); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Model saved:", cfg.Model)
	return nil
}

// --- Subcommand: database ---

func runSetupDatabase(cmd *cobra.Command, args []string) error {
	deepaiDir, configPath, _, err := ConfigPaths()
	if err != nil {
		return err
	}

	if err := ensureDeepaiHome(deepaiDir); err != nil {
		return err
	}

	cfg, cfgErr := LoadConfig(configPath)
	if cfgErr != nil {
		return cfgErr
	}

	if err := setupDatabase(&cfg); err != nil {
		return err
	}
	if err := saveConfig(configPath, &cfg); err != nil {
		return err
	}

	fmt.Println()
	db := cfg.DatabaseURL
	if db == "" {
		db = "(disabled)"
	}
	fmt.Println("Database saved:", db)
	return nil
}

// --- Setup sections ---

func setupProvider(cfg *Config, envPath string) error {
	oldProvider := cfg.Provider
	provider := cfg.Provider
	providerOpts := huh.NewOptions[string]()
	for _, name := range providerNames() {
		providerOpts = append(providerOpts, huh.NewOption(name, name))
	}

	if err := huh.NewSelect[string]().
		Title("Select provider").
		Options(providerOpts...).
		Value(&provider).
		Run(); err != nil {
		return err
	}
	cfg.Provider = provider

	info := providerInfo[provider]
	// Read key for the NEW provider (may differ from old one).
	apiKey := loadEnvValue(envPath, info.envVar)
	// If provider changed and new provider has no key, clear default.
	if provider != oldProvider && apiKey == "" {
		apiKey = ""
	}

	if err := huh.NewInput().
		Title(fmt.Sprintf("API key (%s)", info.envVar)).
		Value(&apiKey).
		EchoMode(huh.EchoModePassword).
		Run(); err != nil {
		return err
	}

	// Save API key to .env immediately.
	if apiKey != "" {
		if err := saveEnvValue(envPath, info.envVar, apiKey); err != nil {
			return fmt.Errorf("save .env: %w", err)
		}
		fmt.Printf("  Saved API key to %s\n", envPath)
	}

	// Base URL (optional for any provider).
	if provider == "openai" || provider == "openai-compat" || provider == "anthropic" {
		if err := huh.NewInput().
			Title("Base URL (empty for default)").
			Value(&cfg.BaseURL).
			Run(); err != nil {
			return err
		}
	}

	return nil
}

func setupModel(cfg *Config) error {
	model := cfg.Model
	info := providerInfo[cfg.Provider]
	if model == "" && len(info.models) > 0 {
		model = info.models[0]
	}

	if err := huh.NewInput().
		Title("Model").
		Value(&model).
		Run(); err != nil {
		return err
	}
	cfg.Model = model
	return nil
}

func setupDatabase(cfg *Config) error {
	databaseURL := cfg.DatabaseURL

	if err := huh.NewInput().
		Title("Database URL (empty to skip memory)").
		Value(&databaseURL).
		Run(); err != nil {
		return err
	}
	cfg.DatabaseURL = databaseURL
	return nil
}

// --- Helpers ---

func ensureDeepaiHome(dir string) error {
	if err := EnsureHome(); err != nil {
		return err
	}
	fmt.Printf("  Directory %s ready\n", dir)
	return nil
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	slog.Debug("saving config", "path", path)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Printf("  Saved %s\n", path)
	return nil
}

func loadEnvValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if val, ok := strings.CutPrefix(line, key+"="); ok {
			return val
		}
	}
	return ""
}

func saveEnvValue(path, key, value string) error {
	var lines []string
	data, err := os.ReadFile(path)
	if err == nil {
		lines = strings.Split(string(data), "\n")
	}

	prefix := key + "="
	replaced := false
	for i, line := range lines {
		if _, ok := strings.CutPrefix(line, prefix); ok {
			lines[i] = prefix + value
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, prefix+value)
	}

	content := strings.Join(lines, "\n")
	return os.WriteFile(path, []byte(content), 0600)
}

func createDefaultDeepaiMD(dir string) {
	p := filepath.Join(dir, "DEEPAI.md")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.WriteFile(p, []byte(defaultDeepaiMD), 0644); err != nil {
			slog.Warn("failed to create DEEPAI.md", "err", err)
		} else {
			fmt.Printf("  Created %s\n", p)
		}
	}
}

func printSummary(cfg *Config) {
	fmt.Println()
	fmt.Println("  Provider:", cfg.Provider)
	fmt.Println("  Model:   ", cfg.Model)
	db := cfg.DatabaseURL
	if db == "" {
		db = "(none)"
	}
	fmt.Println("  Database:", db)
	if cfg.BaseURL != "" {
		fmt.Println("  Base URL:", cfg.BaseURL)
	}
}

func providerNames() []string {
	names := make([]string, 0, len(providerInfo))
	for k := range providerInfo {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
