package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// RefinementRecord is the audit trail for one refine operation, and the input
// rollback replays. PreSnapshot is the full fact set from before the refine;
// PostFactFingerprints is what the facts looked like right after it landed.
//
// Rollback compares the CURRENT fingerprint against PostFactFingerprints, not
// against PreSnapshot: Merge stamps UpdatedAt=now on every fact it touches, so
// "differs from the pre-state" is true for exactly the facts rollback needs to
// restore. Only "differs from the post-state" means a third party (the memory
// builtin tool) edited the fact after the refine.
type RefinementRecord struct {
	ID        string `json:"id"`         // refine_<unix_ns>
	PairID    string `json:"pair_id"`    // shared by the session- and user-scope records of one refine
	Rationale string `json:"rationale"`  // gate rationale, "manual", or "Rollback of <id>"
	SessionID string `json:"session_id"` // storage key: session ID or UserScope(uid).Key()

	PreSnapshot          []Fact            `json:"pre_snapshot"`
	PostFactFingerprints map[string]string `json:"post_fact_fingerprints"`
	FactIDsChanged       []string          `json:"fact_ids_changed"`

	CreatedAt time.Time `json:"created_at"`
}

// RefineReview is the auto-refine gate's verdict: is this checkpoint worth
// paying for a full memory extraction?
type RefineReview struct {
	ShouldRefine bool   `json:"shouldRefine"`
	Rationale    string `json:"rationale"`
}

// Reviewer runs the auto-refine review gate. It is optional: when a Service has
// no Reviewer, auto-refine falls back to extracting unconditionally rather than
// not extracting at all.
type Reviewer interface {
	ReviewRefine(ctx context.Context, current Document, messages []models.Message) (RefineReview, error)
}

// RefinementStore is the optional refinement-history capability. Only
// SQLiteStore implements it (the sole production path); PostgresStore,
// FileStore and test fakes do not, and callers fall back to plain UpdateWith so
// extraction keeps working without a history.
//
// It is deliberately NOT part of Storage: widening that interface would break
// every implementation and test stub in the package.
type RefinementStore interface {
	ListRefinements(ctx context.Context, sessionID string, limit int) ([]RefinementRecord, error)
	GetRefinement(ctx context.Context, sessionID, id string) (RefinementRecord, error)
	InsertRefinement(ctx context.Context, sessionID string, record RefinementRecord) error
	DeleteRefinement(ctx context.Context, sessionID, id string) error
	// SaveWithRefinement commits the document and the record in ONE transaction:
	// facts changing without a record would leave a refine that cannot be rolled back.
	SaveWithRefinement(ctx context.Context, doc Document, record RefinementRecord) error
	// SaveWithRollback commits the rolled-back document, the new rollback record
	// and the deletion of the original record in ONE transaction.
	SaveWithRollback(ctx context.Context, doc Document, newRecord RefinementRecord, deleteID string) error
}

// HasRefinementStore reports whether storage can keep refinement history.
func HasRefinementStore(s Storage) bool {
	_, ok := s.(RefinementStore)
	return ok
}

// factFingerprint hashes the fields rollback overwrites, after applying the
// same normalization Save does (prepareFact trims Content/Category and clamps
// Confidence). Both halves of any comparison must travel the same normalization
// path, or an in-memory fact and its stored form hash differently and rollback
// mistakes every fact for a third-party edit.
//
// Confidence uses %f (6 decimals) as deliberate tolerance, not by accident:
// it absorbs representation differences across the clamps in Merge and
// prepareFact. Do not "fix" it to %v or %g.
func factFingerprint(f Fact) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%f",
		strings.TrimSpace(f.Content),
		strings.TrimSpace(f.Category),
		clampConfidence(f.Confidence),
	)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// factFingerprints indexes factFingerprint by fact ID.
func factFingerprints(facts []Fact) map[string]string {
	out := make(map[string]string, len(facts))
	for _, fact := range facts {
		out[fact.ID] = factFingerprint(fact)
	}
	return out
}

// clampConfidence mirrors the [0,1] clamp in prepareFact and Merge.
func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// lastRefineID keeps refineID monotonic. A bare nanosecond timestamp is not
// guaranteed unique — the clock's granularity is platform-dependent — and
// (session_id, id) is a primary key, so a rollback record and the manual refine
// right after it would collide.
var lastRefineID atomic.Int64

// nextMonotonicNano returns a strictly increasing nanosecond timestamp.
func nextMonotonicNano() int64 {
	for {
		prev := lastRefineID.Load()
		next := time.Now().UTC().UnixNano()
		if next <= prev {
			next = prev + 1
		}
		if lastRefineID.CompareAndSwap(prev, next) {
			return next
		}
	}
}

