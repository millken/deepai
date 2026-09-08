package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
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
	results := evaluateCase(m, output, nil, nil, false, nil, "")
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
	results := evaluateCase(m, "output", stats, usage, false, nil, "")
	assertStatus(t, results, "tool_calls_max:5", "fail")
	assertStatus(t, results, "tokens_max:100", "pass")
}

func TestEvaluateCase_NoWrites(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{{"no_writes": true}}}
	clean := evaluateCase(m, "output", nil, nil, false, nil, "")
	assertStatus(t, clean, "no_writes", "pass")
	dirty := evaluateCase(m, "output", nil, nil, true, nil, "")
	assertStatus(t, dirty, "no_writes", "fail")
}

// ---------------------------------------------------------------------------
// Assertions: files_changed / file_contains / file_not_contains — the
// "did the edit actually land" assertions the M6 batch-editing corpus needs
// (see docs brief for this period). changed is always an ABSOLUTE path list
// (chat.WorktreeSnapshot.ChangedSince's contract — root-joined, see
// pkg/chat/review.go), exactly like runOneCase hands evaluateCase; worktree
// is the same root those absolute paths were joined against, so evaluateCase
// can convert changed -> worktree-relative paths comparable to the
// manifest's repo-relative expectations, and can open file_contains/
// file_not_contains's target files by joining worktree+path itself.
// ---------------------------------------------------------------------------

func TestEvaluateCase_FilesChangedExactMatch(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"files_changed": []any{"a.go", "sub/b.go"}},
	}}
	worktree := t.TempDir()
	changed := []string{
		filepath.Join(worktree, "a.go"),
		filepath.Join(worktree, "sub", "b.go"),
	}
	results := evaluateCase(m, "output", nil, nil, false, changed, worktree)
	assertStatus(t, results, "files_changed", "pass")
}

func TestEvaluateCase_FilesChangedExtraFileFails(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"files_changed": []any{"a.go"}},
	}}
	worktree := t.TempDir()
	changed := []string{
		filepath.Join(worktree, "a.go"),
		filepath.Join(worktree, "oops.go"),
	}
	results := evaluateCase(m, "output", nil, nil, false, changed, worktree)
	assertStatus(t, results, "files_changed", "fail")
	detail := detailFor(t, results, "files_changed")
	if !strings.Contains(detail, "oops.go") {
		t.Errorf("detail %q does not name the unexpected extra file oops.go", detail)
	}
}

func TestEvaluateCase_FilesChangedMissingFileFails(t *testing.T) {
	m := caseManifest{Expect: []map[string]any{
		{"files_changed": []any{"a.go", "b.go"}},
	}}
	worktree := t.TempDir()
	changed := []string{filepath.Join(worktree, "a.go")}
	results := evaluateCase(m, "output", nil, nil, false, changed, worktree)
	assertStatus(t, results, "files_changed", "fail")
	detail := detailFor(t, results, "files_changed")
	if !strings.Contains(detail, "b.go") {
		t.Errorf("detail %q does not name the missing file b.go", detail)
	}
}

