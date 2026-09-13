package chat

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/millken/deepai/pkg/agent"
)

// ---------------------------------------------------------------------------
// IMPLEMENT phase (docs/LONG_TASK_LOOP_DESIGN.md §5.4)
//
// A same-shaped bounded for-loop over runTurn, deliberately NOT runEpisode:
// that function has exactly two exits (fix / done) and the third one —
// "the charter is wrong, re-open design" — cannot be expressed through it
// (R17). What IS reused is the machinery that matters: the reviewer
// dispatch, the degradation ladder, snapshot attribution and the fix-message
// synthesis. None of that is duplicated here.
// ---------------------------------------------------------------------------

// runImplementPhase returns true when the mission continues in ANOTHER phase
// (an escalation put it back in design) and false when the mission ended or
// the turn was interrupted.
func (r *ChatRepl) runImplementPhase(parentCtx context.Context) bool {
	m := r.mission
	if m.charter == nil {
		// Can only happen if charter.lock.json was removed under a running
		// mission. Implementing without a charter would mean no scope check
		// and no acceptance criteria — exactly the unbounded behavior the
		// loop exists to prevent.
		r.ui.Info("  mission: the charter is missing — cannot implement; handing back")
		r.leaveMission(missionStatusHandedOver)
		return false
	}

	if _, err := os.Stat(m.path(missionBaselineFile)); err != nil {
		// First entry into implementation: this is where S_impl is taken,
		// UNCONDITIONALLY — not under review_after_edit (R18). With a zero
		// baseline, changedSince returns nil and every bash-mediated edit
		// would be invisible to both the scope check and the reviewer.
		//
		// Attribution is reset here because the design phase's ordinary
		// conversation may have left records behind, and on the non-git
		// fallback path those records ARE the review scope (R27/R30).
		r.carry.ClearEditedFiles()
		r.reviewPrev = nil
		m.saveBaseline(takeWorktreeSnapshot(r.cfg.WorkDir))
		if m.baseline.root == "" && !r.reviewNonGitWarned {
			r.reviewNonGitWarned = true
			r.ui.Info("  mission: not a git worktree — the charter's hard scope check is OFF and attribution falls back to tool records")
		}
	}

	input := missionImplementMessage(m.charter)
	for {
		round := m.state.ImplementRound
		r.applyMissionTurnMode(missionPhaseImplement)
		r.ui.Info(fmt.Sprintf("  mission: implement phase (review round %d/%d)", round, maxReviewRounds))

		turnErr := r.runMissionTurn(parentCtx, input)
		if turnErr != nil {
			// Same rule as an episode: a turn that was interrupted or
			// errored left incomplete edits, and reviewing those is
			// meaningless. The mission stays active for /mission.
			if turnErr.cancelled {
				r.ui.Info("  mission: implement turn interrupted — the changes so far are UNREVIEWED; /mission resumes, /mission abort ends it")
			} else {
				r.ui.Info(fmt.Sprintf("  mission: implement turn failed (%v) — the changes so far are UNREVIEWED", turnErr))
			}
			return false
		}

		// before = S_impl, the PHASE baseline, never a per-turn snapshot
		// (R33): the charter is enforced against everything the phase has
		// done, so a file the previous turn put out of scope stays a
		// violation until it is actually reverted.
		out := r.reviewGate(parentCtx, "", worktreeSnapshot{}, round)

		switch {
		case out.escalate != "":
			r.escalateToDesign(out.escalate)
			return true
		case out.passed:
			m.state.Reviewed = true
			if err := m.save(); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
			}
			r.ui.Info("  mission: done — the implementation passed an independent review")
			r.leaveMission(missionStatusDone)
			return false
		case out.next == "":
			r.ui.Info("  mission: handed_over — the changes are in the worktree and did NOT pass a review; they need your judgment")
			r.leaveMission(missionStatusHandedOver)
			return false
		}
		input = out.next
	}
}

