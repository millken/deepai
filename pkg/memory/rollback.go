package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RollbackFacts undoes one refinement, returning the fact set to save and the
// IDs whose rollback was declined.
//
// It is a pure function: the lock, the load and the transaction all live in
// Service.RollbackRefinement. now is injected so the caller controls the
// timestamp stamped on reinserted facts.
//
// Facts are classified by four questions — was it in the snapshot taken before
// the refine, was it there right after, does it exist now, and does it still
// look the way the refine left it. The last question is answered by content
// fingerprint rather than by timestamp: Merge stamps UpdatedAt=now on every
// fact it touches, so a timestamp comparison cannot separate "the refine
// changed this" from "someone else did", and SQLite stores timestamps truncated
// to whole seconds anyway.
//
//	in Pre | in Post | exists now | fingerprint matches | action
//	-------+---------+------------+---------------------+--------------------------
//	  yes  |   yes   |    yes     |         yes         | restore in place
//	  yes  |   yes   |    yes     |         no          | keep current (3rd party edit)
//	  yes  |   yes   |    no      |          -          | keep deleted (3rd party delete)
//	  yes  |   no    |    yes     |          -          | keep current (evicted, recreated)
//	  yes  |   no    |    no      |          -          | reinsert whole fact (evicted)
//	  no   |   yes   |    yes     |         yes         | delete (the refine added it)
//	  no   |   yes   |    yes     |         no          | keep current (3rd party edit)
//	  no   |   yes   |    no      |          -          | nothing to do
//	  no   |   no    |    yes     |          -          | keep current (created later)
//	  no   |   no    |    no      |          -          | unreachable
//
// The two "restore" actions are not the same operation. Restoring in place
// overwrites only Content/Category/Confidence and leaves the feedback counters
// alone, because those are accumulated by direct SQL updates that bypass
// Document and the snapshot's copies are stale. Reinserting an evicted fact has
// no current value to preserve, so it takes the snapshot wholesale — but with a
// fresh UpdatedAt, or eviction scoring would drop it again immediately.
//
// skipped reports the facts this refinement touched whose rollback was declined
// (rows 2, 3, 4 and 7), so callers can say which ones were left alone. Facts
// created after the refine are unrelated to it and are not reported.
func RollbackFacts(currentFacts []Fact, record RefinementRecord, now time.Time) (result []Fact, skipped []string) {
	preByID := make(map[string]Fact, len(record.PreSnapshot))
	for _, f := range record.PreSnapshot {
		preByID[f.ID] = f
	}
	currentIDs := make(map[string]struct{}, len(currentFacts))
	for _, f := range currentFacts {
		currentIDs[f.ID] = struct{}{}
	}

	result = make([]Fact, 0, len(currentFacts)+len(record.PreSnapshot))

	for _, current := range currentFacts {
		pre, inPre := preByID[current.ID]
		postFP, inPost := record.PostFactFingerprints[current.ID]

		switch {
		case inPre && inPost:
			if factFingerprint(current) != postFP {
				result = append(result, current)
				skipped = append(skipped, current.ID)
				continue
			}
			restored := current
			restored.Content = pre.Content
			restored.Category = pre.Category
			restored.Confidence = pre.Confidence
			result = append(result, restored)

		case inPre && !inPost:
			// The refine evicted it, then something recreated it. The recreated
			// value is newer than anything this rollback knows about.
			result = append(result, current)
			skipped = append(skipped, current.ID)

		case !inPre && inPost:
			if factFingerprint(current) != postFP {
				// The refine added it and a third party then edited it. Deleting
				// would discard their edit, so keep it — the same principle that
				// keeps third-party edits in the inPre case above.
				result = append(result, current)
				skipped = append(skipped, current.ID)
				continue
			}
			// Untouched output of this refine: drop it.

		default:
			// Created after the refine; nothing to do with this rollback.
			result = append(result, current)
		}
	}

	// Facts in the snapshot that no longer exist. Iterated in snapshot order so
	// the result is deterministic.
	for _, pre := range record.PreSnapshot {
		if _, exists := currentIDs[pre.ID]; exists {
			continue
		}
		if _, inPost := record.PostFactFingerprints[pre.ID]; inPost {
			// It survived the refine and was deleted afterwards. Respect that.
			skipped = append(skipped, pre.ID)
			continue
		}
		reinserted := pre
		reinserted.UpdatedAt = now
		result = append(result, reinserted)
	}

	return result, skipped
}

