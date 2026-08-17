package chat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/memory"
)

// refineAction is one of the reserved subcommands of /refine.
type refineAction int

const (
	refineRun refineAction = iota
	refineUndo
	refineRollback
	refineList
	refineStatus
	refineOn
	refineOff
)

// refineListLimit bounds what /refine list prints.
const refineListLimit = 20

// manualRefineTimeout bounds one scope's extraction during /refine. The async
// path gets the queue's 5-minute budget because nobody is waiting on it; here
// the REPL is blocked and slash commands are dispatched outside the turn loop,
// so the interrupt channel is not armed. A shorter ceiling is what keeps a slow
// provider from wedging the prompt with no way out.
const manualRefineTimeout = 90 * time.Second

// parseRefineCommand interprets the arguments of /refine.
//
// Free text is rejected rather than treated as an instruction to the extractor.
// The subcommands are ordinary words, so accepting both would make "/refine list
// everything I said" ambiguous — and guessing wrong either silently skips a
// refine or silently runs one.
func parseRefineCommand(args string) (refineAction, string, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return refineRun, "", nil
	}

	switch strings.ToLower(fields[0]) {
	case "undo":
		return refineUndo, "", nil
	case "rollback":
		if len(fields) < 2 {
			return refineRun, "", errors.New("/refine rollback needs a refinement id (see /refine list)")
		}
		return refineRollback, fields[1], nil
	case "list":
		return refineList, "", nil
	case "status":
		return refineStatus, "", nil
	case "on":
		return refineOn, "", nil
	case "off":
		return refineOff, "", nil
	default:
		return refineRun, "", fmt.Errorf(
			"/refine takes no free text; use /refine, or one of: undo, rollback <id>, list, status, on, off")
	}
}

// scopedRecord pairs a record with the scope whose partition it came from.
type scopedRecord struct {
	scope  string
	record memory.RefinementRecord
}

// mergeScopedRecords interleaves both partitions newest-first. One refine writes
// a record per scope, so a history split by partition reads as two unrelated
// halves; time order is the only ordering that reflects what happened.
func mergeScopedRecords(session, user []memory.RefinementRecord) []scopedRecord {
	merged := make([]scopedRecord, 0, len(session)+len(user))
	for _, r := range session {
		merged = append(merged, scopedRecord{scope: "session", record: r})
	}
	for _, r := range user {
		merged = append(merged, scopedRecord{scope: "user", record: r})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].record.CreatedAt.Equal(merged[j].record.CreatedAt) {
			return merged[i].record.ID > merged[j].record.ID
		}
		return merged[i].record.CreatedAt.After(merged[j].record.CreatedAt)
	})
	return merged
}

