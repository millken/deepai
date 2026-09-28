package chat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// isPRPassVerdict is deliberately STRICTER than isPassVerdict, which also
// treats "no issues" as a pass. The round-3+ convergence prompt
// (buildReviewPrompt) tells the reviewer to park genuinely-new minor findings
// in the summary sentence OUTSIDE the issue list, so an empty issues array is
// a shape a COMPLIANT failing reviewer now produces on purpose. This check
// decides the loop's terminal pass — CI wait, then auto-merge — so a fail with
// an empty issue list must spend another fix round: exactly the hole
// isMissionPassVerdict closes on the mission path.
func isPRPassVerdict(v *agent.ReviewResult) bool {
	return v != nil && strings.EqualFold(strings.TrimSpace(v.Verdict), "pass")
}

func (r *ChatRepl) runPRLoop(parentCtx context.Context, st *prState) {
	gh := r.prGHOrDefault()
	maxRounds := r.prMaxRounds()
	// Resume: reload the last FAIL verdict so a crash anywhere after the
	// reviewer reported — before OR after the fix turn landed — still
	// re-reviews against the issues that were actually reported. Two gates
	// match the two crash points: round == st.Round is a verdict posted whose
	// fix turn never finished (round not consumed), round == st.Round-1 is a
	// finished fix awaiting the next review. Anything older never masquerades
	// as this round's prev.
	var prev *agent.ReviewResult
	prevRound := 0
	if v, round := loadLastVerdict(r.cfg.WorkDir, st.Number); v != nil && (round == st.Round-1 || round == st.Round) {
		prev, prevRound = v, round
	}
	// A CONFIRMED comment for THIS round gates the resume's skip-to-fix-turn
	// path — one round cannot be reported twice — and keeps the fix prompt's
	// "fixed or argued" premise true, since that turn never ran (round-5
	// review issue 2). The gate is CommentPostedRound, NEVER the verdict log:
	// appendVerdict runs before PostComment, so a transient gh failure between
	// them leaves a logged verdict with no public comment. That state
	// (unpostedVerdict) re-posts from the stored verdict on resume — no second
	// review, no duplicate, no missing timeline entry (new-loop round-1
	// issue 1).
	alreadyPosted := st.Status == prStatusReviewing && st.CommentPostedRound == st.Round
	unpostedVerdict := st.Status == prStatusReviewing && !alreadyPosted && prevRound == st.Round

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
			var verdict *agent.ReviewResult
			if alreadyPosted {
				alreadyPosted = false
				verdict = prev
				r.ui.Info(fmt.Sprintf("  pr: #%d round %d findings already posted — resuming at the fix turn", st.Number, st.Round))
			} else if unpostedVerdict {
				unpostedVerdict = false
				verdict = prev
				// The PR's own timeline is the truth here: a crash after PostComment
				// but before the posted-round marker persisted leaves the marker behind
				// while the comment IS public — re-posting would duplicate the review
				// comment (round-3 issue 3).
				onPR, lookupErr := prReviewerCommentOnPR(parentCtx, st, gh, st.Round)
				if lookupErr != nil {
					r.ui.Info(fmt.Sprintf("  pr: #%d could not verify whether round %d's comment is on the PR (%v) — not posting; /pr resumes", st.Number, st.Round, lookupErr))
					return
				}
				if !onPR {
					r.ui.Info(fmt.Sprintf("  pr: #%d round %d verdict was logged but its comment never reached the PR — posting it now", st.Number, st.Round))
					if err := gh.PostComment(parentCtx, st.Repo, st.Number, reviewerCommentBody(st.Round, verdict)); err != nil {
						r.ui.Info(fmt.Sprintf("  pr: could not post the review comment (%v) — stopping; /pr resumes", err))
						return
					}
				} else {
					r.ui.Info(fmt.Sprintf("  pr: #%d round %d comment is already on the PR — recording the marker only", st.Number, st.Round))
				}
				st.CommentPostedRound = st.Round
				if err := st.save(r.cfg.WorkDir); err != nil {
					r.ui.Info(fmt.Sprintf("  pr: could not persist the posted-round marker (%v)", err))
				}
			} else {
				var ok bool
				verdict, ok = r.dispatchPRReview(parentCtx, st, gh, prev)
				if !ok {
					r.ui.Info("  pr: review could not run — nothing posted, no round consumed; /pr resumes the loop")
					return
				}
				if isPRPassVerdict(verdict) {
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
				st.CommentPostedRound = st.Round
				if err := st.save(r.cfg.WorkDir); err != nil {
					r.ui.Info(fmt.Sprintf("  pr: could not persist the posted-round marker (%v)", err))
				}
			}
			ext := pendingExternalComments(parentCtx, st, gh)
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
				// Both doors into mergePRAndContinue must leave the PR in
				// awaiting_merge first: its failure path promises "still
				// awaiting_merge; /pr merge retries", and a persisted awaiting_ci
				// makes that advice a command the state machine refuses (round-3
				// review issue 2).
				st.setStatus(r.cfg.WorkDir, prStatusAwaitingMerge)
				if r.cfg.PRAutoMerge {
					r.mergePRAndContinue(parentCtx, st, gh)
					return
				}
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
				ext := pendingExternalComments(parentCtx, st, gh)
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

// prIncrementalDiff returns the commits-since diff from the last reviewed
// head — the human second-reviewer's reading list. ok=false when the anchor
// is unusable: an unresolvable sha, a sha that still resolves but is NO
// LONGER an ancestor of HEAD (rebase/reset divergence), or a range that
// contains a merge commit (`git merge origin/main` between rounds) — the
// last two both smuggle upstream commits into base..HEAD. The caller falls
// back to the full PR diff rather than guessing at a poisoned anchor.
func prIncrementalDiff(workDir, base string) (diff string, files []string, ok bool) {
	if base == "" {
		return "", nil, false
	}
	// --quiet: exit 0 only when base IS an ancestor of HEAD; 1 (or an error)
	// means rebase/reset/divergence — treat as a poisoned anchor.
	if _, err := runGit(workDir, "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		return "", nil, false
	}
	// Ancestry alone is not soundness — the merge half of the same trap:
	// `git merge origin/main` between rounds also satisfies --is-ancestor
	// while base..HEAD carries upstream commits, which the reviewer would
	// read as the implementer's fixes. A merge in the range means the delta
	// is not the PR's own commits — fall back too.
	if merges, err := runGit(workDir, "rev-list", "--merges", base+"..HEAD"); err != nil || strings.TrimSpace(string(merges)) != "" {
		return "", nil, false
	}
	out, err := runGit(workDir, "diff", "--unified=3", base+"..HEAD")
	if err != nil {
		return "", nil, false
	}
	names, err := runGit(workDir, "diff", "--name-only", base+"..HEAD")
	if err != nil {
		return "", nil, false
	}
	for _, f := range strings.Split(string(names), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return string(out), files, true
}

// dispatchPRReview runs one correctness review against the PR's change. A
// FIRST review reads the full PR diff; a re-review reads only the commits
// since the last reviewed head (round-1's live audit: a full re-read of an
// unchanged 2000-line diff spends the budget re-deriving findings the
// previous round already made). ok=false is the same fail-soft contract as
// the local gate: interrupted, timed out, unparseable, or an oversized diff
// — no round is consumed and the head anchor does not move.
func (r *ChatRepl) dispatchPRReview(parentCtx context.Context, st *prState, gh prGH, prev *agent.ReviewResult) (*agent.ReviewResult, bool) {
	var diff string
	var scope []string
	incremental := false
	if st.Round > 1 {
		if inc, files, ok := prIncrementalDiff(r.cfg.WorkDir, st.LastReviewHead); ok && inc != "" {
			// The same byte cap as the full path: git computed this range
			// and a poisoned anchor can smuggle upstream churn through —
			// refuse oversized deltas instead of reviewing them (round-3
			// issue 2).
			if len(inc) > reviewDiffByteCap {
				r.ui.Info(fmt.Sprintf("  pr: incremental diff exceeds %dKB — falling back to the full PR diff", reviewDiffByteCap>>10))
			} else {
				diff, scope, incremental = inc, files, true
				r.ui.Info(fmt.Sprintf("  pr: #%d re-review reads the incremental diff since %s (%d file(s))",
					st.Number, shortSHA(st.LastReviewHead), len(files)))
			}
		}
	}
	if !incremental {
		var err error
		diff, err = gh.Diff(parentCtx, st.Repo, st.Number)
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
		scope = files
	}
	// No context bundle in PR mode: the worktree IS the PR branch (the fix
	// turn just pushed it), so read_file sees exactly what the diff shows —
	// which is also what makes incremental review safe: the reviewer verifies
	// previous findings against the checked-out tree, not against the delta.
	verdict, ok := r.runReview(parentCtx, reviewPromptInput{
		initialRequest: st.Brief,
		diff:           diff,
		scope:          relToWorkDir(r.cfg.WorkDir, absPaths(r.cfg.WorkDir, scope)),
		bundled:        false,
		prev:           prev,
		prNumber:       st.Number,
		prRound:        st.Round,
		incremental:    incremental,
		sinceSHA:       st.LastReviewHead,
	}, nil, takeWorktreeSnapshot(r.cfg.WorkDir))
	if ok {
		if head, err := runGit(r.cfg.WorkDir, "rev-parse", "HEAD"); err == nil {
			st.LastReviewHead = strings.TrimSpace(string(head))
			if err := st.save(r.cfg.WorkDir); err != nil {
				r.ui.Info(fmt.Sprintf("  pr: could not persist the review head (%v)", err))
			}
		}
	}
	return verdict, ok
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// prReviewerCommentOnPR reports whether the round's review comment is
// already visible on the PR — the durable truth when state and timeline
// disagree (a crash between PostComment and the marker's save leaves the
// marker behind while the comment is public; re-posting would duplicate
// it — round-3 issue 3). A lookup failure is its own answer: guessing
// either way means a duplicate public comment or a silently dropped one,
// so the caller stops and /pr resumes instead.
func prReviewerCommentOnPR(ctx context.Context, st *prState, gh prGH, round int) (bool, error) {
	comments, err := gh.ListComments(ctx, st.Repo, st.Number)
	if err != nil {
		return false, err
	}
	for _, c := range comments {
		if m, ok := markerFromBody(c.Body); ok && m.Role == prRoleReviewer && m.Round == round {
			return true, nil
		}
		// Legacy header (pre-marker rounds) counts for its round too.
		if _, ok := markerFromBody(c.Body); !ok && strings.HasPrefix(c.Body, "**deepai review — round "+strconv.Itoa(round)+":") {
			return true, nil
		}
	}
	return false, nil
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
	// Every exit path drops any interrupt token still buffered on the shared
	// channel: the select below only consumes while parked, so a Ctrl+C that
	// lands while gh.Checks is in flight (up to 60s) would otherwise survive
	// the wait and instantly cancel the next unrelated turn's
	// runTurnWithSignal watcher (round-3 review issue 4).
	defer r.drainInterrupt()
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

func (r *ChatRepl) drainInterrupt() {
	ch := r.ui.InterruptCh()
	if ch == nil {
		return
	}
	for {
		select {
		case <-ch:
		default:
			return
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

// reviewerCommentBody renders the review verdict as a PR comment. Template
// contract (all three loop comments share it): the machine marker sits on
// line 1 — filterExternalComments depends on it before any rendering — then
// a blank line, then the visible bold header, then content.
func reviewerCommentBody(round int, v *agent.ReviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", prMarker{Role: prRoleReviewer, Round: round})
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
	return fmt.Sprintf("%s\n\n**deepai fix — round %d**\n\n%s\n",
		prMarker{Role: prRoleCoder, Round: round}, round, clip(s, 2048))
}

// mergedCommentBody closes the loop's public trail: the PR's conversation
// ends with an explicit merged marker, so a later resume of the same PR (or
// a human reading the timeline) sees the loop finished rather than vanished.
func mergedCommentBody(round int, nextTask string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", prMarker{Role: prRoleMerged, Round: round})
	b.WriteString("**deepai merged this pull request** — review passed, checks green.\n")
	if s := strings.TrimSpace(nextTask); s != "" {
		fmt.Fprintf(&b, "Next task: %s\n", clip(s, 512))
	}
	return b.String()
}

// clip truncates s to at most n bytes on a rune boundary: the clipped text
// flows into PR comments and fix-turn inputs, and a byte cut mid-rune would
// post mojibake to GitHub and hand the model a dangling UTF-8 lead byte
// (round-3 review issue 3).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n(truncated)"
}

// maybeAttachPRLoop runs after an ordinary turn: if the turn's bash output
// shows a freshly created PR and auto-attach is on, the review loop takes
// over synchronously. Suppressed while a mission runs — the mission's own
// gates own that turn's edits — and for PRs already tracked (a fail-soft
// stop keeps its state active for /pr resume).
func (r *ChatRepl) maybeAttachPRLoop(parentCtx context.Context) {
	if !r.cfg.PRReviewAuto || r.sess == nil {
		return
	}
	repo, number, url, ok := detectPRCreate(turnBashOutputs(r.sess.Messages))
	if !ok {
		return
	}
	// Detected a real PR first, THEN report the suppression: a mission that
	// swallows the attach silently left the chained-review pipeline stopped
	// with no diagnostic at all (round-2 issue 4) — the log is the operator's
	// only way to see where the chain broke and how to restart it.
	if r.mission != nil {
		r.ui.Info(fmt.Sprintf("  pr: #%d was created but auto-attach is suppressed by the active mission — run /pr review %d when the mission hands back", number, number))
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
// ones — the owner's hand-written comments, other humans, cursor, any
// identity — told apart from the loop's own posts by the hidden marker (or
// the legacy visible header), never by author: every gh-CLI comment carries
// the same account login (PR #3's live run). Newer than the state's
// watermark, oldest first, capped.
func pendingExternalComments(ctx context.Context, st *prState, gh prGH) []prComment {
	comments, err := gh.ListComments(ctx, st.Repo, st.Number)
	if err != nil {
		return nil
	}
	external := filterExternalComments(comments)
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

	cur, curIdx := inProgressTodo(r.carry.Todos())
	next, nextIdx := nextPendingTodo(r.carry.Todos())
	// The merged comment closes the loop's public trail with the marker, so
	// the timeline shows the loop finishing rather than vanishing — and a
	// later resume reads it as terminal from the comment list alone.
	if err := gh.PostComment(parentCtx, st.Repo, st.Number, mergedCommentBody(st.Round, next)); err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not post the merged comment (%v) — continuing", err))
	}
	// The handoff names the FINISHED item and the NEXT one separately: naming
	// only the next item ordered the model to mark UNSTARTED work done while
	// the merged PR's item stayed in_progress forever (round-4 issue 1) — and
	// returning on next=="" before consulting cur left a last-unfinished
	// in_progress item unnamed on the exact same defect (round-5 issue 1).
	var b strings.Builder
	switch {
	case cur != "" && next != "":
		fmt.Fprintf(&b, "PR #%d was merged. In ONE todo_write call resend the full todo list with item %d (\"%s\") marked done and item %d (\"%s\") marked in_progress, then start it.",
			st.Number, curIdx, cur, nextIdx, next)
		r.ui.Info(fmt.Sprintf("  pr: next task — %s", next))
	case cur != "":
		fmt.Fprintf(&b, "PR #%d was merged. In ONE todo_write call resend the full todo list with item %d (\"%s\") marked done — it was the last unfinished task, so state that the plan is complete.",
			st.Number, curIdx, cur)
	case next != "":
		fmt.Fprintf(&b, "PR #%d was merged. Mark todo item %d (\"%s\") in_progress with todo_write and start it now.", st.Number, nextIdx, next)
		r.ui.Info(fmt.Sprintf("  pr: next task — %s", next))
	default:
		r.ui.Info("  pr: no pending task in the todo list — all done")
		return
	}
	if turnErr := r.runMissionTurn(parentCtx, b.String()); turnErr != nil {
		r.ui.Info("  pr: next-task turn interrupted — the todo list is unchanged")
		return
	}
	// The handoff turn is a synthesized one: the Run()-level
	// maybeAttachPRLoop hook never fires for it, and its bash results fall
	// behind turnBashOutputs' last-human-message boundary as soon as the
	// user types again — without this call the SECOND and later chained PRs
	// never auto-enter their review loops (new-loop round-1 issue 2). The
	// handoff input lands as a human-role message, so the boundary correctly
	// scopes to this turn's gh output. Recursion depth equals the task
	// count: that chain IS the design.
	r.maybeAttachPRLoop(parentCtx)
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

// inProgressTodo returns the first in_progress item's content and its 1-based
// index — the work the merged PR just finished, which the handoff has to name
// so IT gets marked done rather than the unstarted item after it.
func inProgressTodo(todos []builtin.TodoItem) (string, int) {
	for i, t := range todos {
		if t.Status == builtin.TodoInProgress {
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
