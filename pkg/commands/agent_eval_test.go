package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeStatsPool wraps fakePool with a GetTask that becomes available only
// after a configurable delay — simulating the real Pool.Wait/GetTask split:
// Wait can bail on ctx while the task itself keeps running in the
// background and only later finishes and populates Stats (Pool.runTask's
// defer close(task.done) plus finishTask). Used to exercise
// dispatchEvalTask's short bounded poll for recovering a timed-out run's
// workload stats.
type fakeStatsPool struct {
	fakePool
	availableAfter time.Duration
	stats          *subagent.RunStats

	mu    sync.Mutex
	start time.Time
}

func (f *fakeStatsPool) GetTask(id string) (*subagent.Task, bool) {
	f.mu.Lock()
	if f.start.IsZero() {
		f.start = time.Now()
	}
	elapsed := time.Since(f.start)
	f.mu.Unlock()

	if elapsed < f.availableAfter {
		return &subagent.Task{ID: id, Status: subagent.TaskStatusRunning}, true
	}
	return &subagent.Task{ID: id, Status: subagent.TaskStatusTimedOut, Stats: f.stats}, true
}

// fakeNotFoundStatsPool's GetTask always reports the task missing — the
// state a real Pool.GetTask is in once some OTHER successful Wait has
// already consumed (deleted) the entry (see Pool.Wait's doc comment: "a
// later Wait or GetTask for the same taskID returns not-found"). Since
// StartTask always Stores the entry before dispatchEvalTask can ever reach
// recoverTimedOutStats, and a deleted entry never reappears, !ok here can
// never flip to ok no matter how long recoverTimedOutStats waits — so it
// must return immediately instead of burning the full statsPollBudget.
type fakeNotFoundStatsPool struct {
	fakePool
}

func (f *fakeNotFoundStatsPool) GetTask(id string) (*subagent.Task, bool) {
	return nil, false
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
// Workload stats (tool_calls/llm_turns/budget_exhausted/max_tool_calls):
// A. a normal completed run must persist them from task.Stats.
// B. a run whose Wait bails on ctx (dispatch timeout) must still recover
//    them from a short bounded GetTask poll when the pool supports it, and
//    must degrade to the pre-existing zero-value behavior (no hang, no
//    error) when it doesn't.
// ---------------------------------------------------------------------------

func TestRunOneCase_RecordsToolCallWorkloadStats(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "stats-case",
		"id: stats-case\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	pool := &fakePool{task: fakeTaskResult{
		Status: subagent.TaskStatusCompleted,
		Result: "done",
		Stats: &subagent.RunStats{
			ToolCalls:       7,
			LLMTurns:        4,
			MaxToolCalls:    20,
			BudgetExhausted: true,
			DurationMS:      1234,
			Model:           "glm-5.3",
		},
	}}

	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if !rec.HasStats {
		t.Error("HasStats = false, want true (stats were available from task.Stats)")
	}
	if rec.ToolCalls != 7 {
		t.Errorf("ToolCalls = %d, want 7", rec.ToolCalls)
	}
	if rec.LLMTurns != 4 {
		t.Errorf("LLMTurns = %d, want 4", rec.LLMTurns)
	}
	if rec.MaxToolCalls != 20 {
		t.Errorf("MaxToolCalls = %d, want 20", rec.MaxToolCalls)
	}
	if !rec.BudgetExhausted {
		t.Error("BudgetExhausted = false, want true")
	}
}

// TestRunOneCase_ToSummary_HasStatsWiresToolCallDistribution is the end-to-end
// test the review demanded: runOneCase's real output fed directly into
// buildEvalSummary, so a producer/consumer seam (rec.HasStats set by
// applyRunStats, read by buildEvalSummary) that every other test in this file
// exercises only from hand-built runRecord{HasStats: true} literals is
// actually connected at least once. Deleting `rec.HasStats = true` from
// applyRunStats must turn this test red (verified manually — see the fix
// report) even though every summary-only test with hand-set HasStats stays
// green.
func TestRunOneCase_ToSummary_HasStatsWiresToolCallDistribution(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "stats-case",
		"id: stats-case\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	pool := &fakePool{task: fakeTaskResult{
		Status: subagent.TaskStatusCompleted,
		Result: "done",
		Stats: &subagent.RunStats{
			ToolCalls:       7,
			LLMTurns:        4,
			MaxToolCalls:    20,
			BudgetExhausted: true,
			DurationMS:      1234,
			Model:           "glm-5.3",
		},
	}}

	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}

	s := buildEvalSummary("glm-5.3", 1, "5m", []runRecord{rec})
	if len(s.Roles) != 1 {
		t.Fatalf("len(Roles) = %d, want 1", len(s.Roles))
	}
	r := s.Roles[0]
	if r.ToolCallsP50 != 7 {
		t.Errorf("ToolCallsP50 = %v, want 7 (a real runOneCase record must feed the tool-call distribution)", r.ToolCallsP50)
	}
	if r.ToolCallsMax != 7 {
		t.Errorf("ToolCallsMax = %d, want 7", r.ToolCallsMax)
	}
	if r.BudgetExhaustedRuns != 1 {
		t.Errorf("BudgetExhaustedRuns = %d, want 1", r.BudgetExhaustedRuns)
	}
}

