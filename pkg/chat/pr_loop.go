package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools/builtin"
)

// maxPRReviewRounds bounds the PR loop's review→fix cycle. Same reason as
// maxReviewRounds: an unbounded loop burns tokens on a reviewer the
// implementer demonstrably cannot satisfy — at the cap the findings go to the
// human instead.
const maxPRReviewRounds = 5

const (
	prCIDefaultPoll = 20 * time.Second
	prCIDefaultWait = 15 * time.Minute
)

func (r *ChatRepl) prGHOrDefault() prGH {
	if r.prGH != nil {
		return r.prGH
	}
	return ghPRClient{}
}

// runPRLoop drives one tracked PR through review→fix→re-review rounds to a
// terminal state, the same shape runMission gives the mission phases: an
// ordinary-turn loop that only decides the next turn's input. Every fail-soft
// exit (review could not run, CI lookup error, interrupted fix turn) leaves
// the persisted status untouched so /pr can resume exactly where it stopped.
// Fix turns deliberately go through runMissionTurn, not runEpisode — the
// local review_after_edit gate lives in runEpisode, and running it on top of
// the PR loop's own reviewer would double-review every fix for no signal.
func (r *ChatRepl) prMaxRounds() int {
	if r.cfg.PRReviewRounds > 0 {
		return r.cfg.PRReviewRounds
	}
	return maxPRReviewRounds
}

