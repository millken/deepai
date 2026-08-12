package commands

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/dnsoa/go/env"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/secret"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Config represents the deepai configuration stored in ~/.deepai/config.yaml.
// Secrets (API keys) are stored separately in ~/.deepai/.env.
type Config struct {
	Provider       string `yaml:"provider,omitempty"`
	Model          string `yaml:"model,omitempty"`
	DatabaseURL    string `yaml:"database_url,omitempty"`
	ContextWindow  int    `yaml:"context_window,omitempty"`
	BaseURL        string `yaml:"base_url,omitempty"`
	RequestTimeout int    `yaml:"request_timeout,omitempty"` // agent request timeout in minutes (default 30)
	// ReasoningEffort sets the default reasoning effort for models that support it
	// (e.g., Claude's "thinking" feature). Valid values: "low", "medium", "high", "disabled".
	// Empty means provider default. Model-level config overrides this.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	// Mode controls whether the agent stops to ask clarifying questions.
	// Empty or "interactive" (default): the agent may use ask_clarification to
	// block on user input. "autonomous": ask_clarification short-circuits to a
	// best-judgment response so unattended runs never block.
	Mode string `yaml:"mode,omitempty"`

	// TokenMetrics enables Phase 0 token measurement (JSONL records per turn and
	// per tool result). "1"/"true" writes to $TMPDIR/deepai-token-metrics.jsonl;
	// any other non-empty value is used as the output file path. Empty = off.
	// The DEEPAI_TOKEN_METRICS env var, when set, takes precedence.
	TokenMetrics string `yaml:"token_metrics,omitempty"`
	// TokenAging enables T1 tool-result aging (docs/spec/token-efficiency.md):
	// historical tool results in the outgoing prompt are compressed by age once
	// context pressure passes 40% of the window. The DEEPAI_TOKEN_AGING env var,
	// when set, takes precedence.
	TokenAging bool `yaml:"token_aging,omitempty"`

	// Models defines multiple named model entries for multi-model support.
	// Each entry binds an alias to a provider+model pair. When non-empty, the
	// /model command can switch between them and subagents can select per-task.
	// When empty, the top-level Provider/Model fields are used as the sole model.
	Models []llm.ModelDef `yaml:"models,omitempty"`
}

