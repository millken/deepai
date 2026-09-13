package chat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// sessionMissionKey is the session-metadata key holding the mission this
// session owns. The sessions table already has a metadata JSON column, so
// this needs no migration. Scoping the resume pointer to the SESSION (R23)
// is deliberate: scanning .deepai/missions/ for "some unfinished mission"
// would let a `deepai -c` in one terminal adopt a mission another terminal
// is still running.
const sessionMissionKey = "mission_id"

// handleMissionCommand implements /mission:
//
//	/mission <text>   start a new mission from <text>
//	/mission          resume THIS session's active mission
//	/mission abort    end it (nothing is rolled back)
//	/mission status   print phase, rounds and charter summary
//
// Free-form subcommands are deliberately NOT accepted as "extra instructions
// for the running mission" (§5.7): mid-turn steering is not in this design,
// and silently treating a sentence as a new mission would be worse than
// saying what the command does.
func (r *ChatRepl) handleMissionCommand(parentCtx context.Context, args string) {
	arg := strings.TrimSpace(args)
	switch strings.ToLower(arg) {
	case "abort":
		if r.mission == nil {
			r.ui.Info("  mission: no active mission")
			return
		}
		id := r.mission.state.ID
		r.leaveMission(missionStatusAborted)
		r.ui.Info(fmt.Sprintf("  mission: %s aborted — already-made edits are left in the worktree, unreviewed", id))
		return
	case "status":
		r.ui.Info(r.missionStatusText())
		return
	}

	if arg == "" {
		if r.mission == nil {
			r.ui.Info("  mission: no active mission in this session — run /mission <task> to start one")
			return
		}
		r.resumeMission(parentCtx)
		return
	}

	// A new mission supersedes whatever this session was running: two live
	// missions would fight over the same worktree and the same charter
	// injection.
	if r.mission != nil {
		old := r.mission.state.ID
		r.leaveMission(missionStatusAborted)
		r.ui.Info(fmt.Sprintf("  mission: %s aborted — superseded by a new mission", old))
	}
	r.startMission(parentCtx, arg)
}

// startMission creates the on-disk mission, points the session at it, and
// runs the loop.
func (r *ChatRepl) startMission(parentCtx context.Context, brief string) {
	m, err := createMission(r.cfg.WorkDir, brief)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  mission: could not start (%v)", err))
		return
	}
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.ui.Info(fmt.Sprintf("  mission: %s started — design phase (round 1/%d)", m.state.ID, maxDesignRounds))
	r.runMission(parentCtx)
}

// resumeMission continues this session's active mission from its persisted
// phase.
func (r *ChatRepl) resumeMission(parentCtx context.Context) {
	m := r.mission
	r.ui.Info(fmt.Sprintf("  mission: resuming %s in %s phase", m.state.ID, m.state.Phase))
	r.ui.Info(missionExternalWriterWarning)
	r.runMission(parentCtx)
}

// missionExternalWriterWarning is required on every resume (R34/R39). While
// a mission is active, the implementation baseline is a point in TIME, not a
// record of authorship: anything that changes the worktree after it — the
// user's own editor, a second REPL, a formatter daemon — is attributed to
// the mission, and the scope check will order those files reverted if they
// fall outside the charter.
const missionExternalWriterWarning = "  mission: while a mission is active, ANY change to the worktree counts as the mission's — your own edits, another deepai session, an editor or formatter. Out-of-charter ones will be ordered reverted."

// attachSessionMission re-attaches this session's mission at REPL startup.
// Only status==active resumes (R32): a finished mission keeps its metadata
// pointer so /mission status can still report it, but must never silently
// continue.
func (r *ChatRepl) attachSessionMission() {
	if r.sess == nil {
		return
	}
	id := strings.TrimSpace(r.sess.Metadata[sessionMissionKey])
	if id == "" {
		return
	}
	m, err := openMission(r.cfg.WorkDir, id)
	if err != nil {
		slog.Warn("mission state unreadable", "mission", id, "err", err)
		return
	}
	if m.state.Status != missionStatusActive {
		return
	}
	r.mission = m
	if m.charter != nil {
		r.carry.SetMissionCharter(renderCharter(m.charter))
	}
	r.ui.Info(fmt.Sprintf("  mission: %s is still active (%s phase) — /mission resumes it, /mission abort ends it", id, m.state.Phase))
	r.ui.Info(missionExternalWriterWarning)
}

