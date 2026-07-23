package commands

import (
	"os"
	"path/filepath"
	"testing"
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
