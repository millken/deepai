package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/llm"
)

func TestApplyTokenEfficiencyEnv_SetsFromConfig(t *testing.T) {
	t.Setenv("DEEPAI_TOKEN_METRICS", "")
	t.Setenv("DEEPAI_TOKEN_AGING", "")
	os.Unsetenv("DEEPAI_TOKEN_METRICS") // t.Setenv registered restore; clear for the test
	os.Unsetenv("DEEPAI_TOKEN_AGING")

	applyTokenEfficiencyEnv(Config{TokenMetrics: "/tmp/m.jsonl", TokenAging: true})

	if got := os.Getenv("DEEPAI_TOKEN_METRICS"); got != "/tmp/m.jsonl" {
		t.Errorf("DEEPAI_TOKEN_METRICS = %q, want /tmp/m.jsonl", got)
	}
	if got := os.Getenv("DEEPAI_TOKEN_AGING"); got != "1" {
		t.Errorf("DEEPAI_TOKEN_AGING = %q, want 1", got)
	}
}

func TestApplyTokenEfficiencyEnv_EnvWinsOverConfig(t *testing.T) {
	t.Setenv("DEEPAI_TOKEN_METRICS", "/from-env.jsonl")
	t.Setenv("DEEPAI_TOKEN_AGING", "0")

	applyTokenEfficiencyEnv(Config{TokenMetrics: "/from-config.jsonl", TokenAging: true})

	if got := os.Getenv("DEEPAI_TOKEN_METRICS"); got != "/from-env.jsonl" {
		t.Errorf("env should win: DEEPAI_TOKEN_METRICS = %q", got)
	}
	if got := os.Getenv("DEEPAI_TOKEN_AGING"); got != "0" {
		t.Errorf("env should win: DEEPAI_TOKEN_AGING = %q", got)
	}
}

func TestApplyTokenEfficiencyEnv_NoopWhenUnconfigured(t *testing.T) {
	t.Setenv("DEEPAI_TOKEN_METRICS", "")
	t.Setenv("DEEPAI_TOKEN_AGING", "")
	os.Unsetenv("DEEPAI_TOKEN_METRICS")
	os.Unsetenv("DEEPAI_TOKEN_AGING")

	applyTokenEfficiencyEnv(Config{})

	if _, set := os.LookupEnv("DEEPAI_TOKEN_METRICS"); set {
		t.Error("empty config must not set DEEPAI_TOKEN_METRICS")
	}
	if _, set := os.LookupEnv("DEEPAI_TOKEN_AGING"); set {
		t.Error("false config must not set DEEPAI_TOKEN_AGING")
	}
}

func TestConfig_TokenFieldsYAMLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "provider: anthropic\ntoken_metrics: \"1\"\ntoken_aging: true\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TokenMetrics != "1" {
		t.Errorf("token_metrics = %q, want 1", cfg.TokenMetrics)
	}
	if !cfg.TokenAging {
		t.Error("token_aging should parse as true")
	}
}

func TestResolveDefaultAlias(t *testing.T) {
	defs := []llm.ModelDef{
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514"},
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}
	tests := []struct {
		name          string
		modelOverride string
		configModel   string
		want          string
	}{
		{"override by alias", "fast", "", "fast"},
		{"override by bare model name", "gpt-4o-mini", "", "fast"},
		{"override by provider/model ref", "openai/gpt-4o-mini", "", "fast"},
		{"override case-insensitive alias", "FAST", "", "fast"},
		{"config model by alias", "", "smart", "smart"},
		{"config model by bare name", "", "claude-sonnet-4-20250514", "smart"},
		{"override not found returns empty", "nonexistent", "", ""},
		{"both empty returns empty", "", "", ""},
		{"override wins over config", "fast", "smart", "fast"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveDefaultAlias(defs, tt.modelOverride, tt.configModel)
			if got != tt.want {
				t.Errorf("resolveDefaultAlias(%q, %q) = %q, want %q",
					tt.modelOverride, tt.configModel, got, tt.want)
			}
		})
	}
}

// TestUnknownConfigKeys_CatchesTheTypoThatCostASession uses the exact config
// that silently disabled a 1M context window: `cotext_window` (missing an "n")
// left the window at the 192k default, pulling aging's and compaction's
// thresholds ~5x earlier than intended.
func TestUnknownConfigKeys_CatchesTheTypoThatCostASession(t *testing.T) {
	data := []byte("provider: anthropic\nmodel: glm-5.2\ncotext_window: 1000000\nrequest_timeout: 0\ntoken_aging: true\n")

	got := unknownConfigKeys(data)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 unknown key, got %+v", got)
	}
	if got[0].name != "cotext_window" {
		t.Errorf("name = %q, want cotext_window", got[0].name)
	}
	if got[0].line != 3 {
		t.Errorf("line = %d, want 3", got[0].line)
	}
}

// TestLoadConfig_UnknownKeyIsNotFatal pins the warning-not-error contract: a
// config written by a different deepai version must keep loading, with every
// recognized field intact.
func TestLoadConfig_UnknownKeyIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(
		"provider: anthropic\nmodel: glm-5.2\ncotext_window: 1000000\ntoken_aging: true\nsome_future_knob: 7\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unknown keys must not fail the load: %v", err)
	}
	if cfg.Provider != "anthropic" || cfg.Model != "glm-5.2" || !cfg.TokenAging {
		t.Errorf("recognized fields were lost: %+v", cfg)
	}
	if cfg.ContextWindow != 0 {
		t.Errorf("the typo'd key must stay ignored, got ContextWindow=%d", cfg.ContextWindow)
	}
}

func TestUnknownConfigKeys_CleanConfigIsSilent(t *testing.T) {
	data := []byte("provider: anthropic\nmodel: glm-5.2\ncontext_window: 1000000\ntoken_aging: true\n")
	if got := unknownConfigKeys(data); len(got) != 0 {
		t.Errorf("a valid config reported unknown keys: %+v", got)
	}
}

func TestUnknownConfigKeys_EmptyAndTypeErrorsAreSilent(t *testing.T) {
	if got := unknownConfigKeys(nil); len(got) != 0 {
		t.Errorf("empty config reported unknown keys: %+v", got)
	}
	// A type mismatch is LoadConfig's error to report, not an unknown key.
	if got := unknownConfigKeys([]byte("context_window: not-a-number\n")); len(got) != 0 {
		t.Errorf("type error was misreported as an unknown key: %+v", got)
	}
}
