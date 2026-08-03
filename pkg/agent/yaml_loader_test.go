package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSafeName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "security-reviewer", false},
		{"valid simple", "coder", false},
		{"path traversal", "../../etc/passwd", true},
		{"double dot", "some..name", true},
		{"slash", "foo/bar", true},
		{"backslash", "foo\\bar", true},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSafeName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSafeName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestMergeConfig(t *testing.T) {
	base := AgentTypeConfig{
		Type:         AgentTypeCoder,
		Name:         "Coder",
		SystemPrompt: "You are a coder.",
		DefaultTools: []string{"bash", "read_file"},
		Temperature:  0.1,
		MaxTurns:     0,
	}

	t.Run("yaml overrides system prompt", func(t *testing.T) {
		override := &AgentTypeConfig{SystemPrompt: "Custom prompt."}
		result := mergeConfig(base, override, true)
		if result.SystemPrompt != "Custom prompt." {
			t.Errorf("SystemPrompt = %q, want custom prompt", result.SystemPrompt)
		}
	})

	t.Run("yaml nil returns base", func(t *testing.T) {
		result := mergeConfig(base, nil, true)
		if result.SystemPrompt != "You are a coder." {
			t.Errorf("SystemPrompt = %q, want base", result.SystemPrompt)
		}
	})

	t.Run("yaml overrides tools completely", func(t *testing.T) {
		override := &AgentTypeConfig{DefaultTools: []string{"read_file"}}
		result := mergeConfig(base, override, true)
		if len(result.DefaultTools) != 1 || result.DefaultTools[0] != "read_file" {
			t.Errorf("Tools = %v, want [read_file]", result.DefaultTools)
		}
	})

	t.Run("no tools + builtin base stays nil (unrestricted)", func(t *testing.T) {
		// A real builtin's nil DefaultTools (e.g. general-purpose) means
		// "unrestricted" and must be respected as-is.
		builtinLikeBase := AgentTypeConfig{Type: AgentTypeGeneral}
		result := mergeConfig(builtinLikeBase, &AgentTypeConfig{}, true)
		if result.DefaultTools != nil {
			t.Errorf("DefaultTools = %v, want nil (unrestricted)", result.DefaultTools)
		}
	})

	t.Run("no tools + non-builtin base falls back to read-only five", func(t *testing.T) {
		// An unknown/custom type has no builtin profile to be unrestricted by
		// default — an absent tools: key there is conservative, not wide-open.
		nonBuiltinBase := AgentTypeConfig{Type: AgentType("custom")}
		result := mergeConfig(nonBuiltinBase, &AgentTypeConfig{}, false)
		want := []string{"read_file", "list_dir", "glob", "grep", "find"}
		if len(result.DefaultTools) != len(want) {
			t.Fatalf("DefaultTools = %v, want %v", result.DefaultTools, want)
		}
		for i, w := range want {
			if result.DefaultTools[i] != w {
				t.Errorf("DefaultTools[%d] = %q, want %q", i, result.DefaultTools[i], w)
			}
		}
	})

	t.Run("yaml overrides temperature", func(t *testing.T) {
		override := &AgentTypeConfig{Temperature: 0.5}
		result := mergeConfig(base, override, true)
		if result.Temperature != 0.5 {
			t.Errorf("Temperature = %v, want 0.5", result.Temperature)
		}
	})

	t.Run("yaml overrides max_turns", func(t *testing.T) {
		override := &AgentTypeConfig{MaxTurns: 10}
		result := mergeConfig(base, override, true)
		if result.MaxTurns != 10 {
			t.Errorf("MaxTurns = %d, want 10", result.MaxTurns)
		}
	})

	t.Run("yaml empty fields keep base", func(t *testing.T) {
		override := &AgentTypeConfig{Name: "New Name"}
		result := mergeConfig(base, override, true)
		if result.Name != "New Name" {
			t.Errorf("Name = %q, want New Name", result.Name)
		}
		if result.SystemPrompt != "You are a coder." {
			t.Errorf("SystemPrompt should keep base")
		}
		if result.Type != AgentTypeCoder {
			t.Errorf("Type should keep base")
		}
	})
}