// ---------------------------------------------------------------------------
// relativeChangedPaths — portable symlinked-worktree regression coverage.
//
// materializeFixture's os.MkdirTemp result is the LITERAL path handed in as
// worktree, but a changed path comes back from git (via
// chat.WorktreeSnapshot) already resolved through any symlink in that
// path's prefix — e.g. macOS's default TMPDIR (/var/folders/... which is
// itself /private/var/folders/... via /var -> private/var). A plain
// filepath.Rel(worktree, p) then fails (p isn't under the literal,
// unresolved worktree string), and relativeChangedPaths falls back to
// resolving worktree with filepath.EvalSymlinks before retrying Rel.
//
// os.Symlink here (rather than relying on the host's TMPDIR happening to be
// a symlink, which is true on macOS but NOT on a typical Linux CI runner —
// so a test relying on that accident never even exercises this branch
// there) makes the fallback path itself portable and provable on any OS.
func TestRelativeChangedPaths_ResolvesSymlinkedWorktreeToCleanRelativePath(t *testing.T) {
	realDir := t.TempDir()
	linkParent := t.TempDir()
	linkPath := filepath.Join(linkParent, "link-to-real")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skipf("os.Symlink unsupported on this platform: %v", err)
	}

	// changed carries the RESOLVED path — exactly what chat.WorktreeSnapshot
	// (backed by `git rev-parse --show-toplevel`) would hand back when the
	// worktree is reached through linkPath, a symlink.
	resolvedRoot, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(linkPath): %v", err)
	}
	changedAbs := filepath.Join(resolvedRoot, "pkg", "a.go")
	if err := os.MkdirAll(filepath.Dir(changedAbs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(changedAbs, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := relativeChangedPaths([]string{changedAbs}, linkPath)
	want := []string{"pkg/a.go"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("relativeChangedPaths(%q, worktree=%q) = %v, want %v", changedAbs, linkPath, got, want)
	}
}

// TestRelativeChangedPaths_PathOutsideWorktreeStaysAbsoluteNotDotDotGarbage
// is the regression test for the review-flagged missing `..` guard on the
// SECOND filepath.Rel call (the EvalSymlinks-resolved fallback): before the
// fix, a changed path that isn't actually under the worktree (real or
// resolved) produced a garbage "../../../../..." relative path instead of
// falling back to the absolute path, as the function's own doc comment has
// always claimed it does. No symlink is needed to reproduce this — any two
// unrelated temp directories are enough, since filepath.Rel across them
// still "succeeds" (err == nil) by climbing out with "..".
func TestRelativeChangedPaths_PathOutsideWorktreeStaysAbsoluteNotDotDotGarbage(t *testing.T) {
	worktree := t.TempDir()
	unrelated := t.TempDir()
	otherFile := filepath.Join(unrelated, "x.go")

	got := relativeChangedPaths([]string{otherFile}, worktree)
	if len(got) != 1 {
		t.Fatalf("relativeChangedPaths returned %d entries, want 1", len(got))
	}
	if strings.HasPrefix(got[0], "..") {
		t.Errorf("relativeChangedPaths(%q, worktree=%q) = %q — dot-dot garbage, want the absolute path preserved", otherFile, worktree, got[0])
	}
	want := filepath.ToSlash(otherFile)
	if got[0] != want {
		t.Errorf("relativeChangedPaths(%q, worktree=%q) = %q, want the absolute path %q unchanged", otherFile, worktree, got[0], want)
	}
}

func TestEvaluateCase_FileContainsPassAndFail(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.go"), []byte("package a\n\nfunc NewName() {}\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	m := caseManifest{Expect: []map[string]any{
		{"file_contains": []any{
			map[string]any{"path": "a.go", "text": "NewName"},
			map[string]any{"path": "a.go", "text": "DoesNotExist"},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	assertStatus(t, results, "file_contains:a.go:NewName", "pass")
	assertStatus(t, results, "file_contains:a.go:DoesNotExist", "fail")
}

func TestEvaluateCase_FileContainsMissingFileFailsCleanly(t *testing.T) {
	worktree := t.TempDir()
	m := caseManifest{Expect: []map[string]any{
		{"file_contains": []any{
			map[string]any{"path": "nope.go", "text": "anything"},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	assertStatus(t, results, "file_contains:nope.go:anything", "fail")
	detail := detailFor(t, results, "file_contains:nope.go:anything")
	if detail == "" {
		t.Fatal("expected a non-empty detail explaining the missing file, got none")
	}
}

func TestEvaluateCase_FileNotContainsPassAndFail(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.go"), []byte("package a\n\nfunc NewName() {}\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	m := caseManifest{Expect: []map[string]any{
		{"file_not_contains": []any{
			map[string]any{"path": "a.go", "text": "OldName"},
			map[string]any{"path": "a.go", "text": "NewName"},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	assertStatus(t, results, "file_not_contains:a.go:OldName", "pass")
	assertStatus(t, results, "file_not_contains:a.go:NewName", "fail")
}

func TestEvaluateCase_FileNotContainsMissingFileDoesNotPanicAndFails(t *testing.T) {
	worktree := t.TempDir()
	m := caseManifest{Expect: []map[string]any{
		{"file_not_contains": []any{
			map[string]any{"path": "nope.go", "text": "anything"},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	assertStatus(t, results, "file_not_contains:nope.go:anything", "fail")
}

// ---------------------------------------------------------------------------
// "cannot-fail" manifest shapes (M6 review defect #4): a file_contains with
// blank text, a file_contains written as a map instead of a list, and an
// unrecognized expect key all used to be silently accepted as ZERO
// assertions (or, for blank text, one assertion that can never fail —
// strings.Contains(x, "") is always true). FileEditAssertionFailures exists
// specifically so a real failure can never go invisible; a manifest that
// asserts nothing at all, without even a warning, is a sharper version of
// exactly that same problem. evaluateCase must now turn each of these into
// an explicit "fail" — never a silent no-op, never an always-true pass.
// ---------------------------------------------------------------------------

func TestEvaluateCase_FileContainsBlankTextFailsRatherThanAlwaysPassing(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := caseManifest{Expect: []map[string]any{
		{"file_contains": []any{
			map[string]any{"path": "a.go", "text": ""},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	if len(results) != 1 {
		t.Fatalf("evaluateCase produced %d assertions, want exactly 1 (the malformed entry made explicit): %+v", len(results), results)
	}
	if results[0].Status != "fail" {
		t.Errorf("blank-text file_contains status = %q, want %q (strings.Contains(x, \"\") is always true — this must never silently pass)", results[0].Status, "fail")
	}
}

func TestEvaluateCase_FileContainsBlankPathFails(t *testing.T) {
	worktree := t.TempDir()
	m := caseManifest{Expect: []map[string]any{
		{"file_not_contains": []any{
			map[string]any{"path": "", "text": "OldName"},
		}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	if len(results) != 1 || results[0].Status != "fail" {
		t.Fatalf("blank-path file_not_contains = %+v, want exactly one fail assertion", results)
	}
}

func TestEvaluateCase_FileContainsAsMapInsteadOfListProducesExplicitFail(t *testing.T) {
	worktree := t.TempDir()
	// A manifest typo: file_contains written as a single mapping instead of
	// a list of mappings. Before the fix, toFileTextExpectations returned
	// nil for a non-list value and evaluateCase's switch had no default —
	// this expect entry produced ZERO assertions, with no warning anywhere.
	m := caseManifest{Expect: []map[string]any{
		{"file_contains": map[string]any{"path": "a.go", "text": "Foo"}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, worktree)
	if len(results) != 1 {
		t.Fatalf("evaluateCase produced %d assertions for a map-shaped file_contains, want exactly 1 (an explicit fail), got: %+v", len(results), results)
	}
	if results[0].Status != "fail" {
		t.Errorf("status = %q, want %q", results[0].Status, "fail")
	}
}

func TestEvaluateCase_UnknownExpectKeyProducesExplicitFailNotSilentSkip(t *testing.T) {
	// A manifest typo: file_content / files_change instead of the real
	// keys. Before the fix, evaluateCase's switch had no default case, so
	// an unrecognized key silently contributed nothing at all.
	m := caseManifest{Expect: []map[string]any{
		{"file_content": []any{map[string]any{"path": "a.go", "text": "Foo"}}},
	}}
	results := evaluateCase(m, "output", nil, nil, false, nil, t.TempDir())
	if len(results) != 1 {
		t.Fatalf("evaluateCase produced %d assertions for an unknown expect key, want exactly 1 (an explicit fail), got: %+v", len(results), results)
	}
	if results[0].Status != "fail" {
		t.Errorf("status = %q, want %q", results[0].Status, "fail")
	}
}

// TestLoadEvalCases_RejectsMalformedExpectShapes is the load-time half of
// the same fix: a real manifest with these defects must never even reach
// materialization/dispatch — loadEvalCases (via validateManifestExpect)
// rejects it outright, with a descriptive error naming the manifest.
func TestLoadEvalCases_RejectsMalformedExpectShapes(t *testing.T) {
	cases := []struct {
		name         string
		manifestYAML string
	}{
		{
			name:         "unknown expect key",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - files_change: [a.go]\n",
		},
		{
			name:         "file_contains as a map instead of a list",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - file_contains:\n      path: a.go\n      text: Foo\n",
		},
		{
			name:         "file_contains blank text",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - file_contains:\n      - path: a.go\n        text: \"\"\n",
		},
		{
			name:         "file_contains blank path",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - file_contains:\n      - path: \"\"\n        text: Foo\n",
		},
		{
			name:         "no_writes not a bool",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - no_writes: \"true\"\n",
		},
		{
			name:         "tool_calls_max not an integer",
			manifestYAML: "id: bad\nagent_type: coder\ntask: \"x\"\nexpect:\n  - tool_calls_max: \"forty\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caseDir := filepath.Join(root, "coder", "bad-case")
			if err := os.MkdirAll(caseDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(caseDir, "manifest.yaml"), []byte(tc.manifestYAML), 0o644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			if _, err := loadEvalCases(root, ""); err == nil {
				t.Errorf("loadEvalCases: expected an error for %s, got nil", tc.name)
			}
		})
	}
}

func detailFor(t *testing.T, results []assertionResult, name string) string {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r.Detail
		}
	}
	t.Fatalf("assertion %s not found in results: %+v", name, results)
	return ""
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

// TestRunOneCase_FilesChangedAndFileContainsEndToEnd exercises the full
// chdir/dispatch/snapshot/evaluate chain for the new "did the edit actually
// land" assertion family (files_changed/file_contains/file_not_contains),
// the same way TestRunOneCase_NoWritesViolationDetectedViaSnapshot exercises
// no_writes: a fake subagent edits a.go exactly as the case demands, and the
// resulting runRecord's assertions must all pass — proving the harness (not
// just evaluateCase in isolation) wires changed/worktree through correctly.
func TestRunOneCase_FilesChangedAndFileContainsEndToEnd(t *testing.T) {
	root := t.TempDir()
	c := writeCase(t, root, "coder", "rename-case",
		"id: rename-case\nagent_type: coder\ntask: \"rename OldName to NewName in a.go\"\nexpect:\n  - files_changed: [a.go]\n  - file_contains:\n      - path: a.go\n        text: NewName\n  - file_not_contains:\n      - path: a.go\n        text: OldName\n",
		map[string]string{"a.go": "package a\n\nfunc OldName() {}\n"})

	pool := &fakePool{
		task: fakeTaskResult{Status: subagent.TaskStatusCompleted, Result: "done"},
		sideEffect: func() {
			wd, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd inside side effect: %v", err)
			}
			if err := os.WriteFile(filepath.Join(wd, "a.go"), []byte("package a\n\nfunc NewName() {}\n"), 0o644); err != nil {
				t.Fatalf("side effect write: %v", err)
			}
		},
	}

	rec, err := runOneCase(context.Background(), pool, c, 1, "deadbeef", evalOptions{})
	if err != nil {
		t.Fatalf("runOneCase: %v", err)
	}
	if !rec.WriteViolation {
		t.Error("expected WriteViolation = true (a.go was edited), got false")
	}
	assertStatus(t, rec.Assertions, "files_changed", "pass")
	assertStatus(t, rec.Assertions, "file_contains:a.go:NewName", "pass")
	assertStatus(t, rec.Assertions, "file_not_contains:a.go:OldName", "pass")
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
	a := computeFingerprint("a full assembled system prompt")
	b := computeFingerprint("a full assembled system prompt")
	if a != b {
		t.Fatalf("same input produced different fingerprints: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("fingerprint length = %d, want 8", len(a))
	}
}

func TestComputeFingerprint_OneByteChangeChangesFingerprint(t *testing.T) {
	a := computeFingerprint("system promptX")
	b := computeFingerprint("system promptY")
	if a == b {
		t.Fatalf("changing one byte of the system prompt did not change the fingerprint (%q)", a)
	}
}

func TestCaseFingerprint_ProjectYAMLOverrideWinsAndChangesFingerprint(t *testing.T) {
	repoRoot := t.TempDir()
	evalTools := evalToolCandidates(Config{})
	builtinFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
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
	overrideFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (override): %v", err)
	}
	if builtinFP == overrideFP {
		t.Fatalf("project YAML override did not change the fingerprint (%q)", builtinFP)
	}
}

// TestCaseFingerprint_ProjectMDOverrideWinsAndChangesFingerprint: the
// project .md override path (.deepai/agents/<type>.md, used when no
// <type>.yaml exists — resolveAgentTypeConfigResolved's second priority
// tier) was exercised manually last period but had zero test coverage.
// Mirrors TestCaseFingerprint_ProjectYAMLOverrideWinsAndChangesFingerprint
// above, just for the .md source instead of .yaml.
func TestCaseFingerprint_ProjectMDOverrideWinsAndChangesFingerprint(t *testing.T) {
	repoRoot := t.TempDir()
	evalTools := evalToolCandidates(Config{})
	builtinFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (builtin): %v", err)
	}

	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mdContent := "---\nname: architect\ndescription: overridden via markdown\n---\nA totally different constitution, from a .md override.\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.md"), []byte(mdContent), 0o644); err != nil {
		t.Fatalf("write override md: %v", err)
	}
	overrideFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (override): %v", err)
	}
	if builtinFP == overrideFP {
		t.Fatalf("project .md override did not change the fingerprint (%q)", builtinFP)
	}

	profileCfg, problems, ok := agent.ResolveAgentTypeConfig(agent.AgentType("architect"), repoRoot, nil)
	if !ok || len(problems) > 0 {
		t.Fatalf("resolve architect type from .md override: ok=%v problems=%v", ok, problems)
	}
	if profileCfg.Type != "architect" {
		t.Errorf("profileCfg.Type = %q, want %q (the .md override must resolve to the SAME type it overrides)", profileCfg.Type, "architect")
	}
}

// TestCaseFingerprint_ProjectYAMLOutputSchemaOverride: a project YAML's own
// `output_schema:` key must override the builtin's mounted schema for
// fingerprint purposes too, resolved through the same closed namedSchemas
// table production uses (agent.NamedSchema) — not silently defaulting back
// to "" or to the builtin schema. Asserted as a DELTA (schema key present vs
// absent, system_prompt held identical) rather than a hand-reconstructed
// exact hash: caseFingerprint now covers the full BuildSystemPrompt output
// (see its doc comment), and hand-reconstructing that string here would
// re-derive AssembleSystemPrompt's join order a second time —
// exactly the duplicate-implementation risk this fix exists to eliminate.
func TestCaseFingerprint_ProjectYAMLOutputSchemaOverride(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	evalTools := evalToolCandidates(Config{})
	// architect's builtin schema is unset (M5-4 removed the four non-Strict
	// role contracts); override this project's architect role to point at
	// "review" instead (an arbitrary named schema — the point is only that
	// it differs from "none").
	withSchema := "system_prompt: |\n  a totally different constitution\noutput_schema: review\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(withSchema), 0o644); err != nil {
		t.Fatalf("write override yaml: %v", err)
	}
	withSchemaFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (with schema): %v", err)
	}

	withoutSchema := "system_prompt: |\n  a totally different constitution\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(withoutSchema), 0o644); err != nil {
		t.Fatalf("write override yaml: %v", err)
	}
	withoutSchemaFP, err := caseFingerprint("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("caseFingerprint (without schema): %v", err)
	}

	if withSchemaFP == withoutSchemaFP {
		t.Errorf("project output_schema: review must change the fingerprint versus no output_schema at all (both %q), with system_prompt held identical", withSchemaFP)
	}
}

// TestCaseFingerprint_ProjectYAMLUnknownOutputSchemaErrors: an unknown
// output_schema name in a project YAML must be a hard error at fingerprint
// time too — deliberately stricter than production's Execute (which only
// warns and falls back to the builtin profile), matching this harness's
// pre-existing policy of never letting a broken project override silently
// fingerprint the wrong (fallback) prompt.
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

	if _, err := caseFingerprint("architect", repoRoot, evalToolCandidates(Config{}), nil); err == nil {
		t.Fatal("expected error for unknown output_schema name, got nil")
	}
}

// TestCaseFingerprint_ProjectSkillBodyChangeMovesFingerprint: none of the
// five builtin roles this harness tests declares `skills:`
// (docs/AGENT_CAPABILITY_DESIGN.md §5's five-role corpus), so
// TestCaseFingerprint_EquivalentToRealDispatchedSubagentSystemPrompt never
// exercises PreloadSkillsProfile's production codepath at all — the
// equivalence test is silent on whether a skill's BODY (not just its name)
// is actually reflected in the fingerprint. This is not a regression (the
// pre-M6 fingerprint didn't cover it either), but computeFingerprint
// dropped skillBodies as an explicit parameter to caseFingerprint in this
// same refactor, turning "skill body changes move the fingerprint" from an
// explicit contract into an implicit one — worth a direct test.
//
// Declares a project role (architect) with skills: [probe-skill], writes a
// project skill body, fingerprints it, edits ONLY the skill body (system
// prompt untouched), and asserts the fingerprint moves.
func TestCaseFingerprint_ProjectSkillBodyChangeMovesFingerprint(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	yamlContent := "system_prompt: |\n  a role that preloads a skill\nskills:\n  - probe-skill\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write role yaml: %v", err)
	}

	skillDir := filepath.Join(repoRoot, ".deepai", "skills", "probe-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	writeSkill := func(body string) {
		content := "---\nname: probe-skill\ndescription: a probe skill for the fingerprint test\n---\n" + body
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write SKILL.md: %v", err)
		}
	}

	evalTools := evalToolCandidates(Config{})

	writeSkill("Original skill body instructions.")
	skillRegBefore := skill.NewRegistry()
	if warnings := skillRegBefore.LoadAllReported(repoRoot, nil); len(warnings) > 0 {
		t.Logf("skill load warnings (before): %v", warnings)
	}
	fpBefore, err := caseFingerprint("architect", repoRoot, evalTools, skillRegBefore)
	if err != nil {
		t.Fatalf("caseFingerprint (before): %v", err)
	}

	writeSkill("Completely different skill body instructions, same role, same system_prompt.")
	skillRegAfter := skill.NewRegistry()
	if warnings := skillRegAfter.LoadAllReported(repoRoot, nil); len(warnings) > 0 {
		t.Logf("skill load warnings (after): %v", warnings)
	}
	fpAfter, err := caseFingerprint("architect", repoRoot, evalTools, skillRegAfter)
	if err != nil {
		t.Fatalf("caseFingerprint (after): %v", err)
	}

	if fpBefore == fpAfter {
		t.Errorf("editing a preloaded skill's body (system_prompt held identical) did not move the fingerprint (both %q)", fpBefore)
	}
}

// TestCaseFingerprint_SensitiveToBatchToolCallsPromptGate and
// TestCaseFingerprint_SensitiveToUnderTwoParallelSafeTools used to live here,
// pinning that caseFingerprint moved when a role's restricted tool set
// crossed hasMultipleParallelSafeTools' >=2-ParallelSafe threshold (the gate
// for batchToolCallsPrompt, M6 latency). Both are removed along with
// batchToolCallsPrompt itself: a real-world eval (glm-5.3) found the prompt
// never reduced turn count on any of three task shapes it was tried against,
// while adding ~15% more tool calls on one of them — see the
// batchToolCallsPrompt removal commit for the measurement and
// pkg/agent/promptbuild.go's git history for the removed gate/prompt. With
// no gate left that reads a tool's ParallelSafe field, these two tests would
// only assert that caseFingerprint changes when it no longer has any reason
// to — keeping them would pin dead behavior, not catch a regression.

// ---------------------------------------------------------------------------
// Equivalence: the harness's fingerprint input must be byte-identical to
// what a REAL dispatched subagent's BuildSystemPrompt() produces.
// ---------------------------------------------------------------------------

// captureSystemPromptProvider is a minimal llm.LLMProvider fake that records
// the first Stream request's SystemPrompt — i.e. exactly
// (*agent.Agent).BuildSystemPrompt()'s output, see pkg/agent/react.go's
// "systemPrompt := a.BuildSystemPrompt()" / "SystemPrompt: reqSystemPrompt"
// — then ends the run immediately with a plain final answer. This lets the
// test dispatch a task through the REAL, unmodified
// agent.SubagentExecutor.Execute (the exact code path pkg/tools' task tool
// uses in production) without ever reaching a real model, so "the harness
// matches the real subagent" is proven against production code, not a
// second hand-built approximation of it.
type captureSystemPromptProvider struct {
	mu           sync.Mutex
	systemPrompt string
	captured     bool
}

func (p *captureSystemPromptProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *captureSystemPromptProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	if !p.captured {
		p.systemPrompt = req.SystemPrompt
		p.captured = true
	}
	p.mu.Unlock()
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *captureSystemPromptProvider) firstSystemPrompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.systemPrompt
}

// TestBuildEvalStack_RegistryMatchesEvalToolCandidatesPlusTask closes the
// gap the equivalence test below cannot: that test's "production-side"
// registry is rebuilt FROM evalToolCandidates a second time
// (registry := tools.NewRegistry(); for _, tl := range evalTools {
// mustRegisterTool(registry, tl) }), so it only ever proves "given the same
// candidate list, both formulas hash the same bytes" — it can never observe
// buildEvalStack registering a tool evalToolCandidates doesn't know about
// (or the reverse), because it never looks at what buildEvalStack itself
// actually wired into the dispatch registry.
//
// This test does look: it calls the REAL buildEvalStack and asserts the
// name set of the *tools.Registry it returns equals
// evalToolCandidates(cfg)'s names plus "task" (added once the pool exists —
// see buildEvalStack's own doc comment). A tool registered directly inside
// buildEvalStack but absent from evalToolCandidates — the exact mutation
// this test's own doc comment on buildEvalStack describes reproducing —
// makes this test fail: extra name present in the registry, absent from
// the want set.
func TestBuildEvalStack_RegistryMatchesEvalToolCandidatesPlusTask(t *testing.T) {
	repoRoot := t.TempDir()
	cfg := Config{}
	modelRegistry := llm.NewSingleModelRegistry("test", "m", "")

	_, _, evalTools, registry, err := buildEvalStack(modelRegistry, repoRoot, cfg)
	if err != nil {
		t.Fatalf("buildEvalStack: %v", err)
	}
	if registry == nil {
		t.Fatal("buildEvalStack returned a nil registry")
	}

	want := map[string]bool{"task": true}
	for _, tl := range evalTools {
		want[tl.Name] = true
	}

	got := map[string]bool{}
	for _, tl := range registry.List() {
		got[tl.Name] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("buildEvalStack's dispatch registry is missing tool %q, present in evalToolCandidates(cfg) ∪ {task}", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("buildEvalStack's dispatch registry has tool %q that evalToolCandidates(cfg) ∪ {task} does not — a dispatched subagent could select tools resolveEvalSubagentPrompt/caseFingerprint never sees, silently under-fingerprinting the real prompt", name)
		}
	}
}

// TestCaseFingerprint_EquivalentToRealDispatchedSubagentSystemPrompt is the
// task brief's hard equivalence requirement: for each of five tested roles
// (exceeding the required >=3), dispatch a REAL task through
// agent.SubagentExecutor.Execute — the same production path the task tool
// uses — against THIS repo (so tester's real .deepai/agents/tester.yaml
// project override is exercised, not just the four pure-builtin roles), and
// assert the harness's resolveEvalSubagentPrompt computes a BYTE-IDENTICAL
// string to the real subagent's captured system prompt.
//
// Both sides are built from the SAME evalToolCandidates(cfg) tool list —
// the one buildEvalStack actually registers into the eval dispatch registry
// in production — so SelectSubagentTools (called on both the harness side,
// inside resolveEvalSubagentPrompt, and the production side, inside
// Execute) resolves the identical restricted tool set from the identical
// candidate list. That is what makes this test possible without dispatching
// a real model: the harness never needs to GUESS at the subagent's tool
// set, because both sides derive it from the one list buildEvalStack itself
// uses.
func TestCaseFingerprint_EquivalentToRealDispatchedSubagentSystemPrompt(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	cfg := Config{}
	evalTools := evalToolCandidates(cfg)
	skillReg := skill.NewRegistry()
	if warnings := skillReg.LoadAllReported(repoRoot, nil); len(warnings) > 0 {
		t.Logf("skill load warnings (best-effort, matching buildEvalStack): %v", warnings)
	}

	// "coder" is included alongside the original five: it is the sixth
	// agent_type the eval corpus now covers (this period's coder/ cases,
	// eval/agent-cases/coder/*), and this equivalence check is exactly what
	// makes its fingerprint trustworthy — a fingerprint that silently
	// diverged from what SubagentExecutor.Execute actually assembles would
	// defeat the whole point of `eval compare`'s "fingerprint unchanged"
	// gate for the new role.
	for _, agentType := range []string{"architect", "researcher", "analyst", "product-manager", "tester", "coder"} {
		t.Run(agentType, func(t *testing.T) {
			harnessPrompt, err := resolveEvalSubagentPrompt(agentType, repoRoot, evalTools, skillReg)
			if err != nil {
				t.Fatalf("resolveEvalSubagentPrompt: %v", err)
			}

			registry := tools.NewRegistry()
			for _, tl := range evalTools {
				mustRegisterTool(registry, tl)
			}
			modelReg := llm.NewSingleModelRegistry("test", "m", "")
			provider := &captureSystemPromptProvider{}
			modelReg.InjectProvider("test", "", "", provider)
			exec := agent.NewSubagentExecutor(modelReg, registry, nil).
				WithWorkDir(repoRoot).
				WithSkillRegistry(skillReg)

			if _, err := exec.Execute(context.Background(),
				&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: agentType}},
				func(subagent.TaskEvent) {}); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			realPrompt := provider.firstSystemPrompt()
			if harnessPrompt != realPrompt {
				t.Errorf("harness-computed system prompt differs from the REAL dispatched subagent's BuildSystemPrompt() output for agent_type %q:\n\n--- harness (resolveEvalSubagentPrompt) ---\n%s\n\n--- real (SubagentExecutor.Execute -> BuildSystemPrompt) ---\n%s", agentType, harnessPrompt, realPrompt)
			}
		})
	}
}

// TestCaseFingerprint_ToolRestrictionAppliesToTheAssembledPrompt is the RED
// test for the M3 defect: on the five roles above, the restricted tool set
// SelectSubagentTools computes and the full evalTools candidate list answer
// AssembleSystemPrompt's four gates (hasAnyFileTool/hasSearchTools/
// hasMultipleParallelSafeTools/hasTodoTool) identically — every one of them
// has file tools, has grep, has >=2 ParallelSafe tools, and none has
// todo_write — so a mutant resolveEvalSubagentPrompt that skips
// SelectSubagentTools entirely and hands AssembleSystemPrompt the
// UNRESTRICTED evalTools instead produces byte-identical output on that
// corpus. The equivalence test's strength was riding corpus luck, not an
// actual assertion that restriction is applied.
//
// This manufactures a role whose restricted set genuinely differs: a
// project override pinning architect to tools: [bash] only (no file tools,
// no grep) via .deepai/agents/architect.yaml. With restriction correctly
// applied, hasAnyFileTool is false and the file-operation-rule section must
// be absent; without it (the mutant), evalToolCandidates' file tools are
// still in play and the section would still appear.
func TestCaseFingerprint_ToolRestrictionAppliesToTheAssembledPrompt(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".deepai", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlContent := "system_prompt: |\n  x\ntools:\n  - bash\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "architect.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write override yaml: %v", err)
	}

	evalTools := evalToolCandidates(Config{})

	restrictedPrompt, err := resolveEvalSubagentPrompt("architect", repoRoot, evalTools, nil)
	if err != nil {
		t.Fatalf("resolveEvalSubagentPrompt: %v", err)
	}
	if strings.Contains(restrictedPrompt, "File-operation rule") {
		t.Errorf("resolveEvalSubagentPrompt included the file-operation-rule section for a role restricted to tools: [bash] — SelectSubagentTools does not appear to have been applied before AssembleSystemPrompt")
	}

	// The delta the task calls for directly: assembling against the FULL,
	// unrestricted evalTools (what a mutant skipping SelectSubagentTools
	// would effectively do) must NOT produce the same bytes as the
	// correctly restricted prompt above.
	profileCfg, problems, ok := agent.ResolveAgentTypeConfig(agent.AgentType("architect"), repoRoot, nil)
	if !ok || len(problems) > 0 {
		t.Fatalf("resolve architect type: ok=%v problems=%v", ok, problems)
	}
	unrestrictedReg := tools.NewRegistry()
	for _, tl := range evalTools {
		mustRegisterTool(unrestrictedReg, tl)
	}
	unrestrictedPrompt := agent.AssembleSystemPrompt(profileCfg.SystemPrompt, unrestrictedReg, true, nil)

	if restrictedPrompt == unrestrictedPrompt {
		t.Fatalf("restricted (tools: [bash]) and unrestricted (full evalTools) assembled prompts are byte-identical (%d bytes) — tool restriction has no observable effect on the assembled prompt, which is what this test exists to catch", len(restrictedPrompt))
	}

	// End to end: the harness's prompt must ALSO match a REAL dispatched
	// subagent's BuildSystemPrompt() output for this same restricted role —
	// same shape as TestCaseFingerprint_EquivalentToRealDispatchedSubagentSystemPrompt
	// above, but for a role whose restricted set actually differs from the
	// candidate list.
	registry := tools.NewRegistry()
	for _, tl := range evalTools {
		mustRegisterTool(registry, tl)
	}
	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	provider := &captureSystemPromptProvider{}
	modelReg.InjectProvider("test", "", "", provider)
	exec := agent.NewSubagentExecutor(modelReg, registry, nil).WithWorkDir(repoRoot)
	if _, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "architect"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	realPrompt := provider.firstSystemPrompt()
	if restrictedPrompt != realPrompt {
		t.Errorf("harness-computed system prompt differs from the REAL dispatched subagent's BuildSystemPrompt() output for a tools:[bash]-restricted architect:\n\n--- harness (resolveEvalSubagentPrompt) ---\n%s\n\n--- real (SubagentExecutor.Execute -> BuildSystemPrompt) ---\n%s", restrictedPrompt, realPrompt)
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
	records, err := runEvalCases(context.Background(), pool, repoRoot, evalToolCandidates(Config{}), nil, cases, evalOptions{Runs: 2}, nil)
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
	_, runErr := runEvalCases(context.Background(), pool, repoRoot, evalToolCandidates(Config{}), nil, cases, evalOptions{Runs: 1}, func(rec runRecord) error {
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

// TestRenderEvalCompare_CombinedHeadingReflectsActualRoleCountNotHardcoded5
// pins the fix for the hardcoded "## combined (5 roles)" heading: the eval
// corpus now covers six agent_types (architect, researcher, analyst,
// product-manager, tester, coder — see this file's package doc comment and
// TestCaseFingerprint_EquivalentToRealDispatchedSubagentSystemPrompt), so a
// literal "5" is simply wrong on the current corpus, and would silently go
// on being wrong again the next time a role is added. The heading must
// report however many roles actually fed the combined mentions-hit
// calculation — i.e. those present in BOTH before and after, not just
// len(after.Roles) (a role with no before-baseline row is skipped before
// it's added to the combined totals — see renderEvalCompare's `!ok`
// continue).
func TestRenderEvalCompare_CombinedHeadingReflectsActualRoleCountNotHardcoded5(t *testing.T) {
	agentTypes := []string{"architect", "researcher", "analyst", "product-manager", "tester", "coder"}
	var beforeRoles, afterRoles []roleSummary
	for _, at := range agentTypes {
		beforeRoles = append(beforeRoles, baselineRole(at))
		afterRole := baselineRole(at)
		afterRole.Fingerprint = "def67890" // a real before/after, not a no-op
		afterRoles = append(afterRoles, afterRole)
	}
	// A seventh role with NO before-baseline row: must be excluded from the
	// combined role count (renderEvalCompare's `!ok` branch), not just
	// excluded from len(after.Roles) mattering.
	afterRoles = append(afterRoles, baselineRole("no-baseline-role"))

	before := evalSummary{Model: "glm-5.3", Runs: 8, Timeout: "5m", Roles: beforeRoles}
	after := evalSummary{Model: "glm-5.3", Runs: 8, Timeout: "5m", Roles: afterRoles}

	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if strings.Contains(out, "## combined (5 roles)") {
		t.Errorf("expected the combined heading to reflect the real 6-role count, not the old hardcoded 5, got:\n%s", out)
	}
	if !strings.Contains(out, "## combined (6 roles)") {
		t.Errorf("expected \"## combined (6 roles)\" (6 roles have both a before and an after row; the 7th has no before row), got:\n%s", out)
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
	if len(cases) != 18 {
		t.Fatalf("len(cases) = %d, want 18 (5 original agent types x 3 cases, plus coder x 3 added for the M6 batch-editing corpus)", len(cases))
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
	for _, agentType := range []string{"architect", "product-manager", "researcher", "analyst", "tester", "coder"} {
		if wantAgentTypes[agentType] != 3 {
			t.Errorf("agent_type %s: %d cases, want 3", agentType, wantAgentTypes[agentType])
		}
	}
}

// TestRealAgentCasesCorpus_ExpectationsAreWellFormedAndFalsifiable is the
// M6 review's corpus validator (review defect #5), run against the REAL,
// pinned eval/agent-cases corpus. The review's original proposal — reject a
// case if any file_contains text already exists in the pinned fixture —
// was withdrawn: it would forbid exactly the preservation assertions Fix 2
// adds (e.g. asserting `"include_hidden"` survives a rename, when it is
// present in the fixture both before and after an honest edit). This test
// enforces the adopted alternative instead:
//
//  1. loadEvalCases itself must succeed: validateManifestExpect already
//     rejects an unknown expect key or a malformed shape at load time (see
//     TestLoadEvalCases_RejectsMalformedExpectShapes for synthetic-manifest
//     unit coverage); a non-nil error here means the REAL corpus itself is
//     unsound.
//  2. (folded into 1: a blank path/text is one of the shapes
//     validateManifestExpect rejects.)
//  3. Every file_not_contains text must actually be present in the case's
//     PINNED fixture file — this holds unconditionally, with no legitimate
//     counterexample: if the text was never there, "the text is gone" is
//     true before any edit happens, and the assertion can never fail.
//  4. Every case that uses file_contains at all must have AT LEAST ONE
//     entry whose text is NOT present in the pinned fixture — proof the
//     case can actually fail (falsifiable). This does not forbid a
//     file_contains whose text already exists; it only requires that at
//     least one entry per case is not one of those (the rest may
//     legitimately be preservation assertions).
func TestRealAgentCasesCorpus_ExpectationsAreWellFormedAndFalsifiable(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	casesDir := filepath.Join(repoRoot, "eval", "agent-cases")
	if _, err := os.Stat(casesDir); err != nil {
		t.Skipf("eval/agent-cases not present (%v); skipping real-corpus check", err)
	}

	// Rules 1/2: loadEvalCases (via validateManifestExpect) rejects an
	// unknown key or a malformed/blank-path/blank-text shape at load time —
	// a non-nil error here IS the corpus-validator failure.
	cases, err := loadEvalCases(casesDir, "")
	if err != nil {
		t.Fatalf("loadEvalCases: %v (the real corpus must be well-formed)", err)
	}

	for _, c := range cases {
		hasFileContains := false
		falsifiable := false

		for _, exp := range c.Manifest.Expect {
			key, val, ok := singleKV(exp)
			if !ok {
				continue
			}
			switch key {
			case "file_not_contains":
				items, problems := parseFileTextExpectations(val)
				if len(problems) > 0 {
					t.Errorf("case %s/%s: malformed file_not_contains: %v", c.AgentType, c.ID, problems)
				}
				for _, fe := range items {
					data, err := os.ReadFile(filepath.Join(c.FixtureDir, fe.Path))
					if err != nil {
						t.Errorf("case %s/%s: file_not_contains {path: %s}: read pinned fixture: %v", c.AgentType, c.ID, fe.Path, err)
						continue
					}
					if !strings.Contains(string(data), fe.Text) {
						t.Errorf("case %s/%s: file_not_contains {path: %s, text: %q}: text is NOT present in the pinned fixture — this assertion can never fail (rule 3)", c.AgentType, c.ID, fe.Path, fe.Text)
					}
				}
			case "file_contains":
				hasFileContains = true
				items, problems := parseFileTextExpectations(val)
				if len(problems) > 0 {
					t.Errorf("case %s/%s: malformed file_contains: %v", c.AgentType, c.ID, problems)
				}
				for _, fe := range items {
					data, err := os.ReadFile(filepath.Join(c.FixtureDir, fe.Path))
					if err != nil {
						// The path doesn't exist in the pinned fixture at
						// all: the text is certainly not there either, so
						// this entry is falsifiable by construction.
						falsifiable = true
						continue
					}
					if !strings.Contains(string(data), fe.Text) {
						falsifiable = true
					}
				}
			}
		}

		if hasFileContains && !falsifiable {
			t.Errorf("case %s/%s: every file_contains text is ALREADY present in the pinned fixture — nothing proves this case can fail (rule 4)", c.AgentType, c.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// coder corpus anti-cheat coverage (M6 review defect #2): the independent
// review found that every one of the three coder cases' negative
// constraints — "don't touch this protected string/identifier", "the
// content must actually survive" — scored identically for an honest rename
// and for a cheat/damage variant, because nothing in the manifest asserted
// on them. The fix adds preservation file_contains entries to the three
// manifests (see eval/agent-cases/coder/*/manifest.yaml); these tests prove
// BOTH halves against the real, pinned fixtures: an honest rename passes
// everything, and every cheat/damage shape the review identified fails at
// least one assertion.
// ---------------------------------------------------------------------------

// loadRealCoderCase finds one real coder/<id> case from the pinned
// eval/agent-cases corpus (never a synthetic manifest), so this test proves
// something about the actual shipped manifests, not a stand-in.
func loadRealCoderCase(t *testing.T, id string) evalCase {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	casesDir := filepath.Join(repoRoot, "eval", "agent-cases")
	if _, err := os.Stat(casesDir); err != nil {
		t.Skipf("eval/agent-cases not present (%v); skipping real-corpus check", err)
	}
	cases, err := loadEvalCases(casesDir, "coder")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	for _, c := range cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("coder case %q not found in the real corpus", id)
	return evalCase{}
}

// materializeAndTransform copies c's pinned fixture into a fresh temp
// worktree, runs transform against it (in place), and reports every one of
// c's context_files as "changed" — every scenario below rewrites all of
// them, exactly like a real coder run editing the files it was told about.
func materializeAndTransform(t *testing.T, c evalCase, transform func(worktree string)) (worktree string, changed []string, cleanup func()) {
	t.Helper()
	worktree, cleanup, err := materializeFixture(c.FixtureDir)
	if err != nil {
		t.Fatalf("materializeFixture: %v", err)
	}
	transform(worktree)
	for _, cf := range c.Manifest.ContextFiles {
		changed = append(changed, filepath.Join(worktree, cf))
	}
	return worktree, changed, cleanup
}

func rewriteRel(t *testing.T, worktree, rel string, edit func(content string) string) {
	t.Helper()
	p := filepath.Join(worktree, rel)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(edit(string(data))), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func writeRel(t *testing.T, worktree, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktree, rel), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func failedAssertionNames(results []assertionResult) []string {
	var out []string
	for _, r := range results {
		if r.Status == "fail" {
			out = append(out, r.Name)
		}
	}
	return out
}

func assertAllAssertionsPass(t *testing.T, results []assertionResult) {
	t.Helper()
	if failed := failedAssertionNames(results); len(failed) > 0 {
		t.Errorf("expected every assertion to pass, but these failed: %v (full results: %+v)", failed, results)
	}
}

func assertAtLeastOneAssertionFails(t *testing.T, results []assertionResult) {
	t.Helper()
	if failed := failedAssertionNames(results); len(failed) == 0 {
		t.Errorf("expected at least one assertion to fail, but every one passed: %+v", results)
	}
}

// --- rename-include-hidden (C1) ---

func honestRenameIncludeHidden(t *testing.T, worktree string) {
	for _, rel := range []string{"pkg/tools/builtin/find.go", "pkg/tools/builtin/codemap.go", "pkg/tools/builtin/grep.go"} {
		rewriteRel(t, worktree, rel, func(s string) string {
			return strings.ReplaceAll(s, "includeHidden", "showHidden")
		})
	}
}

func TestCoderCases_RenameIncludeHidden_HonestRenamePassesAllAssertions(t *testing.T) {
	c := loadRealCoderCase(t, "rename-include-hidden")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) { honestRenameIncludeHidden(t, wt) })
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAllAssertionsPass(t, results)
}

func TestCoderCases_RenameIncludeHidden_AlsoMangledProtectedStringFails(t *testing.T) {
	c := loadRealCoderCase(t, "rename-include-hidden")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) {
		honestRenameIncludeHidden(t, wt)
		// The cheat: also rename the protected string-literal arg key,
		// which the task explicitly forbids touching.
		for _, rel := range []string{"pkg/tools/builtin/find.go", "pkg/tools/builtin/codemap.go", "pkg/tools/builtin/grep.go"} {
			rewriteRel(t, wt, rel, func(s string) string {
				return strings.ReplaceAll(s, `"include_hidden"`, `"show_hidden"`)
			})
		}
	})
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAtLeastOneAssertionFails(t, results)
}

func TestCoderCases_RenameIncludeHidden_StubbedFilesFail(t *testing.T) {
	c := loadRealCoderCase(t, "rename-include-hidden")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) {
		// The cheat: gut each file down to a few lines. Every showHidden/
		// includeHidden assertion the ORIGINAL manifest had still passes
		// (showHidden present, includeHidden absent) — only the added
		// structural-preservation assertions can catch this.
		writeRel(t, wt, "pkg/tools/builtin/find.go", "package builtin\n\nvar showHidden bool\n")
		writeRel(t, wt, "pkg/tools/builtin/codemap.go", "package builtin\n\nvar showHidden bool\n")
		writeRel(t, wt, "pkg/tools/builtin/grep.go", "package builtin\n\nvar showHidden bool\n")
	})
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAtLeastOneAssertionFails(t, results)
}

// --- rename-max-tool-calls-cap (C2) ---

func honestRenameMaxToolCallsCap(t *testing.T, worktree string) {
	for _, rel := range []string{"pkg/subagent/pool.go", "pkg/subagent/types.go"} {
		rewriteRel(t, worktree, rel, func(s string) string {
			return strings.ReplaceAll(s, "MaxToolCalls", "ToolCallCap")
		})
	}
}

func TestCoderCases_RenameMaxToolCallsCap_HonestRenamePassesAllAssertions(t *testing.T) {
	c := loadRealCoderCase(t, "rename-max-tool-calls-cap")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) { honestRenameMaxToolCallsCap(t, wt) })
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAllAssertionsPass(t, results)
}

func TestCoderCases_RenameMaxToolCallsCap_AlsoMangledJSONTagFails(t *testing.T) {
	c := loadRealCoderCase(t, "rename-max-tool-calls-cap")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) {
		honestRenameMaxToolCallsCap(t, wt)
		// The cheat: also rewrite the protected JSON struct tag, which the
		// task explicitly forbids touching.
		rewriteRel(t, wt, "pkg/subagent/types.go", func(s string) string {
			return strings.ReplaceAll(s, `json:"max_tool_calls"`, `json:"tool_call_cap"`)
		})
	})
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAtLeastOneAssertionFails(t, results)
}

// --- rename-total-calls (C3) ---

func honestRenameTotalCalls(t *testing.T, worktree string) {
	for _, rel := range []string{"pkg/chat/repl.go", "pkg/commands/analyze.go", "pkg/memory/preference.go"} {
		rewriteRel(t, worktree, rel, func(s string) string {
			return strings.ReplaceAll(s, "totalCalls", "totalToolCalls")
		})
	}
}

func TestCoderCases_RenameTotalCalls_HonestRenamePassesAllAssertions(t *testing.T) {
	c := loadRealCoderCase(t, "rename-total-calls")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) { honestRenameTotalCalls(t, wt) })
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAllAssertionsPass(t, results)
}

func TestCoderCases_RenameTotalCalls_AlsoRenamedForbiddenIdentifiersFails(t *testing.T) {
	c := loadRealCoderCase(t, "rename-total-calls")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) {
		honestRenameTotalCalls(t, wt)
		// The cheat: also rename the identifiers the task explicitly says
		// are NOT targets (failedCalls, maxToolCalls, MainAgentCalls).
		rewriteRel(t, wt, "pkg/chat/repl.go", func(s string) string {
			return strings.ReplaceAll(s, "failedCalls", "failedToolCalls")
		})
		rewriteRel(t, wt, "pkg/commands/analyze.go", func(s string) string {
			s = strings.ReplaceAll(s, "maxToolCalls", "maxToolCallsCap")
			s = strings.ReplaceAll(s, "MainAgentCalls", "MainAgentToolCalls")
			return s
		})
	})
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAtLeastOneAssertionFails(t, results)
}

func TestCoderCases_RenameTotalCalls_OneOccurrenceKeptRestDeletedFails(t *testing.T) {
	c := loadRealCoderCase(t, "rename-total-calls")
	worktree, changed, cleanup := materializeAndTransform(t, c, func(wt string) {
		// The cheat: each 400-2000 line file is gutted down to the one
		// totalCalls occurrence renamed, everything else discarded. Every
		// file_contains/file_not_contains assertion the ORIGINAL manifest
		// had still passes — only the added structural-preservation
		// assertions can catch this.
		writeRel(t, wt, "pkg/chat/repl.go", "package chat\n\nvar totalToolCalls int\n")
		writeRel(t, wt, "pkg/commands/analyze.go", "package commands\n\nvar totalToolCalls int\n")
		writeRel(t, wt, "pkg/memory/preference.go", "package memory\n\nvar totalToolCalls int\n")
	})
	defer cleanup()
	results := evaluateCase(c.Manifest, "", nil, nil, true, changed, worktree)
	assertAtLeastOneAssertionFails(t, results)
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

// TestBuildEvalSummary_FileEditAssertionFailuresCountedSeparatelyFromGuard
// pins the M6 batch-editing corpus's core anti-cheat requirement (see this
// file's package doc comment): a files_changed/file_contains/
// file_not_contains failure must be visible (FileEditAssertionFailures) but
// must NOT inflate GuardViolations or NoWritesViolations — those two are the
// safety-floor tripwire ("nothing unsafe happened"), and a case with no
// no_writes assertion at all (every coder case) must not silently borrow
// that column to report "the edit didn't land".
func TestBuildEvalSummary_FileEditAssertionFailuresCountedSeparatelyFromGuard(t *testing.T) {
	records := []runRecord{
		{
			Case: "c1", AgentType: "coder", Fingerprint: "fp1", Tokens: 100, DurationMS: 1000,
			Assertions: []assertionResult{
				{Name: "files_changed", Status: "pass"},
				{Name: "file_contains:a.go:Foo", Status: "pass"},
				{Name: "file_not_contains:a.go:Bar", Status: "pass"},
				{Name: "tool_calls_max:20", Status: "pass"},
			},
		},
		{
			Case: "c2", AgentType: "coder", Fingerprint: "fp1", Tokens: 100, DurationMS: 1000,
			Assertions: []assertionResult{
				{Name: "files_changed", Status: "fail", Detail: "missing=[b.go] extra=[] actual=[]"},
				{Name: "file_contains:a.go:Foo", Status: "fail"},
				{Name: "file_not_contains:a.go:Bar", Status: "pass"},
				{Name: "tool_calls_max:20", Status: "pass"},
			},
		},
	}
	s := buildEvalSummary("glm-5.3", 1, "5m", records)
	if len(s.Roles) != 1 {
		t.Fatalf("len(Roles) = %d, want 1", len(s.Roles))
	}
	r := s.Roles[0]
	if r.FileEditAssertionFailures != 2 {
		t.Errorf("FileEditAssertionFailures = %d, want 2 (files_changed fail + file_contains fail on c2)", r.FileEditAssertionFailures)
	}
	if r.GuardViolations != 0 {
		t.Errorf("GuardViolations = %d, want 0 (file-edit failures must not count as guard violations)", r.GuardViolations)
	}
	if r.NoWritesViolations != 0 {
		t.Errorf("NoWritesViolations = %d, want 0 (no no_writes assertion in these records)", r.NoWritesViolations)
	}
}

// TestBuildEvalSummary_HonestFileEditIsNotANoWritesViolation is the direct
// reproduction of the M6 review's critical finding: a coder case's manifest
// declares files_changed/file_contains/file_not_contains — never no_writes
// — and every one of those assertions passed (the edit landed exactly as
// asked). runOneCase's WriteViolation is nonetheless unconditionally true
// here (len(changed) > 0 — see runOneCase's rec construction), because the
// case's whole job is to edit files. Before the fix, buildEvalSummary's
// `if r.WriteViolation { rs.NoWritesViolations++ }` counted this as a
// no_writes violation regardless of whether the manifest ever declared
// no_writes, which fed straight into GuardViolations (guardFail +
// NoWritesViolations) and would fail renderEvalCompare's "guard violations
// == 0" gate for a PERFECT run — see the reviewer's literal repro:
// "honest rename-include-hidden fails=0/8 writeViolation=true ...
// NoWritesViolations=3 GuardViolations=3 ... VERDICT: fail".
func TestBuildEvalSummary_HonestFileEditIsNotANoWritesViolation(t *testing.T) {
	records := []runRecord{
		{
			Case: "rename-include-hidden", AgentType: "coder", Fingerprint: "8d029d36",
			Tokens: 100, DurationMS: 1000,
			WriteViolation:   true,  // the fixture genuinely got edited
			NoWritesDeclared: false, // and the manifest never asked for no_writes
			Assertions: []assertionResult{
				{Name: "files_changed", Status: "pass"},
				{Name: "file_contains:pkg/tools/builtin/find.go:showHidden", Status: "pass"},
				{Name: "file_contains:pkg/tools/builtin/codemap.go:showHidden", Status: "pass"},
				{Name: "file_contains:pkg/tools/builtin/grep.go:showHidden", Status: "pass"},
				{Name: "file_not_contains:pkg/tools/builtin/find.go:includeHidden", Status: "pass"},
				{Name: "file_not_contains:pkg/tools/builtin/codemap.go:includeHidden", Status: "pass"},
				{Name: "file_not_contains:pkg/tools/builtin/grep.go:includeHidden", Status: "pass"},
				{Name: "tool_calls_max:40", Status: "pass"},
			},
		},
	}
	s := buildEvalSummary("glm-5.3", 1, "5m", records)
	if len(s.Roles) != 1 {
		t.Fatalf("len(Roles) = %d, want 1", len(s.Roles))
	}
	r := s.Roles[0]
	if r.NoWritesViolations != 0 {
		t.Errorf("NoWritesViolations = %d, want 0 — the manifest never declares no_writes, so an honest edit must not count as one", r.NoWritesViolations)
	}
	if r.GuardViolations != 0 {
		t.Errorf("GuardViolations = %d, want 0 — a perfect, on-task file edit must not trip the safety-floor gate", r.GuardViolations)
	}
	if r.FileEditAssertionFailures != 0 {
		t.Errorf("FileEditAssertionFailures = %d, want 0 (every file-edit assertion passed)", r.FileEditAssertionFailures)
	}
}

// TestRenderEvalCompare_HonestCoderEditPassesTheGuardViolationsGate is the
// compare-time consequence of the same defect: renderEvalCompare's
// "guard violations == 0" verdict gate must PASS for a coder role whose
// only "violation" is the unconditional WriteViolation flag on a
// files-changed case with no no_writes assertion. Before the fix this
// rendered "VERDICT: fail", exactly as the reviewer's repro showed.
func TestRenderEvalCompare_HonestCoderEditPassesTheGuardViolationsGate(t *testing.T) {
	var records []runRecord
	// evalMinDispatchedForValid is 7 — 8 runs so the compare verdict is
	// judged on the guard-violations gate itself, not short-circuited to
	// "invalid" by undersampling.
	for i := 0; i < 8; i++ {
		records = append(records, runRecord{
			Case: "rename-include-hidden", AgentType: "coder", Fingerprint: "8d029d36", WriteViolation: true, NoWritesDeclared: false,
			Assertions: []assertionResult{{Name: "files_changed", Status: "pass"}},
		})
	}
	after := buildEvalSummary("glm-5.3", 8, "5m", records)
	before := after
	beforeRole := after.Roles[0]
	beforeRole.Fingerprint = "deadbeef" // a real before/after, not a no-op
	before.Roles = []roleSummary{beforeRole}

	out, err := renderEvalCompare(before, after)
	if err != nil {
		t.Fatalf("renderEvalCompare: %v", err)
	}
	if strings.Contains(out, "VERDICT: fail") {
		t.Errorf("expected the honest coder edit to pass the guard-violations gate, got:\n%s", out)
	}
	if !strings.Contains(out, "guard violations:        0 -> 0") {
		t.Errorf("expected guard violations 0 -> 0, got:\n%s", out)
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

// TestRenderEvalSummaryMD_FileEditAssertionFailuresVisibleAndDistinctFromGuard
// pins the M6 batch-editing corpus's visibility requirement head-on: a
// files_changed/file_contains/file_not_contains failure count must render
// in summary.md as its own column, not fold into "guard viol" — a reader
// scanning the table must be able to see "the edits didn't land" separately
// from "a safety-floor rule broke", even though both are integers sitting
// next to each other in the same row.
func TestRenderEvalSummaryMD_FileEditAssertionFailuresVisibleAndDistinctFromGuard(t *testing.T) {
	s := evalSummary{
		GeneratedAt: "2026-09-07T00:00:00Z",
		Model:       "glm-5.3",
		Runs:        3,
		Timeout:     "5m",
		Roles: []roleSummary{
			{
				AgentType:                 "coder",
				Fingerprint:               "cafef00d",
				Cases:                     3,
				DispatchedRuns:            9,
				GuardViolations:           0,
				FileEditAssertionFailures: 4,
			},
		},
	}
	md := renderEvalSummaryMD(s)
	if !strings.Contains(md, "file edit assert fail") {
		t.Fatalf("summary.md missing a file-edit-assertion-failures column/header:\n%s", md)
	}
	rowStart := strings.Index(md, "| coder | cafef00d | 3 | 9 | ")
	if rowStart < 0 {
		t.Fatalf("summary.md row prefix unexpected:\n%s", md)
	}
	row := md[rowStart:]
	if idx := strings.Index(row, "\n"); idx >= 0 {
		row = row[:idx]
	}
	// GuardViolations=0 and FileEditAssertionFailures=4 must both be
	// visible, as DIFFERENT cells — a reader must never see one number and
	// have to guess which of the two it is.
	if !strings.Contains(row, "| 0 | 4 |") {
		t.Errorf("summary.md row does not render guard_violations=0 and file_edit_assertion_failures=4 as adjacent, distinct cells:\n%s", row)
	}
}

// ---------------------------------------------------------------------------
// prepareEvalResultDir must never silently reuse an existing result
// directory: a same-day, same-model rerun (a routine workflow — re-running a
// role for review) must land in a fresh, suffixed directory rather than
// letting newRunWriter's os.Create truncate a previous run's runs.jsonl,
// which holds the only record of real, unrecoverable model spend.
// ---------------------------------------------------------------------------

func TestPrepareEvalResultDir_FreshDirUsesPlainName(t *testing.T) {
	outRoot := t.TempDir()
	dir, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err != nil {
		t.Fatalf("prepareEvalResultDir: %v", err)
	}
	dateStr := time.Now().UTC().Format("2006-01-02")
	want := filepath.Join(outRoot, dateStr+"-glm-5.3")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestPrepareEvalResultDir_ExistingDirGetsSuffix2(t *testing.T) {
	outRoot := t.TempDir()
	dateStr := time.Now().UTC().Format("2006-01-02")
	base := filepath.Join(outRoot, dateStr+"-glm-5.3")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("seed existing dir: %v", err)
	}

	dir, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err != nil {
		t.Fatalf("prepareEvalResultDir: %v", err)
	}
	want := base + "-2"
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestPrepareEvalResultDir_TwoExistingDirsGetSuffix3(t *testing.T) {
	outRoot := t.TempDir()
	dateStr := time.Now().UTC().Format("2006-01-02")
	base := filepath.Join(outRoot, dateStr+"-glm-5.3")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("seed existing dir: %v", err)
	}
	if err := os.MkdirAll(base+"-2", 0o755); err != nil {
		t.Fatalf("seed existing -2 dir: %v", err)
	}

	dir, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err != nil {
		t.Fatalf("prepareEvalResultDir: %v", err)
	}
	want := base + "-3"
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestPrepareEvalResultDir_SuffixExhaustionReturnsError(t *testing.T) {
	outRoot := t.TempDir()
	dateStr := time.Now().UTC().Format("2006-01-02")
	base := filepath.Join(outRoot, dateStr+"-glm-5.3")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("seed existing dir: %v", err)
	}
	for i := 2; i <= 100; i++ {
		if err := os.MkdirAll(fmt.Sprintf("%s-%d", base, i), 0o755); err != nil {
			t.Fatalf("seed existing -%d dir: %v", i, err)
		}
	}

	_, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err == nil {
		t.Fatal("expected an error once the suffix budget is exhausted, got nil")
	}
}

// TestPrepareEvalResultDirAndRunWriter_RerunNeverTruncatesPriorRunsJSONL is
// the direct regression test for the reported defect: prepare a result dir,
// write a run record, then simulate a same-day same-model rerun by calling
// prepareEvalResultDir + newRunWriter a second time. The first run's
// runs.jsonl — real, unrecoverable model spend — must be byte-for-byte
// intact afterward.
func TestPrepareEvalResultDirAndRunWriter_RerunNeverTruncatesPriorRunsJSONL(t *testing.T) {
	outRoot := t.TempDir()

	dir1, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err != nil {
		t.Fatalf("prepareEvalResultDir (first run): %v", err)
	}
	runsPath1 := filepath.Join(dir1, "runs.jsonl")
	w1, err := newRunWriter(runsPath1)
	if err != nil {
		t.Fatalf("newRunWriter (first run): %v", err)
	}
	if err := w1.Write(runRecord{Case: "expensive-case", Run: 1, Tokens: 123456}); err != nil {
		t.Fatalf("Write (first run): %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close (first run): %v", err)
	}

	before, err := os.ReadFile(runsPath1)
	if err != nil {
		t.Fatalf("read runs.jsonl after first run: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("first run's runs.jsonl is empty before the rerun — test setup is broken")
	}

	// Simulate a same-day, same-model rerun (a routine "re-run this role for
	// review" workflow).
	dir2, err := prepareEvalResultDir(outRoot, "glm-5.3")
	if err != nil {
		t.Fatalf("prepareEvalResultDir (second run): %v", err)
	}
	if dir2 == dir1 {
		t.Fatalf("second prepareEvalResultDir call returned the SAME directory as the first (%s) — this is the defect: it will truncate the prior run's runs.jsonl", dir1)
	}
	runsPath2 := filepath.Join(dir2, "runs.jsonl")
	w2, err := newRunWriter(runsPath2)
	if err != nil {
		t.Fatalf("newRunWriter (second run): %v", err)
	}
	if err := w2.Write(runRecord{Case: "expensive-case", Run: 1, Tokens: 789}); err != nil {
		t.Fatalf("Write (second run): %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close (second run): %v", err)
	}

	after, err := os.ReadFile(runsPath1)
	if err != nil {
		t.Fatalf("read first run's runs.jsonl after rerun: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("first run's runs.jsonl was modified by the rerun!\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestNewRunWriter_RefusesToOverwriteExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")

	w1, err := newRunWriter(path)
	if err != nil {
		t.Fatalf("newRunWriter (first): %v", err)
	}
	if err := w1.Write(runRecord{Case: "c1", Run: 1, Tokens: 42}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	_, err = newRunWriter(path)
	if err == nil {
		t.Fatal("expected newRunWriter to refuse to open an existing runs.jsonl, got nil error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s after rejected reopen: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("existing runs.jsonl was modified by a rejected newRunWriter call!\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