func (r *ChatRepl) runPRLoop(parentCtx context.Context, st *prState) {
	gh := r.prGHOrDefault()
	maxRounds := r.prMaxRounds()
	// Resume: reload the last FAIL verdict so a crash between the reviewer's
	// post and the fix turn still re-reviews against what was actually
	// reported. Round-gated — a verdict from an older cycle never masquerades
	// as this round's prev.
	var prev *agent.ReviewResult
	if v, round := loadLastVerdict(r.cfg.WorkDir, st.Number); v != nil && round == st.Round-1 {
		prev = v
	}
	// ownLogin identifies this gh account's comments; the external passthrough
	// filters on it. Unknown (Login failed) disables the passthrough rather
	// than mis-classifying deepai's own comments as external input.
	ownLogin, err := gh.Login(parentCtx)
	if err != nil {
		ownLogin = ""
		r.ui.Info(fmt.Sprintf("  pr: could not resolve the gh login (%v) — external PR comments will not be surfaced", err))
	}

	for {
		switch st.Status {
		case prStatusReviewing:
			if st.Round > maxRounds {
				r.ui.Info(fmt.Sprintf("  pr: #%d round cap reached — the loop stops here", st.Number))
				if prev != nil {
					r.presentIssues(fmt.Sprintf("  pr #%d unresolved review findings", st.Number), prev)
				}
				st.setStatus(r.cfg.WorkDir, prStatusAborted)
				return
			}
			r.ui.Info(fmt.Sprintf("  pr: #%d review round %d/%d", st.Number, st.Round, maxRounds))
			verdict, ok := r.dispatchPRReview(parentCtx, st, gh, prev)
			if !ok {
				r.ui.Info("  pr: review could not run — nothing posted, no round consumed; /pr resumes the loop")
				return
			}
			if isPassVerdict(verdict) {
				r.ui.Info(fmt.Sprintf("  pr: #%d review passed — waiting for CI", st.Number))
				prev = nil
				st.setStatus(r.cfg.WorkDir, prStatusAwaitingCI)
				continue
			}
			prev = verdict
			if err := st.appendVerdict(r.cfg.WorkDir, st.Round, verdict); err != nil {
				r.ui.Info(fmt.Sprintf("  pr: could not persist the verdict (%v) — continuing", err))
			}
			if err := gh.PostComment(parentCtx, st.Repo, st.Number, reviewerCommentBody(st.Round, verdict)); err != nil {
				r.ui.Info(fmt.Sprintf("  pr: could not post the review comment (%v) — stopping; /pr resumes", err))
				return
			}
			ext := pendingExternalComments(parentCtx, st, gh, ownLogin)
			turnErr := r.runMissionTurn(parentCtx, prFixMessage(st.Round, maxRounds, verdict)+externalCommentsBlock(ext))
			if turnErr != nil {
				r.ui.Info("  pr: fix turn interrupted or failed — no round consumed; /pr resumes the loop")
				return
			}
			r.advanceExternalWatermark(st, ext)
			if err := gh.PostComment(parentCtx, st.Repo, st.Number, coderCommentBody(st.Round, r.lastTurnText())); err != nil {
				r.ui.Info(fmt.Sprintf("  pr: could not post the fix comment (%v) — continuing", err))
			}
			st.Round++
			st.save(r.cfg.WorkDir)

		case prStatusAwaitingCI:
			r.ui.WaitStart(fmt.Sprintf("PR #%d CI", st.Number))
			done, ok, summary, err := r.waitPRCI(parentCtx, st, gh)
			r.ui.WaitEnd()
			if err != nil {
				if errors.Is(err, errPRCIInterrupted) {
					r.ui.Info("  pr: CI wait interrupted — status intact, /pr resumes the loop")
				} else {
					r.ui.Info(fmt.Sprintf("  pr: CI lookup failed (%v) — /pr resumes the loop", err))
				}
				return
			}
			switch {
			case done && ok:
				if r.cfg.PRAutoMerge {
					r.mergePRAndContinue(parentCtx, st, gh)
					return
				}
				st.setStatus(r.cfg.WorkDir, prStatusAwaitingMerge)
				r.ui.Info(fmt.Sprintf("  pr: #%d all green — awaiting merge (/pr merge, or just say merge)", st.Number))
				return
			case done && !ok:
				if st.Round > maxRounds {
					r.ui.Info(fmt.Sprintf("  pr: #%d round cap reached with CI failing — CI output follows", st.Number))
					r.ui.Info(clip(summary, 2048))
					st.setStatus(r.cfg.WorkDir, prStatusAborted)
					return
				}
				r.ui.Info(fmt.Sprintf("  pr: #%d CI failed — fix round %d", st.Number, st.Round))
				ext := pendingExternalComments(parentCtx, st, gh, ownLogin)
				turnErr := r.runMissionTurn(parentCtx, prCIFailMessage(st.Round, maxRounds, summary)+externalCommentsBlock(ext))
				if turnErr != nil {
					r.ui.Info("  pr: CI fix turn interrupted or failed — /pr resumes the loop")
					return
				}
				r.advanceExternalWatermark(st, ext)
				if err := gh.PostComment(parentCtx, st.Repo, st.Number, coderCommentBody(st.Round, r.lastTurnText())); err != nil {
					r.ui.Info(fmt.Sprintf("  pr: could not post the fix comment (%v) — continuing", err))
				}
				st.Round++
				st.setStatus(r.cfg.WorkDir, prStatusReviewing)
			default:
				r.ui.Info(fmt.Sprintf("  pr: #%d CI still pending after %s — /pr resumes the loop", st.Number, r.prCIWait()))
				return
			}

		default:
			return
		}
	}
}

// dispatchPRReview runs one correctness review against the PR's CURRENT diff
// (the fix turn pushed before re-entering, so the diff is always fresher than
// the previous verdict). ok=false is the same fail-soft contract as the local
// gate: interrupted, timed out, unparseable, or an oversized diff — no round
// is consumed.
func (r *ChatRepl) dispatchPRReview(parentCtx context.Context, st *prState, gh prGH, prev *agent.ReviewResult) (*agent.ReviewResult, bool) {
	diff, err := gh.Diff(parentCtx, st.Repo, st.Number)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: gh pr diff failed (%v)", err))
		return nil, false
	}
	if len(diff) > reviewDiffByteCap {
		r.ui.Info(fmt.Sprintf("  pr: diff exceeds %dKB — NOT reviewed; split the PR", reviewDiffByteCap>>10))
		return nil, false
	}
	files, err := gh.ChangedFiles(parentCtx, st.Repo, st.Number)
	if err != nil {
		files = nil // the diff itself already names every file; not fatal
	}
	if !equalStrings(st.Scope, files) {
		st.Scope = files
		st.save(r.cfg.WorkDir)
	}
	// No context bundle in PR mode: the worktree IS the PR branch (the fix
	// turn just pushed it), so read_file sees exactly what the diff shows.
	return r.runReview(parentCtx, reviewPromptInput{
		initialRequest: st.Brief,
		diff:           diff,
		scope:          relToWorkDir(r.cfg.WorkDir, absPaths(r.cfg.WorkDir, files)),
		bundled:        false,
		prev:           prev,
		prNumber:       st.Number,
		prRound:        st.Round,
	}, nil, takeWorktreeSnapshot(r.cfg.WorkDir))
}