// escalateToDesign re-opens the design phase from the implementation gate
// (§5.4.3). The charter that produced these edits is archived — the
// implementer must not be told to obey a charter the gate just declared
// wrong (R28) — and the escalated design rounds start from zero (R4).
//
// The implementation BASELINE is deliberately kept. The design says the
// phase takes S_impl on entry, and a fresh snapshot here would be simpler,
// but it would also make every edit the first implementation phase already
// made invisible to the next one's review: the mission could then finish
// "done" with unreviewed changes in the tree, which is the one thing the
// status model exists to prevent. Keeping the original baseline means those
// edits are still attributed — reviewed if the new charter covers them, and
// ordered reverted if it does not.
func (r *ChatRepl) escalateToDesign(signal string) {
	m := r.mission
	m.state.Escalation++
	archived := fmt.Sprintf(missionCharterArchived, m.state.Escalation)
	detail := r.missionEscalationDetail(signal)
	if err := m.archiveCharter(m.state.Escalation); err != nil {
		r.ui.Info(fmt.Sprintf("  mission: could not archive the charter (%v)", err))
	}
	r.carry.SetMissionCharter("")
	r.carry.ClearEditedFiles()
	r.reviewPrev = nil

	m.state.Phase = missionPhaseDesign
	m.state.EscalatedDesignRound = 0
	m.state.ImplementRound = 0
	m.state.ScopeRound = 0
	m.state.IdleRound = 0
	if err := m.save(); err != nil {
		r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
	}

	r.missionEscalationNote = detail
	r.missionPendingInput = missionEscalateMessage(maxEscalatedDesignRounds, escalationReasonText(signal), archived, detail)
	r.ui.Info(fmt.Sprintf("  mission: escalating to design (%d/%d), reason=%s — the previous charter is archived as %s",
		m.state.Escalation, maxDesignEscalations, signal, archived))
}