func TestDispatchEvalTask_RecoversToolCallStatsAfterTimeout(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "timeout-case",
		"id: timeout-case\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	pool := &fakeStatsPool{
		fakePool:       fakePool{waitErr: context.DeadlineExceeded},
		availableAfter: 40 * time.Millisecond,
		stats: &subagent.RunStats{
			ToolCalls: 12,
			LLMTurns:  5,
		},
	}

	started := time.Now()
	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("runOneCase took %v, want well under the 2s poll budget", elapsed)
	}
	if rec.Error == "" {
		t.Error("expected rec.Error to still be set for a dispatch timeout")
	}
	assertStatus(t, rec.Assertions, "dispatch", "fail")
	if rec.ToolCalls != 12 {
		t.Errorf("ToolCalls = %d, want 12 (recovered via the short post-timeout GetTask poll)", rec.ToolCalls)
	}
	if rec.LLMTurns != 5 {
		t.Errorf("LLMTurns = %d, want 5", rec.LLMTurns)
	}
}

func TestDispatchEvalTask_TimeoutDegradesGracefullyWithoutGetTask(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "analyst", "timeout-nogettask",
		"id: timeout-nogettask\nagent_type: analyst\ntask: \"analyze\"\nexpect:\n  - no_writes: true\n",
		nil)

	// Plain fakePool has no GetTask method at all, exercising the pool that
	// doesn't satisfy evalTaskPoolStats (mirrors any evalTaskPool test fake
	// that never grew the capability).
	pool := &fakePool{waitErr: context.DeadlineExceeded}

	started := time.Now()
	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("runOneCase took %v, want near-instant when the pool can't provide GetTask", elapsed)
	}
	if rec.Error == "" {
		t.Error("expected rec.Error to be set for a dispatch timeout")
	}
	if rec.ToolCalls != 0 || rec.LLMTurns != 0 || rec.MaxToolCalls != 0 || rec.BudgetExhausted {
		t.Errorf("expected zero-value workload stats when no stats could be recovered, got %+v", rec)
	}
}