// formatRefinementList renders the refinement history for /refine list.
func formatRefinementList(session, user []memory.RefinementRecord, limit int) string {
	merged := mergeScopedRecords(session, user)
	if len(merged) == 0 {
		return "  No refinements recorded yet."
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}

	var sb strings.Builder
	for _, entry := range merged {
		added, updated, removed := entry.record.Summary()
		fmt.Fprintf(&sb, "  %s  %-7s  +%d ~%d -%d  %s  %s\n",
			entry.record.CreatedAt.Local().Format("01-02 15:04"),
			entry.scope,
			added, updated, removed,
			entry.record.ID,
			entry.record.Rationale,
		)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// findUndoTargets picks the most recent refinement and its counterpart in the
// other scope.
//
// One refine writes a record per scope, so undoing only the session half would
// leave the two scopes disagreeing about what the user said. The counterpart may
// legitimately be absent: an extraction that changed nothing writes no record.
func findUndoTargets(session, user []memory.RefinementRecord) (sessionID, userID string, err error) {
	merged := mergeScopedRecords(session, user)
	if len(merged) == 0 {
		return "", "", errors.New("no refinements to undo")
	}
	newest := merged[0].record

	if merged[0].scope == "session" {
		sessionID = newest.ID
	} else {
		userID = newest.ID
	}
	if newest.PairID == "" {
		return sessionID, userID, nil
	}
	for _, entry := range merged[1:] {
		if entry.record.PairID != newest.PairID || entry.scope == merged[0].scope {
			continue
		}
		if entry.scope == "session" {
			sessionID = entry.record.ID
		} else {
			userID = entry.record.ID
		}
		break
	}
	return sessionID, userID, nil
}

// autoRefineEnabled reports whether the review gate should run, preferring a
// session-level /refine on|off over the configured default.
func (r *ChatRepl) autoRefineEnabled() bool {
	if r.refineOverride != nil {
		return *r.refineOverride
	}
	return r.cfg.MemoryAutoRefine
}

// setAutoRefine applies a session-level override. It deliberately does not touch
// config.yaml: /refine off is a decision about this conversation, and a restart
// should return to the configured default.
func (r *ChatRepl) setAutoRefine(enabled bool) {
	r.refineOverride = &enabled
}

// handleRefineCommand implements /refine and its reserved subcommands.
func (r *ChatRepl) handleRefineCommand(ctx context.Context, args string) {
	action, arg, err := parseRefineCommand(args)
	if err != nil {
		r.ui.Info("  " + err.Error())
		return
	}
	if r.cfg.MemoryService == nil {
		r.ui.Info("  Memory is not enabled in this session.")
		return
	}

	switch action {
	case refineOn, refineOff:
		r.setAutoRefine(action == refineOn)
		r.ui.Info("  " + r.refineStatusText())
	case refineStatus:
		r.ui.Info("  " + r.refineStatusText())
	case refineList:
		r.ui.Info(r.refineListText(ctx))
	case refineRun:
		r.runManualRefine(ctx)
	case refineUndo:
		r.runRefineUndo(ctx)
	case refineRollback:
		r.runRefineRollback(ctx, arg)
	}
}

// refineScopes returns the storage keys a refine covers: the session, and the
// cross-session user scope when a workdir identity is configured.
func (r *ChatRepl) refineScopes() (sessionID, userScopeKey string) {
	sessionID = r.sess.ID
	if uid := strings.TrimSpace(r.cfg.WorkDir); uid != "" {
		userScopeKey = memory.UserScope(uid).Key()
	}
	return sessionID, userScopeKey
}

// effectiveExtractInterval is the cadence memory work actually runs at, which is
// the gate's cadence only while the gate is on: with it off — or with a config
// that disables it, which resolves the interval to 0 — memoryScheduleFor falls
// back to unconditional extraction on its own fixed cadence.
func (r *ChatRepl) effectiveExtractInterval() int {
	if r.autoRefineEnabled() && r.cfg.MemoryRefineInterval > 0 {
		return r.cfg.MemoryRefineInterval
	}
	return fallbackExtractInterval
}

func (r *ChatRepl) refineStatusText() string {
	state := "off"
	if r.autoRefineEnabled() {
		state = "on"
	}
	interval := r.effectiveExtractInterval()
	if r.refineOverride == nil {
		return fmt.Sprintf("auto-refine: %s (from config); memory extraction every %d turns", state, interval)
	}
	// Show both layers: the override is session-only, so a restart returns to the
	// configured default and the user should be able to see that coming.
	configured := "off"
	if r.cfg.MemoryAutoRefine {
		configured = "on"
	}
	return fmt.Sprintf("auto-refine: %s for this session (config default: %s); memory extraction every %d turns",
		state, configured, interval)
}

func (r *ChatRepl) refineListText(ctx context.Context) string {
	sessionID, userScopeKey := r.refineScopes()
	session, err := r.cfg.MemoryService.ListRefinements(ctx, sessionID, refineListLimit)
	if err != nil {
		return fmt.Sprintf("  Could not read refinement history: %v", err)
	}
	var user []memory.RefinementRecord
	if userScopeKey != "" {
		user, err = r.cfg.MemoryService.ListRefinements(ctx, userScopeKey, refineListLimit)
		if err != nil {
			return fmt.Sprintf("  Could not read user-scope refinement history: %v", err)
		}
	}
	return formatRefinementList(session, user, refineListLimit)
}

// runManualRefine extracts both scopes now, skipping the gate: the user asked
// for this explicitly, so there is nothing left to decide.
func (r *ChatRepl) runManualRefine(ctx context.Context) {
	sessionID, userScopeKey := r.refineScopes()

	// Drop queued work first, or an in-flight extraction of the same messages
	// lands right after this one and records a second, redundant refinement.
	r.cfg.MemoryService.CancelPendingUpdates(sessionID)
	if userScopeKey != "" {
		r.cfg.MemoryService.CancelPendingUpdates(userScopeKey)
	}

	// Two LLM extractions run back to back here, so say what is happening.
	r.ui.Info("  Refining memory...")

	pairID := memory.NewPairID()
	for _, scope := range []struct{ label, key string }{
		{"session", sessionID},
		{"user", userScopeKey},
	} {
		if scope.key == "" {
			continue
		}
		record, saved, err := r.refineScopeNow(ctx, scope.key, pairID)
		switch {
		case err != nil:
			r.ui.Info(fmt.Sprintf("  %s scope: failed: %v", scope.label, err))
		case !saved:
			r.ui.Info(fmt.Sprintf("  %s scope: nothing new to remember", scope.label))
		case record.ID == "":
			// A write with no record: the merge moved no fact, only the
			// User/History narrative, so RefineAndRecord deliberately recorded
			// nothing a rollback could restore. Summarizing the zero record here
			// would claim "+0 ~0 -0" against an empty id while something did in
			// fact change.
			r.ui.Info(fmt.Sprintf("  %s scope: context updated, no fact changes", scope.label))
		default:
			added, updated, removed := record.Summary()
			r.ui.Info(fmt.Sprintf("  %s scope: +%d ~%d -%d  (%s)", scope.label, added, updated, removed, record.ID))
		}
	}
}

// refineScopeNow extracts one scope synchronously, under its own timeout.
func (r *ChatRepl) refineScopeNow(ctx context.Context, storageKey, pairID string) (memory.RefinementRecord, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, manualRefineTimeout)
	defer cancel()
	return r.cfg.MemoryService.RefineAndRecord(
		ctx, storageKey, r.sess.Messages, r.cfg.MemoryExtractor,
		memory.RefineMeta{PairID: pairID, Rationale: "manual"},
	)
}

// runRefineUndo rolls back the most recent refinement in both scopes.
func (r *ChatRepl) runRefineUndo(ctx context.Context) {
	sessionID, userScopeKey := r.refineScopes()

	session, err := r.cfg.MemoryService.ListRefinements(ctx, sessionID, refineListLimit)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Could not read refinement history: %v", err))
		return
	}
	var user []memory.RefinementRecord
	if userScopeKey != "" {
		if user, err = r.cfg.MemoryService.ListRefinements(ctx, userScopeKey, refineListLimit); err != nil {
			r.ui.Info(fmt.Sprintf("  Could not read user-scope refinement history: %v", err))
			return
		}
	}

	sessionTarget, userTarget, err := findUndoTargets(session, user)
	if err != nil {
		r.ui.Info("  " + err.Error())
		return
	}

	// The two halves are separate transactions under separate session locks, so
	// report each outcome rather than pretending the pair is atomic.
	pairID := memory.NewPairID()
	// Each half is reported on its own: they are separate transactions under
	// separate locks, so one can fail while the other succeeds.
	if sessionTarget != "" {
		_ = r.reportRollback(ctx, "session", sessionID, sessionTarget, pairID)
	}
	if userTarget != "" && userScopeKey != "" {
		_ = r.reportRollback(ctx, "user", userScopeKey, userTarget, pairID)
	}
}

