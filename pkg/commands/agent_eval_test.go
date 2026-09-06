package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/subagent"
)

// ---------------------------------------------------------------------------
// fake task pool — the "fake task tool" the M5-2 brief requires: exercises
// the harness's full chdir/dispatch/snapshot/assert chain without spending a
// single real token.
// ---------------------------------------------------------------------------

// fakeTaskResult is the fields of *subagent.Task the harness actually reads
// (Result/Usage/Stats/Status) — a plain value type, unlike subagent.Task
// itself (which embeds a sync.RWMutex and so must never be copied).
type fakeTaskResult struct {
	Status subagent.TaskStatus
	Result string
	Usage  *subagent.TokenUsage
	Stats  *subagent.RunStats
}

type fakePool struct {
	task       fakeTaskResult
	startErr   error
	waitErr    error
	panicMsg   string
	sideEffect func()
}

func (f *fakePool) newTask(id string) *subagent.Task {
	return &subagent.Task{
		ID:     id,
		Status: f.task.Status,
		Result: f.task.Result,
		Usage:  f.task.Usage,
		Stats:  f.task.Stats,
	}
}

func (f *fakePool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	if f.startErr != nil {
		return nil, f.startErr
	}
	if f.sideEffect != nil {
		f.sideEffect()
	}
	return f.newTask("fake-task-1"), nil
}

func (f *fakePool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return f.newTask(taskID), nil
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

// writeCase materializes a minimal case directory (manifest.yaml + fixture/)
// under dir and returns the evalCase describing it.
func writeCase(t *testing.T, dir, agentType, id string, manifestYAML string, fixtureFiles map[string]string) evalCase {
	t.Helper()
	caseDir := filepath.Join(dir, agentType, id)
	fixtureDir := filepath.Join(caseDir, "fixture")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for name, content := range fixtureFiles {
		p := filepath.Join(fixtureDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
	}
	cases, err := loadEvalCases(dir, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	for _, c := range cases {
		if c.AgentType == agentType && c.ID == id {
			return c
		}
	}
	t.Fatalf("case %s/%s not found after writeCase", agentType, id)
	return evalCase{}
}

// ---------------------------------------------------------------------------
// Materialization: fixture lands in a temp repo outside the project, and
// manifest.yaml is never reachable from it.
// ---------------------------------------------------------------------------

func TestMaterializeFixture_CopiesTreeAndNeverLeaksManifest(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "architect", "case-a",
		"id: case-a\nagent_type: architect\ntask: \"do something\"\nexpect:\n  - no_writes: true\n",
		map[string]string{"a.go": "package a\n", "sub/b.go": "package sub\n"})

	worktree, cleanup, err := materializeFixture(c.FixtureDir)
	if err != nil {
		t.Fatalf("materializeFixture: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(worktree, os.TempDir()) && !strings.Contains(worktree, "deepai-agent-eval-") {
		t.Fatalf("worktree %q does not look like an os.MkdirTemp path", worktree)
	}
	// The materialized tree must be outside the deepai project tree (the
	// fixture's own directory, and the eval/agent-cases corpus), not just
	// "some tmp-looking path".
	if strings.Contains(worktree, "eval-agent-eval-test") {
		t.Fatalf("worktree %q appears to be under the test's own project-relative dir", worktree)
	}

	for _, want := range []string{"a.go", filepath.Join("sub", "b.go")} {
		if _, err := os.Stat(filepath.Join(worktree, want)); err != nil {
			t.Errorf("expected materialized file %s: %v", want, err)
		}
	}

	if err := assertNoManifestLeak(worktree); err != nil {
		t.Fatalf("assertNoManifestLeak on a clean materialization: %v", err)
	}
}

func TestAssertNoManifestLeak_CatchesLeakedManifest(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "manifest.yaml"), []byte("id: leak\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := assertNoManifestLeak(worktree); err == nil {
		t.Fatal("expected assertNoManifestLeak to catch a leaked manifest.yaml, got nil error")
	}
}

func TestMaterializeFixture_EmptyFixtureIsValid(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "researcher", "empty-case",
		"id: empty-case\nagent_type: researcher\ntask: \"research something from scratch\"\nexpect:\n  - no_writes: true\n",
		nil)
	// Remove the fixture dir entirely to simulate "fixture/ can be empty" —
	// an empty *directory* still exists on disk from writeCase; also test the
	// case where it does not exist at all.
	if err := os.RemoveAll(c.FixtureDir); err != nil {
		t.Fatalf("remove fixture dir: %v", err)
	}
	worktree, cleanup, err := materializeFixture(c.FixtureDir)
	if err != nil {
		t.Fatalf("materializeFixture with missing fixture dir: %v", err)
	}
	defer cleanup()
	if err := assertNoManifestLeak(worktree); err != nil {
		t.Fatalf("assertNoManifestLeak: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Assertions: one pass/fail pair per assertion type, field_* skipped on a
// failed schema_parses.
// ---------------------------------------------------------------------------

// JSON keys below are lowercase/snake_case, matching agent.DesignDoc's json
// tags (M5-3): these fixtures decode against the REAL pkg/agent type now
// (agent_eval_schema.go no longer has a PascalCase eval-only mirror), so a
// capitalized "Agent"/"Decisions" key would fail schema validation (required
// property missing + additionalProperties:false) exactly the way a real
// model's mis-cased output would.
const designJSONTwoDecisions = `Here is my design:
{"agent":"architect","goal":"g","decisions":[{"topic":"t1","choice":"c1","rationale":"r1","alternatives":[],"reversible":true},{"topic":"t2","choice":"c2","rationale":"r2","alternatives":[],"reversible":false}],"components":[]}
`

const designJSONOneDecision = `{"agent":"architect","goal":"g","decisions":[{"topic":"t1","choice":"c1","rationale":"r1","alternatives":[],"reversible":true}],"components":[]}`

func TestEvaluateCase_SchemaParsesPassAndFieldMinCount(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "design"},
		{"field_min_count": map[string]any{"path": "decisions", "n": 2}},
	}}
	results := evaluateCase(m, designJSONTwoDecisions, nil, nil, false)
	assertStatus(t, results, "schema_parses:design", "pass")
	assertStatus(t, results, "field_min_count:decisions>=2", "pass")
}

func TestEvaluateCase_SchemaParsesFailSkipsFieldAssertions(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "design"},
		{"field_min_count": map[string]any{"path": "decisions", "n": 2}},
		{"field_nonempty_all": "decisions[].topic"},
	}}
	results := evaluateCase(m, "just plain prose, no JSON at all", nil, nil, false)
	assertStatus(t, results, "schema_parses:design", "fail")
	assertStatus(t, results, "field_min_count:decisions>=2", "skipped")
	assertStatus(t, results, "field_nonempty_all:decisions[].topic", "skipped")
}