// TestMergeConfig_ChainedMergePreservesExplicitZero: mergeConfig's result
// must mark maxTurnsSet/temperatureSet = true whenever an override explicitly
// set them (including to zero), so that if this result is later fed as the
// OVERRIDE into a further merge layer, the explicit zero is not resurrected
// by that layer's base value. Without this, result.temperatureSet would stay
// false (copied from base's zero-value flag), and a subsequent merge using
// this result as an override would silently discard the explicit 0.
func TestMergeConfig_ChainedMergePreservesExplicitZero(t *testing.T) {
	base := AgentTypeConfig{Type: AgentType("reviewer"), Temperature: 0.2, MaxTurns: 8}
	explicitZero := &AgentTypeConfig{temperatureSet: true, maxTurnsSet: true}
	r1 := mergeConfig(base, explicitZero, true)
	if r1.Temperature != 0 || r1.MaxTurns != 0 {
		t.Fatalf("r1 = %+v, want Temperature=0 MaxTurns=0", r1)
	}

	// r1 becomes the override for a second merge layer.
	base2 := AgentTypeConfig{Temperature: 0.9, MaxTurns: 20}
	r2 := mergeConfig(base2, &r1, true)
	if r2.Temperature != 0 {
		t.Errorf("r2.Temperature = %v, want 0 (explicit zero must survive being re-merged as an override)", r2.Temperature)
	}
	if r2.MaxTurns != 0 {
		t.Errorf("r2.MaxTurns = %v, want 0 (explicit zero must survive being re-merged as an override)", r2.MaxTurns)
	}
}

