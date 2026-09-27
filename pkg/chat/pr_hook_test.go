package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools/builtin"
)

func TestDetectPRCreate(t *testing.T) {
	create := "Creating pull request for feature/x into develop in millken/jp-small\n\nhttps://github.com/millken/jp-small/pull/80\n"
	repo, n, url, ok := detectPRCreate([]string{create})
	if !ok || repo != "millken/jp-small" || n != 80 || url != "https://github.com/millken/jp-small/pull/80" {
		t.Fatalf("detectPRCreate = (%q,%d,%q,%v)", repo, n, url, ok)
	}

	// Two PRs in one turn: the newest wins.
	two := "Creating pull request... https://github.com/o/r/pull/41\nCreating pull request... https://github.com/o/r/pull/42\n"
	if _, n, _, ok := detectPRCreate([]string{two}); !ok || n != 42 {
		t.Fatalf("last PR must win, got %d %v", n, ok)
	}

	// gh pr view of an OLD PR prints the same URL shape — must not attach.
	view := "title: something\nurl: https://github.com/millken/jp-small/pull/12\n"
	if _, _, _, ok := detectPRCreate([]string{view}); ok {
		t.Fatal("gh pr view output must not look like a create")
	}

	if _, _, _, ok := detectPRCreate(nil); ok {
		t.Fatal("empty input must not match")
	}
}

func TestTurnBashOutputs(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "ship it"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "bash", Status: models.CallStatusCompleted}}},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash", Content: `{"stdout":"Creating pull request https://github.com/o/r/pull/9"}`}},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "read_file", Content: "unrelated"}},
	}
	out := turnBashOutputs(msgs)
	if len(out) != 1 || !strings.Contains(out[0], "pull/9") {
		t.Fatalf("turnBashOutputs = %v, want the one bash output", out)
	}

	// Everything before the last human message belongs to earlier turns.
	msgs = append([]models.Message{{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash", Content: "stale"}}}, msgs...)
	if got := turnBashOutputs(msgs); len(got) != 1 {
		t.Fatalf("previous turn's outputs leaked in: %v", got)
	}
}

func TestMaybeAttachPRLoop(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)
	r.cfg.PRReviewAuto = true
	r.sess = &models.Session{Messages: []models.Message{
		{Role: models.RoleHuman, Content: "ship"},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash",
			Content: `{"stdout":"Creating pull request for feature/x into develop in millken/jp-small\n\nhttps://github.com/millken/jp-small/pull/81\n"}`}},
	}}

	r.maybeAttachPRLoop(context.Background())

	st, err := openPRState(r.cfg.WorkDir, 81)
	if err != nil {
		t.Fatalf("loop never tracked #81: %v", err)
	}
	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge (pass verdict + green CI)", st.Status)
	}

	// Re-running the hook on the same session must not restart a finished
	// loop's PR (it is terminal) — and a tracked ACTIVE one is skipped by the
	// openPRState guard in the hook.
	before := fake.calls
	r.maybeAttachPRLoop(context.Background())
	if fake.calls != before {
		t.Fatalf("second attach dispatched more reviews (%d -> %d)", before, fake.calls)
	}
}

func TestMaybeAttachPRLoop_SuppressedByMissionAndConfig(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line"}
	r := newPRLoopRepl(t, fake, gh)
	r.cfg.PRReviewAuto = true
	r.sess = &models.Session{Messages: []models.Message{
		{Role: models.RoleHuman, Content: "ship"},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash",
			Content: `{"stdout":"Creating pull request https://github.com/o/r/pull/5"}`}},
	}}

	r.mission = &mission{state: missionState{Status: missionStatusActive}}
	r.maybeAttachPRLoop(context.Background())
	if _, err := openPRState(r.cfg.WorkDir, 5); err == nil {
		t.Fatal("a running mission must suppress auto-attach")
	}

	r.mission = nil
	r.cfg.PRReviewAuto = false
	r.maybeAttachPRLoop(context.Background())
	if _, err := openPRState(r.cfg.WorkDir, 5); err == nil {
		t.Fatal("pr_review_auto disabled must suppress auto-attach")
	}
	if fake.calls != 0 {
		t.Fatalf("no review should have run, got %d", fake.calls)
	}
}

func TestIsMergeInput(t *testing.T) {
	for _, yes := range []string{"merge", " Merge ", "merge it", "合并", "可以合并"} {
		if !isMergeInput(yes) {
			t.Errorf("isMergeInput(%q) = false, want true", yes)
		}
	}
	for _, no := range []string{"merge the config files", "let's merge later", "先看看 diff 再 merge", "resume"} {
		if isMergeInput(no) {
			t.Errorf("isMergeInput(%q) = true, want false", no)
		}
	}
}

