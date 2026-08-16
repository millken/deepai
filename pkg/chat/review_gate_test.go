package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// fakeTaskTool registers a stand-in "task" tool that captures the dispatch
// arguments and returns a canned reviewer response (or error).
type fakeTaskTool struct {
	calls   int
	args    map[string]any
	content string
	err     error
	// sideEffect runs inside the handler — used to simulate a reviewer
	// writing the worktree during the review.
	sideEffect func()
}

func (f *fakeTaskTool) registry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	err := reg.Register(models.Tool{
		Name: "task",
		Handler: func(_ context.Context, call models.ToolCall) (models.ToolResult, error) {
			f.calls++
			f.args = call.Arguments
			if f.sideEffect != nil {
				f.sideEffect()
			}
			if f.err != nil {
				return models.ToolResult{}, f.err
			}
			return models.ToolResult{Content: f.content, Status: models.CallStatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("register fake task tool: %v", err)
	}
	return reg
}

func passVerdictJSON() string {
	return `{"agent":"correctness-reviewer","verdict":"pass","summary":"holds up","issues":[]}`
}

func failVerdictJSON() string {
	return `{"agent":"correctness-reviewer","verdict":"fail","summary":"broken",
		"issues":[{"severity":"high","file":"a.go","line":7,"message":"nil deref",
		"scenario":"call F(nil) -> panic","suggestion":"guard nil"}]}`
}

func newReviewRepl(t *testing.T, workDir string, fake *fakeTaskTool) (*ChatRepl, *mockUI) {
	t.Helper()
	ui := &mockUI{}
	r := &ChatRepl{
		cfg: ReplConfig{
			WorkDir:         workDir,
			ToolRegistry:    fake.registry(t),
			ReviewAfterEdit: true,
		},
		ui:    ui,
		carry: agent.NewSessionCarry(),
	}
	return r, ui
}

// seedEditedFile creates a file in workDir and records it on the carry, so
// the gate's scope is non-empty through the tool-record channel.
func seedEditedFile(t *testing.T, r *ChatRepl, name, content string) string {
	t.Helper()
	path := filepath.Join(r.cfg.WorkDir, name)
	writeFileOrFatal(t, path, content)
	r.carry.RecordEditedFile(path)
	return path
}

func TestReviewGateDisabled(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	r.cfg.ReviewAfterEdit = false
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("disabled gate returned %q, want empty", got)
	}
	if fake.calls != 0 {
		t.Fatalf("disabled gate dispatched %d reviews, want 0", fake.calls)
	}
}

func TestReviewGatePlanModeSkipped(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	r.planMode = true
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("plan-mode gate returned %q, want empty", got)
	}
	if fake.calls != 0 {
		t.Fatalf("plan-mode gate dispatched %d reviews, want 0", fake.calls)
	}
}

func TestReviewGateEmptyScopeZeroCost(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("empty-scope gate returned %q, want empty", got)
	}
	if fake.calls != 0 {
		t.Fatalf("empty-scope gate dispatched %d reviews, want 0", fake.calls)
	}
}

func TestReviewGatePassClearsSlate(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("pass verdict returned fix message %q", got)
	}
	if fake.calls != 1 {
		t.Fatalf("dispatched %d reviews, want 1", fake.calls)
	}
	if r.carry.EditedFiles() != nil {
		t.Fatal("pass must clear the edited-file slate")
	}
	if !strings.Contains(ui.lastInfo(), "review: pass") {
		t.Fatalf("lastInfo = %q, want pass confirmation", ui.lastInfo())
	}
}

func TestReviewGateFailSynthesizesFixMessage(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, _ := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	got := r.reviewGate(context.Background(), "the original request", worktreeSnapshot{}, 0)
	for _, want := range []string{
		"[adversarial-review round 1/2]",
		"either fix it, or state explicitly why it is not a real problem",
		"a.go:7",
		"failure scenario: call F(nil) -> panic",
		"suggestion: guard nil",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fix message missing %q:\n%s", want, got)
		}
	}
	if r.carry.EditedFiles() == nil {
		t.Fatal("fail must NOT clear the edited-file slate — the fix round accumulates onto it")
	}
	// The reviewer prompt anchors on the episode's initial request, and
	// never includes the implementer's reasoning.
	prompt, _ := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "the original request") {
		t.Fatalf("reviewer prompt missing the initial request:\n%s", prompt)
	}
	if agentType, _ := fake.args["agent_type"].(string); agentType != "correctness-reviewer" {
		t.Fatalf("agent_type = %q", agentType)
	}
}

