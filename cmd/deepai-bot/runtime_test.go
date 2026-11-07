package main

import (
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
