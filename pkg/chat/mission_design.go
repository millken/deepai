package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// ---------------------------------------------------------------------------
// DESIGN phase (docs/LONG_TASK_LOOP_DESIGN.md §5.2)
//
// The main agent designs, in plan mode — not an architect subagent (C9). It
// is the one party holding the user's clarifications and the session; a
// subagent starts from zero with no memory, which is one of the failure
// modes that sank pkg/orchestrator.
//
// Approval is taken away from the user (D8) and given to an independent
// design-reviewer subagent. The gate's test for "is there a plan" is the
// content of design.md, NOT whether exit_plan_mode was called: a model that
// writes a good plan and never calls the tool must not stall the loop.
// ---------------------------------------------------------------------------

// runDesignPhase runs design rounds until the gate passes the plan (returns
// true, mission now in the implementation phase) or the mission ends
// (returns false — leaveMission has already run, or the turn was
// interrupted and the mission stays active for a later /mission).
//
// A pending review (PendingReview==design) re-enters through the gate
// rather than a fresh turn: the previous run's turn completed but its
// review never ran, and an ordinary design turn would rewrite a plan that
// was one review away from the gate. A resume carrying USER input is the
// exception — the user's words are the mission's correction channel
// (§5.5), so they get a turn (amending the plan) and the amended plan then
// takes the still-owing review. The pending flag is cleared only AFTER the
// review reports, so a crash or a Ctrl+C mid-review leaves it standing
// (PR #11 review round 2).
func (r *ChatRepl) runDesignPhase(parentCtx context.Context) bool {
	m := r.mission
	escalated := m.state.Escalation > 0
	maxRounds := maxDesignRounds
	if escalated {
		maxRounds = maxEscalatedDesignRounds
	}

	// hasUserInput survives the whole phase entry: a resume that carried
	// user words must spend them on a turn even under a pending review —
	// the user's message is the mission's correction channel (§5.5), and
	// swallowing it behind a re-dispatch would review a plan the user just
	// tried to change (PR #11 review round 2).
	hasUserInput := strings.TrimSpace(r.missionPendingInput) != ""
	input := r.missionDesignInput() // consumes missionPendingInput when set
	var prev *agent.DesignReviewResult

	for {
		// Three ways into a round. An ordinary one spends a new round on a
		// turn. A pending review with no user input dispatches the gate on
		// the UNCHANGED plan — the previous run's turn completed but its
		// review never ran, and a fresh turn (missionDesignFirstMessage
		// says "replace the whole file") would rewrite a plan that was one
		// review away from the gate. A pending review WITH user input runs
		// the user's turn as an amendment to this round's plan and then
		// takes the still-owing review; the round is not re-spent, because
		// its turn already ran once.
		pendingReview := m.state.PendingReview == missionPhaseDesign
		var plan string
		round := 0

		if pendingReview {
			round = m.designRound() // the round was already spent by its turn
			plan = strings.TrimSpace(m.readDesign())
			if plan == "" {
				pendingReview = false // plan vanished; an ordinary turn must rewrite it
			}
		}
		if !pendingReview {
			round = m.designRound() + 1
			// The pending path skips this check deliberately: the round it
			// belongs to was already spent by its turn, so a 3/3-exhausted
			// plan whose review never ran must still get that review, not a
			// failDesign on arrival.
			if round > maxRounds {
				r.failDesign(nil, maxRounds, "")
				return false
			}
		}

		if !pendingReview || hasUserInput {
			// Force plan mode by PHASE, every turn, before the Agent is built
			// (R10/R21). Flipping only at transitions is not enough: a stray
			// enter_plan_mode or exit_plan_mode changes the REPL's flag through
			// the post-turn readback, and the next turn would then run with the
			// wrong tool set — read-only while implementing, or writable while
			// designing.
			r.applyMissionTurnMode(missionPhaseDesign)
			if pendingReview {
				r.ui.Info(fmt.Sprintf("  mission: design phase — user amendment to %s %d/%d, then its pending review",
					roundLabel(escalated), round, maxRounds))
			} else {
				r.ui.Info(fmt.Sprintf("  mission: design phase (%s %d/%d)", roundLabel(escalated), round, maxRounds))
			}

			turnErr := r.runMissionTurn(parentCtx, input)
			if turnErr != nil {
				// An interrupted or errored turn leaves a half-written plan;
				// reviewing it would burn a round on an artifact the author was
				// not done with. The mission stays active — /mission resumes.
				// The pending flag is untouched on disk, so a later resume
				// still gets its review-first entry.
				if turnErr.cancelled {
					r.ui.Info("  mission: design turn interrupted — no design review ran; /mission resumes, /mission abort ends it")
				} else {
					r.ui.Info(fmt.Sprintf("  mission: design turn failed (%v) — no design review ran", turnErr))
				}
				return false
			}
			// The round is spent only by a turn that actually completed —
			// matching the implementation phase, where ImplementRound moves
			// when a fix round is issued, not when a turn is interrupted. A
			// Ctrl+C that already costs the user its work must not also cost
			// the mission one of its three chances to get the plan right.
			// An amendment turn does not move it either: its round was
			// already spent the first time through.
			if !pendingReview {
				m.setDesignRound(round)
				if err := m.save(); err != nil {
					r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
				}
			}

			plan = strings.TrimSpace(m.readDesign())
			if plan == "" {
				m.appendReview(missionReviewRecord{Phase: "design", Round: round, Verdict: "empty", Summary: "no plan was written"})
				if pendingReview {
					// The amendment wiped the plan; the owing review has
					// nothing to act on, so retire the flag and let the
					// ordinary empty-plan flow ask for a rewrite.
					pendingReview = false
					m.state.PendingReview = ""
					if err := m.save(); err != nil {
						r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
					}
				}
				if round >= maxRounds {
					r.failDesign(nil, maxRounds, "")
					return false
				}
				r.ui.Info("  mission: no plan written yet — asking again")
				input = missionEmptyPlanMessage(round+1, maxRounds, escalated, m.designPath())
				hasUserInput = false
				continue
			}
			hasUserInput = false // consumed by this turn
		} else {
			r.ui.Info(fmt.Sprintf("  mission: re-running the design review on the unchanged plan (%s %d/%d)",
				roundLabel(escalated), round, maxRounds))
		}

		verdict, outcome := r.dispatchDesignReview(parentCtx, m, plan, prev, round)

		// Retire the pending flag only AFTER the review has reported. Clearing
		// it before the dispatch meant a crash mid-review, or a user Ctrl+C
		// landing on it, left the flag gone from disk while the review never
		// happened — the next resume would drop back into turn-first mode and
		// rewrite the plan (PR #11 review round 2). Transient re-sets it
		// below; interrupted leaves it standing; every other outcome —
		// including the terminal ones — retires it here.
		if pendingReview && outcome != reviewInterrupted && outcome != reviewFailedTransient {
			m.state.PendingReview = ""
			if err := m.save(); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
			}
		}

		switch outcome {
		case reviewInterrupted:
			// Ctrl+C landed on the REVIEW, not on the plan. Nothing is
			// wrong with the mission and nothing has been implemented, so
			// it stays active and the user can simply resume — ending it
			// here would throw away a finished plan because the user
			// interrupted the thing reading it.
			//
			// The review is marked PENDING for the same reason a transient
			// outage is: the round's turn already ran, so the resume owes
			// this plan a review and not another rewrite of it. Leaving the
			// flag unset was worse than wasteful on the LAST round — the
			// resume computed round = maxRounds+1, took the round-cap exit
			// and ended the mission design_failed with a finished plan on
			// disk that no reviewer had ever read.
			m.state.PendingReview = missionPhaseDesign
			if err := m.save(); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
			}
			r.ui.Info("  mission: design review interrupted — the plan was NOT reviewed; your next message re-runs the review on the unchanged plan, /mission abort ends it")
			return false
		case reviewFailedTerminal:
			// Design-side fail-soft is the OPPOSITE of the implementation
			// gate's (§六-1): there, the edits already exist and stopping
			// cannot un-write them, so the gate lets them through with a
			// warning. Here nothing has been implemented yet, and letting an
			// unreviewed plan through would skip the whole first half of the
			// loop — so the mission stops and the plan goes to the user.
			r.ui.Info("  mission: design review unavailable — NOT implementing; the plan is at " + m.designPath())
			r.warnUnreviewedImplementation()
			r.leaveMission(missionStatusHandedOver)
			return false
		case reviewFailedTransient:
			// Nothing is implemented, but the failure was the REVIEWER's
			// outage (already retried once), not the plan's: mark the review
			// pending and stay active. /mission re-dispatches the gate on
			// the unchanged plan BEFORE any new turn (PR #11 review).
			m.state.PendingReview = missionPhaseDesign
			if err := m.save(); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
			}
			r.ui.Info("  mission: design review unavailable — NOT implementing; the plan is at " + m.designPath())
			r.warnUnreviewedImplementation()
			r.ui.Info("  mission: the mission stays active — /mission re-runs the review on the unchanged plan, /mission abort ends it")
			return false
		}

		scope, pass, gateReason := designOutcome(r.cfg.WorkDir, verdict)
		m.appendReview(missionReviewRecord{
			Phase: "design", Round: round, Verdict: verdictWord(pass), Summary: verdict.Summary,
			Detail: map[string]any{"issues": verdict.Issues, "scope_files": scope,
				"acceptance": verdict.Acceptance, "gate_reason": gateReason},
		})

		if pass {
			c := &Charter{ScopeFiles: scope, Acceptance: verdict.Acceptance}
			if err := m.lockCharter(c); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not lock the charter (%v) — NOT implementing", err))
				r.warnUnreviewedImplementation()
				r.leaveMission(missionStatusHandedOver)
				return false
			}
			r.carry.SetMissionCharter(renderCharter(c))
			r.ui.Info(fmt.Sprintf("  mission: design review PASS — charter locked, %d file(s) in scope, %d acceptance criteria",
				len(c.ScopeFiles), len(c.Acceptance)))
			m.state.Phase = missionPhaseImplement
			if err := m.save(); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
			}
			return true
		}

		if gateReason != "" {
			// The reviewer passed and the gate did not. Say so in those
			// words: "the design review found 0 issue(s)" over an empty
			// list reads as a machine refusing a plan for no reason.
			r.ui.Info("  mission: design review passed but the charter could not be locked — " + gateReason)
		}
		if round >= maxRounds {
			r.failDesign(verdict, maxRounds, gateReason)
			return false
		}
		if gateReason == "" {
			r.presentDesignIssues(fmt.Sprintf("  mission: design review found %d issue(s) — revising (round %d/%d)",
				len(verdict.Issues), round+1, maxRounds), verdict)
		}
		input = missionDesignRevisionMessage(round+1, maxRounds, escalated, m.designPath(), verdict, gateReason)
		prev = verdict
	}
}