func TestEvaluateCase_FieldMinCountFailsWhenUnderThreshold(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "design"},
		{"field_min_count": map[string]any{"path": "decisions", "n": 2}},
	}}
	results := evaluateCase(m, designJSONOneDecision, nil, nil, false)
	assertStatus(t, results, "schema_parses:design", "pass")
	assertStatus(t, results, "field_min_count:decisions>=2", "fail")
}

// TestEvaluateCase_FieldMinCountMultiWordJSONTagPath is the M5-3 review fix:
// fieldByPath used to match ONLY the Go field name via EqualFold, which
// happens to equal the json tag for a single-word field ("decisions" ==
// EqualFold "Decisions") but silently diverges for a multi-word one — the
// model-visible schema property is "scope_in" (RequirementsSpec's json tag),
// while the Go field is ScopeIn; EqualFold("ScopeIn", "scope_in") is false. A
// manifest author who copies the property name straight out of the schema
// Prompt (the obvious, expected thing to do) would get "field \"scope_in\"
// not found" — a hard error/fail, not a skip — and it would read as "the
// model produced no scope_in", not "the manifest's path syntax is wrong".
func TestEvaluateCase_FieldMinCountMultiWordJSONTagPath(t *testing.T) {
	reqJSON := `{"agent":"a","problem":"p","stories":[],"scope_in":["x","y"],"scope_out":[],"acceptance":[],"priorities":[]}`
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "requirements"},
		{"field_min_count": map[string]any{"path": "scope_in", "n": 2}},
	}}
	results := evaluateCase(m, reqJSON, nil, nil, false)
	assertStatus(t, results, "schema_parses:requirements", "pass")
	assertStatus(t, results, "field_min_count:scope_in>=2", "pass")
}

func TestEvaluateCase_FieldNonemptyAll(t *testing.T) {
	pass := `{"agent":"a","question":"q","answer":"ans","findings":[{"claim":"c1","evidence":[{"file":"f.go","line":1,"quote":"q"}],"confidence":"high"}]}`
	fail := `{"agent":"a","question":"q","answer":"ans","findings":[{"claim":"c1","evidence":[],"confidence":"high"}]}`
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "research"},
		{"field_nonempty_all": "findings[].evidence"},
	}}
	okResults := evaluateCase(m, pass, nil, nil, false)
	assertStatus(t, okResults, "field_nonempty_all:findings[].evidence", "pass")

	failResults := evaluateCase(m, fail, nil, nil, false)
	assertStatus(t, failResults, "field_nonempty_all:findings[].evidence", "fail")
}

// TestEvaluateCase_FieldNonemptyAllMultiWordSubfield covers a NESTED
// multi-word json-tag path (array[].subfield, both segments multi-word on
// the model-visible schema): RequirementsSpec.Stories[].SoThat is tagged
// `json:"so_that"`. This is exactly the shape an M5-4 product-manager/tester
// manifest is expected to write.
func TestEvaluateCase_FieldNonemptyAllMultiWordSubfield(t *testing.T) {
	pass := `{"agent":"a","problem":"p","stories":[{"role":"r","want":"w","so_that":"s"}],"scope_in":[],"scope_out":[],"acceptance":[],"priorities":[]}`
	fail := `{"agent":"a","problem":"p","stories":[{"role":"r","want":"w","so_that":""}],"scope_in":[],"scope_out":[],"acceptance":[],"priorities":[]}`
	m := caseManifest{Expect: []map[string]any{
		{"schema_parses": "requirements"},
		{"field_nonempty_all": "stories[].so_that"},
	}}
	okResults := evaluateCase(m, pass, nil, nil, false)
	assertStatus(t, okResults, "field_nonempty_all:stories[].so_that", "pass")

	failResults := evaluateCase(m, fail, nil, nil, false)
	assertStatus(t, failResults, "field_nonempty_all:stories[].so_that", "fail")
}

func TestEvaluateCase_MentionsAndNotMentions(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"mentions": []any{"filterTaskTool", "selectSubagentTools"}},
		{"not_mentions": []any{"SubagentRunner", "ExecuteFork"}},
	}}
	output := "the design touches filterTaskTool and invents a nonexistent SubagentRunner"
	results := evaluateCase(m, output, nil, nil, false)
	assertStatus(t, results, "mentions:filterTaskTool", "pass")
	assertStatus(t, results, "mentions:selectSubagentTools", "fail")
	assertStatus(t, results, "not_mentions:SubagentRunner", "fail")
	assertStatus(t, results, "not_mentions:ExecuteFork", "pass")
}

func TestEvaluateCase_ToolCallsMaxAndTokensMax(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"tool_calls_max": 5},
		{"tokens_max": 100},
	}}
	stats := &subagent.RunStats{ToolCalls: 6}
	usage := &subagent.TokenUsage{TotalTokens: 50}
	results := evaluateCase(m, "output", stats, usage, false)
	assertStatus(t, results, "tool_calls_max:5", "fail")
	assertStatus(t, results, "tokens_max:100", "pass")
}

func TestEvaluateCase_NoWrites(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{{"no_writes": true}}}
	clean := evaluateCase(m, "output", nil, nil, false)
	assertStatus(t, clean, "no_writes", "pass")
	dirty := evaluateCase(m, "output", nil, nil, true)
	assertStatus(t, dirty, "no_writes", "fail")
}