func TestMergePRAndContinue_NextTodoKickoff(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line"}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 30, 1, prStatusAwaitingMerge)

	r.carry.SetTodos([]builtin.TodoItem{
		{Content: "done task", Status: builtin.TodoDone},
		{Content: "ledger export", Status: builtin.TodoPending},
	})

	var got string
	r.missionTurn = func(ctx context.Context, input string) *turnError {
		got = input
		return nil
	}

	r.mergePRAndContinue(context.Background(), st, gh)

	if st.Status != prStatusMerged {
		t.Fatalf("status = %s, want merged", st.Status)
	}
	if !strings.Contains(got, "PR #30 was merged") || !strings.Contains(got, `"ledger export"`) {
		t.Fatalf("next-task turn input = %q", got)
	}
}

func TestMergePRAndContinue_FailedMergeStaysAwaiting(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	gh := &fakeGH{diff: "+line", mergeErr: context.DeadlineExceeded}
	r := newPRLoopRepl(t, fake, gh)
	st := newTrackedPR(t, r, 31, 1, prStatusAwaitingMerge)
	r.carry.SetTodos([]builtin.TodoItem{{Content: "next", Status: builtin.TodoPending}})

	ran := false
	r.missionTurn = func(ctx context.Context, input string) *turnError { ran = true; return nil }

	r.mergePRAndContinue(context.Background(), st, gh)

	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("failed merge must keep awaiting_merge, got %s", st.Status)
	}
	if ran {
		t.Fatal("no next-task turn may run when the merge failed")
	}
}

func TestNextPendingTodo(t *testing.T) {
	todos := []builtin.TodoItem{
		{Content: "a", Status: builtin.TodoDone},
		{Content: "b", Status: builtin.TodoInProgress},
		{Content: "c", Status: builtin.TodoPending},
	}
	if c, i := nextPendingTodo(todos); c != "c" || i != 3 {
		t.Fatalf("nextPendingTodo = (%q,%d), want (c,3)", c, i)
	}
	if c, i := nextPendingTodo(nil); c != "" || i != 0 {
		t.Fatalf("empty list = (%q,%d), want empty", c, i)
	}
	// In-progress counts as "not yet started for the loop's purposes" only if
	// no pending follows; the loop must not resurrect a list with nothing to do.
	done := []builtin.TodoItem{{Content: "x", Status: builtin.TodoDone}}
	if c, _ := nextPendingTodo(done); c != "" {
		t.Fatalf("all-done list returned %q", c)
	}
}

// Verify the agent package's Todos/SetTodos round trip stays available for
// the merge path (compile-time contract with pkg/agent).
func TestSessionCarryTodosRoundTrip(t *testing.T) {
	c := agent.NewSessionCarry()
	if len(c.Todos()) != 0 {
		t.Fatal("fresh carry has todos")
	}
	c.SetTodos([]builtin.TodoItem{{Content: "x", Status: builtin.TodoPending}})
	if got := c.Todos(); len(got) != 1 || got[0].Content != "x" {
		t.Fatalf("Todos() = %+v", got)
	}
}

func TestReviewPRCommand_AttachesExternalPR(t *testing.T) {
	fake := &fakeTaskTool{content: passVerdictJSON()}
	// The URL is the only place the repo can come from — the attach must pin
	// it so later gh calls never target the process's cwd instead (round-2
	// review issue 3).
	gh := &fakeGH{diff: "+line", files: []string{"a.go"}, viewTitle: "feat: PR review pipeline",
		viewURL:      "https://github.com/other/libY/pull/55",
		checksScript: []fakeChecks{{done: true, ok: true}}}
	r := newPRLoopRepl(t, fake, gh)

	r.handlePRCommand(context.Background(), "review 55")

	st, err := openPRState(r.cfg.WorkDir, 55)
	if err != nil {
		t.Fatalf("review 55 never tracked the PR: %v", err)
	}
	if st.Brief != "feat: PR review pipeline" {
		t.Fatalf("brief = %q, want the PR title", st.Brief)
	}
	if st.Repo != "other/libY" {
		t.Fatalf("repo = %q, want the repo pinned from the fetched URL", st.Repo)
	}
	if st.Status != prStatusAwaitingMerge {
		t.Fatalf("status = %s, want awaiting_merge after pass+green", st.Status)
	}

	// Already tracked: a second attach must not reset the loop.
	r.handlePRCommand(context.Background(), "review 55")
	if st2, _ := openPRState(r.cfg.WorkDir, 55); st2.UpdatedAt != st.UpdatedAt {
		t.Fatal("re-attach of a tracked PR must be a no-op")
	}
}

func TestReviewPRCommand_BadArgs(t *testing.T) {
	r, ui := newReviewRepl(t, t.TempDir(), &fakeTaskTool{})
	r.prGH = &fakeGH{}
	ui.infoMsgs = nil

	r.handlePRCommand(context.Background(), "review")
	r.handlePRCommand(context.Background(), "review abc")
	if len(ui.infoMsgs) != 2 {
		t.Fatalf("both bad args must be rejected, got %d messages", len(ui.infoMsgs))
	}
	for _, m := range ui.infoMsgs {
		if !strings.Contains(m, "review needs a PR number") {
			t.Fatalf("unexpected message: %q", m)
		}
	}
}