// applyTokenEfficiencyEnv bridges config.yaml token-efficiency settings to the
// environment variables read at the agent.New() chokepoint, so REPL agents and
// subagents pick them up uniformly. Explicit env values win over config.yaml.
func applyTokenEfficiencyEnv(cfg Config) {
	if v := strings.TrimSpace(cfg.TokenMetrics); v != "" && os.Getenv("DEEPAI_TOKEN_METRICS") == "" {
		os.Setenv("DEEPAI_TOKEN_METRICS", v)
	}
	if cfg.TokenAging && os.Getenv("DEEPAI_TOKEN_AGING") == "" {
		os.Setenv("DEEPAI_TOKEN_AGING", "1")
	}
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
			RunE:    runSetupProvider,
		},
		&cobra.Command{
			Use:   "model",
			Short: "Configure model name",
			Long: `Interactive wizard to pick or enter a model name for the configured provider.
The model name is written to ~/.deepai/config.yaml and used as the default
for every chat session unless overridden with -m.`,
			Example: "  deepai setup model",
			RunE:    runSetupModel,
		},
		&cobra.Command{
			Use:   "database",
			Short: "Configure external database URL",
			Long: `Optionally set an external PostgreSQL database URL for session and memory
storage. Leave blank to keep using the default local SQLite database at
~/.deepai/deepai.db.`,
			Example: "  deepai setup database",
			RunE:    runSetupDatabase,
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
	// The stored key is sealed on disk. Prefilling the form would mean
	// decrypting it back into memory for no benefit, so an empty answer
	// means "keep what is there".
	title := fmt.Sprintf("API key (%s)", info.envVar)
	if loadEnvValue(envPath, info.envVar) != "" {
		title += " — leave blank to keep the current key"
	}

	var apiKey string
	if err := huh.NewInput().
		Title(title).
		Value(&apiKey).
		EchoMode(huh.EchoModePassword).
		Run(); err != nil {
		return err
	}

	if apiKey != "" {
		sealed, err := secret.Seal(apiKey)
		if err != nil {
			return fmt.Errorf("seal API key: %w", err)
		}
		if err := saveEnvValue(envPath, info.envVar, sealed); err != nil {
			return fmt.Errorf("save .env: %w", err)
		}
		fmt.Printf("  Saved sealed API key to %s\n", envPath)
		if w := sealWarning(); w != "" {
			fmt.Println(w)
		}
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
	warnUnknownConfigKeys(path, data)
	return cfg, nil
}

// warnUnknownConfigKeys reports config keys the decoder did not recognize.
//
// A mistyped key is silently dropped by yaml.Unmarshal, and a silently dropped
// key means the setting the user wrote is simply not in effect — with no
// feedback anywhere. That is not hypothetical: `cotext_window: 1000000`
// (missing an "n") left the context window at the 192k default, which moved
// aging's 40% gate and compaction's 75% trigger about 5x earlier than intended
// and helped drive a read loop through a whole session before anyone noticed.
//
// Deliberately a warning, not an error: a config file written by a newer or
// older deepai must keep loading. Unrecognized keys are ignored exactly as
// before — the only change is that you hear about it.
func warnUnknownConfigKeys(path string, data []byte) {
	for _, key := range unknownConfigKeys(data) {
		msg := fmt.Sprintf("%s: unknown key %q was ignored — check for a typo", path, key.name)
		if key.line > 0 {
			msg = fmt.Sprintf("%s line %d: unknown key %q was ignored — check for a typo",
				path, key.line, key.name)
		}
		fmt.Fprintf(os.Stderr, "  config warning: %s\n", msg)
		slog.Warn("unknown config key ignored", "path", path, "key", key.name, "line", key.line)
	}
}

// configKeyIssue is one unrecognized key: its name and, when yaml reported it,
// the line it sits on.
type configKeyIssue struct {
	name string
	line int
}

// unknownFieldRE matches gopkg.in/yaml.v3's KnownFields complaint, e.g.
// `line 5: field cotext_window not found in type commands.Config`. The line
// prefix is absent for some documents, hence the optional group.
var unknownFieldRE = regexp.MustCompile(`(?:line (\d+): )?field (\S+) not found in type`)

// unknownConfigKeys re-decodes data in strict mode purely to collect the keys
// Config does not declare. Type errors (a string where an int belongs) are
// skipped: those are the business of the real Unmarshal in LoadConfig, which
// reports them as errors and whose verdict must not be second-guessed here.
func unknownConfigKeys(data []byte) []configKeyIssue {
	var probe Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(&probe)
	if err == nil {
		return nil
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return nil // EOF on an empty file, or a syntax error LoadConfig already surfaced
	}
	var out []configKeyIssue
	for _, e := range typeErr.Errors {
		m := unknownFieldRE.FindStringSubmatch(e)
		if m == nil {
			continue
		}
		line, _ := strconv.Atoi(m[1])
		out = append(out, configKeyIssue{name: m[2], line: line})
	}
	return out
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

// loadEnvValue reads one key from a .env file using the same parsing rules as
// goenv (env.Load): quotes are stripped, inline comments removed, the export
// prefix honored. This must agree with how root.go loads .env into the process
// environment, otherwise the value a CLI command sees here would differ from
// the value the provider receives at runtime -- which is how a key written as
// KEY="value" once got its quote characters sealed into the ciphertext.
func loadEnvValue(path, key string) string {
	m, err := env.ReadMap(path)
	if err != nil {
		return ""
	}
	return m[key]
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
	return writeEnvAtomic(path, []byte(content))
}

// writeEnvAtomic replaces path's contents without ever exposing a partial
// or world-readable file. os.CreateTemp creates at 0600, so the mode is
// right from creation rather than being widened and then narrowed. The temp
// file is made in the same directory because rename is only atomic within
// one filesystem.
func writeEnvAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".env-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	// A no-op once the rename below succeeds; on any earlier failure it
	// keeps a copy of the credentials from being left behind.
	defer os.Remove(tmp)

	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return os.Rename(tmp, path)
}

// sealWarning returns a message when this host cannot bind sealed keys to
// hardware, and "" when it can. Degrading is a silent loss of protection,
// so it must be stated rather than assumed.
func sealWarning() string {
	info := secret.Fingerprint()
	switch info.Mode {
	case secret.ModeHardware:
		return ""
	case secret.ModeInstall:
		return "  Note: no disk serial number is readable here, so the key is bound to\n" +
			"  this OS install rather than to hardware. Reinstalling the OS will\n" +
			"  require re-entering it."
	default:
		return "  Warning: no disk serial number and no OS machine ID are readable here\n" +
			"  (common on cloud instances and WSL2), so the key is obfuscated but NOT\n" +
			"  bound to this machine: anyone with the file and a deepai binary can\n" +
			"  read it. It is still safe from tools that merely read the file."
	}
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
