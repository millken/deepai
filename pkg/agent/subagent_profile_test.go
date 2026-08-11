package agent

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// profileTestExecutor builds a SubagentExecutor backed by a captureProvider and
// a registry holding one tool per group the builtin profiles select from, so a
// test can assert BOTH the request knobs (temperature) and the resolved tool
// set without pulling in the real builtin tool implementations.
func profileTestExecutor(t *testing.T) (*SubagentExecutor, *captureProvider) {
	t.Helper()
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	provider := &captureProvider{}
	reg.InjectProvider("test", "", "", provider)
	return NewSubagentExecutor(reg, profileTestTools(t), nil), provider
}

// profileTestTools registers one no-op tool per name/group combination the
// builtin profiles (and the conservative custom-type fallback) select from, so
// tool selection resolves the same way it does in production.
func profileTestTools(t *testing.T) *tools.Registry {
	t.Helper()
	toolReg := tools.NewRegistry()
	for _, spec := range []struct {
		name   string
		groups []string
	}{
		{"read_file", []string{"builtin", "file_ops"}},
		{"write_file", []string{"builtin", "file_ops"}},
		{"edit_file", []string{"builtin", "file_ops"}},
		{"list_dir", []string{"builtin", "file_ops"}},
		{"glob", []string{"builtin", "file_ops"}},
		{"grep", []string{"builtin", "file_ops"}},
		{"find", []string{"builtin", "file_ops"}},
		{"code_map", []string{"builtin", "file_ops"}},
		{"bash", []string{"builtin"}},
		{"web_search", []string{"builtin", "web"}},
	} {
		tl := noopTool()
		tl.Name = spec.name
		tl.Groups = spec.groups
		if err := toolReg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", spec.name, err)
		}
	}
	return toolReg
}

func requestToolNames(req llm.ChatRequest) []string {
	out := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		out = append(out, tl.Name)
	}
	sort.Strings(out)
	return out
}

// TestSubagentExecutor_AppliesProfileTemperature is the RED test for the
// dropped-temperature bug: buildAgentConfig never set AgentConfig.Temperature,
// so New()'s ApplyAgentType saw an empty AgentType, fell back to
// general-purpose, and every subagent ran at general-purpose's 0.2 — the
// per-type Temperature in BuiltinAgentTypes (coder 0.1, bash 0.0, ...) reached
// the provider for no type at all.
func TestSubagentExecutor_AppliesProfileTemperature(t *testing.T) {
	for _, at := range []AgentType{AgentTypeCoder, AgentTypeBash, AgentTypeResearch, AgentTypeGeneral} {
		t.Run(string(at), func(t *testing.T) {
			exec, provider := profileTestExecutor(t)
			if _, err := exec.Execute(context.Background(),
				&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: string(at)}},
				func(subagent.TaskEvent) {}); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			want := BuiltinAgentTypes[at].Temperature
			got := provider.firstRequest().Temperature
			if got == nil {
				t.Fatalf("request Temperature = nil, want %v (the %s profile value)", want, at)
			}
			if *got != want {
				t.Fatalf("request Temperature = %v, want %v (the %s profile value)", *got, want, at)
			}
		})
	}
}

// TestSubagentExecutor_ProfileTemperatureFromProjectYAML proves the applied
// temperature comes from the RESOLVED profile (project YAML > builtin), not
// from a hardcoded builtin lookup.
func TestSubagentExecutor_ProfileTemperatureFromProjectYAML(t *testing.T) {
	dir := t.TempDir()
	writeAgentYAML(t, dir, "coder", "name: coder\ntemperature: 0.7\n")

	exec, provider := profileTestExecutor(t)
	exec.WithWorkDir(dir)
	if _, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "coder"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got := provider.firstRequest().Temperature
	if got == nil {
		t.Fatal("request Temperature = nil, want 0.7 from the project YAML override")
	}
	if *got != 0.7 {
		t.Fatalf("request Temperature = %v, want 0.7 from the project YAML override", *got)
	}
}

// TestSubagentExecutor_UnknownAgentTypeFails is the RED test for the silent
// agent_type fallback: a hallucinated or typo'd agent_type used to resolve to
// general-purpose and run to completion with an UNRESTRICTED tool set (more
// privilege than an explicit general-purpose), with no error and no warning.
// That contradicts selectSubagentTools, which deliberately fails hard rather
// than widening privileges when a tools selector matches nothing.
func TestSubagentExecutor_UnknownAgentTypeFails(t *testing.T) {
	exec, provider := profileTestExecutor(t)
	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "code-reviewer"}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want an unknown-agent_type error")
	}
	if !strings.Contains(err.Error(), "code-reviewer") {
		t.Fatalf("Execute() error = %q, want it to name the offending type", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown agent_type") {
		t.Fatalf("Execute() error = %q, want it to say \"unknown agent_type\"", err.Error())
	}
	// The error must name valid alternatives so the model can self-correct
	// instead of retrying the same bad type.
	if !strings.Contains(err.Error(), string(AgentTypeCoder)) {
		t.Fatalf("Execute() error = %q, want it to list available types", err.Error())
	}
	// Rejection happens before any LLM call — a bad type must not burn tokens.
	if provider.firstRequest().Model != "" {
		t.Fatal("provider was called for an unknown agent_type; rejection must precede the run")
	}
}