// errPRCIInterrupted marks a CI wait cut short by Ctrl+C. A sentinel so the
// caller can say "interrupted, status intact, /pr resumes" instead of
// reporting it as a CI lookup failure (round-2 review issue 2: the wait
// blocked the REPL goroutine with no interrupt path for up to 15 minutes).
var errPRCIInterrupted = fmt.Errorf("CI wait interrupted")

// waitPRCI polls gh until the PR's checks settle or the wait budget runs out.
// (done=false, err=nil) means still pending at the deadline — fail-soft, the
// status stays awaiting_ci for /pr to resume. Ctrl+C returns
// errPRCIInterrupted with the status equally untouched. Progress is printed
// roughly once a minute so a long wait is visibly alive.
func (r *ChatRepl) waitPRCI(parentCtx context.Context, st *prState, gh prGH) (done, ok bool, summary string, err error) {
	deadline := time.Now().Add(r.prCIWait())
	poll := prCIDefaultPoll
	if r.prCIPollInterval > 0 {
		poll = r.prCIPollInterval
	}
	start := time.Now()
	lastProgress := time.Now()
	for {
		done, ok, summary, err = gh.Checks(parentCtx, st.Repo, st.Number)
		if err != nil || done {
			return
		}
		if time.Now().After(deadline) {
			return false, false, summary, nil
		}
		if now := time.Now(); now.Sub(lastProgress) >= time.Minute {
			lastProgress = now
			r.ui.Info(fmt.Sprintf("  pr: #%d CI still pending (%.0fs elapsed, will wait %.0fs more) — Ctrl+C stops the wait",
				st.Number, now.Sub(start).Seconds(), time.Until(deadline).Seconds()))
		}
		select {
		case <-parentCtx.Done():
			return false, false, "", parentCtx.Err()
		case <-r.ui.InterruptCh():
			return false, false, "", errPRCIInterrupted
		case <-time.After(poll):
		}
	}
}

func (r *ChatRepl) prCIWait() time.Duration {
	if r.prCIWaitTimeout > 0 {
		return r.prCIWaitTimeout
	}
	return prCIDefaultWait
}

// lastTurnText returns the final assistant text of the turn that just ran,
// for the coder comment's summary. Empty when nothing is recoverable — the
// comment body falls back to a fixed line.
func (r *ChatRepl) lastTurnText() string {
	if r.sess == nil {
		return ""
	}
	for i := len(r.sess.Messages) - 1; i >= 0; i-- {
		m := r.sess.Messages[i]
		if m.Role == "ai" && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// prFixMessage is the fix turn's synthesized input. "Fix it OR state why it
// is not a real problem" mirrors synthesizeFixMessage: reviewers err too,
// and the rebuttal flows into the next review round for the reviewer to
// judge.
func prFixMessage(round, maxRounds int, v *agent.ReviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[pr-review round %d/%d] An independent review of the pull request found the following issues. For each one: either fix it, or state explicitly why it is not a real problem. Commit the fixes and push them to the PR branch.\n", round, maxRounds)
	writeIssueList(&b, v.Issues)
	return b.String()
}

func prCIFailMessage(round, maxRounds int, summary string) string {
	return fmt.Sprintf("[pr-review round %d/%d] CI on the pull request failed. Fix the failure, commit and push to the PR branch. CI output:\n%s",
		round, maxRounds, clip(summary, 4096))
}

func reviewerCommentBody(round int, v *agent.ReviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**deepai review — round %d: %s**\n\n", round, v.Verdict)
	writeIssueList(&b, v.Issues)
	if s := strings.TrimSpace(v.Summary); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}
	return b.String()
}

func coderCommentBody(round int, turnSummary string) string {
	s := strings.TrimSpace(turnSummary)
	if s == "" {
		s = "pushed fixes for this round"
	}
	return fmt.Sprintf("**deepai fix — round %d**\n\n%s\n", round, clip(s, 2048))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n(truncated)"
}

// maybeAttachPRLoop runs after an ordinary turn: if the turn's bash output
// shows a freshly created PR and auto-attach is on, the review loop takes
// over synchronously. Suppressed while a mission runs — the mission's own
// gates own that turn's edits — and for PRs already tracked (a fail-soft
// stop keeps its state active for /pr resume).
func (r *ChatRepl) maybeAttachPRLoop(parentCtx context.Context) {
	if !r.cfg.PRReviewAuto || r.mission != nil || r.sess == nil {
		return
	}
	repo, number, url, ok := detectPRCreate(turnBashOutputs(r.sess.Messages))
	if !ok {
		return
	}
	if _, err := openPRState(r.cfg.WorkDir, number); err == nil {
		return
	}
	st, err := newPRState(r.cfg.WorkDir, number, repo, "", "", url, r.lastUserRequest())
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not start the review loop for #%d (%v)", number, err))
		return
	}
	r.ui.Info(fmt.Sprintf("  pr: #%d created — entering the review loop", number))
	r.runPRLoop(parentCtx, st)
}

