package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgentMD(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseAgentMarkdown_Full(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviewer.md")
	writeAgentMD(t, path, `---
name: code-reviewer
description: Reviews code changes.
tools: Read, Grep, Bash
model: sonnet
---
You are a senior code reviewer.
Focus on quality and security.`)
	cfg, err := ParseAgentMarkdown(path)
	if err != nil {
		t.Fatalf("ParseAgentMarkdown: %v", err)
	}
	if cfg.Type != "code-reviewer" || cfg.Name != "code-reviewer" {
		t.Fatalf("name fields: %+v", cfg)
	}
	if cfg.Description != "Reviews code changes." {
		t.Fatalf("description: %q", cfg.Description)
	}
	if cfg.SystemPrompt != "You are a senior code reviewer.\nFocus on quality and security." {
		t.Fatalf("system prompt: %q", cfg.SystemPrompt)
	}
	// Claude tool names mapped to deepai names.
	want := []string{"read_file", "grep", "bash"}
	if len(cfg.DefaultTools) != len(want) {
		t.Fatalf("tools: %v", cfg.DefaultTools)
	}
	for i, w := range want {
		if cfg.DefaultTools[i] != w {
			t.Fatalf("tool[%d] = %q, want %q (full: %v)", i, cfg.DefaultTools[i], w, cfg.DefaultTools)
		}
	}
}

func TestParseAgentMarkdown_NameFallback(t *testing.T) {
	// Missing name → filename stem is used as type/name (compat fallback).
	path := filepath.Join(t.TempDir(), "lint-runner.md")
	writeAgentMD(t, path, "---\ndescription: lint\n---\nlint things")
	cfg, err := ParseAgentMarkdown(path)
	if err != nil {
		t.Fatalf("ParseAgentMarkdown: %v", err)
	}
	if cfg.Type != "lint-runner" || cfg.Name != "lint-runner" {
		t.Fatalf("expected filename-stem fallback, got %+v", cfg)
	}
}

func TestMapClaudeTools_Passthrough(t *testing.T) {
	// Unknown names pass through unchanged (allows already-deepai names).
	got := mapClaudeTools("Read, bash, custom-tool")
	want := []string{"read_file", "bash", "custom-tool"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d]=%q want %q", i, got[i], w)
		}
	}
	if mapClaudeTools("") != nil {
		t.Fatalf("empty tools → nil")
	}
}

