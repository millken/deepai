package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// ---------------------------------------------------------------------------
// makeSkillHandler routing (M5-1 §2.3): the skill tool's Data must never
// carry "system_prompt" for the two branches where the body must NOT leak
// into the calling agent's context (main agent, and a non-target subagent).
// This is the single most load-bearing invariant of the whole M5-1 change.
// react.go:1017 reads Data["system_prompt"] and only acts when the string is
// non-empty (loadedSkillPrompt != ""), so omitting the key entirely and
// setting it to "" are equally safe in practice — but these two branches
// omit it outright anyway, since neither ever renders a body to put there
// (see the dynamic-injection tests below for why that matters beyond just
// this key).
// ---------------------------------------------------------------------------

func newNonForkSkillRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "plain-skill"),
		"---\nname: plain-skill\ndescription: A non-fork skill.\n---\n\nPlain body.\n")
	reg := NewRegistry()
	if err := reg.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	return reg
}

func newForkSkillRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	createSkillDirFull(t, filepath.Join(dir, "docx-polish"),
		"---\nname: docx-polish\ndescription: Polish docx.\ncontext: fork\nagent: document-editor\n---\n\nFork body.\n")
	reg := NewRegistry()
	if err := reg.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	return reg
}

func callSkillTool(t *testing.T, reg *Registry, ctx context.Context, name string) models.ToolResult {
	t.Helper()
	tool := SkillToolWithRegistry(reg)
	result, err := tool.Handler(ctx, models.ToolCall{
		ID:   "call-1",
		Name: "skill",
		Arguments: map[string]any{
			"name": name,
		},
	})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	return result
}

// Branch 1: non-fork skill — current behavior, body returned inline
// regardless of caller.
func TestSkillTool_NonFork_ReturnsBodyInline(t *testing.T) {
	reg := newNonForkSkillRegistry(t)
	result := callSkillTool(t, reg, context.Background(), "plain-skill")

	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	sp, ok := result.Data["system_prompt"].(string)
	if !ok || sp == "" {
		t.Fatalf("Data[system_prompt] = %v, want the rendered body", result.Data["system_prompt"])
	}
}

// Branch 2: caller already IS the fork skill's bound agent — inline return,
// same as non-fork (e.g. document-editor calling docx-polish on itself).
func TestSkillTool_ForkSkill_CallerIsTargetAgent_ReturnsBodyInline(t *testing.T) {
	reg := newForkSkillRegistry(t)
	ctx := WithCallerAgentType(context.Background(), "document-editor")
	result := callSkillTool(t, reg, ctx, "docx-polish")

	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	sp, ok := result.Data["system_prompt"].(string)
	if !ok || sp == "" {
		t.Fatalf("Data[system_prompt] = %v, want the rendered body", result.Data["system_prompt"])
	}
}

// Branch 3: main agent (caller == "") calling a fork skill — must NOT get
// the body, and (this test asserts) the key is omitted outright because this
// branch never renders one at all — it returns before ever calling Execute
// (see the dynamic-injection tests below for why that ordering matters).
func TestSkillTool_ForkSkill_MainAgent_RoutesWithoutBody(t *testing.T) {
	reg := newForkSkillRegistry(t)
	result := callSkillTool(t, reg, context.Background(), "docx-polish")

	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	if _, ok := result.Data["system_prompt"]; ok {
		t.Fatalf("Data[system_prompt] present = %v, want the key absent entirely", result.Data["system_prompt"])
	}
	// Post-review fix (required-fix 3): the main-agent routing branch must
	// not carry "skill_name" either — react.go:1031 unconditionally marks
	// that name as the ActiveSkill regardless of whether system_prompt is
	// present, which would tag a skill whose body the main agent never
	// actually loaded as active (and later penalize/attribute memory facts
	// to it — promptbuild.go's memory fence, compact.go's fact extraction).
	if _, ok := result.Data["skill_name"]; ok {
		t.Fatalf("Data[skill_name] present = %v, want absent: routing must not mark a never-loaded skill active", result.Data["skill_name"])
	}
	forkAgent, ok := result.Data["fork_agent_type"].(string)
	if !ok || forkAgent != "document-editor" {
		t.Fatalf("Data[fork_agent_type] = %v, want document-editor", result.Data["fork_agent_type"])
	}
	if result.Content == "" {
		t.Fatal("Content should route the caller to task(agent_type=..., skill=...)")
	}
}

// Branch 4: a DIFFERENT subagent (not the bound agent, not the main agent)
// calling a fork skill — must fail, and must not leak the body either.
func TestSkillTool_ForkSkill_OtherSubagent_Fails(t *testing.T) {
	reg := newForkSkillRegistry(t)
	ctx := WithCallerAgentType(context.Background(), "coder")
	result := callSkillTool(t, reg, ctx, "docx-polish")

	if result.Status != models.CallStatusFailed {
		t.Fatalf("Status = %v, want Failed", result.Status)
	}
	if _, ok := result.Data["system_prompt"]; ok {
		t.Fatalf("Data[system_prompt] present = %v, want the key absent entirely", result.Data["system_prompt"])
	}
	if result.Error == "" {
		t.Fatal("Error should explain the skill is bound to a different agent")
	}
}