// rollbackNarrative decides what the narrative half of the document becomes.
//
// It mirrors the fact table's third-party rule with the same fingerprint test:
// the pre-refine narrative is restored only while the current narrative still
// looks the way the refine left it. Merge never timestamps the narrative, so
// content is the only available evidence.
//
// A record written before narrative rollback existed has nil PreUser/PreHistory
// — the JSON simply lacks the fields — and there is no pre-state to restore.
// Restoring the zero value would blank the narrative instead, which is strictly
// worse than leaving it, so such records decline.
//
// restored reports whether the narrative was actually rolled back, so callers
// can say when it was left alone.
func rollbackNarrative(current Document, record RefinementRecord) (user UserMemory, history HistoryMemory, restored bool) {
	if record.PreUser == nil || record.PreHistory == nil {
		return current.User, current.History, false
	}
	if narrativeFingerprint(current.User, current.History) != record.PostNarrativeFingerprint {
		return current.User, current.History, false
	}
	return *record.PreUser, *record.PreHistory, true
}

// RollbackRefinement undoes one recorded refinement, under the session lock.
//
// Rollback is a read-modify-write of the whole fact set, so it belongs in the
// same critical section as everything else that touches a document — the memory
// builtin tool writes through Service.Save, and a write landing between the load
// and the save here would be silently overwritten. That is also why this cannot
// live in the REPL layer: getSessionLock is unexported, so a caller outside this
// package structurally cannot hold it.
//
// The rollback is itself recorded as a refinement, carrying the fingerprints of
// the state it produced, so a mistaken rollback can be rolled back in turn.
// pairID ties this to the matching record in the other scope's partition when
// "/refine undo" rolls back both halves of one refine.
//
// skipped lists the facts whose rollback was declined because something else
// changed them after the refine; see RollbackFacts. narrativeRestored reports
// whether the User/History half was rolled back too — it is declined for a
// record predating narrative snapshots, and when a third party rewrote the
// narrative after the refine; see rollbackNarrative.
//
// Unlike RefineAndRecord there is no degraded mode: a backend without
// refinement history has nothing to roll back to, so this fails rather than
// falling back.
func (s *Service) RollbackRefinement(ctx context.Context, sessionID, recordID, pairID string) (skipped []string, narrativeRestored bool, err error) {
	if s == nil {
		return nil, false, errors.New("memory service is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, false, errors.New("session id is required")
	}
	recordID = strings.TrimSpace(recordID)
	if recordID == "" {
		return nil, false, errors.New("refinement id is required")
	}
	if s.storage == nil {
		return nil, false, errors.New("memory storage is not configured")
	}
	rs, ok := s.storage.(RefinementStore)
	if !ok {
		return nil, false, errors.New("rollback requires a storage backend with refinement history")
	}

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	// Read the record inside the lock: a concurrent rollback of the same record
	// must not be able to read it before this one deletes it.
	record, err := rs.GetRefinement(ctx, sessionID, recordID)
	if err != nil {
		return nil, false, err
	}
	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	now := time.Now().UTC()
	facts, skipped := RollbackFacts(current.Facts, record, now)
	user, history, narrativeRestored := rollbackNarrative(current, record)

	doc := current
	// Load returns a zero Document for ErrNotFound, and prepareDocument rejects
	// an empty session id.
	doc.SessionID = sessionID
	doc.Facts = facts
	doc.User = user
	doc.History = history
	doc.UpdatedAt = now

	// The rollback record carries its own narrative snapshot for the same reason
	// it carries PostFactFingerprints: without it, rolling THIS rollback back
	// would find no pre-state and decline, so the undo would not be reversible.
	preUser, preHistory := current.User, current.History
	rollbackRecord := RefinementRecord{
		ID:                       refineID(),
		PairID:                   strings.TrimSpace(pairID),
		Rationale:                "Rollback of " + recordID,
		SessionID:                sessionID,
		PreSnapshot:              cloneFacts(current.Facts),
		PostFactFingerprints:     factFingerprints(facts),
		FactIDsChanged:           diffFactIDs(current.Facts, facts),
		PreUser:                  &preUser,
		PreHistory:               &preHistory,
		PostNarrativeFingerprint: narrativeFingerprint(user, history),
		CreatedAt:                now,
	}
	if err := rs.SaveWithRollback(ctx, doc, rollbackRecord, recordID); err != nil {
		return nil, false, err
	}
	return skipped, narrativeRestored, nil
}