func TestLoadAgentYAML(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("file not found returns nil", func(t *testing.T) {
		cfg, err := loadAgentYAML("nonexistent", dir)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Error("expected nil for nonexistent file")
		}
	})

	t.Run("empty workdir returns nil", func(t *testing.T) {
		cfg, err := loadAgentYAML("test", "")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Error("expected nil for empty workdir")
		}
	})

	t.Run("valid yaml file", func(t *testing.T) {
		yamlContent := `type: custom-agent
name: Custom Agent
description: A custom agent for testing
system_prompt: "You are a custom agent."
tools:
  - read_file
  - grep
temperature: 0.3
max_turns: 5
`
		if err := os.WriteFile(filepath.Join(agentsDir, "custom-agent.yaml"), []byte(yamlContent), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadAgentYAML("custom-agent", dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected config, got nil")
		}
		if cfg.Name != "Custom Agent" {
			t.Errorf("Name = %q, want Custom Agent", cfg.Name)
		}
		if cfg.SystemPrompt != "You are a custom agent." {
			t.Errorf("SystemPrompt = %q", cfg.SystemPrompt)
		}
		if len(cfg.DefaultTools) != 2 {
			t.Errorf("Tools = %v, want 2 items", cfg.DefaultTools)
		}
		if cfg.Temperature != 0.3 {
			t.Errorf("Temperature = %v, want 0.3", cfg.Temperature)
		}
		if cfg.MaxTurns != 5 {
			t.Errorf("MaxTurns = %d, want 5", cfg.MaxTurns)
		}
	})

	t.Run("system_prompt_file", func(t *testing.T) {
		promptDir := filepath.Join(agentsDir, "prompts")
		if err := os.MkdirAll(promptDir, 0o755); err != nil {
			t.Fatal(err)
		}
		promptContent := "You are a prompt-file-based agent.\nWith multiple lines.\n"
		if err := os.WriteFile(filepath.Join(promptDir, "custom.md"), []byte(promptContent), 0o644); err != nil {
			t.Fatal(err)
		}

		yamlContent := `type: file-agent
system_prompt_file: prompts/custom.md
tools:
  - read_file
`
		if err := os.WriteFile(filepath.Join(agentsDir, "file-agent.yaml"), []byte(yamlContent), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadAgentYAML("file-agent", dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected config, got nil")
		}
		if cfg.SystemPrompt != "You are a prompt-file-based agent.\nWith multiple lines." {
			t.Errorf("SystemPrompt = %q, want content from file", cfg.SystemPrompt)
		}
	})

	t.Run("system_prompt_file path traversal rejected", func(t *testing.T) {
		yamlContent := `type: evil-agent
system_prompt_file: ../../etc/passwd
`
		if err := os.WriteFile(filepath.Join(agentsDir, "evil-agent.yaml"), []byte(yamlContent), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := loadAgentYAML("evil-agent", dir)
		if err == nil {
			t.Error("expected error for path traversal in system_prompt_file")
		}
	})
}

// TestLoadAgentYAML_ExplicitZeroOverrides: an explicit `temperature: 0` (or
// `max_turns: 0`) in project YAML must be distinguishable from the field being
// absent — otherwise a reviewer agent wanting temperature 0 silently keeps
// the base's non-zero default.
func TestLoadAgentYAML_ExplicitZeroOverrides(t *testing.T) {
	base := AgentTypeConfig{
		Type:        AgentType("reviewer"),
		Temperature: 0.2,
		MaxTurns:    8,
	}

	writeYAML := func(t *testing.T, dir, name, body string) {
		t.Helper()
		agentsDir := filepath.Join(dir, ".deepai", "agents")
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentsDir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("explicit temperature: 0 resolves to 0", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, dir, "reviewer", "type: reviewer\ntemperature: 0\n")
		cfg, err := loadAgentYAML("reviewer", dir)
		if err != nil {
			t.Fatalf("loadAgentYAML: %v", err)
		}
		result := mergeConfig(base, cfg, true)
		if result.Temperature != 0 {
			t.Errorf("Temperature = %v, want 0 (explicit override)", result.Temperature)
		}
	})

	t.Run("absent temperature keeps base", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, dir, "reviewer", "type: reviewer\nsystem_prompt: no temperature key here\n")
		cfg, err := loadAgentYAML("reviewer", dir)
		if err != nil {
			t.Fatalf("loadAgentYAML: %v", err)
		}
		result := mergeConfig(base, cfg, true)
		if result.Temperature != 0.2 {
			t.Errorf("Temperature = %v, want 0.2 (base, unset in yaml)", result.Temperature)
		}
	})

	t.Run("explicit max_turns: 0 resolves to 0", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, dir, "reviewer", "type: reviewer\nmax_turns: 0\n")
		cfg, err := loadAgentYAML("reviewer", dir)
		if err != nil {
			t.Fatalf("loadAgentYAML: %v", err)
		}
		result := mergeConfig(base, cfg, true)
		if result.MaxTurns != 0 {
			t.Errorf("MaxTurns = %v, want 0 (explicit override)", result.MaxTurns)
		}
	})

	t.Run("absent max_turns keeps base", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, dir, "reviewer", "type: reviewer\nsystem_prompt: no max_turns key here\n")
		cfg, err := loadAgentYAML("reviewer", dir)
		if err != nil {
			t.Fatalf("loadAgentYAML: %v", err)
		}
		result := mergeConfig(base, cfg, true)
		if result.MaxTurns != 8 {
			t.Errorf("MaxTurns = %v, want 8 (base, unset in yaml)", result.MaxTurns)
		}
	})
}

