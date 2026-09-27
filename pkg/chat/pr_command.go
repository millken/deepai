package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// handlePRCommand implements /pr:
//
//	/pr status        list tracked PRs and their loop state
//	/pr review <n>    attach the loop to EXISTING PR #n and run it now
//	/pr resume [n]    resume the oldest active loop (or PR #n's)
//	/pr merge [n]     merge the PR parked in awaiting_merge (or #n) and start the next todo
//	/pr abort <n>     mark PR #n's loop aborted (nothing is rolled back)
func (r *ChatRepl) handlePRCommand(parentCtx context.Context, args string) {
	arg := strings.TrimSpace(args)
	cmd, rest, _ := strings.Cut(arg, " ")
	switch strings.ToLower(cmd) {
	case "":
		fallthrough
	case "status":
		r.printPRStatus()
	case "review":
		r.reviewPRCommand(parentCtx, strings.TrimSpace(rest))
	case "resume":
		r.resumePRLoop(parentCtx, strings.TrimSpace(rest))
	case "merge":
		r.mergePRCommand(parentCtx, strings.TrimSpace(rest))
	case "abort":
		r.abortPRLoop(strings.TrimSpace(rest))
	default:
		r.ui.Info("  pr: unknown subcommand " + strconv.Quote(cmd) + " — /pr [status|review|resume|merge|abort]")
	}
}

// reviewPRCommand attaches the loop to a PR created outside this REPL —
// this session's own push, another terminal, a teammate — and runs it now.
// The PR title is the loop's review brief (what the PR claims to do), so an
// external PR needs no local context to be reviewed.
func (r *ChatRepl) reviewPRCommand(parentCtx context.Context, arg string) {
	n, err := strconv.Atoi(arg)
	if err != nil || n <= 0 {
		r.ui.Info("  pr: review needs a PR number — /pr review <n>")
		return
	}
	if _, err := openPRState(r.cfg.WorkDir, n); err == nil {
		r.ui.Info(fmt.Sprintf("  pr: #%d is already tracked — /pr resume continues it", n))
		return
	}
	gh := r.prGHOrDefault()
	title, branch, base, url, err := gh.View(parentCtx, "", n)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not read #%d (%v)", n, err))
		return
	}
	// Pin the repo from the URL the same way the auto-attach path does: an
	// empty repo would retarget every later gh call at the process's cwd,
	// which on a different clone means reviewing — and merging — the wrong
	// PR (round-2 review issue 3).
	repo := repoFromPRURL(url)
	st, err := newPRState(r.cfg.WorkDir, n, repo, branch, base, url, title)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not track #%d (%v)", n, err))
		return
	}
	r.ui.Info(fmt.Sprintf("  pr: #%d attached — %s", n, title))
	r.runPRLoop(parentCtx, st)
}

func (r *ChatRepl) printPRStatus() {
	active := activePRStates(r.cfg.WorkDir)
	if len(active) == 0 {
		r.ui.Info("  pr: no tracked pull requests — the loop attaches when a PR is created")
		return
	}
	for _, st := range active {
		r.ui.Info(fmt.Sprintf("  pr: #%d %s — round %d, %s", st.Number, st.Status, st.Round, prLoopPhaseHint(st)))
	}
}

// prLoopPhaseHint names what the loop is waiting on, so /pr status reads as
// an action list rather than a state dump.
func prLoopPhaseHint(st *prState) string {
	switch st.Status {
	case prStatusReviewing:
		return "next: review the PR diff"
	case prStatusAwaitingCI:
		return "next: wait for CI, then merge"
	case prStatusAwaitingMerge:
		return "all green — say merge (or /pr merge) to merge and move to the next task"
	}
	return ""
}

func (r *ChatRepl) abortPRLoop(arg string) {
	if arg == "" {
		r.ui.Info("  pr: abort needs a PR number — /pr abort <n>")
		return
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: %q is not a PR number", arg))
		return
	}
	st, err := openPRState(r.cfg.WorkDir, n)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  pr: #%d is not tracked (%v)", n, err))
		return
	}
	if !st.Status.active() {
		r.ui.Info(fmt.Sprintf("  pr: #%d already %s", n, st.Status))
		return
	}
	if err := st.setStatus(r.cfg.WorkDir, prStatusAborted); err != nil {
		r.ui.Info(fmt.Sprintf("  pr: could not abort #%d (%v)", n, err))
		return
	}
	r.ui.Info(fmt.Sprintf("  pr: #%d loop aborted — the PR itself is untouched on GitHub", n))
}

func (r *ChatRepl) resumePRLoop(parentCtx context.Context, arg string) {
	st := r.pickPRState(arg)
	if st == nil {
		return
	}
	if st.Status == prStatusAwaitingMerge {
		r.ui.Info(fmt.Sprintf("  pr: #%d is all green — /pr merge (or say merge) finishes it", st.Number))
		return
	}
	r.ui.Info(fmt.Sprintf("  pr: resuming #%d — %s", st.Number, prLoopPhaseHint(st)))
	r.runPRLoop(parentCtx, st)
}

func (r *ChatRepl) mergePRCommand(parentCtx context.Context, arg string) {
	st := r.pickPRState(arg)
	if st == nil {
		return
	}
	if st.Status != prStatusAwaitingMerge {
		r.ui.Info(fmt.Sprintf("  pr: #%d is %s, not awaiting_merge — the loop must finish first (/pr resume)", st.Number, st.Status))
		return
	}
	r.mergePRAndContinue(parentCtx, st, r.prGHOrDefault())
}

// pickPRState resolves /pr's optional PR-number argument: #n when given,
// else the oldest ACTIVE loop — and awaiting_merge wins over older active
// ones for the no-arg case, because "the one I can act on now" is what the
// bare /pr merge means in practice.
func (r *ChatRepl) pickPRState(arg string) *prState {
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil {
			r.ui.Info(fmt.Sprintf("  pr: %q is not a PR number", arg))
			return nil
		}
		st, err := openPRState(r.cfg.WorkDir, n)
		if err != nil {
			r.ui.Info(fmt.Sprintf("  pr: #%d is not tracked (%v)", n, err))
			return nil
		}
		if !st.Status.active() {
			r.ui.Info(fmt.Sprintf("  pr: #%d already %s", n, st.Status))
			return nil
		}
		return st
	}
	active := activePRStates(r.cfg.WorkDir)
	if len(active) == 0 {
		r.ui.Info("  pr: no active loop — /pr status lists tracked pull requests")
		return nil
	}
	if st := r.awaitingMergePR(); st != nil {
		return st
	}
	return active[0]
}