func assertStatus(t *testing.T, results []assertionResult, name, want string) {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			if r.Status != want {
				t.Errorf("assertion %s: status = %q, want %q (detail=%q)", name, r.Status, want, r.Detail)
			}
			return
		}
	}
	t.Errorf("assertion %s not found in results: %+v", name, results)
}

// ---------------------------------------------------------------------------
// no_writes end-to-end: a fake subagent that writes a file must be caught by
// the snapshot diff, not just the assertion-evaluation unit test above.
// ---------------------------------------------------------------------------

func TestRunOneCase_NoWritesViolationDetectedViaSnapshot(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "tester", "writes-case",
		"id: writes-case\nagent_type: tester\ntask: \"write a test\"\nexpect:\n  - no_writes: true\n",
		map[string]string{"keep.go": "package keep\n"})

	pool := &fakePool{
		task: fakeTaskResult{Status: subagent.TaskStatusCompleted, Result: "done"},
		sideEffect: func() {
			// Simulate a subagent writing a file via the bash tool, which
			// (per the M5-2 brief) inherits the harness's process cwd —
			// exactly what os.Chdir(worktree) before dispatch sets up.
			wd, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd inside side effect: %v", err)
			}
			if err := os.WriteFile(filepath.Join(wd, "unexpected.go"), []byte("package x\n"), 0o644); err != nil {
				t.Fatalf("side effect write: %v", err)
			}
		},
	}

	prevWD := mustGetwd(t)
	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if !rec.WriteViolation {
		t.Error("expected WriteViolation = true, got false")
	}
	assertStatus(t, rec.Assertions, "no_writes", "fail")

	if wd := mustGetwd(t); wd != prevWD {
		t.Fatalf("cwd not restored: got %q, want %q", wd, prevWD)
	}
}

func TestRunOneCase_CleanRunNoWritesPasses(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "tester", "clean-case",
		"id: clean-case\nagent_type: tester\ntask: \"write a test\"\nexpect:\n  - no_writes: true\n",
		map[string]string{"keep.go": "package keep\n"})

	pool := &fakePool{task: fakeTaskResult{Status: subagent.TaskStatusCompleted, Result: "done, no writes"}}
	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if rec.WriteViolation {
		t.Error("expected WriteViolation = false, got true")
	}
	assertStatus(t, rec.Assertions, "no_writes", "pass")
}

// ---------------------------------------------------------------------------
// chdir must be restored even when the dispatched task panics or errors.
// ---------------------------------------------------------------------------

func TestRunOneCase_ChdirRestoredOnPanic(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "panic-case",
		"id: panic-case\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	pool := &fakePool{panicMsg: "boom"}
	prevWD := mustGetwd(t)

	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase should convert the panic into a run error, not propagate: %v", err)
	}
	if rec.Error == "" {
		t.Error("expected rec.Error to record the recovered panic")
	}
	if wd := mustGetwd(t); wd != prevWD {
		t.Fatalf("cwd not restored after panic: got %q, want %q", wd, prevWD)
	}
}

func TestRunOneCase_ChdirRestoredOnDispatchError(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "err-case",
		"id: err-case\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	pool := &fakePool{startErr: errString("start failed")}
	prevWD := mustGetwd(t)

	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if rec.Error == "" {
		t.Error("expected rec.Error to be set for a dispatch error")
	}
	if wd := mustGetwd(t); wd != prevWD {
		t.Fatalf("cwd not restored after dispatch error: got %q, want %q", wd, prevWD)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// Fingerprint
// ---------------------------------------------------------------------------

func TestComputeFingerprint_SameInputSameFingerprint(t *testing.T) {
	a := computeFingerprint("system prompt", []string{"skill body"}, "")
	b := computeFingerprint("system prompt", []string{"skill body"}, "")
	if a != b {
		t.Fatalf("same input produced different fingerprints: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("fingerprint length = %d, want 8", len(a))
	}
}

func TestComputeFingerprint_OneByteChangeChangesFingerprint(t *testing.T) {
	a := computeFingerprint("system promptX", nil, "")
	b := computeFingerprint("system promptY", nil, "")
	if a == b {
		t.Fatalf("changing one byte of the system prompt did not change the fingerprint (%q)", a)
	}
}

func TestCaseFingerprint_ProjectYAMLOverrideWinsAndChangesFingerprint(t *testing.T) {
	repoRoot := t.TempDir()
	builtinFP, err := caseFingerprint("architect", repoRoot, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (builtin): %v", err)
	}

	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte("system_prompt: |\n  a totally different constitution\n"), 0o644); err != nil {
		t.Fatalf("write override yaml: %v", err)
	}
	overrideFP, err := caseFingerprint("architect", repoRoot, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (override): %v", err)
	}
	if builtinFP == overrideFP {
		t.Fatalf("project YAML override did not change the fingerprint (%q)", builtinFP)
	}
}

// TestCaseFingerprint_IncludesOutputSchemaPromptForBuiltinRole is the M5-3
// review fix: caseFingerprint used to hardcode the schema-Prompt component to
// "" unconditionally (post-M5-3 that premise is false for architect/
// product-manager/researcher/analyst, which now carry a mounted
// OutputSchema). This pins the fingerprint to the ACTUAL formula — sha256 of
// SystemPrompt + skill bodies + the role's resolved OutputSchema.Prompt —
// against the same agent.GetAgentTypeConfig the production executor reads,
// not a re-derivation.
func TestCaseFingerprint_IncludesOutputSchemaPromptForBuiltinRole(t *testing.T) {
	repoRoot := t.TempDir() // no .deepai/agents/architect.yaml: pure builtin path

	got, err := caseFingerprint("architect", repoRoot, nil)
	if err != nil {
		t.Fatalf("caseFingerprint: %v", err)
	}

	cfg := agent.GetAgentTypeConfig(agent.AgentTypeArchitect)
	if cfg.OutputSchema == nil || cfg.OutputSchema.Prompt == "" {
		t.Fatal("architect's builtin OutputSchema.Prompt is empty in this build; the fixture this test needs is gone")
	}
	want := computeFingerprint(cfg.SystemPrompt, nil, cfg.OutputSchema.Prompt)
	if got != want {
		t.Errorf("caseFingerprint(architect) = %q, want %q (sha256 of SystemPrompt+OutputSchema.Prompt)", got, want)
	}

	// The failure this guards against: computing the fingerprint as if the
	// schema Prompt were still "" (the pre-fix behavior) must NOT match —
	// otherwise adding/changing a field on DesignDoc (or any future schema
	// change with the system prompt held constant) would silently produce
	// the SAME fingerprint as before the change, letting a stale before-run
	// vouch for a new contract it never tested.
	stale := computeFingerprint(cfg.SystemPrompt, nil, "")
	if got == stale {
		t.Error("caseFingerprint(architect) matches the schema-blind (pre-fix) fingerprint; OutputSchema.Prompt is not being folded in")
	}
}

// TestCaseFingerprint_ProjectYAMLOutputSchemaOverride: a project YAML's own
// `output_schema:` key must override the builtin's mounted schema for
// fingerprint purposes too, resolved through the same closed namedSchemas
// table production uses (agent.NamedSchema) — not silently defaulting back
// to "" or to the builtin schema.
func TestCaseFingerprint_ProjectYAMLOutputSchemaOverride(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// architect's builtin schema is "design"; override this project's
	// architect role to point at "review" instead (an arbitrary different
	// named schema — the point is only that it differs from "design").
	yamlContent := "system_prompt: |\n  a totally different constitution\noutput_schema: review\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write override yaml: %v", err)
	}

	got, err := caseFingerprint("architect", repoRoot, nil)
	if err != nil {
		t.Fatalf("caseFingerprint: %v", err)
	}
	reviewSchema, ok := agent.NamedSchema("review")
	if !ok {
		t.Fatal("namedSchemas[\"review\"] missing")
	}
	want := computeFingerprint("a totally different constitution\n", nil, reviewSchema.Prompt)
	if got != want {
		t.Errorf("caseFingerprint = %q, want %q (project output_schema: review must be folded in)", got, want)
	}
}