// turnBashOutputs returns the bash tool-result contents of the CURRENT turn:
// every message after the last human one. Stateless — derived from message
// order rather than a turn-start bookmark, so nothing can go stale if the
// hook moves.
func turnBashOutputs(messages []models.Message) []string {
	var start int
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == models.RoleHuman {
			start = i + 1
			break
		}
	}
	var out []string
	for _, m := range messages[start:] {
		if m.Role == models.RoleTool && m.ToolResult != nil && m.ToolResult.ToolName == "bash" {
			out = append(out, m.ToolResult.Content)
		}
	}
	return out
}

// isMergeInput matches the terse confirmations the user types when a PR is
// all green. Tight on purpose: a sentence merely containing "merge" must
// reach the model as ordinary input.
func isMergeInput(line string) bool {
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "merge", "merge it", "合并", "可以合并":
		return true
	}
	return false
}

// awaitingMergePR returns the oldest PR parked in awaiting_merge, or nil.
func (r *ChatRepl) awaitingMergePR() *prState {
	for _, st := range activePRStates(r.cfg.WorkDir) {
		if st.Status == prStatusAwaitingMerge {
			return st
		}
	}
	return nil
}

// pendingExternalComments fetches the PR's comments and returns the external
// ones — authored by another identity (humans, cursor, …), told apart by
// author login rather than body markers — newer than the state's watermark,
// oldest first, capped. An empty ownLogin (gh login unknown) disables the
// passthrough: guessing would feed deepai's own comments back to the coder
// as external demands.
func pendingExternalComments(ctx context.Context, st *prState, gh prGH, ownLogin string) []prComment {
	if ownLogin == "" {
		return nil
	}
	comments, err := gh.ListComments(ctx, st.Repo, st.Number)
	if err != nil {
		return nil
	}
	external := filterExternalComments(comments, ownLogin)
	var fresh []prComment
	for _, c := range external {
		if externalCommentPending(c, st) {
			fresh = append(fresh, c)
		}
	}
	// Cap at the newest: external chatter is context for the coder, not a
	// mandate, and an unbounded block could crowd the issues out of the fix
	// turn's input. The newest comment always survives even alone over the
	// cap (externalCommentsBlock clips its body) — an over-budget first hit
	// must not empty the whole passthrough.
	const capBytes = 8 << 10
	total := 0
	start := len(fresh) - 1
	for i := len(fresh) - 1; i >= 0; i-- {
		total += len(fresh[i].Body)
		if total > capBytes && i < len(fresh)-1 {
			break
		}
		start = i
	}
	if start < 0 {
		start = 0
	}
	return fresh[start:]
}