func TestReviewGateRoundCapPresentsToHuman(t *testing.T) {
	fake := &fakeTaskTool{content: failVerdictJSON()}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, maxReviewRounds); got != "" {
		t.Fatalf("at round cap the gate must end the episode, got fix message %q", got)
	}
	if !strings.Contains(ui.lastInfo(), "STILL FAILING") {
		t.Fatalf("lastInfo = %q, want unresolved-issues presentation", ui.lastInfo())
	}
	if !strings.Contains(ui.lastInfo(), "nil deref") {
		t.Fatalf("presentation must carry the issues; lastInfo = %q", ui.lastInfo())
	}
}

func TestReviewGateFailSoftOnToolError(t *testing.T) {
	fake := &fakeTaskTool{err: fmt.Errorf("pool exploded")}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("tool error must fail soft, got %q", got)
	}
	if !strings.Contains(ui.lastInfo(), "changes are unreviewed") {
		t.Fatalf("lastInfo = %q, want explicit unreviewed warning", ui.lastInfo())
	}
}

func TestReviewGateFailSoftOnUnparseableVerdict(t *testing.T) {
	fake := &fakeTaskTool{content: "I could not decide, sorry, no JSON here"}
	r, ui := newReviewRepl(t, t.TempDir(), fake)
	seedEditedFile(t, r, "a.go", "package a\n")

	if got := r.reviewGate(context.Background(), "req", worktreeSnapshot{}, 0); got != "" {
		t.Fatalf("unparseable verdict must fail soft, got %q", got)
	}
	if !strings.Contains(ui.lastInfo(), "unparseable") {
		t.Fatalf("lastInfo = %q, want unparseable warning", ui.lastInfo())
	}
}

// A reviewer that writes the worktree gets its verdict discarded — even a
// "pass" — and the slate stays dirty (design §4.4 B4: the snapshot is the
// only hard line; bash is unsandboxed).
func TestReviewGateTamperDiscardsVerdict(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui := newReviewRepl(t, dir, fake)
	seedEditedFile(t, r, "a.go", "package a\n")
	fake.sideEffect = func() {
		writeFileOrFatal(t, filepath.Join(dir, "tampered.go"), "package a // reviewer wrote this\n")
	}

	before := takeWorktreeSnapshot(dir)
	if got := r.reviewGate(context.Background(), "req", before, 0); got != "" {
		t.Fatalf("tampered review must fail soft, got %q", got)
	}
	if !strings.Contains(ui.lastInfo(), "DISCARDED") {
		t.Fatalf("lastInfo = %q, want discard warning", ui.lastInfo())
	}
	if !strings.Contains(ui.lastInfo(), "tampered.go") {
		t.Fatalf("discard warning must name the written file; lastInfo = %q", ui.lastInfo())
	}
	if r.carry.EditedFiles() == nil {
		t.Fatal("a discarded verdict must not clear the slate")
	}
}

// Degradation rung (b): a scope whose context bundle would exceed the
// subagent cap drops context_files entirely (diff-only review).
func TestReviewGateDiffOnlyDegradation(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, dir, fake)
	// Rung (b) needs a SMALL diff over LARGE files: 5 × 70KiB committed
	// files (bundle accounting caps each at 64KiB → 320KiB > 256KiB) that
	// each receive a one-line change (diff stays tiny, rung (c) untouched).
	// Many short lines: a one-line append then diffs with a few short
	// context lines, not one 70KiB context line.
	big := strings.Repeat("line\n", 14<<10)
	for i := 0; i < 5; i++ {
		writeFileOrFatal(t, filepath.Join(dir, fmt.Sprintf("big%d.txt", i)), big)
	}
	runGitOrFatal(t, dir, "add", ".")
	runGitOrFatal(t, dir, "commit", "-q", "-m", "big files")
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("big%d.txt", i)
		writeFileOrFatal(t, filepath.Join(dir, name), big+"changed\n")
		r.carry.RecordEditedFile(filepath.Join(dir, name))
	}

	before := takeWorktreeSnapshot(dir)
	// Snapshot AFTER seeding so the gate's own S1 sees the same tree and
	// attribution comes from the carry records.
	if got := r.reviewGate(context.Background(), "req", before, 0); got != "" {
		t.Fatalf("want pass, got fix message %q", got)
	}
	if fake.calls != 1 {
		t.Fatalf("dispatched %d reviews, want 1", fake.calls)
	}
	if _, present := fake.args["context_files"]; present {
		t.Fatal("oversized bundle must degrade to diff-only (no context_files)")
	}
	prompt, _ := fake.args["prompt"].(string)
	if !strings.Contains(prompt, "File contents omitted for size") {
		t.Fatal("diff-only prompt must tell the reviewer to pull files itself")
	}
}