// TestCaseFingerprint_ProjectYAMLUnknownOutputSchemaErrors: an unknown
// output_schema name in a project YAML must be a hard error at fingerprint
// time too, the same policy loadAgentYAML enforces for actual execution —
// silently falling back to "" would hide a typo'd manifest as a passing,
// schema-blind fingerprint.
func TestCaseFingerprint_ProjectYAMLUnknownOutputSchemaErrors(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlContent := "system_prompt: |\n  x\noutput_schema: not-a-real-schema\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	if _, err := caseFingerprint("architect", repoRoot, nil); err == nil {
		t.Fatal("expected error for unknown output_schema name, got nil")
	}
}

// ---------------------------------------------------------------------------
// loadEvalCases / filter
// ---------------------------------------------------------------------------

func TestLoadEvalCases_FiltersByAgentTypeAndTag(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "architect", "case-a",
		"id: case-a\nagent_type: architect\ntask: t\ntags: [go]\nexpect:\n  - no_writes: true\n", nil)
	writeCase(t, root, "researcher", "case-b",
		"id: case-b\nagent_type: researcher\ntask: t\ntags: [self-referential]\nexpect:\n  - no_writes: true\n", nil)

	all, err := loadEvalCases(root, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	byType, err := loadEvalCases(root, "architect")
	if err != nil {
		t.Fatalf("loadEvalCases filter by type: %v", err)
	}
	if len(byType) != 1 || byType[0].AgentType != "architect" {
		t.Fatalf("filter by agent_type failed: %+v", byType)
	}

	byTag, err := loadEvalCases(root, "self-referential")
	if err != nil {
		t.Fatalf("loadEvalCases filter by tag: %v", err)
	}
	if len(byTag) != 1 || byTag[0].ID != "case-b" {
		t.Fatalf("filter by tag failed: %+v", byTag)
	}
}

// ---------------------------------------------------------------------------
// runEvalCases: full multi-case, multi-run harness loop with a fake pool.
// ---------------------------------------------------------------------------

func TestRunEvalCases_MultiCaseMultiRunSerial(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "architect", "case-a",
		"id: case-a\nagent_type: architect\ntask: t\nexpect:\n  - no_writes: true\n  - tokens_max: 1000\n", nil)
	writeCase(t, root, "architect", "case-b",
		"id: case-b\nagent_type: architect\ntask: t\nexpect:\n  - no_writes: true\n", nil)

	cases, err := loadEvalCases(root, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}

	pool := &fakePool{task: fakeTaskResult{
		Status: subagent.TaskStatusCompleted,
		Result: "the design mentions filterTaskTool",
		Usage:  &subagent.TokenUsage{TotalTokens: 42},
		Stats:  &subagent.RunStats{ToolCalls: 3, DurationMS: 10},
	}}

	repoRoot := t.TempDir()
	records, err := runEvalCases(context.Background(), pool, repoRoot, nil, cases, evalOptions{Runs: 2}, nil)
	if err != nil {
		t.Fatalf("runEvalCases: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("len(records) = %d, want 4 (2 cases x 2 runs)", len(records))
	}
	fp := records[0].Fingerprint
	for _, r := range records {
		if r.Fingerprint != fp {
			t.Errorf("fingerprint differs across runs of the same agent_type: %q vs %q", r.Fingerprint, fp)
		}
		if r.Tokens != 42 {
			t.Errorf("Tokens = %d, want 42", r.Tokens)
		}
	}
}

// ---------------------------------------------------------------------------
// Incremental durability: a runWriter must make each record durable the
// moment it is written, and runEvalCases must call onRun (which writes) for
// every completed run BEFORE moving to the next one — so an interruption
// partway through a long real-model run never loses runs already paid for.
// ---------------------------------------------------------------------------