// TestSubagentExecutor_ResolvableAgentTypesStillRun guards the unknown-type
// rejection against over-reach: an empty type, a builtin, a project YAML type
// with no builtin counterpart, and a project MD type must all still run.
func TestSubagentExecutor_ResolvableAgentTypesStillRun(t *testing.T) {
	dir := t.TempDir()
	writeAgentYAML(t, dir, "my-yaml-agent", "name: my-yaml-agent\ndescription: custom\nsystem_prompt: custom yaml\n")
	writeAgentFile(t, dir, "my-md-agent.md", "---\nname: my-md-agent\ndescription: custom\n---\ncustom md\n")

	for _, at := range []string{"", "general-purpose", "coder", "my-yaml-agent", "my-md-agent"} {
		t.Run("type="+at, func(t *testing.T) {
			exec, _ := profileTestExecutor(t)
			exec.WithWorkDir(dir)
			if _, err := exec.Execute(context.Background(),
				&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: at}},
				func(subagent.TaskEvent) {}); err != nil {
				t.Fatalf("Execute() error = %v, want nil for resolvable type %q", err, at)
			}
		})
	}
}

// TestSubagentExecutor_ProjectYAMLWinsOverPoolDefaults is the RED test for the
// pool-Defaults shadowing bug: NewPool used to seed PoolConfig.Defaults with
// hardcoded per-type configs for general-purpose and bash, resolveConfig wrote
// them into task.Config, and Execute prefers task.Config over the resolved
// profile — so a project .deepai/agents/general-purpose.yaml could not change
// max_turns or tools for exactly those two types. Goes through the real pool
// (StartTask/Wait), which is the only path production uses.
func TestSubagentExecutor_ProjectYAMLWinsOverPoolDefaults(t *testing.T) {
	dir := t.TempDir()
	writeAgentYAML(t, dir, "general-purpose",
		"name: general-purpose\ndescription: project override\nsystem_prompt: overridden\nmax_turns: 20\ntools: [bash, read_file]\n")

	exec, provider := profileTestExecutor(t)
	exec.WithWorkDir(dir)
	pool := NewSubagentPool(exec, 2, 30*time.Second)

	task, err := pool.StartTask(context.Background(), "d", "hi", subagent.SubagentConfig{AgentType: "general-purpose"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	done, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if done.Status != subagent.TaskStatusCompleted {
		t.Fatalf("task status = %s (%s), want completed", done.Status, done.Error)
	}

	if done.Config.MaxTurns != 0 {
		t.Fatalf("task.Config.MaxTurns = %d, want 0 so the profile's max_turns decides (pool must inject no per-type default)", done.Config.MaxTurns)
	}
	if len(done.Config.Tools) != 0 {
		t.Fatalf("task.Config.Tools = %v, want empty so the profile's tools decide", done.Config.Tools)
	}
	got := requestToolNames(provider.firstRequest())
	want := []string{"bash", "read_file"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("subagent tools = %v, want %v from the project YAML", got, want)
	}
	if sp := provider.firstRequest().SystemPrompt; !strings.Contains(sp, "overridden") {
		t.Fatalf("system prompt = %q, want the project YAML's prompt", sp)
	}
}

// TestSubagentExecutor_GeneralPurposeToolsAreRestricted pins general-purpose to
// an explicit tool allowlist. Its builtin profile used to carry no DefaultTools,
// which selectSubagentTools reads as "no restriction" — so a general-purpose
// delegate received every registered tool, including mutating ones
// (git_auto_commit) and whatever MCP servers happened to be connected. The
// allowlist lives in BuiltinAgentTypes like every other type's, so a project
// .deepai/agents/general-purpose.yaml can still widen or narrow it.
func TestSubagentExecutor_GeneralPurposeToolsAreRestricted(t *testing.T) {
	exec, provider := profileTestExecutor(t)
	// Two tools a general-purpose delegate must NOT inherit implicitly: a
	// mutating git tool, and a stand-in for an arbitrary MCP-provided tool.
	for _, name := range []string{"git_auto_commit", "some_mcp_tool"} {
		tl := noopTool()
		tl.Name = name
		if err := exec.tools.Register(tl); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	if _, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := requestToolNames(provider.firstRequest())
	if len(got) == 0 {
		t.Fatal("general-purpose subagent got no tools at all")
	}
	granted := make(map[string]bool, len(got))
	for _, name := range got {
		granted[name] = true
	}
	for _, denied := range []string{"git_auto_commit", "some_mcp_tool"} {
		if granted[denied] {
			t.Fatalf("general-purpose subagent was granted %q; tools = %v", denied, got)
		}
	}
	// Everything it DID get must be named by the profile allowlist.
	allowed := make(map[string]bool)
	for _, name := range BuiltinAgentTypes[AgentTypeGeneral].DefaultTools {
		allowed[name] = true
	}
	if len(allowed) == 0 {
		t.Fatal("BuiltinAgentTypes[general-purpose].DefaultTools is empty, which means \"no restriction\"")
	}
	for _, name := range got {
		if !allowed[name] {
			t.Fatalf("general-purpose subagent got %q, which the profile allowlist does not name; tools = %v", name, got)
		}
	}
}

// TestSubagentExecutor_AgentMDModelResolves is the RED test for the ignored
// `model:` frontmatter key in agent markdown: ParseAgentMarkdown only logged it
// at debug level, so an MD-defined agent (every Claude-plugin agent) silently
// ran on the registry default while a YAML-defined agent could pin a model.
func TestSubagentExecutor_AgentMDModelResolves(t *testing.T) {
	reg, err := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "test", Model: "default-model"},
		{Name: "fast", Provider: "test", Model: "fast-model"},
	}, "default")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	provider := &captureProvider{}
	reg.InjectProvider("test", "", "", provider)

	dir := t.TempDir()
	writeAgentFile(t, dir, "md-model-agent.md",
		"---\nname: md-model-agent\ndescription: pins a model\nmodel: fast\n---\nbody\n")

	exec := NewSubagentExecutor(reg, profileTestTools(t), nil).WithWorkDir(dir)
	if _, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "md-model-agent"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := provider.firstRequest().Model; got != "fast-model" {
		t.Fatalf("request Model = %q, want fast-model (the md `model: fast` alias)", got)
	}
}