// missionEscalationDetail renders what the implementation phase learned, for
// both the new design turn and the design reviewer. Built from the LAST
// failing verdict and the scope violations the gate recorded.
func (r *ChatRepl) missionEscalationDetail(signal string) string {
	var b strings.Builder
	switch signal {
	case escalateFaultLayer:
		b.WriteString("The code reviewer reported the defect as a fault in the PLAN, not the code.\n")
	case escalateRepeatFile:
		b.WriteString("Two consecutive review rounds failed on the same file, which is what a wrong plan looks like from inside an implementation.\n")
	case escalateScope:
		b.WriteString("The implementation kept having to edit files the charter does not cover, which usually means the charter left out a file that must change.\n")
	}
	if r.reviewPrev != nil && len(r.reviewPrev.Issues) > 0 {
		b.WriteString("\nWhat the code review found:\n")
		writeIssueList(&b, r.reviewPrev.Issues)
	}
	if len(r.missionScopeViolations) > 0 {
		b.WriteString("\nFiles the implementation needed but the charter did not cover:\n")
		for _, f := range r.missionScopeViolations {
			b.WriteString("- " + f + "\n")
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The mission's implementation gate
// ---------------------------------------------------------------------------

// missionReviewGate is reviewGate's mission branch. Compared with the
// ordinary gate it adds three things and removes one:
//
//   - the hard scope check, which costs no reviewer run at all;
//   - the charter as the review's anchor instead of the latest user message;
//   - the escalation exits;
//   - and it does NOT consult review_after_edit: inside a mission the review
//     is the contract, not an opt-in (§5.4.4).
func (r *ChatRepl) missionReviewGate(parentCtx context.Context, round int) gateResult {
	m := r.mission
	after := takeWorktreeSnapshot(r.cfg.WorkDir)
	// The baseline is read from the MISSION, not from the caller's argument
	// (R33). The phase loop passes the same snapshot anyway, but an ordinary
	// turn taken while a mission is paused mid-implementation reaches this
	// gate through runEpisode, whose per-turn baseline (or zero value, when
	// review_after_edit is off) would quietly narrow the charter check to
	// that one turn.
	before := m.baseline
	gitOK := after.root != "" && before.root != ""

	var reviewScope []string
	var violations []string
	if gitOK {
		// The authority for "what has this phase changed" is the CURRENT
		// difference from the phase baseline, not the accumulated tool
		// records (R25). Records only ever grow: a file the model reverted
		// on the gate's own orders stays in them forever, so a
		// record-based check would report the same violation again, spend
		// the second scope round, and escalate a mission that actually did
		// what it was told.
		reviewScope, violations = classifyAgainstCharter(r.cfg.WorkDir, m.charter, after.changedSince(before))
	} else {
		// Non-git degradation is NARROW (R30): the hard scope check and the
		// scope escalation switch off, because there is no trustworthy
		// current-state view to compute them from — but the REVIEW keeps
		// running off tool records, exactly as the ordinary gate does. The
		// alternative (deriving the review scope from an empty snapshot
		// delta) would silently review nothing at all.
		reviewScope = resolveWorktreePaths(r.carry.EditedFiles())
	}

	r.missionScopeViolations = violations // stale violations must not end up in a later escalation's explanation
	if len(violations) > 0 {
		if m.state.ScopeRound >= maxScopeFixRounds {
			// S3: still reaching outside the charter after two revert
			// rounds. The charter, not the implementer, is the likely
			// problem.
			if esc, ok := r.tryEscalate(escalateScope); ok {
				return esc
			}
			r.ui.Info(fmt.Sprintf("  mission: %d file(s) still outside the charter after %d scope rounds and no escalation left — handing back",
				len(violations), maxScopeFixRounds))
			return gateResult{}
		}
		m.state.ScopeRound++
		if err := m.save(); err != nil {
			r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
		}
		r.ui.Info(fmt.Sprintf("  mission: %d file(s) out of charter scope — scope-fix %d/%d (does not spend a review round): %s",
			len(violations), m.state.ScopeRound, maxScopeFixRounds, strings.Join(violations, ", ")))
		m.appendReview(missionReviewRecord{Phase: "implement-scope", Round: m.state.ScopeRound,
			Verdict: "out_of_scope", Summary: strings.Join(violations, ", ")})
		return gateResult{next: missionScopeMessage(m.state.ScopeRound, maxScopeFixRounds, violations)}
	}

	if len(reviewScope) == 0 {
		// Nothing to review is NOT a pass (R31). A turn that only talked,
		// or one that reverted its out-of-scope files and changed nothing
		// else, has produced no implementation — and a mission that ended
		// here would be reported as reviewed and done having done neither.
		m.state.IdleRound++
		if err := m.save(); err != nil {
			r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
		}
		if m.state.IdleRound > maxIdleRounds {
			r.ui.Info(fmt.Sprintf("  mission: nothing to review after %d idle turns — handing back (NOT done)", maxIdleRounds))
			return gateResult{}
		}
		r.ui.Info(fmt.Sprintf("  mission: nothing to review — idle %d/%d (does not spend a review round, and is not completion)",
			m.state.IdleRound, maxIdleRounds))
		return gateResult{next: missionIdleMessage(m.state.IdleRound, maxIdleRounds)}
	}

	verdict, ok := r.dispatchReview(parentCtx, m.brief, reviewScope, after, r.reviewPrev)
	if !ok {
		// Implementation-side fail-soft: the edits exist and stopping cannot
		// un-write them, so the loop ends rather than looping — but as
		// handed_over, never done. dispatchReview has already warned.
		r.reviewPrev = nil
		return gateResult{}
	}
	m.appendReview(missionReviewRecord{Phase: "implement", Round: round + 1,
		Verdict: verdict.Verdict, Summary: verdict.Summary, Detail: map[string]any{"issues": verdict.Issues}})

	if isPassVerdict(verdict) {
		r.carry.ClearEditedFiles()
		r.reviewPrev = nil
		r.ui.Info("  mission: implementation review pass — " + verdictSummary(verdict))
		return gateResult{passed: true}
	}

	// prev must be read BEFORE r.reviewPrev is overwritten below: S2 asks
	// whether the PREVIOUS round failed on the same file, and comparing a
	// verdict with itself would make every single-round failure look like a
	// repeat.
	prev := r.reviewPrev

	// S1: the reviewer itself says the plan is at fault. Acted on
	// immediately — spending fix rounds on code that faithfully implements
	// a wrong plan is exactly the failure this signal exists for.
	if hasDesignFault(verdict) {
		r.reviewPrev = verdict
		if esc, ok := r.tryEscalate(escalateFaultLayer); ok {
			return esc
		}
	}

	if round >= maxReviewRounds {
		// S2: about to hand the mission to a human, and the same file has
		// failed twice running. One re-design first — a false positive
		// costs one design phase, while never firing costs the escalation
		// path its only code-side trigger.
		if sharesFailingFile(prev, verdict) {
			r.reviewPrev = verdict
			if esc, ok := r.tryEscalate(escalateRepeatFile); ok {
				return esc
			}
		}
		r.reviewPrev = nil
		r.presentIssues(fmt.Sprintf(
			"  mission: implementation STILL FAILING after %d fix rounds — human judgment needed. Unresolved issues:", maxReviewRounds), verdict)
		return gateResult{}
	}

	r.reviewPrev = verdict
	m.state.ImplementRound = round + 1
	if err := m.save(); err != nil {
		r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
	}
	r.ui.Info(fmt.Sprintf("  mission: %d issue(s) — fix round %d/%d", len(verdict.Issues), round+1, maxReviewRounds))
	return gateResult{next: synthesizeFixMessage(round+1, verdict)}
}

// tryEscalate reports an escalation only while the mission has one left
// (§5.4.3: maxDesignEscalations). Out of escalations, the caller falls
// through to its hand-over path — a second re-design would be the unbounded
// loop the whole design refuses to build.
func (r *ChatRepl) tryEscalate(signal string) (gateResult, bool) {
	if r.mission == nil || r.mission.state.Escalation >= maxDesignEscalations {
		return gateResult{}, false
	}
	return gateResult{escalate: signal}, true
}

// hasDesignFault is S1: any issue the reviewer marked as a fault in the
// plan. Case-insensitive because the field is free text filled by a model.
func hasDesignFault(v *agent.ReviewResult) bool {
	if v == nil {
		return false
	}
	for _, is := range v.Issues {
		if strings.EqualFold(strings.TrimSpace(is.FaultLayer), "design") {
			return true
		}
	}
	return false
}

// sharesFailingFile is S2: two consecutive failing verdicts naming at least
// one file in common. Empty File fields never match — an issue with no
// location cannot establish that the SAME thing failed twice.
func sharesFailingFile(prev, cur *agent.ReviewResult) bool {
	if prev == nil || cur == nil {
		return false
	}
	seen := make(map[string]struct{}, len(prev.Issues))
	for _, is := range prev.Issues {
		if f := strings.TrimSpace(is.File); f != "" {
			seen[f] = struct{}{}
		}
	}
	for _, is := range cur.Issues {
		if f := strings.TrimSpace(is.File); f != "" {
			if _, ok := seen[f]; ok {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Charter scope arithmetic (§5.4.2)
// ---------------------------------------------------------------------------

// scopeExemptions are generated or tool-owned paths that never count as an
// out-of-scope EDIT. They are exempt from the violation test only — they do
// NOT enter the review scope (R38), because feeding a lock file or the
// mission's own state.json to a reviewer wastes the 20 tool calls the gate
// gives it on content no human would review either.
func isScopeExempt(rel string) bool {
	switch {
	case strings.HasPrefix(rel, ".deepai/"):
		return true // the mission's own bookkeeping, plans, sessions
	case rel == "go.sum" || rel == "go.work.sum":
		return true
	case strings.HasSuffix(rel, ".lock"):
		return true
	}
	return false
}

// isTestCompanion decides whether a test file or fixture rides along with an
// in-scope implementation file (R3). This repo's discipline is red-test-first,
// so EVERY implementation round writes *_test.go files, and a design phase
// cannot enumerate them all in advance — without this exemption the hard
// scope check would fire on almost every honest turn and burn both scope
// rounds before any real work was reviewed.
//
// The exemption covers tests and their fixtures ONLY. A new non-test .go
// file that appears next to an in-scope file is still a violation: that is
// the case the check exists for.
func isTestCompanion(rel string, scope map[string]struct{}, scopeDirs map[string]struct{}) bool {
	dir := path.Dir(rel)
	if strings.HasSuffix(rel, "_test.go") {
		// foo_test.go beside an in-scope foo.go.
		if _, ok := scope[strings.TrimSuffix(rel, "_test.go")+".go"]; ok {
			return true
		}
		// any *_test.go in a directory that holds an in-scope .go file.
		if _, ok := scopeDirs[dir]; ok {
			return true
		}
		return false
	}
	// testdata under a directory that holds an in-scope .go file.
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) == "testdata" {
			if _, ok := scopeDirs[path.Dir(d)]; ok {
				return true
			}
		}
	}
	return false
}

// classifyAgainstCharter splits the phase's current change set into what the
// review should see and what must be reverted. changed carries ABSOLUTE
// paths (changedSince's form); reviewScope comes back absolute because that
// is what the reviewer dispatch needs, while violations come back
// workdir-relative because that is what a human — and the revert message —
// reads (R19: every comparison happens in ONE form, workdir-relative).
func classifyAgainstCharter(workDir string, c *Charter, changed []string) (reviewScope []string, violations []string) {
	if c == nil {
		return nil, nil
	}
	scope := make(map[string]struct{}, len(c.ScopeFiles))
	scopeDirs := make(map[string]struct{}, len(c.ScopeFiles))
	for _, f := range c.ScopeFiles {
		scope[f] = struct{}{}
		if strings.HasSuffix(f, ".go") {
			scopeDirs[path.Dir(f)] = struct{}{}
		}
	}
	for _, abs := range changed {
		rel, inTree := workdirRel(workDir, abs)
		if !inTree {
			// A change outside the working directory cannot be in any
			// charter's scope; report it rather than silently ignoring it.
			violations = append(violations, abs)
			continue
		}
		_, inScope := scope[rel]
		switch {
		case inScope || isTestCompanion(rel, scope, scopeDirs):
			reviewScope = append(reviewScope, abs)
		case isScopeExempt(rel):
			// Exempt from the violation test, and deliberately NOT reviewed.
		default:
			violations = append(violations, rel)
		}
	}
	return reviewScope, violations
}