func TestRunWriter_RecordIsDurableWithoutClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	w, err := newRunWriter(path)
	if err != nil {
		t.Fatalf("newRunWriter: %v", err)
	}
	defer w.Close()

	if err := w.Write(runRecord{Case: "c1", Run: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(runRecord{Case: "c2", Run: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read through an INDEPENDENT handle, with w's file still open and
	// nothing closed — proves Write() itself made the bytes durable (via
	// Sync), not merely buffered pending a later Close().
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("runs.jsonl has %d line(s) before Close, want 2: %q", len(lines), data)
	}
	for i, line := range lines {
		var rec runRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%q)", i, err, line)
		}
	}
}

func TestRunEvalCases_InterruptionPreservesAlreadyWrittenRecords(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "architect", "case-a", "id: case-a\nagent_type: architect\ntask: t\nexpect:\n  - no_writes: true\n", nil)
	writeCase(t, root, "architect", "case-b", "id: case-b\nagent_type: architect\ntask: t\nexpect:\n  - no_writes: true\n", nil)
	writeCase(t, root, "architect", "case-c", "id: case-c\nagent_type: architect\ntask: t\nexpect:\n  - no_writes: true\n", nil)
	cases, err := loadEvalCases(root, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("len(cases) = %d, want 3", len(cases))
	}

	resultDir := t.TempDir()
	runsPath := filepath.Join(resultDir, "runs.jsonl")
	writer, err := newRunWriter(runsPath)
	if err != nil {
		t.Fatalf("newRunWriter: %v", err)
	}
	defer writer.Close()

	pool := &fakePool{task: fakeTaskResult{Status: subagent.TaskStatusCompleted, Result: "ok"}}
	repoRoot := t.TempDir()

	completed := 0
	simulatedInterruption := errString("simulated interruption after 2 completed runs")
	_, runErr := runEvalCases(context.Background(), pool, repoRoot, nil, cases, evalOptions{Runs: 1}, func(rec runRecord) error {
		if err := writer.Write(rec); err != nil {
			return err
		}
		completed++
		if completed == 2 {
			return simulatedInterruption
		}
		return nil
	})
	if runErr == nil {
		t.Fatal("expected runEvalCases to abort with the injected interruption error")
	}
	if !strings.Contains(runErr.Error(), "simulated interruption") {
		t.Fatalf("runEvalCases error = %v, want it to wrap the injected interruption", runErr)
	}

	// The whole point: case-c's run never happened, but case-a and case-b's
	// records must already be sitting durably in runs.jsonl — not lost
	// because the overall command errored out.
	data, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatalf("read %s: %v", runsPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("runs.jsonl has %d line(s) after interruption, want 2 (the completed runs before the injected error): %q", len(lines), data)
	}
	var seenCases []string
	for i, line := range lines {
		var rec runRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v (%q)", i, err, line)
		}
		seenCases = append(seenCases, rec.Case)
	}
	if seenCases[0] != "case-a" || seenCases[1] != "case-b" {
		t.Fatalf("persisted cases = %v, want [case-a case-b] (case-c must never have been reached)", seenCases)
	}
}

// ---------------------------------------------------------------------------
// compare: fingerprint-unchanged warning.
// ---------------------------------------------------------------------------

// baselineRole is a small helper building a fully-populated, already-passing
// roleSummary so each compare test only needs to override the one field it
// is exercising.
func baselineRole(agentType string) roleSummary {
	return roleSummary{
		AgentType:                agentType,
		Fingerprint:              "abc12345",
		DispatchedRuns:           8,
		ContractRate:             0.9,
		MentionsHitRate:          1.0,
		MentionsHits:             16,
		MentionsTotal:            16,
		NotMentionsViolationRate: 0,
		GuardViolations:          0,
		AvgDurationMS:            100000,
		AvgTokens:                0,
		DispatchErrors:           0,
		AssertionPassRate:        0.9,
	}
}

func TestRenderEvalCompare_WarnsWhenFingerprintUnchanged(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	afterSame := before
	afterSame.Roles = []roleSummary{baselineRole("architect")}
	out, err := renderEvalCompare(before, afterSame)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected a fingerprint-unchanged warning, got:\n%s", out)
	}

	afterChanged := before
	changedRole := baselineRole("architect")
	changedRole.Fingerprint = "def67890"
	afterChanged.Roles = []roleSummary{changedRole}
	out2, err := renderEvalCompare(before, afterChanged)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if strings.Contains(out2, "WARNING") {
		t.Errorf("did not expect a warning when the fingerprint changed, got:\n%s", out2)
	}
}

func TestRenderEvalCompare_RecheckWhenAvgDurationExceeds1_5x(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	after := before
	afterRole := baselineRole("architect")
	afterRole.Fingerprint = "def67890" // a real before/after, not a no-op
	afterRole.AvgDurationMS = 160001   // > 1.5x of 100000
	after.Roles = []roleSummary{afterRole}

	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if !strings.Contains(out, "RECHECK") {
		t.Errorf("expected a RECHECK row for a >1.5x duration regression, got:\n%s", out)
	}
	if !strings.Contains(out, "VERDICT: recheck") {
		t.Errorf("expected VERDICT: recheck (no FAIL-tier metric failed), got:\n%s", out)
	}
}

func TestRenderEvalCompare_FailWhenContractRateBelowThreshold(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	after := before
	afterRole := baselineRole("architect")
	afterRole.Fingerprint = "def67890"
	afterRole.ContractRate = 0.5 // < 80% threshold
	after.Roles = []roleSummary{afterRole}

	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if !strings.Contains(out, "VERDICT: fail") {
		t.Errorf("expected VERDICT: fail for a sub-80%% contract rate, got:\n%s", out)
	}
}

func TestRenderEvalCompare_InvalidWhenTooFewDispatchedRuns(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	after := before
	afterRole := baselineRole("architect")
	afterRole.Fingerprint = "def67890"
	afterRole.DispatchedRuns = 5 // < 7, undersampled
	after.Roles = []roleSummary{afterRole}

	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if !strings.Contains(out, "VERDICT: invalid") {
		t.Errorf("expected VERDICT: invalid when dispatched runs < 7, got:\n%s", out)
	}
}