// externalCommentPending reports whether c is newer than the surfaced
// watermark: strictly after its timestamp, or at exactly the watermark
// second with an id not yet surfaced — gh timestamps are second-granular,
// so time alone cannot tell "already surfaced" from "posted in the same
// second later" (round-2 review issue 4).
func externalCommentPending(c prComment, st *prState) bool {
	if c.CreatedAt.After(st.LastExternalCommentAt) {
		return true
	}
	if c.CreatedAt.Equal(st.LastExternalCommentAt) {
		for _, id := range st.SurfacedCommentIDs {
			if id == c.ID {
				return false
			}
		}
		return true
	}
	return false
}

// advanceExternalWatermark moves the dedup cursor past the comments the fix
// turn just saw (empty keeps the current cursor). The same-second id set
// ACCUMULATES while the watermark second does not move — rebuilding it from
// just this round's list would resurrect previously-surfaced same-second
// ids — and is rebuilt fresh when the second advances, because older ids
// then sit behind the timestamp for good. A failed save is logged by the
// caller; re-surfacing one comment after a crash is the cheaper failure.
func (r *ChatRepl) advanceExternalWatermark(st *prState, comments []prComment) {
	max := st.LastExternalCommentAt
	for _, c := range comments {
		if c.CreatedAt.After(max) {
			max = c.CreatedAt
		}
	}
	if !max.Equal(st.LastExternalCommentAt) {
		st.SurfacedCommentIDs = nil
	}
	st.LastExternalCommentAt = max
	for _, c := range comments {
		if c.CreatedAt.Equal(max) {
			st.SurfacedCommentIDs = append(st.SurfacedCommentIDs, c.ID)
		}
	}
	if err := st.save(r.cfg.WorkDir); err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not persist the comment watermark (%v)", err))
	}
}

// externalCommentsBlock renders the passthrough section of a fix turn's
// input. Empty string when there is nothing new — the fix message must not
// grow an empty section header.
func externalCommentsBlock(comments []prComment) string {
	if len(comments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nComments left on the PR by others (address them alongside the review issues where they overlap):\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "\n— %s (via PR comment):\n%s\n", c.Author, clip(c.Body, 2048))
	}
	return b.String()
}

// mergePRAndContinue merges the PR and kicks off the next pending todo as
// an ordinary turn. The merge command path ("merge" input routing and /pr
// merge) and pr_auto_merge both land here — one merge semantics, two doors.
// A failed merge or no-next-task both leave the PR in awaiting_merge; the
// loop never wedges.
func (r *ChatRepl) mergePRAndContinue(parentCtx context.Context, st *prState, gh prGH) {
	if err := gh.Merge(parentCtx, st.Repo, st.Number); err != nil {
		r.ui.Info(fmt.Sprintf("  pr: #%d merge failed (%v) — still awaiting_merge; /pr merge retries", st.Number, err))
		return
	}
	if err := st.setStatus(r.cfg.WorkDir, prStatusMerged); err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not persist merged status (%v)", err))
	}
	r.ui.Info(fmt.Sprintf("  pr: #%d merged", st.Number))

	next, idx := nextPendingTodo(r.carry.Todos())
	if next == "" {
		r.ui.Info("  pr: no pending task in the todo list — all done")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d was merged. Mark todo item %d (\"%s\") done with todo_write and start it now.", st.Number, idx, next)
	r.ui.Info(fmt.Sprintf("  pr: next task — %s", next))
	if turnErr := r.runMissionTurn(parentCtx, b.String()); turnErr != nil {
		r.ui.Info("  pr: next-task turn interrupted — the task list still has it pending")
	}
}

// nextPendingTodo returns the first pending item's content and its 1-based
// index. The list is the model's own plan; the loop only names what to start,
// never reorders or rewrites it.
func nextPendingTodo(todos []builtin.TodoItem) (string, int) {
	for i, t := range todos {
		if t.Status == builtin.TodoPending {
			return t.Content, i + 1
		}
	}
	return "", 0
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// absPaths joins workdir-relative PR file paths back to absolute paths for
// relToWorkDir's input contract. PR paths that are already absolute pass
// through untouched.
func absPaths(workDir string, files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f, "/") {
			out = append(out, f)
		} else {
			out = append(out, workDir+"/"+f)
		}
	}
	return out
}