// designRound/setDesignRound read and write whichever counter this design
// phase is spending: the initial one, or the escalated one that starts from
// zero after every escalation (R4).
func (m *mission) designRound() int {
	if m.state.Escalation > 0 {
		return m.state.EscalatedDesignRound
	}
	return m.state.DesignRound
}

func (m *mission) setDesignRound(n int) {
	if m.state.Escalation > 0 {
		m.state.EscalatedDesignRound = n
		return
	}
	m.state.DesignRound = n
}

func roundLabel(escalated bool) string {
	if escalated {
		return "escalated round"
	}
	return "round"
}

func verdictWord(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}

// failDesign ends the mission with design_failed and says which of the two
// situations that name covers (R37). After an escalation the worktree holds
// implementation edits that never passed review — the user has to be told,
// or "the design failed" reads as "nothing happened".
//
// gateReason is the gate's own refusal of a verdict the reviewer passed. It
// is reported INSTEAD of the issue list, which is empty on such a verdict:
// a mission that died this way looked, in the transcript, like a review
// that failed a plan without naming a single thing wrong with it.
func (r *ChatRepl) failDesign(v *agent.DesignReviewResult, maxRounds int, gateReason string) {
	escalated := r.mission != nil && r.mission.state.Escalation > 0
	planPath := ""
	if r.mission != nil {
		planPath = r.mission.designPath()
	}
	switch {
	case gateReason != "":
		r.ui.Info(fmt.Sprintf(
			"  mission: the design review PASSED the plan on round %d, but the charter still could not be locked from it — %s",
			maxRounds, gateReason))
	case v != nil:
		r.presentDesignIssues(fmt.Sprintf(
			"  mission: design STILL FAILING after %d round(s) — human judgment needed. Unresolved issues:", maxRounds), v)
	}
	// design_failed covers two different situations and they must not be
	// described with the same sentence (R37): before any escalation nothing
	// has been built, but after one the worktree already holds edits made
	// under the archived charter that no review ever passed.
	if escalated {
		r.ui.Info(fmt.Sprintf("  mission: design_failed after an escalation — the re-designed plan is at %s and was NOT implemented, but the edits made under the PREVIOUS charter are still in the worktree and were NEVER passed by a review.", planPath))
	} else {
		r.ui.Info(fmt.Sprintf("  mission: design_failed — the plan is at %s; nothing was implemented from it", planPath))
	}
	r.leaveMission(missionStatusDesignFailed)
}