func TestRenderEvalCompare_ErrorsOnModelMismatch(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	after := evalSummary{Model: "gpt-x", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	if _, err := renderEvalCompare(before, after); err == nil {
		t.Fatal("expected an error when Model differs between before and after (not comparable)")
	}
}

func TestRenderEvalCompare_ErrorsOnRunsMismatch(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	after := evalSummary{Model: "glm-5.3", Runs: 5, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	if _, err := renderEvalCompare(before, after); err == nil {
		t.Fatal("expected an error when Runs differs between before and after (not comparable)")
	}
}

// Timeout is a field this revision adds; a summary.json produced by the
// pre-revision harness has no such field at all, decoding as "". That must
// warn, not hard-error — see the harness brief's explicit call on this.
func TestRenderEvalCompare_MissingTimeoutWarnsNotErrors(t *testing.T) {
	before := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "", Roles: []roleSummary{baselineRole("architect")}}
	after := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: expected no error for a missing (unknown) before.Timeout, got %v", err)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(strings.ToLower(out), "timeout") {
		t.Errorf("expected a timeout-unknown WARNING, got:\n%s", out)
	}

	beforeMismatch := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "5m", Roles: []roleSummary{baselineRole("architect")}}
	afterMismatch := evalSummary{Model: "glm-5.3", Runs: 3, Timeout: "10m", Roles: []roleSummary{baselineRole("architect")}}
	if _, err := renderEvalCompare(beforeMismatch, afterMismatch); err == nil {
		t.Fatal("expected an error when both summaries HAVE a Timeout and they differ")
	}
}

func TestRenderEvalCompare_RoundTripsThroughJSON(t *testing.T) {
	s := buildEvalSummary("test-model", 3, "5m", []runRecord{
		{Case: "c1", AgentType: "architect", Fingerprint: "abc12345", Tokens: 100, Assertions: []assertionResult{
			{Name: "no_writes", Status: "pass"},
			{Name: "mentions:foo", Status: "pass"},
			{Name: "not_mentions:bar", Status: "fail"},
		}},
	})
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round evalSummary
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(round.Roles) != 1 || round.Roles[0].AgentType != "architect" {
		t.Fatalf("round trip mismatch: %+v", round)
	}
}

// ---------------------------------------------------------------------------
// harness stat-scope fix (M5-2 correction, pre-M5-3): a dispatch timeout
// (duration_ms=0, error != "") must not be counted into DispatchedRuns, the
// avg-duration/avg-token denominators, or any assertion/family counter. The
// old buildEvalSummary let the run's lone synthetic "dispatch" fail
// assertion feed into totalFail (diluting the diagnostic assertion-pass-rate
// column) and let its duration_ms=0 feed into the avg-duration numerator
// AND denominator (deflating avg_duration_ms — verified against the real
// 2026-09-06-glm-5.3 baseline: analyst's true dispatched-only mean is
// 152,923ms but the old summary.md reported 135,931ms, tester's is 196,229ms
// vs. the old 174,426ms — both ~12.5% low, exactly the shortfall from
// dividing by 9 instead of 8).
// ---------------------------------------------------------------------------

func TestBuildEvalSummary_ErroredRunExcludedFromEverythingButDispatchErrors(t *testing.T) {
	records := []runRecord{
		{
			Case: "c1", AgentType: "widget", Fingerprint: "fp1", Tokens: 100, DurationMS: 1000,
			Assertions: []assertionResult{
				{Name: "schema_parses:design", Status: "fail"},
				{Name: "mentions:Foo", Status: "pass"},
				{Name: "mentions:Bar", Status: "pass"},
				{Name: "not_mentions:Baz", Status: "pass"},
				{Name: "tool_calls_max:20", Status: "pass"},
				{Name: "no_writes", Status: "pass"},
				{Name: "tokens_max:1000", Status: "pass"},
			},
		},
		{
			Case: "c2", AgentType: "widget", Fingerprint: "fp1", Tokens: 300, DurationMS: 3000,
			Assertions: []assertionResult{
				{Name: "schema_parses:design", Status: "fail"},
				{Name: "mentions:Foo", Status: "pass"},
				{Name: "mentions:Bar", Status: "fail"},
				{Name: "not_mentions:Baz", Status: "pass"},
				{Name: "tool_calls_max:20", Status: "pass"},
				{Name: "no_writes", Status: "pass"},
				{Name: "tokens_max:1000", Status: "pass"},
			},
		},
		{
			// A dispatch timeout, shaped exactly like the real
			// eval/results/2026-09-06-glm-5.3/runs.jsonl records: a lone
			// synthetic "dispatch" fail assertion, duration_ms=0, tokens=0.
			Case: "c3", AgentType: "widget", Fingerprint: "fp1", Tokens: 0, DurationMS: 0,
			Error:      "context deadline exceeded",
			Assertions: []assertionResult{{Name: "dispatch", Status: "fail", Detail: "context deadline exceeded"}},
		},
	}
	s := buildEvalSummary("glm-5.3", 3, "5m", records)
	if len(s.Roles) != 1 {
		t.Fatalf("len(Roles) = %d, want 1", len(s.Roles))
	}
	r := s.Roles[0]

	if r.DispatchedRuns != 2 {
		t.Errorf("DispatchedRuns = %d, want 2 (the errored run must not count)", r.DispatchedRuns)
	}
	if r.DispatchErrors != 1 {
		t.Errorf("DispatchErrors = %d, want 1", r.DispatchErrors)
	}
	if r.ContractRate != 0 {
		t.Errorf("ContractRate = %v, want 0 (both dispatched runs failed schema_parses)", r.ContractRate)
	}
	if r.MentionsHits != 3 || r.MentionsTotal != 4 {
		t.Errorf("Mentions = %d/%d, want 3/4", r.MentionsHits, r.MentionsTotal)
	}
	if r.NotMentionsViolationRate != 0 {
		t.Errorf("NotMentionsViolationRate = %v, want 0", r.NotMentionsViolationRate)
	}
	if r.GuardViolations != 0 {
		t.Errorf("GuardViolations = %d, want 0", r.GuardViolations)
	}
	// The whole point: (1000+3000)/2 = 2000, NOT (1000+3000+0)/3 = 1333.33.
	if r.AvgDurationMS != 2000 {
		t.Errorf("AvgDurationMS = %v, want 2000 (mean of the 2 dispatched runs only, excluding the timeout's duration_ms=0)", r.AvgDurationMS)
	}
	if r.AvgTokens != 200 {
		t.Errorf("AvgTokens = %v, want 200 (mean of the 2 dispatched runs only)", r.AvgTokens)
	}
	// Diagnostic assertion-pass-rate must not contain the synthetic
	// "dispatch" pseudo-assertion: across the two dispatched runs' 14
	// assertions, 11 pass and 3 fail (schema_parses x2, mentions:Bar x1) ->
	// 11/14, not (11+0)/15 with the errored run's synthetic fail folded in.
	wantRate := 11.0 / 14.0
	if diff := r.AssertionPassRate - wantRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("AssertionPassRate = %v, want %v (must exclude the errored run's synthetic dispatch assertion)", r.AssertionPassRate, wantRate)
	}
}

