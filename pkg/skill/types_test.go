package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkill_FullFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, `---
name: code-review
description: 综合代码审查，覆盖安全、性能、质量和可维护性
user-invocable: true
disable-model-invocation: false
allowed-tools:
  - Read
  - Grep
  - Glob
model: anthropic/claude-sonnet-4-6
max-turns: 20
temperature: 0.1
---

## 审查流程

1. 安全审查
2. 性能分析
3. 代码质量
`)

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if skill.Meta.Name != "code-review" {
		t.Errorf("Name = %q, want %q", skill.Meta.Name, "code-review")
	}
	if skill.Meta.Description != "综合代码审查，覆盖安全、性能、质量和可维护性" {
		t.Errorf("Description mismatch: %q", skill.Meta.Description)
	}
	if !skill.Meta.IsUserInvocable() {
		t.Error("IsUserInvocable() = false, want true")
	}
	if !skill.Meta.IsAutoInvocable() {
		t.Error("IsAutoInvocable() = false, want true")
	}
	if skill.Meta.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("Model = %q", skill.Meta.Model)
	}
	if len(skill.Meta.AllowedTools) != 3 {
		t.Errorf("AllowedTools count = %d, want 3", len(skill.Meta.AllowedTools))
	}
	if skill.Meta.MaxTurns == nil || *skill.Meta.MaxTurns != 20 {
		t.Error("MaxTurns mismatch")
	}
	if skill.Meta.Temperature == nil || *skill.Meta.Temperature != 0.1 {
		t.Error("Temperature mismatch")
	}
	// Lazy loading: body is NOT loaded at parse time
	if skill.Loaded {
		t.Error("Loaded = true, want false (lazy loading)")
	}
	if skill.Body != "" {
		t.Error("Body should be empty before LoadBody")
	}
}

func TestParseSkill_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	os.MkdirAll(skillDir, 0755)
	writeSKILL(t, skillDir, "Just markdown content\nwithout frontmatter.\n")

	skill, err := ParseSkill(skillDir)
	if err != nil {
		t.Fatal(err)
	}

	if skill.Meta.Name != "my-skill" {
		t.Errorf("Name fallback = %q, want %q", skill.Meta.Name, "my-skill")
	}
	if skill.Meta.Description != "Just markdown content without frontmatter." {
		t.Errorf("Description fallback = %q", skill.Meta.Description)
	}
}

func TestParseSkill_DisableModelInvocation(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, `---
name: deploy
description: Deploy to production
disable-model-invocation: true
---

Deploy steps here.
`)

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if skill.Meta.IsAutoInvocable() {
		t.Error("IsAutoInvocable() = true, want false")
	}
	if !skill.Meta.IsUserInvocable() {
		t.Error("IsUserInvocable() = false, want true (default)")
	}
}

func TestParseSkill_UserInvocableFalse(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, `---
name: bg-knowledge
description: Background knowledge
user-invocable: false
---

Some background info.
`)

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if skill.Meta.IsUserInvocable() {
		t.Error("IsUserInvocable() = true, want false")
	}
}

func TestParseSkill_DescriptionTruncation(t *testing.T) {
	dir := t.TempDir()
	longDesc := "This is a very long description that exceeds 250 characters. " +
		"123456789012345678901234567890123456789012345678901234567890" +
		"123456789012345678901234567890123456789012345678901234567890" +
		"123456789012345678901234567890123456789012345678901234567890"
	writeSKILL(t, dir, "---\nname: test\ndescription: "+longDesc+"\n---\nBody.\n")

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(skill.Meta.Description) > 250 {
		t.Errorf("Description length = %d, want <= 250", len(skill.Meta.Description))
	}
}

func TestParseSkill_ContextFork(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, `---
name: research
description: Deep research
context: fork
agent: Explore
---

Research $ARGUMENTS thoroughly.
`)

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if skill.Meta.Context != "fork" {
		t.Errorf("Context = %q, want %q", skill.Meta.Context, "fork")
	}
	if skill.Meta.Agent != "Explore" {
		t.Errorf("Agent = %q, want %q", skill.Meta.Agent, "Explore")
	}
}

func TestParseSkill_InvalidName(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"uppercase", "---\nname: My-Skill\n---\nBody.\n"},
		{"spaces", "---\nname: my skill\n---\nBody.\n"},
		{"special chars", "---\nname: my_skill!\n---\nBody.\n"},
		{"too long", "---\nname: " + strings.Repeat("a", 65) + "\n---\nBody.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSKILL(t, dir, tt.yaml)
			_, err := ParseSkill(dir)
			if err == nil {
				t.Error("expected error for invalid name")
			}
		})
	}
}

func TestParseSkill_AllowedToolsStringFormat(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, `---
name: test
description: Test
allowed-tools: Read Grep Glob
---

Body.
`)

	skill, err := ParseSkill(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(skill.Meta.AllowedTools) != 3 {
		t.Fatalf("AllowedTools count = %d, want 3", len(skill.Meta.AllowedTools))
	}
	if skill.Meta.AllowedTools[0] != "Read" || skill.Meta.AllowedTools[1] != "Grep" || skill.Meta.AllowedTools[2] != "Glob" {
		t.Errorf("AllowedTools = %v", skill.Meta.AllowedTools)
	}
}

func writeSKILL(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