// warnUnreviewedImplementation says the quiet part whenever a mission ends
// from the design phase AFTER an escalation: the worktree is not clean, and
// what is in it never passed a review. Every design-side exit that is not
// failDesign (which words it itself) calls this.
func (r *ChatRepl) warnUnreviewedImplementation() {
	if r.mission == nil || r.mission.state.Escalation == 0 {
		return
	}
	r.ui.Info("  mission: NOTE — edits made under the previous charter are still in the worktree and were NEVER passed by a review.")
}

func (r *ChatRepl) presentDesignIssues(header string, v *agent.DesignReviewResult) {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	writeFindings(&b, v.Issues, v.Summary)
	r.ui.Info(b.String())
}

// designOutcome applies the CODE test for a passing design (C4). The
// reviewer's verdict string alone is not enough: scope_files and acceptance
// become the charter, and a "pass" that left either empty would lock a
// charter whose hard scope check admits everything (empty allow-list means
// every edited file is in violation; an empty scope with no violations means
// nothing is ever reviewed). Normalization can also empty a scope list that
// looked non-empty — every path outside the worktree is dropped (R19).
//
// reason is set whenever the gate refuses a verdict the REVIEWER passed,
// and it has to be: the issue list on such a verdict is empty, so without
// this the author was handed "the design review found the following issues"
// with nothing under it. There is no information in that message to act on,
// so the next plan comes back the same, the reviewer passes it again, the
// gate refuses it again, and the mission burns every design round before
// ending design_failed — all of it over a short acceptance string or a
// scope path spelled outside the worktree. reason is empty for an ordinary
// failing verdict, whose issues speak for themselves.
func designOutcome(workDir string, v *agent.DesignReviewResult) (scope []string, pass bool, reason string) {
	if v == nil {
		return nil, false, "the design review returned no verdict"
	}
	// An EXPLICIT pass is required — the same tightening isMissionPassVerdict
	// applies, and for the same reason: "no issues" is a shape a Strict
	// schema lets a failing reviewer emit, and here it would lock a charter
	// from a plan the reviewer rejected. The design text allows the empty-
	// issues fallback (isDesignPass, §5.2); both reviewer prompts promise
	// the literal word, and the cost of demanding it is one more revision
	// round, while the cost of the fallback is an implementation phase
	// spent on a plan that failed review.
	if !strings.EqualFold(strings.TrimSpace(v.Verdict), "pass") {
		return nil, false, ""
	}
	if len(v.Acceptance) == 0 {
		return nil, false, "the review passed the plan but returned no acceptance criteria, so there is nothing for the charter to hold the implementation to. State the acceptance criteria in the plan as complete Given/When/Then sentences, each with one observable outcome."
	}
	for _, a := range v.Acceptance {
		// A criterion this short cannot state an observable outcome; it is
		// the shape a hedged pass takes ("works", "ok").
		if len(strings.TrimSpace(a)) < 12 {
			return nil, false, fmt.Sprintf("the review passed the plan, but acceptance criterion %q is too short to state an observable outcome, so the charter cannot be locked from it. Write every criterion as a full Given/When/Then sentence naming the thing an observer would see.", strings.TrimSpace(a))
		}
	}
	scope = normalizeScopeFiles(workDir, v.ScopeFiles)
	if len(scope) == 0 {
		return nil, false, fmt.Sprintf("the review passed the plan, but none of the in-scope paths it returned (%s) resolve to a file inside this working directory, so the charter would lock an empty scope. Name in-scope files in the plan as repo-relative paths (pkg/chat/mission.go), not absolute paths and not paths outside the repository.",
			strings.Join(v.ScopeFiles, ", "))
	}
	return scope, true, ""
}