func (r *ChatRepl) runRefineRollback(ctx context.Context, recordID string) {
	sessionID, userScopeKey := r.refineScopes()

	// An id lives in exactly one partition; try the session's first.
	for _, scope := range []struct{ label, key string }{
		{"session", sessionID},
		{"user", userScopeKey},
	} {
		if scope.key == "" {
			continue
		}
		record, err := r.cfg.MemoryService.GetRefinement(ctx, scope.key, recordID)
		if err != nil {
			continue
		}
		if !r.reportRollback(ctx, scope.label, scope.key, recordID, memory.NewPairID()) {
			return
		}
		// Only worth saying when the other scope actually holds a counterpart.
		if record.PairID != "" && userScopeKey != "" {
			r.ui.Info("  The paired record in the other scope was left alone; use /refine undo to roll back both.")
		}
		return
	}
	r.ui.Info(fmt.Sprintf("  No refinement %q in this session (see /refine list).", recordID))
}

// reportRollback rolls one scope back and prints the outcome, reporting whether
// it succeeded so callers do not follow a failure with advice that implies one.
func (r *ChatRepl) reportRollback(ctx context.Context, label, storageKey, recordID, pairID string) bool {
	skipped, err := r.cfg.MemoryService.RollbackRefinement(ctx, storageKey, recordID, pairID)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  %s scope: rollback failed: %v", label, err))
		return false
	}
	if len(skipped) == 0 {
		r.ui.Info(fmt.Sprintf("  %s scope: rolled back %s", label, recordID))
		return true
	}
	r.ui.Info(fmt.Sprintf("  %s scope: rolled back %s; left alone (changed since): %s",
		label, recordID, strings.Join(skipped, ", ")))
	return true
}