// refineID returns a unique, time-ordered refinement ID.
func refineID() string {
	return "refine_" + strconv.FormatInt(nextMonotonicNano(), 10)
}

// RefineMeta is the caller-supplied provenance for one refinement. It is a
// struct rather than two string parameters so call sites cannot silently
// transpose them.
type RefineMeta struct {
	// PairID ties the session-scope and user-scope records of one refine
	// together, so "/refine undo" can roll back both halves.
	PairID string
	// Rationale is the gate's reasoning, "manual", or "Rollback of <id>".
	Rationale string
}

// RefineAndRecord runs one extraction and records it, as a single critical
// section: snapshot, extract, save and record all happen under one acquisition
// of the session lock.
//
// This cannot be composed from the outside as "snapshot, then UpdateWith, then
// diff". UpdateWith takes the session lock itself and does not expose the state
// it loaded, so an external snapshot is read before that lock is taken — and the
// memory builtin tool writes through Service.Save, a separate critical section
// that can land in between. The record would then blame the refine for the
// tool's edit, and rolling it back would silently revert the user's own change.
//
// saved reports whether anything was actually written. Extraction is skipped
// without messages, when a synchronous flush has superseded this work, and when
// the merge changed nothing; none of those insert a record, because a record
// that undoes nothing is a rollback target that lies.
//
// If storage cannot keep refinement history, this degrades to a plain
// UpdateWith: losing the history is acceptable, silently losing extraction is
// not.
func (s *Service) RefineAndRecord(
	ctx context.Context,
	sessionID string,
	messages []models.Message,
	ext Extractor,
	meta RefineMeta,
) (RefinementRecord, bool, error) {
	if s == nil {
		return RefinementRecord{}, false, errors.New("memory service is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return RefinementRecord{}, false, errors.New("session id is required")
	}
	if s.storage == nil {
		return RefinementRecord{}, false, errors.New("memory storage is not configured")
	}
	if ext == nil {
		return RefinementRecord{}, false, errors.New("memory extractor is required")
	}

	// Bound the trajectory before filtering, matching the async path (which
	// bounds at enqueue). A synchronous caller such as manual /refine hands over
	// the whole session, which would otherwise overflow the model's context.
	filtered := filterMessagesForMemory(prepareAsyncMessages(messages))
	if len(filtered) == 0 {
		return RefinementRecord{}, false, nil
	}

	rs, hasHistory := s.storage.(RefinementStore)
	if !hasHistory {
		// Extraction still runs; only the history is unavailable.
		return RefinementRecord{}, false, s.UpdateWith(ctx, sessionID, messages, ext)
	}

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return RefinementRecord{}, false, fmt.Errorf("load memory %q: %w", sessionID, err)
	}
	preSnapshot := cloneFacts(current.Facts)

	// Capture the flush generation before the slow call, preferring the value
	// captured when the job was queued (see withCapturedFlushVersion): a flush
	// between enqueue and execution supersedes this work just as much as one
	// during it.
	captured := s.captureFlushVersion(sessionID)
	if v, ok := capturedFlushVersionFromContext(ctx); ok {
		captured = v
	}

	update, err := ext.ExtractUpdate(ctx, current, cloneMessages(filtered))
	if err != nil {
		return RefinementRecord{}, false, err
	}
	update = sanitizeUpdateForStorage(update)
	merged := MergeWithFactSource(current, update, sessionID, factSourceFromMessages(filtered), time.Now().UTC())

	if s.isFlushStale(sessionID, captured) {
		return RefinementRecord{}, false, nil
	}

	changed := diffFactIDs(preSnapshot, merged.Facts)
	if len(changed) == 0 {
		// Merge folds User/History narrative context into the document, and
		// buildInjectionWithIDs injects it alongside facts. An extraction can
		// refresh that without touching a single fact, so "no fact changed" is
		// not "nothing changed" — skipping the save here would silently drop it,
		// which the unconditional UpdateWith path never did.
		if current.User == merged.User && current.History == merged.History {
			return RefinementRecord{}, false, nil
		}
		// Nothing fact-shaped moved, so there is nothing for a rollback to
		// restore: persist the document, but record no refinement.
		if err := s.storage.Save(ctx, merged); err != nil {
			return RefinementRecord{}, false, err
		}
		return RefinementRecord{}, true, nil
	}

	record := RefinementRecord{
		ID:                   refineID(),
		PairID:               strings.TrimSpace(meta.PairID),
		Rationale:            meta.Rationale,
		SessionID:            sessionID,
		PreSnapshot:          preSnapshot,
		PostFactFingerprints: factFingerprints(merged.Facts),
		FactIDsChanged:       changed,
		CreatedAt:            time.Now().UTC(),
	}
	if err := rs.SaveWithRefinement(ctx, merged, record); err != nil {
		return RefinementRecord{}, false, err
	}
	return record, true, nil
}

