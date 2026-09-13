package chat

import (
	"context"
	"errors"
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
func (r *ChatRepl) runDesignPhase(parentCtx context.Context) bool {
	m := r.mission
	escalated := m.state.Escalation > 0
	maxRounds := maxDesignRounds
	if escalated {
		maxRounds = maxEscalatedDesignRounds
	}

	input := r.missionDesignInput()
	var prev *agent.DesignReviewResult

	for {
		round := m.designRound() + 1
		if round > maxRounds {
			r.failDesign(nil, maxRounds)
			return false
		}

		// Force plan mode by PHASE, every turn, before the Agent is built
		// (R10/R21). Flipping only at transitions is not enough: a stray
		// enter_plan_mode or exit_plan_mode changes the REPL's flag through
		// the post-turn readback, and the next turn would then run with the
		// wrong tool set — read-only while implementing, or writable while
		// designing.
		r.applyMissionTurnMode(missionPhaseDesign)
		r.ui.Info(fmt.Sprintf("  mission: design phase (%s %d/%d)", roundLabel(escalated), round, maxRounds))

		turnErr := r.runMissionTurn(parentCtx, input)
		m.setDesignRound(round)
		if err := m.save(); err != nil {
			r.ui.Info(fmt.Sprintf("  mission: could not persist state (%v)", err))
		}
		if turnErr != nil {
			// An interrupted or errored turn leaves a half-written plan;
			// reviewing it would burn a round on an artifact the author was
			// not done with. The mission stays active — /mission resumes.
			if turnErr.cancelled {
				r.ui.Info("  mission: design turn interrupted — no design review ran; /mission resumes, /mission abort ends it")
			} else {
				r.ui.Info(fmt.Sprintf("  mission: design turn failed (%v) — no design review ran", turnErr))
			}
			return false
		}

		plan := strings.TrimSpace(m.readDesign())
		if plan == "" {
			m.appendReview(missionReviewRecord{Phase: "design", Round: round, Verdict: "empty", Summary: "no plan was written"})
			if round >= maxRounds {
				r.failDesign(nil, maxRounds)
				return false
			}
			r.ui.Info("  mission: no plan written yet — asking again")
			input = missionEmptyPlanMessage(round+1, maxRounds, escalated, m.designPath())
			continue
		}

		verdict, ok := r.dispatchDesignReview(parentCtx, m, plan, prev, round)
		if !ok {
			// Design-side fail-soft is the OPPOSITE of the implementation
			// gate's (§六-1): there, the edits already exist and stopping
			// cannot un-write them, so the gate lets them through with a
			// warning. Here nothing has been implemented yet, and letting an
			// unreviewed plan through would skip the whole first half of the
			// loop — so the mission stops and the plan goes to the user.
			r.ui.Info("  mission: design review unavailable — NOT implementing; the plan is at " + m.designPath())
			r.leaveMission(missionStatusHandedOver)
			return false
		}

		scope, pass := designOutcome(r.cfg.WorkDir, verdict)
		m.appendReview(missionReviewRecord{
			Phase: "design", Round: round, Verdict: verdictWord(pass), Summary: verdict.Summary,
			Detail: map[string]any{"issues": verdict.Issues, "scope_files": scope, "acceptance": verdict.Acceptance},
		})

		if pass {
			c := &Charter{ScopeFiles: scope, Acceptance: verdict.Acceptance}
			if err := m.lockCharter(c); err != nil {
				r.ui.Info(fmt.Sprintf("  mission: could not lock the charter (%v) — NOT implementing", err))
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

		if round >= maxRounds {
			r.failDesign(verdict, maxRounds)
			return false
		}
		r.presentDesignIssues(fmt.Sprintf("  mission: design review found %d issue(s) — revising (round %d/%d)",
			len(verdict.Issues), round+1, maxRounds), verdict)
		input = missionDesignRevisionMessage(round+1, maxRounds, escalated, m.designPath(), verdict)
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
func (r *ChatRepl) failDesign(v *agent.DesignReviewResult, maxRounds int) {
	escalated := r.mission != nil && r.mission.state.Escalation > 0
	planPath := ""
	if r.mission != nil {
		planPath = r.mission.designPath()
	}
	if v != nil && len(v.Issues) > 0 {
		r.presentDesignIssues(fmt.Sprintf(
			"  mission: design STILL FAILING after %d round(s) — human judgment needed. Unresolved issues:", maxRounds), v)
	}
	r.ui.Info(fmt.Sprintf("  mission: design_failed — the plan is at %s; nothing was implemented from it", planPath))
	if escalated {
		r.ui.Info("  mission: NOTE — this mission had already implemented under an earlier charter. Those edits are still in the worktree and were NEVER passed by a review.")
	}
	r.leaveMission(missionStatusDesignFailed)
}

func (r *ChatRepl) presentDesignIssues(header string, v *agent.DesignReviewResult) {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	writeIssueList(&b, v.Issues)
	r.ui.Info(b.String())
}

// designOutcome applies the CODE test for a passing design (C4). The
// reviewer's verdict string alone is not enough: scope_files and acceptance
// become the charter, and a "pass" that left either empty would lock a
// charter whose hard scope check admits everything (empty allow-list means
// every edited file is in violation; an empty scope with no violations means
// nothing is ever reviewed). Normalization can also empty a scope list that
// looked non-empty — every path outside the worktree is dropped (R19).
func designOutcome(workDir string, v *agent.DesignReviewResult) (scope []string, pass bool) {
	if v == nil {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(v.Verdict), "pass") && len(v.Issues) > 0 {
		return nil, false
	}
	if len(v.Acceptance) == 0 {
		return nil, false
	}
	for _, a := range v.Acceptance {
		// A criterion this short cannot state an observable outcome; it is
		// the shape a hedged pass takes ("works", "ok").
		if len(strings.TrimSpace(a)) < 12 {
			return nil, false
		}
	}
	scope = normalizeScopeFiles(workDir, v.ScopeFiles)
	if len(scope) == 0 {
		return nil, false
	}
	return scope, true
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
// progress events). ok=false is the fail-soft path — interrupted, timed out,
// tool failure, tampered worktree, unparseable output — and the caller then
// refuses to implement.
func (r *ChatRepl) dispatchDesignReview(parentCtx context.Context, m *mission, plan string, prev *agent.DesignReviewResult, round int) (*agent.DesignReviewResult, bool) {
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
		maxToolCalls: reviewMaxToolCalls,
		timeout:      timeout,
	}
	args := map[string]any{
		"description": "Adversarial design review",
		"agent_type":  string(agent.AgentTypeDesignReviewer),
		"prompt":      buildDesignReviewPrompt(in),
		// The gate caps the reviewer, not the profile — the same split the
		// correctness gate uses, and the same constant: only a gate races a
		// wall clock, and exhausting a tool-call cap degrades into a verdict
		// while the clock expiring loses the whole review.
		"max_tool_calls": reviewMaxToolCalls,
	}
	if r.cfg.ReviewTokenBudget > 0 {
		args["token_budget"] = r.cfg.ReviewTokenBudget
	}

	// A design reviewer has no bash and only read-only tools, so this
	// snapshot is a much weaker concern than it is for the correctness
	// reviewer — but it is still taken, because "the reviewer wrote the
	// tree" must be detectable rather than assumed impossible (§六-2).
	preReview := takeWorktreeSnapshot(r.cfg.WorkDir)

	var result models.ToolResult
	var execErr error
	turnErr := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
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

	if tampered := takeWorktreeSnapshot(r.cfg.WorkDir).changedSince(preReview); len(tampered) > 0 {
		r.ui.Info(fmt.Sprintf(
			"  mission: design reviewer modified the working tree (%s) — verdict DISCARDED",
			strings.Join(relToWorkDir(r.cfg.WorkDir, tampered), ", ")))
		return nil, false
	}
	if turnErr != nil && turnErr.cancelled {
		r.ui.Info("  mission: design review interrupted — the plan is unreviewed")
		return nil, false
	}
	if execErr != nil {
		if errors.Is(execErr, context.DeadlineExceeded) {
			r.ui.Info(fmt.Sprintf(
				"  mission: design review hit its %s deadline — the plan is unreviewed (raise review_timeout in config.yaml)", timeout))
			return nil, false
		}
		r.ui.Info(fmt.Sprintf("  mission: design review failed (%v) — the plan is unreviewed", execErr))
		return nil, false
	}

	schema := agent.GetAgentTypeConfig(agent.AgentTypeDesignReviewer).OutputSchema
	verdict, err := agent.ParseOutput[agent.DesignReviewResult](schema, result.Content)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  mission: design verdict unparseable (%v) — the plan is unreviewed", err))
		return nil, false
	}
	return verdict, true
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
