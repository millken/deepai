package chat

import (
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/agent"
)

// ---------------------------------------------------------------------------
// Synthesized mission messages (docs/LONG_TASK_LOOP_DESIGN.md §5.6).
//
// Each one enters the history as an ordinary user message, the same choice
// the adversarial review made: a persisted assistant reply to a message that
// was never persisted reads, on resume, as an answer to nothing. The
// "[mission-…]" prefixes make them recognizable to a human reading the
// transcript and to lastUserRequest, which must never mistake one for the
// user's own words.
// ---------------------------------------------------------------------------

const missionMessagePrefix = "[mission-"

// missionDesignFirstMessage seeds the very first design turn. The mission's
// own brief is NOT used as the turn input directly (R20): plan mode's
// standing prompt asks for "files to modify, approach, and key
// considerations" and still tells the model exit_plan_mode waits for the
// user, so a first plan written under it structurally fails a review that
// requires Given/When/Then acceptance — one whole round lost to a
// misunderstanding the loop created itself.
func missionDesignFirstMessage(maxRounds int, planPath, brief string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mission-design round 1/%d] Produce an implementation plan and write_plan it to %s (write_plan REPLACES the whole file — always send the complete plan).\n\n", maxRounds, planPath)
	b.WriteString("The plan MUST include:\n")
	b.WriteString("1. In-scope files, repo-relative — including the *_test.go files you expect to add or edit, and files you will create.\n")
	b.WriteString("2. Acceptance criteria as Given/When/Then, each with ONE observable outcome. A criterion nobody could falsify does not count.\n")
	b.WriteString("3. Approach and risks; when a decision cites existing code, name the file and the exact identifier.\n\n")
	b.WriteString("exit_plan_mode no longer waits for the user: an independent design review runs as soon as the plan file is non-empty, and its verdict — not your call to exit_plan_mode — is what starts the implementation. Ask with ask_clarification only if the request is genuinely ambiguous.\n\n")
	b.WriteString("Original request:\n")
	b.WriteString(strings.TrimSpace(brief))
	return b.String()
}

// missionEmptyPlanMessage is the nudge for a design turn that produced no
// plan at all. It costs a round: a turn that wrote nothing is indistinguishable
// from one that refused, and both must be bounded.
func missionEmptyPlanMessage(round, maxRounds int, escalated bool, planPath string) string {
	return fmt.Sprintf("[mission-design %s %d/%d] %s is still empty — nothing was submitted for design review. Write the complete plan with write_plan now (in-scope files including *_test.go, Given/When/Then acceptance criteria, approach and risks). Talking about the plan is not writing it.",
		roundLabel(escalated), round, maxRounds, planPath)
}

// missionDesignResumeMessage restarts a design phase that was interrupted
// (Ctrl+C, a crash, a REPL exit) partway through its rounds. The plan file
// already holds whatever the previous round wrote, so this points at it
// rather than starting over.
func missionDesignResumeMessage(round, maxRounds int, escalated bool, planPath, brief string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mission-design %s %d/%d] Resuming this mission's design phase. Your current plan is at %s — read it, then write the COMPLETE revised plan with write_plan (it replaces the file).\n\n", roundLabel(escalated), round, maxRounds, planPath)
	b.WriteString("It must name its in-scope files (including *_test.go), give Given/When/Then acceptance criteria with one observable outcome each, and state approach and risks. An independent design review reads the file as soon as it is non-empty.\n\n")
	b.WriteString("Original request:\n")
	b.WriteString(strings.TrimSpace(brief))
	return b.String()
}