func TestEnumerateAgents_ProjectYAMLOverMDConsistentWithResolve(t *testing.T) {
	// A project type with BOTH foo.yaml and foo.md: advertising must show the
	// YAML description (what execution resolves to), not the .md's.
	workDir := t.TempDir()
	agentsDir := filepath.Join(workDir, ".deepai", "agents")
	writeAgentMD(t, filepath.Join(agentsDir, "foo.md"),
		"---\nname: foo\ndescription: FROM MD\n---\nmd prompt")
	if err := os.WriteFile(filepath.Join(agentsDir, "foo.yaml"),
		[]byte("type: foo\ndescription: FROM YAML\nsystem_prompt: yaml prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agents := EnumerateAgents(workDir, nil)
	var advertised string
	for _, a := range agents {
		if a.Type == "foo" {
			advertised = a.Description
		}
	}
	if advertised != "FROM YAML" {
		t.Fatalf("advertised description = %q, want FROM YAML (yaml wins over md)", advertised)
	}
	// And execution must agree — resolve("foo") uses the yaml.
	cfg := resolveAgentTypeConfigWithPlugins("foo", workDir, nil)
	if cfg.Description != "FROM YAML" {
		t.Fatalf("resolve description = %q, want FROM YAML", cfg.Description)
	}
	if advertised != cfg.Description {
		t.Fatalf("advertising (%q) != execution (%q)", advertised, cfg.Description)
	}
}

func TestEnumerateAgents_ProjectPluginBuiltin(t *testing.T) {
	workDir := t.TempDir()
	// Project agent "shared" + "proj-only".
	writeAgentMD(t, filepath.Join(workDir, ".deepai", "agents", "shared.md"),
		"---\nname: shared\ndescription: from project\n---\np")
	writeAgentMD(t, filepath.Join(workDir, ".deepai", "agents", "proj-only.md"),
		"---\nname: proj-only\ndescription: project only\n---\np")
	// Plugin agent "shared" (collides; project must win) + "plug-only".
	plugA := t.TempDir()
	writeAgentMD(t, filepath.Join(plugA, "shared.md"),
		"---\nname: shared\ndescription: from plugin A\n---\np")
	writeAgentMD(t, filepath.Join(plugA, "plug-only.md"),
		"---\nname: plug-only\ndescription: plugin only\n---\np")

	agents := EnumerateAgents(workDir, []string{plugA})
	byType := map[AgentType]string{}
	for _, a := range agents {
		byType[a.Type] = a.Description
	}
	if byType["shared"] != "from project" {
		t.Fatalf("project must win over plugin for shared; got %q", byType["shared"])
	}
	if byType["proj-only"] != "project only" {
		t.Fatalf("missing/wrong proj-only: %q", byType["proj-only"])
	}
	if byType["plug-only"] != "plugin only" {
		t.Fatalf("missing/wrong plug-only: %q", byType["plug-only"])
	}
	if _, ok := byType[AgentTypeGeneral]; !ok {
		t.Fatalf("builtin general-purpose must be advertised; have %v", byType)
	}
}

func TestEnumerateAgents_PluginOrderDeterminesWinner(t *testing.T) {
	// Two plugins both define "dup"; the FIRST in pluginAgentDirs wins (and the
	// same order is used by resolve, so advertising==execution).
	plugA := t.TempDir()
	plugB := t.TempDir()
	writeAgentMD(t, filepath.Join(plugA, "dup.md"), "---\nname: dup\ndescription: A\n---\np")
	writeAgentMD(t, filepath.Join(plugB, "dup.md"), "---\nname: dup\ndescription: B\n---\np")
	_ = plugB // keep
	agents := EnumerateAgents("", []string{plugA, plugB})
	for _, a := range agents {
		if a.Type == "dup" && a.Description != "A" {
			t.Fatalf("first plugin should win; got %q", a.Description)
		}
	}
}

func TestResolveAgentTypeConfigWithPlugins(t *testing.T) {
	workDir := t.TempDir()
	plug := t.TempDir()
	// Project defines "a"; plugin defines "b"; neither defines "general-purpose".
	writeAgentMD(t, filepath.Join(workDir, ".deepai", "agents", "a.md"),
		"---\nname: a\ndescription: proj a\n---\nproject prompt A")
	writeAgentMD(t, filepath.Join(plug, "b.md"),
		"---\nname: b\ndescription: plug b\n---\nplugin prompt B")

	// Plugin agent resolves to its own config (not general fallback).
	cfgB := resolveAgentTypeConfigWithPlugins("b", workDir, []string{plug})
	if cfgB.SystemPrompt != "plugin prompt B" {
		t.Fatalf("plugin agent b should resolve to plugin prompt; got %q", cfgB.SystemPrompt)
	}
	// Project agent resolves.
	cfgA := resolveAgentTypeConfigWithPlugins("a", workDir, []string{plug})
	if cfgA.SystemPrompt != "project prompt A" {
		t.Fatalf("project agent a should resolve; got %q", cfgA.SystemPrompt)
	}
	// Project beats plugin for a name only in plugin? Test project-over-plugin:
	// put "c" in both; project wins.
	writeAgentMD(t, filepath.Join(workDir, ".deepai", "agents", "c.md"),
		"---\nname: c\ndescription: proj\n---\nPROJECT C")
	writeAgentMD(t, filepath.Join(plug, "c.md"),
		"---\nname: c\ndescription: plug\n---\nPLUGIN C")
	cfgC := resolveAgentTypeConfigWithPlugins("c", workDir, []string{plug})
	if cfgC.SystemPrompt != "PROJECT C" {
		t.Fatalf("project must beat plugin; got %q", cfgC.SystemPrompt)
	}
	// Unknown type → general builtin fallback.
	cfgUnk := resolveAgentTypeConfigWithPlugins("nope-not-real", workDir, []string{plug})
	if cfgUnk.Type != AgentTypeGeneral {
		t.Fatalf("unknown should fall back to general; got %q", cfgUnk.Type)
	}
}