// TestRecoverTimedOutStats_TaskNotFoundReturnsImmediately covers the case
// the review flagged: GetTask reporting !ok must not poll out the full
// statsPollBudget, because a deleted pool entry never comes back. The var
// (not const) statsPollBudget/statsPollInterval from item 5 lets this run
// with a shrunk budget so a regression back to "poll for the full budget"
// fails fast instead of needing a real 2s timeout to notice.
func TestRecoverTimedOutStats_TaskNotFoundReturnsImmediately(t *testing.T) {
	origInterval, origBudget := statsPollInterval, statsPollBudget
	statsPollInterval = time.Millisecond
	statsPollBudget = 2 * time.Second // deliberately left "real-sized": the assertion below is what proves we don't wait it out
	t.Cleanup(func() {
		statsPollInterval = origInterval
		statsPollBudget = origBudget
	})

	pool := &fakeNotFoundStatsPool{}
	started := time.Now()
	got := recoverTimedOutStats(pool, "missing-task")
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("recoverTimedOutStats took %v, want near-instant when GetTask reports the task not found (it can never reappear)", elapsed)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

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

// TestBuildEvalSummary_ErroredRunExcludedFromEverythingButDispatchErrors pins
// design §8 revision two item 6: a run that errored (a dispatch timeout) has
// no output, so it can be scored on nothing — it contributes to
// DispatchErrors and to no other denominator. Folding its zero duration into
// the mean is what understated two roles' cost by 12.5% in the M5-2 baseline.
// This test lost its ContractRate assertion when M5-4 removed contracts; the
// rule it guards is unrelated to contracts and outlives them.
func TestBuildEvalSummary_ErroredRunExcludedFromEverythingButDispatchErrors(t *testing.T) {
	records := []runRecord{
		{
			Case: "c1", AgentType: "widget", Fingerprint: "fp1", Tokens: 100, DurationMS: 1000,
			Assertions: []assertionResult{
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
				{Name: "mentions:Foo", Status: "pass"},
				{Name: "mentions:Bar", Status: "fail"},
				{Name: "not_mentions:Baz", Status: "pass"},
				{Name: "tool_calls_max:20", Status: "pass"},
				{Name: "no_writes", Status: "pass"},
				{Name: "tokens_max:1000", Status: "pass"},
			},
		},
		{
			// A dispatch timeout, shaped exactly like the real M5-2
			// baseline records (docs/AGENT_CAPABILITY_DESIGN.md §8): a lone
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
	// "dispatch" pseudo-assertion: across the two dispatched runs' 12
	// assertions, 11 pass and 1 fails (mentions:Bar) -> 11/12, not
	// (11+0)/13 with the errored run's synthetic fail folded in.
	wantRate := 11.0 / 12.0
	if diff := r.AssertionPassRate - wantRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("AssertionPassRate = %v, want %v (must exclude the errored run's synthetic dispatch assertion)", r.AssertionPassRate, wantRate)
	}
}

// TestBuildEvalSummary_ToolCallsMaxPicksMaxFromTheMiddle guards against the
// review-flagged gap: every pre-existing ToolCallsMax fixture in this file
// happens to put the largest value LAST ([2,4,20], [9,15]), so a mutated
// maxInt body that unconditionally does `max = v` (dropping the `if v >
// max` guard entirely) would still pass every one of them — the last
// iteration's assignment happens to equal the true max by coincidence of
// fixture ordering, not because the comparison ran. This fixture puts the
// max in the middle ([2, 20, 4]) so that mutation is caught.
func TestBuildEvalSummary_ToolCallsMaxPicksMaxFromTheMiddle(t *testing.T) {
	records := []runRecord{
		{Case: "c1", AgentType: "widget", Fingerprint: "fp1", HasStats: true, ToolCalls: 2},
		{Case: "c2", AgentType: "widget", Fingerprint: "fp1", HasStats: true, ToolCalls: 20},
		{Case: "c3", AgentType: "widget", Fingerprint: "fp1", HasStats: true, ToolCalls: 4},
	}
	s := buildEvalSummary("glm-5.3", 3, "5m", records)
	r := s.Roles[0]
	if r.ToolCallsMax != 20 {
		t.Errorf("ToolCallsMax = %d, want 20 (the middle sample, not the last)", r.ToolCallsMax)
	}
}

// ---------------------------------------------------------------------------
// Summary: tool-call distribution (p50/max) and budget_exhausted count.
//
// Unlike duration_ms/tokens (which the existing convention excludes for a
// dispatch-errored record — see buildEvalSummary's doc comment), tool_calls
// stats are drawn from every record that HAS them (rec.HasStats), including
// a dispatch-timeout record recovered via the post-timeout GetTask poll.
// That is the whole point of this feature: a timeout is exactly the
// right-tail sample a tool-call budget most needs to see.
// ---------------------------------------------------------------------------

func TestBuildEvalSummary_ToolCallsP50MaxAndBudgetExhaustedAcrossDispatchedAndTimeoutRuns(t *testing.T) {
	records := []runRecord{
		{Case: "c1", AgentType: "widget", Fingerprint: "fp1", HasStats: true, ToolCalls: 2},
		{Case: "c2", AgentType: "widget", Fingerprint: "fp1", HasStats: true, ToolCalls: 4},
		{
			// A dispatch timeout whose stats were nonetheless recovered: it
			// must count toward the tool_calls distribution even though it
			// contributes 0 to DispatchedRuns/AvgDurationMS/AvgTokens.
			Case: "c3", AgentType: "widget", Fingerprint: "fp1",
			Error:           "context deadline exceeded",
			Assertions:      []assertionResult{{Name: "dispatch", Status: "fail"}},
			HasStats:        true,
			ToolCalls:       20,
			BudgetExhausted: true,
		},
	}
	s := buildEvalSummary("glm-5.3", 3, "5m", records)
	if len(s.Roles) != 1 {
		t.Fatalf("len(Roles) = %d, want 1", len(s.Roles))
	}
	r := s.Roles[0]
	if r.StatsRuns != 3 {
		t.Errorf("StatsRuns = %d, want 3 (the denominator for p50/max/budget_exhausted: every record with HasStats)", r.StatsRuns)
	}
	if r.ToolCallsMax != 20 {
		t.Errorf("ToolCallsMax = %d, want 20 (the recovered timeout run's count)", r.ToolCallsMax)
	}
	if r.ToolCallsP50 != 4 {
		t.Errorf("ToolCallsP50 = %v, want 4 (median of [2,4,20])", r.ToolCallsP50)
	}
	if r.BudgetExhaustedRuns != 1 {
		t.Errorf("BudgetExhaustedRuns = %d, want 1", r.BudgetExhaustedRuns)
	}
	// Sanity: the timeout record still must not leak into the
	// dispatched-only aggregates.
	if r.DispatchedRuns != 2 {
		t.Errorf("DispatchedRuns = %d, want 2", r.DispatchedRuns)
	}
}

func TestBuildEvalSummary_ToolCallsFromTimeoutRunsOnly(t *testing.T) {
	records := []runRecord{
		{
			Case: "c1", AgentType: "widget", Fingerprint: "fp1",
			Error: "context deadline exceeded", Assertions: []assertionResult{{Name: "dispatch", Status: "fail"}},
			HasStats: true, ToolCalls: 9,
		},
		{
			Case: "c2", AgentType: "widget", Fingerprint: "fp1",
			Error: "context deadline exceeded", Assertions: []assertionResult{{Name: "dispatch", Status: "fail"}},
			HasStats: true, ToolCalls: 15,
		},
	}
	s := buildEvalSummary("glm-5.3", 2, "5m", records)
	r := s.Roles[0]
	if r.DispatchedRuns != 0 {
		t.Errorf("DispatchedRuns = %d, want 0 (every run in this fixture timed out)", r.DispatchedRuns)
	}
	if r.ToolCallsMax != 15 {
		t.Errorf("ToolCallsMax = %d, want 15", r.ToolCallsMax)
	}
	if r.ToolCallsP50 != 12 {
		t.Errorf("ToolCallsP50 = %v, want 12 (median of [9,15])", r.ToolCallsP50)
	}
}

func TestBuildEvalSummary_ToolCallsAllMissingStatsIsZeroNotPanic(t *testing.T) {
	records := []runRecord{
		{Case: "c1", AgentType: "widget", Fingerprint: "fp1"},
		{
			Case: "c2", AgentType: "widget", Fingerprint: "fp1",
			Error: "boom", Assertions: []assertionResult{{Name: "dispatch", Status: "fail"}},
		},
	}
	s := buildEvalSummary("glm-5.3", 2, "5m", records)
	r := s.Roles[0]
	if r.StatsRuns != 0 {
		t.Errorf("StatsRuns = %d, want 0", r.StatsRuns)
	}
	if r.ToolCallsMax != 0 {
		t.Errorf("ToolCallsMax = %d, want 0", r.ToolCallsMax)
	}
	if r.ToolCallsP50 != 0 {
		t.Errorf("ToolCallsP50 = %v, want 0", r.ToolCallsP50)
	}
	if r.BudgetExhaustedRuns != 0 {
		t.Errorf("BudgetExhaustedRuns = %d, want 0", r.BudgetExhaustedRuns)
	}
}

func TestRenderEvalSummaryMD_IncludesToolCallBudgetColumns(t *testing.T) {
	s := evalSummary{
		GeneratedAt: "2026-09-07T00:00:00Z",
		Model:       "glm-5.3",
		Runs:        3,
		Timeout:     "5m",
		Roles: []roleSummary{
			{
				AgentType:           "analyst",
				Fingerprint:         "deadbeef",
				Cases:               2,
				DispatchedRuns:      5,
				StatsRuns:           7,
				ToolCallsP50:        4.5,
				ToolCallsMax:        20,
				BudgetExhaustedRuns: 2,
			},
		},
	}
	md := renderEvalSummaryMD(s)
	if !strings.Contains(md, "tool_calls p50") {
		t.Errorf("summary.md missing a tool_calls p50 column/header:\n%s", md)
	}
	if !strings.Contains(md, "tool_calls max") {
		t.Errorf("summary.md missing a tool_calls max column/header:\n%s", md)
	}
	if !strings.Contains(md, "budget exhausted") {
		t.Errorf("summary.md missing a budget exhausted column/header:\n%s", md)
	}
	if !strings.Contains(md, "stats runs") {
		t.Errorf("summary.md missing a stats runs column/header (the denominator for tool_calls/budget_exhausted):\n%s", md)
	}
	if !strings.Contains(md, "4.5") {
		t.Errorf("summary.md does not render ToolCallsP50=4.5:\n%s", md)
	}
	if !strings.Contains(md, "20") {
		t.Errorf("summary.md does not render ToolCallsMax=20:\n%s", md)
	}
	// StatsRuns=7 must actually be the rendered cell value, not merely
	// present somewhere by coincidence (Cases=2, DispatchedRuns=5, etc. are
	// all small ints too) — assert the exact row.
	if !strings.Contains(md, "| analyst | deadbeef | 2 | 5 | ") {
		t.Fatalf("summary.md row prefix unexpected:\n%s", md)
	}
	rowStart := strings.Index(md, "| analyst | deadbeef | 2 | 5 | ")
	row := md[rowStart:]
	if idx := strings.Index(row, "\n"); idx >= 0 {
		row = row[:idx]
	}
	if !strings.Contains(row, "| 7 | 4.5 | 20 | 2 |") {
		t.Errorf("summary.md row does not render stats_runs=7 immediately before tool_calls p50/max/budget_exhausted:\n%s", row)
	}
}