// ---------------------------------------------------------------------------
// Design review dispatch
// ---------------------------------------------------------------------------

// designReviewInput is the reviewer's seed material. Everything here is
// either the user's own words, the plan document, or the review machinery's
// own previous output — never the author's reasoning or the session history
// (the same information isolation the correctness reviewer runs under, §5.2).
type designReviewInput struct {
	brief string
	plan  string
	// prev is the previous round's verdict, so a re-review judges the
	// author's rebuttals instead of re-deriving the whole review (and
	// instead of re-reporting the same finding in different words).
	prev *agent.DesignReviewResult
	// escalation explains why a design phase was re-entered from
	// implementation: the plan has to answer why the old charter could not
	// be built, not just restate itself.
	escalation   string
	planPath     string
	maxToolCalls int
	timeout      time.Duration
}

// dispatchDesignReview runs one design-reviewer subagent through the same
// task-tool chain the correctness reviewer uses (pool, schema validation,
// progress events). Every non-OK outcome is fail-soft and already warned,
// and the outcome class carries what runDesignPhase needs to know — the
// same three-way split runReview reports (see reviewOutcome).
//
// Transient failures (a dropped connection, one malformed verdict) get ONE
// retry before fail-soft gives up — a mission once ended on the reviewer's
// first hiccup with design rounds still unspent. Deterministic failures
// (interruption, deadline, tampering) do not retry.
func (r *ChatRepl) dispatchDesignReview(parentCtx context.Context, m *mission, plan string, prev *agent.DesignReviewResult, round int) (*agent.DesignReviewResult, reviewOutcome) {
	timeout := r.cfg.ReviewTimeout
	if timeout <= 0 {
		timeout = DefaultReviewTimeout
	}
	in := designReviewInput{
		brief:        m.brief,
		plan:         plan,
		prev:         prev,
		escalation:   r.missionEscalationNote,
		planPath:     m.designPath(),
		maxToolCalls: r.reviewMaxToolCallsOrDefault(),
		timeout:      timeout,
	}
	args := map[string]any{
		"description": "Adversarial design review",
		"agent_type":  string(agent.AgentTypeDesignReviewer),
		"prompt":      buildDesignReviewPrompt(in),
		// The gate caps the reviewer, not the profile — the same split the
		// correctness gate uses, and the same configured value: one config
		// key drives every gate-dispatched reviewer (round-2 review caught this
		// desync — mission reviewers silently ignored review_max_tool_calls).
		"max_tool_calls": r.reviewMaxToolCallsOrDefault(),
	}
	if r.cfg.ReviewTokenBudget > 0 {
		args["token_budget"] = r.cfg.ReviewTokenBudget
	}
	// Same reviewer model as the implementation gate (§5.7's rule for the
	// two review knobs: one setting, both gates). A design reviewer sharing
	// the author's model is if anything worse off than a code reviewer
	// would be — a plan has no compiler to disagree with either of them.
	if model := strings.TrimSpace(r.cfg.ReviewModel); model != "" {
		args["model"] = model
	}

	// A design reviewer has no bash and only read-only tools, so this
	// snapshot is a much weaker concern than it is for the correctness
	// reviewer — but it is still taken, because "the reviewer wrote the
	// tree" must be detectable rather than assumed impossible (§六-2).
	preReview := takeWorktreeSnapshot(r.cfg.WorkDir)

	var result models.ToolResult
	var execErr error
	var turnErr *turnError
	for attempt := 0; ; attempt++ {
		result = models.ToolResult{}
		execErr = nil
		turnErr = r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
				r.ui.RenderSubagentEvent(evt)
			})
			result, execErr = r.cfg.ToolRegistry.Execute(ctx, models.ToolCall{
				ID:        fmt.Sprintf("design-review-t%d-r%d-%d", r.turn, round, time.Now().UnixNano()),
				Name:      "task",
				Arguments: args,
			})
			return nil // review failures are fail-soft, never a turn error
		})

		if tampered := takeWorktreeSnapshot(r.cfg.WorkDir).changesSince(preReview); len(tampered) > 0 {
			r.ui.Info(fmt.Sprintf(
				"  mission: design reviewer modified the working tree (%s) — verdict DISCARDED",
				strings.Join(relToWorkDir(r.cfg.WorkDir, tampered), ", ")))
			return nil, reviewFailedTerminal
		}
		if turnErr != nil && turnErr.cancelled {
			return nil, reviewInterrupted
		}
		if execErr != nil {
			if isReviewDeadline(execErr) {
				r.ui.Info(fmt.Sprintf(
					"  mission: design review hit its %s deadline — the plan is unreviewed (raise review_timeout in config.yaml)", timeout))
				return nil, reviewFailedTerminal
			}
			if attempt == 0 {
				r.ui.Info(fmt.Sprintf("  mission: design review failed (%v) — retrying once", execErr))
				continue
			}
			r.ui.Info(fmt.Sprintf("  mission: design review failed (%v) — the plan is unreviewed", execErr))
			return nil, reviewFailedTransient
		}

		schema := agent.GetAgentTypeConfig(agent.AgentTypeDesignReviewer).OutputSchema
		parsed, err := agent.ParseOutput[agent.DesignReviewResult](schema, result.Content)
		if err != nil {
			if attempt == 0 {
				r.ui.Info(fmt.Sprintf("  mission: design verdict unparseable (%v) — retrying once", err))
				continue
			}
			r.ui.Info(fmt.Sprintf("  mission: design verdict unparseable (%v) — the plan is unreviewed", err))
			return nil, reviewFailedTransient
		}
		return parsed, reviewOK
	}
}

