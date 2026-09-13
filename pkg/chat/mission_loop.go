package chat

import "context"

// runMission drives one mission to a terminal status. It is the outer
// bounded loop of docs/LONG_TASK_LOOP_DESIGN.md §4: DESIGN until the design
// gate passes, then IMPLEMENT until the implementation gate passes, with a
// single escalation edge back from IMPLEMENT to DESIGN.
//
// Both phases are written here, as two same-shaped for loops over runTurn —
// NOT as a state machine outside the REPL (87772b6) and NOT through
// runEpisode, whose two exits cannot express the escalation (R17).
func (r *ChatRepl) runMission(parentCtx context.Context) {
	for r.mission != nil {
		switch r.mission.state.Phase {
		case missionPhaseDesign:
			if !r.runDesignPhase(parentCtx) {
				return
			}
		case missionPhaseImplement:
			if !r.runImplementPhase(parentCtx) {
				return
			}
		default:
			r.ui.Info("  mission: unknown phase — handing back")
			r.leaveMission(missionStatusHandedOver)
			return
		}
	}
}

// Escalation signals (§5.4.3). Named constants rather than bare strings so
// the gate that raises one and the message that explains it cannot drift.
const (
	// escalateFaultLayer: the reviewer itself said the plan is at fault.
	escalateFaultLayer = "fault_layer"
	// escalateRepeatFile: two review rounds failed on the same file, and the
	// next stop would have been the user. One re-design first.
	escalateRepeatFile = "repeat_file"
	// escalateScope: the implementation kept reaching for files the charter
	// does not name, which usually means the charter missed a file that must
	// change.
	escalateScope = "scope"
)

// applyMissionTurnMode forces plan mode BY PHASE before every mission turn
// (R10/R21). Doing this only at phase transitions is not enough: plan mode
// lives in two places — the REPL flag and the Agent — and the post-turn
// readback copies the Agent's state back into the flag. One stray
// enter_plan_mode inside an implementation turn would therefore make the
// NEXT implementation turn read-only, with no way out, since the only exit
// (exit_plan_mode) is deferred to a gate that the implementation phase does
// not run.
func (r *ChatRepl) applyMissionTurnMode(phase missionPhase) {
	if phase == missionPhaseDesign {
		r.planMode = true
		r.planFile = r.mission.designPath()
		r.disableEnterPlan = false
		r.deferPlanApproval = true
	} else {
		r.planMode = false
		r.planFile = ""
		r.disableEnterPlan = true
		r.deferPlanApproval = false
	}
	r.ui.SetStatus(r.currentModel, r.planMode)
}

// clearMissionTurnMode restores the ordinary (non-mission) turn
// configuration. Called from leaveMission: a mission that ended in its
// design phase would otherwise leave the REPL in plan mode with approval
// still routed to a gate that no longer exists, so the user's next ordinary
// turn would be read-only and unable to exit.
func (r *ChatRepl) clearMissionTurnMode() {
	r.planMode = false
	r.planFile = ""
	r.disableEnterPlan = false
	r.deferPlanApproval = false
	r.missionPendingInput = ""
	r.missionEscalationNote = ""
	r.ui.SetStatus(r.currentModel, r.planMode)
}

// runMissionTurn runs one ordinary turn with a synthesized input. The
// missionTurn seam exists for tests only (the same role orphanWait and
// lockHeartbeatInterval play on this type); production always takes the
// runTurn path, so every mission turn is persisted, memory-scheduled and
// counted exactly like a turn the user typed.
func (r *ChatRepl) runMissionTurn(parentCtx context.Context, input string) *turnError {
	if r.missionTurn != nil {
		return r.missionTurn(parentCtx, input)
	}
	r.turn++
	in := input
	// Images ride with the input they were pasted alongside, and only with
	// it: consumed here so a later synthesized turn in the same phase does
	// not re-attach them.
	images := r.missionPendingImages
	r.missionPendingImages = nil
	return r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
		return r.runTurn(ctx, in, images, false)
	})
}

// missionDesignInput picks the design phase's next turn input: whatever the
// user typed to resume the mission, an escalation message computed by the
// implementation gate, the first-turn brief expansion, or a resume nudge for
// a phase picked up mid-flight.
func (r *ChatRepl) missionDesignInput() string {
	m := r.mission
	if in := r.missionPendingInput; in != "" {
		r.missionPendingInput = ""
		return in
	}
	escalated := m.state.Escalation > 0
	maxRounds := maxDesignRounds
	if escalated {
		maxRounds = maxEscalatedDesignRounds
	}
	if m.designRound() == 0 {
		return missionDesignFirstMessage(maxRounds, m.designPath(), m.brief)
	}
	return missionDesignResumeMessage(m.designRound()+1, maxRounds, escalated, m.designPath(), m.brief)
}
