package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/commands"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
)

func TestRegisterToolsMatchesREPLCoreSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := tools.NewRegistry()
	models := llm.NewSingleModelRegistry("openai", "m", "")
	provider, _, err := models.ProviderFor(models.DefaultName())
	if err != nil {
		t.Skipf("provider: %v", err)
	}
	if err := registerTools(reg, models, provider, commands.Config{}, t.TempDir(), nil, nil, skill.NewRegistry(), ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "ask_clarification", "task", "read_file", "edit_file", "todo_write"} {
		if reg.Get(name) == nil {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestBuildModelRegistryOverride(t *testing.T) {
	cfg := commands.Config{Provider: "openai", Models: []llm.ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-fast"},
		{Name: "smart", Provider: "openai", Model: "gpt-smart"},
	}}
	for override, want := range map[string]string{"": "fast", "smart": "smart", "SMART": "smart", "gpt-other": "gpt-other"} {
		reg, err := buildModelRegistry(cfg, override)
		if err != nil {
			t.Fatalf("-m %q: %v", override, err)
		}
		if got := reg.DefaultName(); got != want {
			t.Errorf("-m %q: default = %q, want %q", override, got, want)
		}
	}
}

func TestNewBotRuntimeEphemeralProvidesTempBotDataDirAndCleansUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENAI_API_KEY", "test-key")
	cfgDir := filepath.Join(home, ".deepai")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "provider: openai\nmodels:\n  - name: fast\n    provider: openai\n    model: gpt-4o-mini\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	rt, err := newBotRuntime(context.Background(), botOptions{
		WorkDir:   t.TempDir(),
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("newBotRuntime(Ephemeral=true): %v", err)
	}
	if rt.BotDataDir == "" {
		t.Fatal("expected non-empty ephemeral BotDataDir so save_bot_skill works symmetrically with update_bot_persona")
	}
	if st, statErr := os.Stat(rt.BotDataDir); statErr != nil || !st.IsDir() {
		t.Fatalf("ephemeral BotDataDir %q does not exist: %v", rt.BotDataDir, statErr)
	}
	dir := rt.BotDataDir
	rt.Close()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("ephemeral BotDataDir %q was not cleaned up on Close: %v", dir, statErr)
	}
}
