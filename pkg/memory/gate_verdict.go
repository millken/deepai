package memory

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"
)

// GateOutcome values for GateVerdictRecord.Outcome. Kept as plain strings
// (not a Go type) because they round-trip through a TEXT column and through
// the CLI report verbatim.
const (
	GateOutcomeApprove = "approve"
	GateOutcomeReject  = "reject"
	// GateOutcomeError marks a gate call that could not decide (ReviewRefine
	// returned an error). It is NOT a third kind of "approve" or "reject": the
	// gate fails open and extraction still runs, but the decision itself must
	// never be read as evidence the gate is passing everything, so it is
	// counted separately and excluded from the rejection-rate denominator.
	GateOutcomeError = "error"
)

// GateVerdictRecord is one recorded auto-refine gate decision. It exists to
// answer, from real usage rather than the assumption in
// docs/REFINE_DESIGN.md §7.2, whether the gate's rejection rate clears the
// 60-80% break-even band.
//
// One row per gate CALL, not per extraction it triggers: a single verdict is
// shared with the paired user-scope job (see gateVerdict in refine.go), and
// recording it twice would double-count every paired decision.
type GateVerdictRecord struct {
	ID        string
	ScopeKey  string // job.sessionID: a session ID or a Scope.Key()
	DecidedAt time.Time
	Outcome   string // GateOutcomeApprove / GateOutcomeReject / GateOutcomeError
	Rationale string // the gate's rationale, or the error message for GateOutcomeError

	GateMS int64 // wall-clock time of the ReviewRefine call itself

	// ExtractMS/Saved describe the extraction THIS gate call authorized
	// (approve, or the fail-open path on error), backfilled after the fact —
	// runRefineGateJob does not know them until RefineAndRecord returns. Both
	// are nil for a GateOutcomeReject row: nothing was ever extracted for it,
	// and nil must not be confused with a measured zero.
	ExtractMS *int64
	Saved     *bool

	// Paired reports whether this gate call also decided the paired
	// user-scope job's extraction (ScheduleRefine queued one). It describes
	// the gate call, not whether that second extraction actually ran (the
	// paired job can still be deduped away or find the verdict superseded).
	Paired bool
}

// GateVerdictStore is the optional refine-gate audit-log capability. Only
// SQLiteStore implements it; other backends (and test fakes) do not, and the
// gate simply runs unrecorded — the same optional-capability pattern as
// RefinementStore (see refine.go), and for the same reason: widening Storage
// itself would break every implementation and stub in this package.
//
// A record store failure must never change gate behavior: callers log and
// carry on. The gate's fail-open contract does not bend for bookkeeping.
type GateVerdictStore interface {
	// InsertGateVerdict records one gate decision and returns nothing to
	// correlate later inserts with reads by design — callers hold the ID they
	// generated (see gateVerdictID) and pass it back to RecordGateExtraction.
	InsertGateVerdict(ctx context.Context, record GateVerdictRecord) error
	// RecordGateExtraction backfills ExtractMS/Saved on an existing row. It is
	// a no-op, not an error, if id is empty or unknown — the gate call that
	// produced it may predate this feature, or its own insert may have failed.
	RecordGateExtraction(ctx context.Context, id string, extractMS int64, saved bool) error
	// ListGateVerdicts returns every recorded verdict, oldest first. There is
	// no per-scope filter and no limit: `deepai memory gate-stats` is a
	// whole-database report, and the retention story is "don't bother
	// trimming" (see GateStatsQuerySQL and the comment on
	// SQLiteStore.InsertGateVerdict) rather than a bounded window.
	ListGateVerdicts(ctx context.Context) ([]GateVerdictRecord, error)
}

// HasGateVerdictStore reports whether storage can record gate verdicts.
func HasGateVerdictStore(s Storage) bool {
	_, ok := s.(GateVerdictStore)
	return ok
}

// gateVerdictID returns a unique, time-ordered gate-verdict ID. Reuses the
// same monotonic counter as refineID so IDs from both never collide even
// though they are stored in different tables.
func gateVerdictID() string {
	return "gatev_" + strconv.FormatInt(nextMonotonicNano(), 10)
}

// recordGateVerdict inserts one gate decision and returns its ID for a later
// RecordGateExtraction backfill, or "" if storage cannot record verdicts or
// the insert failed. Never returns an error: a bookkeeping failure must not
// change what the caller (the gate) does next.
func (s *Service) recordGateVerdict(ctx context.Context, job updateJob, outcome, rationale string, gateMS time.Duration) string {
	gs, ok := s.storage.(GateVerdictStore)
	if !ok {
		return ""
	}
	record := GateVerdictRecord{
		ID:        gateVerdictID(),
		ScopeKey:  job.sessionID,
		DecidedAt: time.Now().UTC(),
		Outcome:   outcome,
		Rationale: rationale,
		GateMS:    gateMS.Milliseconds(),
		Paired:    job.pairQueued && job.pairID != "",
	}
	// jobRefine shares one timeout ctx between the gate call this verdict
	// describes and the extraction it authorizes (see queue.go). When the
	// gate itself times out, that ctx is already expired by the time we get
	// here — using it as-is would fail this insert for exactly the gate-error
	// rows the audit log most needs to capture (GateOutcomeError), silently
	// erasing them from every stat derived from ListGateVerdicts. Detach from
	// the deadline/cancellation (not from values) so the write always lands.
	if err := gs.InsertGateVerdict(context.WithoutCancel(ctx), record); err != nil {
		s.logger.Warn("failed to record refine gate verdict", "session", job.sessionID, "err", err)
		return ""
	}
	return record.ID
}

