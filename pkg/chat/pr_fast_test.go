package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestPRLoop_PassWithBlockingIssuesStillFails(t *testing.T) {
	// Model says "pass" while listing a high finding — severity gating must win.
	passWithHigh := `{"agent":"correctness-reviewer","verdict":"pass","summary":"mostly fine",
		"issues":[{"severity":"high","file":"a.go","line":10,"message":"race","scenario":"concurrent"}]}`
	fake := &fakeTaskTool{content: passWithHigh}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 80, 1, prStatusReviewing)

	fixTurns := 0
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		fixTurns++
		fake.content = passVerdictJSON()
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if fixTurns != 1 {
		t.Fatalf("fix turns = %d, want 1 (pass+high must block)", fixTurns)
	}
	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge after the fix round", st.Status)
	}
	if len(gh.commentsPosted) < 1 || !strings.Contains(gh.commentsPosted[0], "fail") {
		t.Fatalf("public comment must be relabeled fail, got %+v", gh.commentsPosted)
	}
}

func TestPRLoop_FailEmptyPayloadDoesNotPass(t *testing.T) {
	emptyFail := `{"agent":"correctness-reviewer","verdict":"fail","summary":"","issues":[]}`
	fake := &fakeTaskTool{content: emptyFail}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 81, 1, prStatusReviewing)

	fixTurns := 0
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		fixTurns++
		fake.content = passVerdictJSON()
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if fixTurns != 1 || st.Status != prStatusAwaitingMerge {
		t.Fatalf("empty fail payload must spend a fix round: fixTurns=%d status=%s", fixTurns, st.Status)
	}
}

func TestPRLocalWorktreeAligned(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	gitStep(t, dir, "checkout", "-q", "-b", "feat/align")
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "x")

	if prLocalWorktreeAligned(dir, "feat/align") {
		t.Fatal("missing origin/feat/align tracking ref must NOT align (fall back to gh.Diff)")
	}
	if prLocalWorktreeAligned(dir, "main") {
		t.Fatal("wrong expected branch must not align")
	}
	if prLocalWorktreeAligned(dir, "") {
		t.Fatal("empty branch must not align")
	}

	// Simulate a pushed tip that drifts from local HEAD.
	gitStep(t, dir, "update-ref", "refs/remotes/origin/feat/align", "HEAD")
	if !prLocalWorktreeAligned(dir, "feat/align") {
		t.Fatal("HEAD == origin/feat/align must align")
	}
	if err := os.WriteFile(filepath.Join(dir, "y.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "ahead")
	if prLocalWorktreeAligned(dir, "feat/align") {
		t.Fatal("local ahead of origin/feat/align must NOT align")
	}
}

func TestPRLoop_LowOnlyFindingsPassAsAdvisory(t *testing.T) {
	lowOnly := `{"agent":"correctness-reviewer","verdict":"fail","summary":"minor nit only",
		"issues":[{"severity":"low","file":"a.go","line":10,"message":"minor naming","scenario":"cosmetic"}]}`
	fake := &fakeTaskTool{content: lowOnly}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 70, 1, prStatusReviewing)

	fixTurns := 0
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		fixTurns++
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge (low-only issues must not block under default min_severity=medium)", st.Status)
	}
	if fixTurns != 0 {
		t.Fatalf("fix turns = %d, want 0 for low-only findings", fixTurns)
	}
	if len(gh.commentsPosted) != 1 || !strings.Contains(gh.commentsPosted[0], "advisory notes only") {
		t.Fatalf("expected one advisory comment posted, got %+v", gh.commentsPosted)
	}
}

func TestPRLoop_MinSeverityLowBlocksOnLow(t *testing.T) {
	lowOnly := `{"agent":"correctness-reviewer","verdict":"fail","summary":"minor nit only",
		"issues":[{"severity":"low","file":"a.go","line":10,"message":"minor nit","scenario":"edge"}]}`
	fake := &fakeTaskTool{content: lowOnly}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	r.cfg.PRReviewMinSeverity = "low"
	st := newTrackedPR(t, r, 71, 1, prStatusReviewing)

	fixTurns := 0
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		fixTurns++
		fake.content = passVerdictJSON()
		return nil
	}

	r.runPRLoop(context.Background(), st)

	if fixTurns != 1 || st.Status != prStatusAwaitingMerge {
		t.Fatalf("with min_severity=low: fixTurns=%d status=%s, want 1 and awaiting_merge", fixTurns, st.Status)
	}
}