// ---------------------------------------------------------------------------
// Post-review required-fix 1: a fork skill's `!`command`` dynamic injection
// (pkg/skill/render.go's injectDynamicContext) has real side effects — it
// runs a real shell command. Before this fix, makeSkillHandler always called
// executor.Execute (which renders, and so runs dynamic injection) BEFORE
// deciding which of the four routing branches applied, so branch 3 (main
// agent) and branch 4 (a rejected, unrelated subagent) both triggered the
// command despite never receiving — or even being allowed to receive — the
// resulting body. A subagent with no right to use a skill could trigger its
// side effects merely by attempting the call. The fix resolves routing
// (registry.Get + CallerAgentTypeFromContext) BEFORE calling Execute, so
// branches 3/4 return without ever invoking it.
// ---------------------------------------------------------------------------

// newForkSkillRegistryWithSideEffect builds a fork skill (docx-polish, bound
// to document-editor) whose body contains a `!`command`` block that creates
// a marker file — a stand-in for any real side effect a skill's dynamic
// injection might perform (network call, disk write, ...). Returns the
// registry; the caller supplies markerPath so it can assert whether the
// command ran.
func newForkSkillRegistryWithSideEffect(t *testing.T, markerPath string) *Registry {
	t.Helper()
	dir := t.TempDir()
	content := "---\nname: docx-polish\ndescription: Polish docx.\ncontext: fork\nagent: document-editor\n---\n\n" +
		"Body: !`touch " + markerPath + "`\n"
	createSkillDirFull(t, filepath.Join(dir, "docx-polish"), content)
	reg := NewRegistry()
	if err := reg.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	return reg
}

func assertMarkerAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("side-effect marker %s exists, want the dynamic injection to have never run", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%s): %v", path, err)
	}
}

func assertMarkerPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("side-effect marker %s missing, want the dynamic injection to have run: %v", path, err)
	}
}

// Branch 3 (main agent, routed away from the body) must not run the fork
// skill's dynamic injection at all.
func TestSkillTool_ForkSkill_MainAgent_DoesNotRunDynamicInjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	reg := newForkSkillRegistryWithSideEffect(t, marker)

	result := callSkillTool(t, reg, context.Background(), "docx-polish")
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	assertMarkerAbsent(t, marker)
}

// Branch 4 (a different, rejected subagent) must not run the dynamic
// injection either — a Failed result must never have side effects.
func TestSkillTool_ForkSkill_OtherSubagent_DoesNotRunDynamicInjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	reg := newForkSkillRegistryWithSideEffect(t, marker)

	ctx := WithCallerAgentType(context.Background(), "coder")
	result := callSkillTool(t, reg, ctx, "docx-polish")
	if result.Status != models.CallStatusFailed {
		t.Fatalf("Status = %v, want Failed", result.Status)
	}
	assertMarkerAbsent(t, marker)
}

// Branch 2 (caller already inside the bound agent) must still run the
// dynamic injection — inline behavior is unchanged by this fix.
func TestSkillTool_ForkSkill_CallerIsTargetAgent_RunsDynamicInjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	reg := newForkSkillRegistryWithSideEffect(t, marker)

	ctx := WithCallerAgentType(context.Background(), "document-editor")
	result := callSkillTool(t, reg, ctx, "docx-polish")
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	assertMarkerPresent(t, marker)
}

// Branch 1 (non-fork skill) must still run the dynamic injection —
// unchanged by this fix.
func TestSkillTool_NonFork_RunsDynamicInjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	dir := t.TempDir()
	content := "---\nname: plain-skill\ndescription: A non-fork skill.\n---\n\n" +
		"Body: !`touch " + marker + "`\n"
	createSkillDirFull(t, filepath.Join(dir, "plain-skill"), content)
	reg := NewRegistry()
	if err := reg.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	result := callSkillTool(t, reg, context.Background(), "plain-skill")
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("Status = %v, want Completed", result.Status)
	}
	assertMarkerPresent(t, marker)
}

// TestSkillTool_UnknownSkill_ErrorMessageUnchanged pins that moving the
// registry.Get lookup earlier does not change the error message a model
// sees for a nonexistent skill name — that message is what the model
// self-corrects from, so its wording must stay Execute's own "not found"
// error, not a duplicate/rephrased one from the new pre-Execute lookup.
func TestSkillTool_UnknownSkill_ErrorMessageUnchanged(t *testing.T) {
	reg := NewRegistry()
	result := callSkillTool(t, reg, context.Background(), "nonexistent-skill")
	if result.Status != models.CallStatusFailed {
		t.Fatalf("Status = %v, want Failed", result.Status)
	}
	if result.Error == "" || !strings.Contains(result.Error, "not found") {
		t.Fatalf("Error = %q, want it to contain %q (Execute's own error)", result.Error, "not found")
	}
}