// TestBuiltinAgentTypes_DocumentEditorProfile guards the document-editor
// profile's two load-bearing knobs (design docs/DOCX_TOOLS_DESIGN.md §6/§5.8):
// MaxTurns must be an explicit 30 (a zero value gets floored to 15 by
// subagent.go's safety net, which only covers four or five 2-3-turn polishing
// chunks), and DefaultTools must include both docx tools or the profile can
// never touch a document at all.
func TestBuiltinAgentTypes_DocumentEditorProfile(t *testing.T) {
	cfg, ok := BuiltinAgentTypes[AgentTypeDocEditor]
	if !ok {
		t.Fatal("BuiltinAgentTypes[document-editor] is not registered")
	}
	if cfg.MaxTurns != 30 {
		t.Fatalf("document-editor MaxTurns = %d, want 30 (0 would be floored to 15 by the subagent safety net)", cfg.MaxTurns)
	}
	want := map[string]bool{"docx_read": false, "docx_edit": false, "docx_format": false}
	for _, name := range cfg.DefaultTools {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("document-editor DefaultTools = %v, missing %q", cfg.DefaultTools, name)
		}
	}
}

// TestDocEditorSystemPrompt_RoutesFormattingAndForbidsScriptedEditing pins
// the P2a Task 2 brief's system-prompt requirement directly: the model must
// be told formatting goes through docx_format, and must be told never to
// edit a .docx via a Python (or other) script run through bash — that path
// skips the backup, the protect list, and the audit trail this profile
// exists to guarantee.
func TestDocEditorSystemPrompt_RoutesFormattingAndForbidsScriptedEditing(t *testing.T) {
	cfg, ok := BuiltinAgentTypes[AgentTypeDocEditor]
	if !ok {
		t.Fatal("BuiltinAgentTypes[document-editor] is not registered")
	}
	prompt := cfg.SystemPrompt
	if !strings.Contains(prompt, "docx_format") {
		t.Error("document-editor system prompt never mentions docx_format")
	}
	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "python") || !strings.Contains(lower, "bash") {
		t.Errorf("document-editor system prompt does not warn against editing .docx via a script through bash: %q", prompt)
	}
}

// TestDocEditorSystemPrompt_DefaultsToTrackChangesAndReportsPendingReview
// pins P2b Task 3's system-prompt requirement (design §4.2's "润色首选" —
// track_changes defaults on for polishing once it exists): the profile must
// tell the model to pass track_changes on docx_edit calls by default, and —
// just as importantly — to report the result as changes PENDING REVIEW in
// Word rather than already applied. A model that says "I've made the
// changes" when they are sitting unaccepted in a review pane has misled the
// user, so the prompt must forbid that phrasing pattern, not just describe
// the argument.
func TestDocEditorSystemPrompt_DefaultsToTrackChangesAndReportsPendingReview(t *testing.T) {
	cfg, ok := BuiltinAgentTypes[AgentTypeDocEditor]
	if !ok {
		t.Fatal("BuiltinAgentTypes[document-editor] is not registered")
	}
	prompt := cfg.SystemPrompt
	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "track_changes") {
		t.Error("document-editor system prompt never mentions track_changes")
	}
	if !strings.Contains(lower, "default") {
		t.Errorf("document-editor system prompt does not say track_changes is the default for polishing: %q", prompt)
	}
	if !strings.Contains(lower, "pending review") && !strings.Contains(lower, "review pane") {
		t.Errorf("document-editor system prompt does not tell the model to report tracked changes as pending review, not applied: %q", prompt)
	}
}

func writeAgentYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	writeAgentFile(t, dir, name+".yaml", body)
}

func writeAgentFile(t *testing.T, dir, filename, body string) {
	t.Helper()
	agentsDir := filepath.Join(dir, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}