// recordGateExtraction backfills the extraction outcome onto a previously
// recorded verdict row. verdictID=="" is the normal case for a rejected
// verdict (nothing was extracted) and for the paired user-scope job (its
// extraction is a second one the gate did not directly own a row for; see the
// comment on GateVerdictRecord) — both are silent no-ops, not errors.
func (s *Service) recordGateExtraction(ctx context.Context, verdictID string, extractMS time.Duration, saved bool) {
	if verdictID == "" {
		return
	}
	gs, ok := s.storage.(GateVerdictStore)
	if !ok {
		return
	}
	// Same reasoning as recordGateVerdict above: this backfill runs after the
	// extraction it describes, on the same shared job ctx, so a slow
	// extraction that finishes right at (or past) the deadline would have its
	// timing and outcome discarded right when they are least representative
	// — leaving ExtractMsP50/P90 sampled only from the extractions that were
	// comfortably fast. Detach from the deadline/cancellation before writing.
	if err := gs.RecordGateExtraction(context.WithoutCancel(ctx), verdictID, extractMS.Milliseconds(), saved); err != nil {
		s.logger.Warn("failed to record refine gate extraction outcome", "id", verdictID, "err", err)
	}
}

// GateStats summarizes recorded gate verdicts for `deepai memory gate-stats`.
// It is a pure function of the records (see ComputeGateStats) so the report
// logic is testable without a database.
type GateStats struct {
	Total    int
	Approved int
	Rejected int
	Errored  int

	OldestAt time.Time
	NewestAt time.Time

	// GateMsP50/P90 are computed over every recorded verdict (approve, reject
	// and error all pay for the gate call itself).
	GateMsP50 float64
	GateMsP90 float64

	// ExtractMsP50/P90 are computed over every row with a backfilled
	// ExtractMS — approve and fail-open error rows, never reject (nothing was
	// extracted for those).
	ExtractMsP50      float64
	ExtractMsP90      float64
	ExtractSampleSize int

	// ApprovedWithSample/NoOpApproved measure "approved, but the extraction it
	// authorized changed nothing" among GENUINE approvals only — a fail-open
	// error row extracting nothing is a gate failure, not evidence about
	// whether the gate's approvals are worth their cost, so it is excluded
	// here (unlike ExtractMsP50/P90 above, which is a raw timing question).
	ApprovedWithSample int
	NoOpApproved       int
}

// RejectionRate is Rejected / (Approved+Rejected). Errors are excluded from
// the denominator by design (see GateOutcomeError): a gate that is failing
// open must not be misread as a gate that is passing everything. ok is false
// when there is nothing to divide by.
func (g GateStats) RejectionRate() (rate float64, ok bool) {
	denom := g.Approved + g.Rejected
	if denom == 0 {
		return 0, false
	}
	return float64(g.Rejected) / float64(denom), true
}

// NoOpRate is the fraction of genuine approvals whose extraction saved
// nothing. ok is false until at least one approval has a backfilled outcome.
func (g GateStats) NoOpRate() (rate float64, ok bool) {
	if g.ApprovedWithSample == 0 {
		return 0, false
	}
	return float64(g.NoOpApproved) / float64(g.ApprovedWithSample), true
}

// GateToExtractRatio is the measured analogue of the "C(gate) ≈ 0.6-0.8 ·
// C(extract)" assumption in docs/REFINE_DESIGN.md §7.2, using median latency
// as the cost proxy (the LLM relay does not return token usage — see
// HANDOFF.md §7 — so wall-clock time is the only signal available). ok is
// false until at least one extraction has been timed.
func (g GateStats) GateToExtractRatio() (ratio float64, ok bool) {
	if g.ExtractMsP50 <= 0 {
		return 0, false
	}
	return g.GateMsP50 / g.ExtractMsP50, true
}

// ComputeGateStats reduces raw verdict rows into GateStats. It has no
// database dependency so it can be tested directly against hand-built
// records for the empty/small-n/full-data shapes the CLI report must handle.
func ComputeGateStats(records []GateVerdictRecord) GateStats {
	var stats GateStats
	gateMs := make([]int64, 0, len(records))
	extractMs := make([]int64, 0, len(records))

	for _, r := range records {
		stats.Total++
		switch r.Outcome {
		case GateOutcomeApprove:
			stats.Approved++
		case GateOutcomeReject:
			stats.Rejected++
		case GateOutcomeError:
			stats.Errored++
		}
		if stats.OldestAt.IsZero() || r.DecidedAt.Before(stats.OldestAt) {
			stats.OldestAt = r.DecidedAt
		}
		if r.DecidedAt.After(stats.NewestAt) {
			stats.NewestAt = r.DecidedAt
		}
		gateMs = append(gateMs, r.GateMS)
		if r.ExtractMS != nil {
			extractMs = append(extractMs, *r.ExtractMS)
			if r.Outcome == GateOutcomeApprove && r.Saved != nil {
				stats.ApprovedWithSample++
				if !*r.Saved {
					stats.NoOpApproved++
				}
			}
		}
	}
	stats.ExtractSampleSize = len(extractMs)

	sort.Slice(gateMs, func(i, j int) bool { return gateMs[i] < gateMs[j] })
	sort.Slice(extractMs, func(i, j int) bool { return extractMs[i] < extractMs[j] })
	stats.GateMsP50 = percentileInt64(gateMs, 50)
	stats.GateMsP90 = percentileInt64(gateMs, 90)
	stats.ExtractMsP50 = percentileInt64(extractMs, 50)
	stats.ExtractMsP90 = percentileInt64(extractMs, 90)
	return stats
}

// percentileInt64 returns the p-th percentile of an already-sorted slice
// using the nearest-rank method. Returns 0 for an empty slice.
func percentileInt64(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx])
}