// TestResolveAgentTypeConfig_GeneralPurposeYAMLNoToolsInheritsBuiltinAllowlist:
// a project general-purpose.yaml that only sets system_prompt (no tools: key)
// must inherit the BUILTIN general-purpose allowlist, not be narrowed to the
// conservative read-only five that a custom (non-builtin) type falls back to.
// general-purpose used to carry nil DefaultTools ("unrestricted"), which handed
// a delegated general-purpose subagent every registered tool; it now carries an
// explicit allowlist, and a tools-less YAML override must leave it intact.
func TestResolveAgentTypeConfig_GeneralPurposeYAMLNoToolsInheritsBuiltinAllowlist(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "system_prompt: \"Custom general-purpose prompt.\"\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "general-purpose.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := resolveAgentTypeConfig(AgentTypeGeneral, dir)
	want := BuiltinAgentTypes[AgentTypeGeneral].DefaultTools
	if len(want) == 0 {
		t.Fatal("builtin general-purpose has no DefaultTools, which means \"unrestricted\"")
	}
	if strings.Join(cfg.DefaultTools, ",") != strings.Join(want, ",") {
		t.Errorf("DefaultTools = %v, want the builtin allowlist %v", cfg.DefaultTools, want)
	}
	if cfg.SystemPrompt != "Custom general-purpose prompt." {
		t.Errorf("SystemPrompt = %q, want the YAML override", cfg.SystemPrompt)
	}
}

// TestResolveAgentTypeConfig_CustomTypeYAMLNoToolsGetsReadOnlyFallback: a
// custom (non-builtin) type's project YAML with only type+system_prompt (no
// tools: key) must fall back to the conservative read-only five — unlike a
// real builtin, it has no "unrestricted" default to preserve.
func TestResolveAgentTypeConfig_CustomTypeYAMLNoToolsGetsReadOnlyFallback(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "type: my-reviewer\nsystem_prompt: \"Custom reviewer prompt.\"\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "my-reviewer.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := resolveAgentTypeConfig("my-reviewer", dir)
	want := []string{"read_file", "list_dir", "glob", "grep", "find"}
	if len(cfg.DefaultTools) != len(want) {
		t.Fatalf("DefaultTools = %v, want %v", cfg.DefaultTools, want)
	}
	for i, w := range want {
		if cfg.DefaultTools[i] != w {
			t.Errorf("DefaultTools[%d] = %q, want %q", i, cfg.DefaultTools[i], w)
		}
	}
}

// TestResolveAgentTypeConfig_GeneralPurposeYAMLExplicitZeroTemperature: an
// end-to-end check (real builtin base, temperature 0.2) that an explicit
// `temperature: 0` in project general-purpose.yaml resolves to 0.
func TestResolveAgentTypeConfig_GeneralPurposeYAMLExplicitZeroTemperature(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "general-purpose.yaml"), []byte("temperature: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := resolveAgentTypeConfig(AgentTypeGeneral, dir)
	if cfg.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 (explicit override of builtin's 0.2)", cfg.Temperature)
	}
}

func TestResolveAgentTypeConfig(t *testing.T) {
	t.Run("builtin type", func(t *testing.T) {
		cfg := resolveAgentTypeConfig(AgentTypeCoder, "")
		if cfg.Type != AgentTypeCoder {
			t.Errorf("Type = %q, want coder", cfg.Type)
		}
		if cfg.SystemPrompt != coderSystemPrompt {
			t.Error("should use builtin system prompt")
		}
	})

	t.Run("unknown type falls back to general", func(t *testing.T) {
		cfg := resolveAgentTypeConfig("unknown-type", "")
		if cfg.Type != AgentTypeGeneral {
			t.Errorf("Type = %q, want general-purpose", cfg.Type)
		}
	})

	t.Run("yaml overrides builtin", func(t *testing.T) {
		dir := t.TempDir()
		agentsDir := filepath.Join(dir, ".deepai", "agents")
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		yamlContent := `type: coder
name: Custom Coder
system_prompt: "Custom coder prompt."
`
		if err := os.WriteFile(filepath.Join(agentsDir, "coder.yaml"), []byte(yamlContent), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := resolveAgentTypeConfig(AgentTypeCoder, dir)
		if cfg.Name != "Custom Coder" {
			t.Errorf("Name = %q, want Custom Coder", cfg.Name)
		}
		if cfg.SystemPrompt != "Custom coder prompt." {
			t.Errorf("SystemPrompt = %q, want custom prompt", cfg.SystemPrompt)
		}
	})
}