// ---------------------------------------------------------------------------
// Golden test: recomputing the real 2026-09-06-glm-5.3 BEFORE baseline with
// the new harness must reproduce docs/AGENT_CAPABILITY_DESIGN.md §8's "新口径
// 基线(before)" table exactly, cell for cell. This is read-only (it never
// writes eval/results/**) — the actual regeneration of summary.json/
// summary.md is a one-time, separately-verified action per the M5-3
// harness-changeover brief.
//
// The path below points at "...-before": the M5-3 after-run reuses the
// original "2026-09-06-glm-5.3" directory name for its OWN output while it
// is in flight, so that name no longer holds the frozen before data this
// golden test needs — it holds a partial/different after run. Both the path
// AND a record-count guard exist so that if a future rename/relayout ever
// points this test at the wrong (or a truncated/in-progress) runs.jsonl
// again, it skips with a clear reason instead of failing with confusing
// "product-manager missing"/count-mismatch errors that look like a real
// regression. The expected count (45) is 5 roles × 9 dispatch attempts each
// (43 that actually dispatched + the 2 recorded timeouts — design §8's
// "超时/已派发 2/43" — analyst 9, architect 9, product-manager 9, researcher
// 9, tester 9), matching the archived eval/results/2026-09-06-glm-5.3-before
// /runs.jsonl on disk.
// ---------------------------------------------------------------------------

const wantGLM53BaselineRecordCount = 45

func TestBuildEvalSummary_MatchesGLM53DesignDocBaseline(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	runsPath := filepath.Join(repoRoot, "eval", "results", "2026-09-06-glm-5.3-before", "runs.jsonl")
	data, err := os.ReadFile(runsPath)
	if err != nil {
		t.Skipf("real baseline runs.jsonl not present (%v); skipping golden check", err)
	}

	var records []runRecord
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec runRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		records = append(records, rec)
	}
	// Defensive: this golden test's expected numbers are hardcoded against
	// ONE specific archived file. That file is gitignored (eval/results/**),
	// so a fresh clone — or a workspace mid-way through re-running eval,
	// where the file may briefly be truncated or hold a different run's
	// data — will not have exactly this content. Skip rather than fail: a
	// red result here must never be mistaken for a real regression by
	// someone who does not have (or does not yet have) this exact file.
	if len(records) != wantGLM53BaselineRecordCount {
		t.Skipf("eval/results/2026-09-06-glm-5.3-before/runs.jsonl has %d records, want %d (not the archived before-baseline this golden test expects, or in-flight from a concurrent eval run); skipping golden check", len(records), wantGLM53BaselineRecordCount)
	}

	s := buildEvalSummary("glm-5.3", 3, "5m", records)

	type want struct {
		dispatched, dispatchErrors int
		contractRate               float64
		mentionsHits, mentionsTot  int
		notMentionsViolationRate   float64
		guardViolations            int
		avgDurationMS              float64 // rounded to nearest ms, per the doc table
		diagAssertPass, diagTotal  int     // pass count / (pass+fail) count, diagnostic column
	}
	wants := map[string]want{
		"analyst":         {8, 1, 0, 16, 16, 0, 0, 152923, 56, 64},
		"architect":       {9, 0, 0, 15, 18, 0, 0, 200192, 60, 72},
		"product-manager": {9, 0, 0, 16, 18, 0, 0, 115466, 61, 72},
		"researcher":      {9, 0, 0, 15, 15, 0, 0, 125406, 60, 69},
		"tester":          {8, 1, 0, 13, 13, 0, 0, 196229, 53, 61},
	}

	seen := map[string]bool{}
	var (
		combinedDispatched, combinedTimeouts        int
		combinedMentionsHits, combinedMentionsTotal int
		combinedDiagPass, combinedDiagTotal         int
		combinedDurationWeighted                    float64
	)
	for _, r := range s.Roles {
		w, ok := wants[r.AgentType]
		if !ok {
			t.Errorf("unexpected agent_type %q in recomputed summary", r.AgentType)
			continue
		}
		seen[r.AgentType] = true
		if r.DispatchedRuns != w.dispatched {
			t.Errorf("%s: DispatchedRuns = %d, want %d", r.AgentType, r.DispatchedRuns, w.dispatched)
		}
		if r.DispatchErrors != w.dispatchErrors {
			t.Errorf("%s: DispatchErrors = %d, want %d", r.AgentType, r.DispatchErrors, w.dispatchErrors)
		}
		if r.ContractRate != w.contractRate {
			t.Errorf("%s: ContractRate = %v, want %v", r.AgentType, r.ContractRate, w.contractRate)
		}
		if r.MentionsHits != w.mentionsHits || r.MentionsTotal != w.mentionsTot {
			t.Errorf("%s: Mentions = %d/%d, want %d/%d", r.AgentType, r.MentionsHits, r.MentionsTotal, w.mentionsHits, w.mentionsTot)
		}
		if r.NotMentionsViolationRate != w.notMentionsViolationRate {
			t.Errorf("%s: NotMentionsViolationRate = %v, want %v", r.AgentType, r.NotMentionsViolationRate, w.notMentionsViolationRate)
		}
		if r.GuardViolations != w.guardViolations {
			t.Errorf("%s: GuardViolations = %d, want %d", r.AgentType, r.GuardViolations, w.guardViolations)
		}
		gotMS := round(r.AvgDurationMS)
		if gotMS != w.avgDurationMS {
			t.Errorf("%s: AvgDurationMS (rounded) = %v, want %v (raw=%v)", r.AgentType, gotMS, w.avgDurationMS, r.AvgDurationMS)
		}
		wantDiagRate := float64(w.diagAssertPass) / float64(w.diagTotal)
		if diff := r.AssertionPassRate - wantDiagRate; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: AssertionPassRate (diagnostic) = %v, want %v (%d/%d)", r.AgentType, r.AssertionPassRate, wantDiagRate, w.diagAssertPass, w.diagTotal)
		}

		combinedDispatched += r.DispatchedRuns
		combinedTimeouts += r.DispatchErrors
		combinedMentionsHits += r.MentionsHits
		combinedMentionsTotal += r.MentionsTotal
		combinedDiagPass += w.diagAssertPass
		combinedDiagTotal += w.diagTotal
		combinedDurationWeighted += r.AvgDurationMS * float64(r.DispatchedRuns)
	}
	for agentType := range wants {
		if !seen[agentType] {
			t.Errorf("agent_type %q missing from recomputed summary", agentType)
		}
	}

	if combinedDispatched != 43 {
		t.Errorf("combined DispatchedRuns = %d, want 43", combinedDispatched)
	}
	if combinedTimeouts != 2 {
		t.Errorf("combined DispatchErrors = %d, want 2", combinedTimeouts)
	}
	if combinedMentionsHits != 75 || combinedMentionsTotal != 80 {
		t.Errorf("combined Mentions = %d/%d, want 75/80", combinedMentionsHits, combinedMentionsTotal)
	}
	if got := round(combinedDurationWeighted / float64(combinedDispatched)); got != 157274 {
		t.Errorf("combined avg duration (weighted, rounded) = %v, want 157274", got)
	}
	if combinedDiagPass != 290 || combinedDiagTotal != 338 {
		t.Errorf("combined diagnostic assert pass = %d/%d, want 290/338", combinedDiagPass, combinedDiagTotal)
	}
}