// designPlanPromptCap bounds how much of the plan goes into the reviewer's
// seed message. write_plan already caps a plan at 64KiB (plan.go), so this
// only ever fires on a plan at that ceiling; the reviewer is told to read
// the rest itself, which its read-only tools and 20-call budget can do
// (§六-4).
const designPlanPromptCap = 64 << 10

func buildDesignReviewPrompt(in designReviewInput) string {
	var b strings.Builder
	b.WriteString("Adversarially review the implementation plan below against the brief it is supposed to answer.\n\n")
	b.WriteString("## Original brief (verbatim user request — the fixed anchor)\n\n")
	b.WriteString(strings.TrimSpace(in.brief))
	b.WriteString("\n\n## Plan under review (" + in.planPath + ")\n\n")
	plan := in.plan
	if len(plan) > designPlanPromptCap {
		plan = plan[:designPlanPromptCap]
		b.WriteString(plan)
		b.WriteString("\n\n(TRUNCATED — read the rest from the path above with read_file.)\n")
	} else {
		b.WriteString(plan)
		b.WriteString("\n")
	}
	if in.escalation != "" {
		b.WriteString("\n## Why this plan is being re-designed\n\n")
		b.WriteString(in.escalation)
		b.WriteString("\nA revised plan must answer this. A plan that repeats what could not be built is a fail.\n")
	}
	if in.prev != nil && len(in.prev.Issues) > 0 {
		b.WriteString("\n## What you reported on the previous version of this plan\n\n")
		writeIssueList(&b, in.prev.Issues)
		b.WriteString("\nThe author has since either fixed each of these or argued it is not a real problem. " +
			"Decide independently whether each still holds against the plan above — report it again ONLY if you can " +
			"still construct the failure scenario. Then look for problems the rewrite itself introduced.\n")
	}
	b.WriteString("\n## What a pass commits to\n\n")
	b.WriteString("On pass, your scope_files and acceptance BECOME the mission's locked charter: the implementer may " +
		"edit only those files (plus *_test.go companions of them), and the code reviewer judges the change against " +
		"those criteria. scope_files must be repo-relative paths — include files the plan will CREATE, and the " +
		"*_test.go files the implementation needs. A pass with either list empty is counted as a FAIL by the gate.\n")
	if in.maxToolCalls > 0 || in.timeout > 0 {
		b.WriteString("\n## Your budget\n\n")
		if in.maxToolCalls > 0 {
			fmt.Fprintf(&b, "- At most %d tool calls. Past that you get one final turn with NO tools, in which you must still emit the verdict JSON.\n", in.maxToolCalls)
		}
		if in.timeout > 0 {
			fmt.Fprintf(&b, "- %s of wall clock for the whole review. Running out kills the review and your findings are lost.\n", in.timeout)
		}
		b.WriteString("- Reason from the plan first; spend tool calls only to check that identifiers and paths it names really exist.\n")
	}
	return b.String()
}