// cloneFacts copies a fact slice so the snapshot cannot alias the live document.
func cloneFacts(facts []Fact) []Fact {
	if len(facts) == 0 {
		return nil
	}
	return append([]Fact(nil), facts...)
}

// diffFactIDs returns the IDs added, removed, or changed between two fact sets,
// comparing the same normalized fields the rollback fingerprint covers.
func diffFactIDs(before, after []Fact) []string {
	beforeFP := factFingerprints(before)
	afterFP := factFingerprints(after)

	var changed []string
	for _, fact := range after {
		if prev, existed := beforeFP[fact.ID]; !existed || prev != afterFP[fact.ID] {
			changed = append(changed, fact.ID)
		}
	}
	for _, fact := range before {
		if _, stillThere := afterFP[fact.ID]; !stillThere {
			changed = append(changed, fact.ID)
		}
	}
	return changed
}

// gateVerdict is one review-gate decision, handed from the session's job to the
// user scope's job so the gate is paid for once per refine rather than once per
// scope.
type gateVerdict struct {
	shouldRefine bool
	rationale    string
	createdAt    time.Time
}

// verdictTTL bounds how long an unclaimed verdict is kept. Well above the
// queue's per-job timeout, so it can never evict a verdict still in use.
const verdictTTL = 10 * time.Minute

// autoRefineNoGateRationale marks a refinement that ran without a gate verdict,
// either because no reviewer is configured or because the gate never ran.
const autoRefineNoGateRationale = "auto (no gate)"

// WithReviewer attaches the auto-refine review gate. Without one, auto-refine
// extracts unconditionally — the gate is a cost optimisation, never a
// precondition for remembering anything.
func (s *Service) WithReviewer(reviewer Reviewer) *Service {
	if s != nil {
		s.reviewer = reviewer
	}
	return s
}