func round(f float64) float64 {
	if f < 0 {
		return -round(-f)
	}
	return float64(int64(f + 0.5))
}

// ---------------------------------------------------------------------------
// Real corpus structural check: eval/agent-cases must load and materialize
// cleanly, with ground truth (manifest.yaml) never reachable from any case's
// worktree. Does not dispatch any task — no tokens spent.
// ---------------------------------------------------------------------------

func TestRealAgentCasesCorpus_LoadsAndMaterializesCleanly(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	casesDir := filepath.Join(repoRoot, "eval", "agent-cases")
	if _, err := os.Stat(casesDir); err != nil {
		t.Skipf("eval/agent-cases not present (%v); skipping real-corpus check", err)
	}

	cases, err := loadEvalCases(casesDir, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	if len(cases) != 15 {
		t.Fatalf("len(cases) = %d, want 15 (5 agent types x 3 cases)", len(cases))
	}

	wantAgentTypes := map[string]int{}
	for _, c := range cases {
		wantAgentTypes[c.AgentType]++

		if c.Manifest.Task == "" {
			t.Errorf("case %s/%s: empty task", c.AgentType, c.ID)
		}
		if len(c.Manifest.Expect) == 0 {
			t.Errorf("case %s/%s: no expect assertions", c.AgentType, c.ID)
		}

		worktree, cleanup, err := materializeFixture(c.FixtureDir)
		if err != nil {
			t.Fatalf("case %s/%s: materializeFixture: %v", c.AgentType, c.ID, err)
		}
		if err := assertNoManifestLeak(worktree); err != nil {
			cleanup()
			t.Fatalf("case %s/%s: %v", c.AgentType, c.ID, err)
		}
		for _, cf := range c.Manifest.ContextFiles {
			if filepath.IsAbs(cf) {
				continue
			}
			if _, err := os.Stat(filepath.Join(worktree, cf)); err != nil {
				cleanup()
				t.Fatalf("case %s/%s: context_files entry %s not found in materialized fixture: %v", c.AgentType, c.ID, cf, err)
			}
		}
		cleanup()
	}
	for _, agentType := range []string{"architect", "product-manager", "researcher", "analyst", "tester"} {
		if wantAgentTypes[agentType] != 3 {
			t.Errorf("agent_type %s: %d cases, want 3", agentType, wantAgentTypes[agentType])
		}
	}
}

// ---------------------------------------------------------------------------
// buildEvalModelRegistry: no network calls, just resolution.
// ---------------------------------------------------------------------------

func TestBuildEvalModelRegistry_ResolvesDefault(t *testing.T) {
	cfg := Config{Provider: "anthropic", Model: "claude-x"}
	reg, alias, err := buildEvalModelRegistry(cfg, "")
	if err != nil {
		t.Fatalf("buildEvalModelRegistry: %v", err)
	}
	if reg == nil || alias == "" {
		t.Fatalf("expected a non-nil registry and non-empty alias, got reg=%v alias=%q", reg, alias)
	}
}

func TestBuildEvalModelRegistry_NoProviderConfigured(t *testing.T) {
	_, _, err := buildEvalModelRegistry(Config{}, "")
	if err == nil {
		t.Fatal("expected an error when no provider is configured")
	}
}