// Degradation rung (c): a diff over the byte cap is not reviewed at all,
// and the user is told loudly.
func TestReviewGateOversizedDiffSkips(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, ui := newReviewRepl(t, dir, fake)
	seedEditedFile(t, r, "huge.txt", strings.Repeat("line of text\n", (reviewDiffByteCap/13)+2000))

	before := takeWorktreeSnapshot(dir)
	if got := r.reviewGate(context.Background(), "req", before, 0); got != "" {
		t.Fatalf("oversized diff must end the episode, got %q", got)
	}
	if fake.calls != 0 {
		t.Fatalf("oversized diff dispatched %d reviews, want 0", fake.calls)
	}
	if !strings.Contains(ui.lastInfo(), "NOT reviewed") {
		t.Fatalf("lastInfo = %q, want loud not-reviewed warning", ui.lastInfo())
	}
}

// The scoped diff must include untracked new files via --no-index (design
// N1) and exclude the user's own unrelated dirty files (design B2).
func TestBuildReviewDiffScopedWithUntracked(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t) // has committed.go (tracked) + userdirty.go (untracked, user's)

	// Agent's changes: modify tracked file, add a new untracked file.
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc Changed() {}\n")
	newFile := filepath.Join(dir, "brandnew.go")
	writeFileOrFatal(t, newFile, "package x\n\nfunc New() {}\n")

	snap := takeWorktreeSnapshot(dir)
	scope := []string{filepath.Join(snap.root, "committed.go"), filepath.Join(snap.root, "brandnew.go")}
	diff, oversized := buildReviewDiff(dir, snap, scope)
	if oversized {
		t.Fatal("small diff flagged oversized")
	}
	for _, want := range []string{"func Changed()", "func New()", "new file (untracked)"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "userdirty") {
		t.Fatalf("user's unrelated dirty file leaked into the scoped diff:\n%s", diff)
	}
}

func TestSynthesizeFixMessageFormat(t *testing.T) {
	v := &agent.ReviewResult{
		Verdict: "fail",
		Issues: []agent.Issue{
			{Severity: "high", File: "a.go", Line: 3, Message: "m1", Scenario: "s1"},
			{Severity: "low", File: "b.go", Line: 9, Message: "m2"}, // no scenario/suggestion
		},
	}
	msg := synthesizeFixMessage(2, v)
	if !strings.HasPrefix(msg, "[adversarial-review round 2/2]") {
		t.Fatalf("missing round prefix: %q", msg)
	}
	if !strings.Contains(msg, "failure scenario: s1") {
		t.Fatal("scenario line missing")
	}
	if strings.Count(msg, "failure scenario:") != 1 {
		t.Fatal("empty scenario must not render a scenario line")
	}
}

func TestContextBundleBytesMirrorsPerFileCap(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	writeFileOrFatal(t, big, strings.Repeat("x", 300<<10))
	small := filepath.Join(dir, "small.txt")
	writeFileOrFatal(t, small, "hello")
	missing := filepath.Join(dir, "gone.txt")

	got := contextBundleBytes([]string{big, small, missing})
	want := reviewContextPerFileCap + 5
	if got != want {
		t.Fatalf("contextBundleBytes = %d, want %d (per-file cap + small file)", got, want)
	}
}

// JSON sanity for the canned verdicts used above — a typo here would turn
// every gate test into a fail-soft test.
func TestCannedVerdictsParse(t *testing.T) {
	for _, s := range []string{passVerdictJSON(), failVerdictJSON()} {
		var v agent.ReviewResult
		if err := json.Unmarshal([]byte(strings.ReplaceAll(s, "\n", "")), &v); err != nil {
			t.Fatalf("canned verdict does not parse: %v\n%s", err, s)
		}
	}
}

func TestUnionSorted(t *testing.T) {
	got := unionSorted([]string{"/b", "/a"}, []string{"/c", "/a"})
	want := []string{"/a", "/b", "/c"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("unionSorted = %v, want %v", got, want)
	}
	if unionSorted(nil, nil) != nil {
		t.Fatal("empty union must be nil")
	}
}

func TestRunGitNoIndexDiffExitCodeOne(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	writeFileOrFatal(t, f, "content\n")
	out, err := runGitNoIndexDiff(dir, f)
	if err != nil {
		t.Fatalf("no-index diff: %v", err)
	}
	if !strings.Contains(string(out), "+content") {
		t.Fatalf("no-index diff output missing added line:\n%s", out)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal(err)
	}
}
