package agent

import (
	"os"
	"path/filepath"
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
		result := mergeConfig(base, override)
		if result.SystemPrompt != "Custom prompt." {
			t.Errorf("SystemPrompt = %q, want custom prompt", result.SystemPrompt)
		}
	})

	t.Run("yaml nil returns base", func(t *testing.T) {
		result := mergeConfig(base, nil)
		if result.SystemPrompt != "You are a coder." {
			t.Errorf("SystemPrompt = %q, want base", result.SystemPrompt)
		}
	})

	t.Run("yaml overrides tools completely", func(t *testing.T) {
		override := &AgentTypeConfig{DefaultTools: []string{"read_file"}}
		result := mergeConfig(base, override)
		if len(result.DefaultTools) != 1 || result.DefaultTools[0] != "read_file" {
			t.Errorf("Tools = %v, want [read_file]", result.DefaultTools)
		}
	})

	t.Run("no tools defaults to minimal read-only set", func(t *testing.T) {
		emptyBase := AgentTypeConfig{Type: AgentType("custom")}
		result := mergeConfig(emptyBase, &AgentTypeConfig{})
		if len(result.DefaultTools) == 0 {
			t.Error("expected default minimal read-only tool set")
		}
		if result.DefaultTools[0] != "read_file" {
			t.Errorf("first default tool = %q, want read_file", result.DefaultTools[0])
		}
	})

	t.Run("yaml overrides temperature", func(t *testing.T) {
		override := &AgentTypeConfig{Temperature: 0.5}
		result := mergeConfig(base, override)
		if result.Temperature != 0.5 {
			t.Errorf("Temperature = %v, want 0.5", result.Temperature)
		}
	})

	t.Run("yaml overrides max_turns", func(t *testing.T) {
		override := &AgentTypeConfig{MaxTurns: 10}
		result := mergeConfig(base, override)
		if result.MaxTurns != 10 {
			t.Errorf("MaxTurns = %d, want 10", result.MaxTurns)
		}
	})

	t.Run("yaml empty fields keep base", func(t *testing.T) {
		override := &AgentTypeConfig{Name: "New Name"}
		result := mergeConfig(base, override)
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