// ScheduleRefine queues one auto-refine cycle: the review gate plus, if it
// approves, an extraction for the session scope and one for the user scope.
//
// Both jobs are queued up front, each keyed by the scope it writes, because
// dedup, cancellation and flush versioning are all sharded by that key — a
// single job covering two scopes would be mis-sharded for one of them. Queuing
// them together also avoids submitting from inside the worker, which would
// block against a full queue until the submit timeout silently dropped the job.
//
// userScopeKey may be empty when no cross-session identity is configured.
func (s *Service) ScheduleRefine(sessionID, userScopeKey string, messages []models.Message, ext Extractor) {
	if s == nil || s.storage == nil || s.queue == nil || ext == nil || len(messages) == 0 {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	userScopeKey = strings.TrimSpace(userScopeKey)

	s.purgeStaleVerdicts()

	pairID := refineID()
	prepared := prepareAsyncMessages(messages)

	// Without a distinct user scope there is no second job to hand the verdict
	// to, and publishing one would leak an entry nobody ever claims.
	paired := userScopeKey != "" && userScopeKey != sessionID

	s.queue.submit(updateJob{
		typ:        jobRefine,
		sessionID:  sessionID,
		messages:   prepared,
		ext:        ext,
		pairID:     pairID,
		pairQueued: paired,
	})
	if !paired {
		return
	}
	s.queue.submit(updateJob{
		typ:       jobRefineApproved,
		sessionID: userScopeKey,
		messages:  prepared,
		ext:       ext,
		pairID:    pairID,
	})
}

// runRefineGateJob runs the review gate and, if it approves, extracts this job's
// scope. The verdict is published for the paired user-scope job.
func (s *Service) runRefineGateJob(ctx context.Context, job updateJob) {
	approved, rationale := true, autoRefineNoGateRationale

	if s.reviewer != nil {
		current, err := s.storage.Load(ctx, job.sessionID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.logger.Warn("refine gate could not load memory", "session", job.sessionID, "err", err)
		}
		// Filter exactly as the extraction path does. Queued messages are raw, and
		// filterMessagesForMemory is what drops tool results and strips uploaded
		// file blocks — the gate must not be the one path that leaks them to a
		// provider.
		review, err := s.reviewer.ReviewRefine(ctx, current, filterMessagesForMemory(job.messages))
		if err != nil {
			// Fail open: an undecidable gate must not stop extraction.
			s.logger.Warn("refine gate failed, extracting anyway", "session", job.sessionID, "err", err)
		} else {
			approved, rationale = review.ShouldRefine, review.Rationale
			s.logger.Debug("refine gate verdict",
				"session", job.sessionID, "should_refine", approved, "rationale", rationale)
		}
	}

	if job.pairQueued && job.pairID != "" {
		s.verdicts.Store(job.pairID, gateVerdict{
			shouldRefine: approved,
			rationale:    rationale,
			createdAt:    time.Now().UTC(),
		})
	}
	if !approved {
		return
	}
	s.runRefineExtraction(ctx, job, rationale)
}

// runRefineApprovedJob extracts this job's scope using the verdict published by
// the paired gate job.
func (s *Service) runRefineApprovedJob(ctx context.Context, job updateJob) {
	value, found := s.verdicts.LoadAndDelete(job.pairID)
	if !found {
		// The gate job was cancelled or deduped away, so no decision was ever
		// made. A missing verdict carries no information and must not be read as
		// a rejection: this scope's extraction would be dropped silently, and
		// compaction's synchronous flush only covers the session scope.
		s.logger.Debug("refine verdict missing, extracting anyway", "session", job.sessionID)
		s.runRefineExtraction(ctx, job, autoRefineNoGateRationale)
		return
	}
	v, ok := value.(gateVerdict)
	if !ok || !v.shouldRefine {
		return
	}
	s.runRefineExtraction(ctx, job, v.rationale)
}

func (s *Service) runRefineExtraction(ctx context.Context, job updateJob, rationale string) {
	_, saved, err := s.RefineAndRecord(ctx, job.sessionID, job.messages, job.ext,
		RefineMeta{PairID: job.pairID, Rationale: rationale})
	if err != nil {
		s.logger.Warn("auto-refine failed", "session", job.sessionID, "err", err)
		return
	}
	s.logger.Debug("auto-refine finished", "session", job.sessionID, "saved", saved)
}

// purgeStaleVerdicts drops verdicts nobody claimed. A verdict is normally
// consumed by LoadAndDelete, but the consuming job can be deduped away or
// cancelled, and without this the map would grow for the life of the process.
func (s *Service) purgeStaleVerdicts() {
	cutoff := time.Now().UTC().Add(-verdictTTL)
	s.verdicts.Range(func(key, value any) bool {
		if v, ok := value.(gateVerdict); ok && v.createdAt.Before(cutoff) {
			s.verdicts.Delete(key)
		}
		return true
	})
}

// Summary counts what one refinement did, for a "+N ~M -K" line. It reads only
// the record, so callers do not need the document: a fact present only in the
// snapshot was removed, one present only afterwards was added, and one present
// in both whose fingerprint moved was rewritten.
func (r RefinementRecord) Summary() (added, updated, removed int) {
	preFP := factFingerprints(r.PreSnapshot)
	for id, fp := range r.PostFactFingerprints {
		switch before, existed := preFP[id]; {
		case !existed:
			added++
		case before != fp:
			updated++
		}
	}
	for id := range preFP {
		if _, survived := r.PostFactFingerprints[id]; !survived {
			removed++
		}
	}
	return added, updated, removed
}

// NewPairID returns an id tying together the records one refine writes across
// scopes. Callers that fan a single logical refine out to several storage keys
// generate one of these and pass it to every RefineAndRecord call.
func NewPairID() string {
	return "pair_" + strconv.FormatInt(nextMonotonicNano(), 10)
}

// ListRefinements returns the most recent refinements for a storage key.
// A backend that keeps no history has an empty history, not an error: reading it
// is a display concern and must not surface as a failure.
func (s *Service) ListRefinements(ctx context.Context, sessionID string, limit int) ([]RefinementRecord, error) {
	if s == nil || s.storage == nil {
		return nil, nil
	}
	rs, ok := s.storage.(RefinementStore)
	if !ok {
		return nil, nil
	}
	return rs.ListRefinements(ctx, strings.TrimSpace(sessionID), limit)
}

// GetRefinement returns one refinement by id, or ErrNotFound.
func (s *Service) GetRefinement(ctx context.Context, sessionID, id string) (RefinementRecord, error) {
	if s == nil || s.storage == nil {
		return RefinementRecord{}, ErrNotFound
	}
	rs, ok := s.storage.(RefinementStore)
	if !ok {
		return RefinementRecord{}, ErrNotFound
	}
	return rs.GetRefinement(ctx, strings.TrimSpace(sessionID), strings.TrimSpace(id))
}