// leaveMission is the ONE exit from an active mission (R36). Every terminal
// status goes through it, because three things have to happen together and
// forgetting any one of them leaks the mission into ordinary conversation:
//
//   - the status is persisted, so a later resume refuses to continue;
//   - the rendered charter comes off the SessionCarry, so the trailing turn
//     injection stops telling an ordinary turn to "stay inside scope_files"
//     forever after the mission ended;
//   - r.mission goes nil, which is what makes reviewGate fall back to its
//     ordinary review_after_edit guard on the very next turn.
//
// The lock file itself is left on disk for audit. Only abort clears the
// session's mission pointer: done/handed_over/design_failed keep it so
// /mission status can still report what happened, while the status check
// keeps a bare /mission from resuming them.
func (r *ChatRepl) leaveMission(status missionStatus) {
	m := r.mission
	if m == nil {
		return
	}
	if err := m.setStatus(status); err != nil {
		slog.Warn("persist mission status", "mission", m.state.ID, "status", status, "err", err)
	}
	r.carry.SetMissionCharter("")
	r.clearMissionTurnMode()
	r.mission = nil
	r.reviewPrev = nil
	if status == missionStatusAborted {
		r.setSessionMission("")
	}
}

// setSessionMission writes (or clears) the session's mission pointer.
func (r *ChatRepl) setSessionMission(id string) {
	if r.sess == nil {
		return
	}
	if r.sess.Metadata == nil {
		r.sess.Metadata = make(map[string]string)
	}
	if id == "" {
		delete(r.sess.Metadata, sessionMissionKey)
	} else {
		r.sess.Metadata[sessionMissionKey] = id
	}
	r.saveSession()
}

// missionStatusText reports the mission this session points at, live or
// finished. It reads DISK rather than r.mission so it still answers after a
// terminal status has already detached the in-memory handle.
func (r *ChatRepl) missionStatusText() string {
	m := r.mission
	if m == nil {
		id := ""
		if r.sess != nil {
			id = strings.TrimSpace(r.sess.Metadata[sessionMissionKey])
		}
		if id == "" {
			return "  mission: none in this session"
		}
		loaded, err := openMission(r.cfg.WorkDir, id)
		if err != nil {
			return fmt.Sprintf("  mission: %s — state unreadable (%v)", id, err)
		}
		m = loaded
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  mission: %s | status %s | phase %s\n", m.state.ID, m.state.Status, m.state.Phase)
	fmt.Fprintf(&b, "    rounds: design %d/%d", m.state.DesignRound, maxDesignRounds)
	if m.state.Escalation > 0 {
		fmt.Fprintf(&b, " | escalated design %d/%d", m.state.EscalatedDesignRound, maxEscalatedDesignRounds)
	}
	fmt.Fprintf(&b, " | implement %d/%d | scope %d/%d | idle %d/%d | escalations %d/%d\n",
		m.state.ImplementRound, maxReviewRounds, m.state.ScopeRound, maxScopeFixRounds,
		m.state.IdleRound, maxIdleRounds, m.state.Escalation, maxDesignEscalations)
	if m.charter == nil {
		b.WriteString("    charter: not locked yet\n")
	} else {
		fmt.Fprintf(&b, "    charter: %d file(s) in scope, %d acceptance criteria\n",
			len(m.charter.ScopeFiles), len(m.charter.Acceptance))
		for _, f := range m.charter.ScopeFiles {
			b.WriteString("      - " + f + "\n")
		}
	}
	fmt.Fprintf(&b, "    reviewers run on: %s\n", r.reviewModelLabel())
	fmt.Fprintf(&b, "    dir: %s", m.dir)
	return b.String()
}

// maybeUpgradeToMission turns an ordinary turn that entered plan mode into a
// mission, when mission_on_plan is set (§5.1, C11). It runs AFTER the turn,
// never inside it: runTurn is not re-entrant, and by the end of the turn the
// model has usually written the plan already.
//
// The plan the turn wrote is COPIED into the mission's own design.md. The
// timestamped file it lived in is not adopted as the authority — the mission
// gate reads exactly one path for the whole design phase (R9), and a plan
// that stayed behind at its old path would be silently dropped on the first
// revision round.
func (r *ChatRepl) maybeUpgradeToMission(parentCtx context.Context, brief string) {
	if !r.cfg.MissionOnPlan || r.mission != nil || !r.planMode {
		return
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return
	}
	m, err := createMission(r.cfg.WorkDir, brief)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  mission: could not start (%v)", err))
		return
	}
	if r.lastPlanFile != "" {
		if data, readErr := os.ReadFile(r.lastPlanFile); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
			if writeErr := os.WriteFile(m.designPath(), data, 0o644); writeErr != nil {
				slog.Warn("copy plan into mission", "mission", m.state.ID, "err", writeErr)
			}
		}
	}
	r.mission = m
	r.setSessionMission(m.state.ID)
	r.ui.Info(fmt.Sprintf("  mission: %s started from this plan (mission_on_plan) — design review next; /mission abort ends it", m.state.ID))
	r.runMission(parentCtx)
}