// missionDesignRevisionMessage carries the reviewer's findings back to the
// author. "Fix it, or say why it is not a real problem" is deliberate and
// matches the code-review loop: reviewers are wrong sometimes, and the
// rebuttal goes to the NEXT reviewer to judge rather than being overruled
// here.
func missionDesignRevisionMessage(round, maxRounds int, escalated bool, planPath string, v *agent.DesignReviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mission-design-review %s %d/%d] An independent design review of your plan at %s found the following issues. Rewrite the ENTIRE plan via write_plan (the file is replaced, not patched — a plan you only describe in prose is not submitted). For each issue: fix it in the new plan, or state explicitly why it is not a real problem.\n",
		roundLabel(escalated), round, maxRounds, planPath)
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString("\nReviewer summary: " + s + "\n")
	}
	writeIssueList(&b, v.Issues)
	if len(v.ScopeFiles) == 0 || len(v.Acceptance) == 0 {
		b.WriteString("\nThe review could not fill the charter from this plan (scope files and/or acceptance criteria). A plan that does not name its in-scope files and its Given/When/Then acceptance criteria cannot be locked, and the mission cannot start implementing.\n")
	}
	return b.String()
}

// missionImplementMessage is the implementation phase's first input (R26):
// without it the history would simply stop at the design conversation and
// the next turn would have no instruction at all.
func missionImplementMessage(c *Charter) string {
	var b strings.Builder
	b.WriteString("[mission-implement] The charter is locked. Implement it.\n\n")
	b.WriteString("- Write the tests first (TDD), then the code that satisfies them.\n")
	b.WriteString("- Stay inside the charter's in-scope files. Their *_test.go companions and testdata are in scope too; anything else is caught by a hard scope check and has to be reverted.\n")
	b.WriteString("- Do NOT edit the charter. If the plan turns out to be unbuildable, say so plainly in your answer — the review has a path for that.\n\n")
	b.WriteString("Acceptance criteria:\n")
	for i, a := range c.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	b.WriteString("\nIn scope:\n")
	for _, f := range c.ScopeFiles {
		b.WriteString("- " + f + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// missionScopeMessage orders out-of-charter files reverted. It deliberately
// does NOT offer editing the charter as an option: the only way the charter
// changes is an escalation, and a scope round that invited the model to
// widen its own scope would turn the hard check into a suggestion.
func missionScopeMessage(round, maxRounds int, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mission-scope round %d/%d] These files are outside the locked charter scope, and are not test companions of an in-scope file. Revert them (git checkout -- <path>, or delete them if the mission created them):\n\n", round, maxRounds)
	for _, f := range files {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("\nThen continue the in-scope work. Repeatedly editing out-of-charter implementation files escalates the mission back to design; do not edit the charter yourself.")
	return b.String()
}

// missionIdleMessage answers a turn that changed nothing in scope. An empty
// review scope is NOT a finished mission (R31) — without this the loop would
// read "nothing to review" as "nothing left to do" and exit clean having
// implemented and reviewed nothing.
func missionIdleMessage(round, maxRounds int) string {
	return fmt.Sprintf("[mission-idle %d/%d] Nothing in the charter's scope has changed since the implementation baseline. Talking about the work is not doing it: write the tests and the change the charter requires, using edit_file/write_file. After %d idle turns the mission is handed back to the user — an empty change set is never treated as done.",
		round, maxRounds, maxIdleRounds)
}

// missionEscalateMessage re-opens design from the implementation gate. It
// carries WHY, because a design phase that only sees the brief again will
// produce the same plan again.
func missionEscalateMessage(maxRounds int, reason string, archived string, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mission-escalate 1/%d] The implementation review concluded the charter itself is wrong (%s). You are back in design mode, read-only.\n\n", maxDesignEscalations, reason)
	b.WriteString(detail)
	fmt.Fprintf(&b, "\nThe previous charter has been archived as %s and no longer binds you. Write a NEW complete plan with write_plan that answers the SAME original brief and explains what the previous plan got wrong. You have %d design-review round(s).\n", archived, maxRounds)
	return b.String()
}

// escalationReasonText names the signal in words the author can act on.
func escalationReasonText(signal string) string {
	switch signal {
	case escalateFaultLayer:
		return "the reviewer attributed the defect to the plan, not the code"
	case escalateRepeatFile:
		return "two review rounds failed on the same file"
	case escalateScope:
		return "the implementation kept needing files the charter does not cover"
	default:
		return signal
	}
}