func TestPRLoop_CommentsFetchedOncePerRound(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	fake := &fakeTaskTool{content: failVerdictJSON()}
	var listCalls int
	baseGH := &fakeGH{diff: "+line", files: []string{"a.txt"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	gh := &countingCommentsGH{fakeGH: baseGH, onList: func() { listCalls++ }}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		fake.content = passVerdictJSON()
		return nil
	}
	st := newTrackedPR(t, r, 72, 1, prStatusReviewing)

	r.runPRLoop(context.Background(), st)

	// 2 review rounds (fail + pass), each calling ListComments ONCE (fix turn reuses round 1's list).
	if listCalls != 2 {
		t.Fatalf("ListComments called %d times, want 2 (1 per review round, reused by fix turn)", listCalls)
	}
}

type countingCommentsGH struct {
	*fakeGH
	onList func()
}

func (c *countingCommentsGH) ListComments(ctx context.Context, repo string, number int) ([]prComment, error) {
	c.onList()
	return c.fakeGH.ListComments(ctx, repo, number)
}

func TestDispatchPRReview_BundlesContextFilesAndReasoningEffort(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", files: []string{"a.txt", "b.txt"}}
	r, _ := newReviewRepl(t, dir, fake)
	r.prGH = gh

	st := newTrackedPR(t, r, 73, 1, prStatusReviewing)
	if _, ok := r.dispatchPRReview(context.Background(), st, gh, nil); !ok {
		t.Fatal("dispatch failed")
	}
	rawCF, ok := fake.args["context_files"].([]any)
	if !ok || len(rawCF) != 2 {
		t.Fatalf("PR review must bundle readable context_files under cap, got %#v", fake.args["context_files"])
	}
	if eff := fake.args["reasoning_effort"]; eff != defaultReviewReasoningEffort {
		t.Fatalf("reasoning_effort = %v, want %q", eff, defaultReviewReasoningEffort)
	}
}

func TestPRLocalDiffAgainstBaseBranch(t *testing.T) {
	dir, _, _ := gitTestRepo(t)
	// Create a feature branch off main and commit a change.
	gitStep(t, dir, "checkout", "-q", "-b", "feat/local-diff")
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitStep(t, dir, "add", "-A")
	gitStep(t, dir, "commit", "-q", "-m", "add feature")

	diff, files, ok := prLocalDiff(dir, "main")
	if !ok {
		t.Fatal("prLocalDiff should succeed against local main")
	}
	if len(files) != 1 || files[0] != "feature.go" {
		t.Fatalf("files = %v, want [feature.go]", files)
	}
	if !strings.Contains(diff, "feature.go") {
		t.Fatalf("diff missing feature.go:\n%s", diff)
	}
	if _, _, ok := prLocalDiff(dir, "nonexistent-branch"); ok {
		t.Fatal("missing base branch must return ok=false")
	}
}

func TestSplitUnifiedDiffByModuleAndMerge(t *testing.T) {
	var b strings.Builder
	// 12 files across 3 modules -> should split into 3 shards.
	modules := []string{"pkg/schedule", "pkg/bot", "cmd/deepai-bot"}
	for _, mod := range modules {
		for i := 0; i < 4; i++ {
			p := fmt.Sprintf("%s/file%d.go", mod, i)
			fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-old\n+new\n", p, p, p, p)
		}
	}
	shards := splitUnifiedDiffByModule(b.String(), prMaxShards)
	if len(shards) != 3 {
		t.Fatalf("shards = %d, want 3", len(shards))
	}
	totalFiles := 0
	for _, sh := range shards {
		totalFiles += len(sh.files)
	}
	if totalFiles != 12 {
		t.Fatalf("total files across shards = %d, want 12", totalFiles)
	}

	merged := mergeShardVerdicts([]*agent.ReviewResult{
		{Verdict: "pass", Summary: "mod A clean"},
		{Verdict: "fail", Summary: "mod B bug", Issues: []agent.Issue{
			{Severity: "low", File: "pkg/bot/a.go", Line: 1, Message: "nit"},
			{Severity: "high", File: "pkg/bot/b.go", Line: 2, Message: "race"},
		}},
		{Verdict: "fail", Summary: "mod C dup", Issues: []agent.Issue{
			{Severity: "high", File: "pkg/bot/b.go", Line: 2, Message: "race"}, // duplicate
		}},
	})
	if merged.Verdict != "fail" || len(merged.Issues) != 2 {
		t.Fatalf("merged = %+v, want fail with 2 deduplicated issues", merged)
	}
	if merged.Issues[0].Severity != "high" {
		t.Fatalf("merged issues must sort highest severity first: %+v", merged.Issues)
	}
}

func TestDispatchPRReview_ShardsLargeDiffInParallel(t *testing.T) {
	dir := t.TempDir()
	var diffB strings.Builder
	var files []string
	for _, mod := range []string{"pkg/alpha", "pkg/beta", "pkg/gamma"} {
		for i := 0; i < 4; i++ {
			rel := fmt.Sprintf("%s/f%d.go", mod, i)
			files = append(files, rel)
			abs := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(abs), 0o755)
			_ = os.WriteFile(abs, []byte("package x\n"), 0o644)
			fmt.Fprintf(&diffB, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-old\n+new\n", rel, rel, rel, rel)
		}
	}

	var calls atomic.Int32
	fake := &parallelCountingTaskTool{
		handler: func(call models.ToolCall) (models.ToolResult, error) {
			calls.Add(1)
			return models.ToolResult{CallID: call.ID, ToolName: "task", Status: models.CallStatusCompleted, Content: passVerdictJSON()}, nil
		},
	}
	r := newReviewReplWithHandler(t, dir, fake)
	gh := &fakeGH{diff: diffB.String(), files: files}
	r.prGH = gh
	st := newTrackedPR(t, r, 74, 1, prStatusReviewing)

	verdict, ok := r.dispatchPRReview(context.Background(), st, gh, nil)
	if !ok || verdict == nil || verdict.Verdict != "pass" {
		t.Fatalf("sharded review failed: ok=%v verdict=%+v", ok, verdict)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("parallel shard dispatches = %d, want 3", got)
	}
}

func TestRunShardedPRReview_RetriesOnlyFailingShards(t *testing.T) {
	dir := t.TempDir()
	shards := []prDiffShard{
		{name: "pkg/alpha", files: []string{"pkg/alpha/a.go"}, diff: "+a"},
		{name: "pkg/beta", files: []string{"pkg/beta/b.go"}, diff: "+b"},
		{name: "pkg/gamma", files: []string{"pkg/gamma/c.go"}, diff: "+c"},
	}

	var mu sync.Mutex
	attempts := map[string]int{}
	fake := &parallelCountingTaskTool{
		handler: func(call models.ToolCall) (models.ToolResult, error) {
			desc, _ := call.Arguments["description"].(string)
			mu.Lock()
			attempts[desc]++
			n := attempts[desc]
			mu.Unlock()
			switch {
			case strings.Contains(desc, "pkg/beta") && n == 1:
				return models.ToolResult{}, errors.New("temporary network reset")
			case strings.Contains(desc, "pkg/gamma") && n == 1:
				return models.ToolResult{CallID: call.ID, ToolName: "task", Status: models.CallStatusCompleted, Content: "not valid json"}, nil
			default:
				return models.ToolResult{CallID: call.ID, ToolName: "task", Status: models.CallStatusCompleted, Content: passVerdictJSON()}, nil
			}
		},
	}
	r := newReviewReplWithHandler(t, dir, fake)
	base := reviewPromptInput{prNumber: 90, prRound: 1, scope: []string{"pkg/alpha/a.go", "pkg/beta/b.go", "pkg/gamma/c.go"}, diff: "+all"}

	verdict, outcome := r.runShardedPRReview(context.Background(), base, shards, worktreeSnapshot{})
	if outcome != reviewOK || verdict == nil || verdict.Verdict != "pass" {
		t.Fatalf("expected retry recovery to pass, got outcome=%v verdict=%+v", outcome, verdict)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := attempts["Adversarial correctness review (pkg/alpha)"]; got != 1 {
		t.Errorf("healthy shard pkg/alpha ran %d times, want 1", got)
	}
	if got := attempts["Adversarial correctness review (pkg/beta)"]; got != 2 {
		t.Errorf("transient-error shard pkg/beta ran %d times, want 2", got)
	}
	if got := attempts["Adversarial correctness review (pkg/gamma)"]; got != 2 {
		t.Errorf("parse-error shard pkg/gamma ran %d times, want 2", got)
	}
}

func TestRunShardedPRReview_DeadlineAbortsSiblingRetries(t *testing.T) {
	dir := t.TempDir()
	shards := []prDiffShard{
		{name: "pkg/alpha", files: []string{"pkg/alpha/a.go"}, diff: "+a"},
		{name: "pkg/beta", files: []string{"pkg/beta/b.go"}, diff: "+b"},
	}

	var calls atomic.Int32
	fake := &parallelCountingTaskTool{
		handler: func(call models.ToolCall) (models.ToolResult, error) {
			calls.Add(1)
			desc, _ := call.Arguments["description"].(string)
			if strings.Contains(desc, "pkg/alpha") {
				return models.ToolResult{}, context.DeadlineExceeded
			}
			return models.ToolResult{}, errors.New("transient error")
		},
	}
	r := newReviewReplWithHandler(t, dir, fake)
	base := reviewPromptInput{prNumber: 91, prRound: 1, scope: []string{"pkg/alpha/a.go", "pkg/beta/b.go"}, diff: "+all"}

	_, outcome := r.runShardedPRReview(context.Background(), base, shards, worktreeSnapshot{})
	if outcome != reviewFailedTerminal {
		t.Fatalf("outcome = %v, want reviewFailedTerminal", outcome)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("total shard calls = %d, want 2 (terminal deadline must abort retry of sibling shard)", got)
	}
}

type parallelCountingTaskTool struct {
	handler func(call models.ToolCall) (models.ToolResult, error)
}

func newReviewReplWithHandler(t *testing.T, workDir string, p *parallelCountingTaskTool) *ChatRepl {
	t.Helper()
	fake := &fakeTaskTool{content: passVerdictJSON()}
	r, _ := newReviewRepl(t, workDir, fake)
	// Build a fresh registry with the concurrency-safe task handler.
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return p.handler(call)
		},
	})
	r.cfg.ToolRegistry = reg
	return r
}
