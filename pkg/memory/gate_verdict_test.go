package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// --- SQLiteStore round trip ---------------------------------------------------

func TestSQLiteGateVerdictRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)

	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	record := GateVerdictRecord{
		ID:        "gatev_1",
		ScopeKey:  "s1",
		DecidedAt: at,
		Outcome:   GateOutcomeApprove,
		Rationale: "captured a stable preference",
		GateMS:    120,
		Paired:    true,
	}
	if err := store.InsertGateVerdict(ctx, record); err != nil {
		t.Fatalf("InsertGateVerdict() error = %v", err)
	}

	got, err := store.ListGateVerdicts(ctx)
	if err != nil {
		t.Fatalf("ListGateVerdicts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListGateVerdicts() = %d rows, want 1", len(got))
	}
	row := got[0]
	if row.ID != "gatev_1" || row.ScopeKey != "s1" || row.Outcome != GateOutcomeApprove ||
		row.Rationale != record.Rationale || row.GateMS != 120 || !row.Paired {
		t.Fatalf("round-tripped record = %+v", row)
	}
	if row.ExtractMS != nil || row.Saved != nil {
		t.Fatalf("freshly inserted row must have NULL extract_ms/saved, got %+v", row)
	}
}

func TestSQLiteGateVerdictInsertRequiresNoMemoriesRow(t *testing.T) {
	t.Parallel()

	// Unlike memory_refinements, this table has no FK to memories(session_id):
	// the very first gate call for a scope runs before any document exists.
	ctx := context.Background()
	store := newRefinementTestStore(t)

	err := store.InsertGateVerdict(ctx, GateVerdictRecord{
		ID:        "gatev_1",
		ScopeKey:  "never-saved",
		DecidedAt: time.Now().UTC(),
		Outcome:   GateOutcomeReject,
	})
	if err != nil {
		t.Fatalf("InsertGateVerdict() must not require a memories row, got error = %v", err)
	}
}

func TestSQLiteRecordGateExtractionBackfills(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	if err := store.InsertGateVerdict(ctx, GateVerdictRecord{
		ID: "gatev_1", ScopeKey: "s1", DecidedAt: time.Now().UTC(), Outcome: GateOutcomeApprove,
	}); err != nil {
		t.Fatalf("InsertGateVerdict() error = %v", err)
	}

	if err := store.RecordGateExtraction(ctx, "gatev_1", 4321, true); err != nil {
		t.Fatalf("RecordGateExtraction() error = %v", err)
	}

	got, err := store.ListGateVerdicts(ctx)
	if err != nil {
		t.Fatalf("ListGateVerdicts() error = %v", err)
	}
	if len(got) != 1 || got[0].ExtractMS == nil || *got[0].ExtractMS != 4321 || got[0].Saved == nil || !*got[0].Saved {
		t.Fatalf("backfill not applied: %+v", got)
	}
}

func TestSQLiteRecordGateExtractionOnUnknownIDIsANoOp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	if err := store.RecordGateExtraction(ctx, "does-not-exist", 100, true); err != nil {
		t.Fatalf("RecordGateExtraction() on unknown id must not error, got %v", err)
	}
}

// --- Old-database migration ---------------------------------------------------

// TestAutoMigrateAddsGateVerdictsTableToAnExistingDatabase simulates a
// database created before this feature existed: memories/memory_facts/
// memory_refinements exist, memory_gate_verdicts does not. AutoMigrate must
// add it without disturbing what is already there.
func TestAutoMigrateAddsGateVerdictsTableToAnExistingDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	// Pre-feature schema, deliberately hand-written rather than reusing
	// AutoMigrate, so this test still simulates an old database after
	// AutoMigrate itself changes in the future.
	for _, ddl := range []string{
		`CREATE TABLE memories (
			session_id TEXT PRIMARY KEY,
			user_memory TEXT NOT NULL DEFAULT '{}',
			history_memory TEXT NOT NULL DEFAULT '{}',
			source TEXT NOT NULL DEFAULT '',
			updated_at REAL NOT NULL
		)`,
		`CREATE TABLE memory_facts (
			session_id TEXT NOT NULL REFERENCES memories(session_id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			content TEXT NOT NULL,
			category TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT '',
			retrieval_count INTEGER NOT NULL DEFAULT 0,
			helpful_count INTEGER NOT NULL DEFAULT 0,
			suspect_count INTEGER NOT NULL DEFAULT 0,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			PRIMARY KEY (session_id, id)
		)`,
	} {
		if _, err := raw.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("seed old schema: %v", err)
		}
	}
	if _, err := raw.ExecContext(ctx, `insert into memories (session_id, updated_at) values ('s1', 1000)`); err != nil {
		t.Fatalf("seed memories row: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `insert into memory_facts (session_id, id, content, created_at, updated_at) values ('s1', 'f1', 'pre-existing fact', 1000, 1000)`); err != nil {
		t.Fatalf("seed memory_facts row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() on an old database must succeed, got error = %v", err)
	}

	// The new table works.
	if err := store.InsertGateVerdict(ctx, GateVerdictRecord{
		ID: "gatev_1", ScopeKey: "s1", DecidedAt: time.Now().UTC(), Outcome: GateOutcomeApprove,
	}); err != nil {
		t.Fatalf("new table not usable after migrating an old db: %v", err)
	}

	// Pre-existing data survived untouched.
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(doc.Facts) != 1 || doc.Facts[0].Content != "pre-existing fact" {
		t.Fatalf("pre-existing data disturbed by migration: %+v", doc.Facts)
	}
}

// --- GateStats (pure) ----------------------------------------------------------

func TestComputeGateStatsOnEmptyInput(t *testing.T) {
	t.Parallel()

	stats := ComputeGateStats(nil)
	if stats.Total != 0 {
		t.Fatalf("Total = %d, want 0", stats.Total)
	}
	if _, ok := stats.RejectionRate(); ok {
		t.Fatal("RejectionRate() must report ok=false with no data")
	}
	if _, ok := stats.NoOpRate(); ok {
		t.Fatal("NoOpRate() must report ok=false with no data")
	}
	if _, ok := stats.GateToExtractRatio(); ok {
		t.Fatal("GateToExtractRatio() must report ok=false with no data")
	}
}

func ptr[T any](v T) *T { return &v }

func TestComputeGateStatsCountsAndExcludesErrorsFromRejectionRate(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	records := []GateVerdictRecord{
		{Outcome: GateOutcomeApprove, GateMS: 100, DecidedAt: base},
		{Outcome: GateOutcomeApprove, GateMS: 100, DecidedAt: base.Add(time.Hour)},
		{Outcome: GateOutcomeReject, GateMS: 100, DecidedAt: base.Add(2 * time.Hour)},
		{Outcome: GateOutcomeReject, GateMS: 100, DecidedAt: base.Add(3 * time.Hour)},
		{Outcome: GateOutcomeReject, GateMS: 100, DecidedAt: base.Add(4 * time.Hour)},
		{Outcome: GateOutcomeError, GateMS: 100, DecidedAt: base.Add(5 * time.Hour)},
	}
	stats := ComputeGateStats(records)
	if stats.Total != 6 || stats.Approved != 2 || stats.Rejected != 3 || stats.Errored != 1 {
		t.Fatalf("counts = %+v", stats)
	}
	rate, ok := stats.RejectionRate()
	if !ok {
		t.Fatal("RejectionRate() must be computable")
	}
	// 3 reject / (2 approve + 3 reject) = 0.6, NOT divided by 6.
	if got, want := rate, 0.6; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("RejectionRate() = %v, want %v (denominator must exclude the error row)", got, want)
	}
	if !stats.NewestAt.Equal(base.Add(5*time.Hour)) || !stats.OldestAt.Equal(base) {
		t.Fatalf("time span = %v..%v", stats.OldestAt, stats.NewestAt)
	}
}

func TestComputeGateStatsNoOpRateOnlyCountsGenuineApprovals(t *testing.T) {
	t.Parallel()

	records := []GateVerdictRecord{
		// Genuine approval that saved something.
		{Outcome: GateOutcomeApprove, ExtractMS: ptr(int64(50)), Saved: ptr(true)},
		// Genuine approval that extracted nothing.
		{Outcome: GateOutcomeApprove, ExtractMS: ptr(int64(60)), Saved: ptr(false)},
		// Fail-open error row that also extracted nothing: must NOT count toward
		// NoOpRate, which is specifically about genuine approvals.
		{Outcome: GateOutcomeError, ExtractMS: ptr(int64(70)), Saved: ptr(false)},
		// Approval with no backfill yet: must not be counted either way.
		{Outcome: GateOutcomeApprove},
	}
	stats := ComputeGateStats(records)
	if stats.ApprovedWithSample != 2 {
		t.Fatalf("ApprovedWithSample = %d, want 2 (excluding the error row and the un-backfilled approval)", stats.ApprovedWithSample)
	}
	rate, ok := stats.NoOpRate()
	if !ok || rate != 0.5 {
		t.Fatalf("NoOpRate() = %v, %v, want 0.5, true", rate, ok)
	}
	if stats.ExtractSampleSize != 3 {
		t.Fatalf("ExtractSampleSize = %d, want 3 (timing includes the error row)", stats.ExtractSampleSize)
	}
}

func TestComputeGateStatsPercentiles(t *testing.T) {
	t.Parallel()

	var records []GateVerdictRecord
	for _, ms := range []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100} {
		records = append(records, GateVerdictRecord{Outcome: GateOutcomeApprove, GateMS: ms})
	}
	stats := ComputeGateStats(records)
	if stats.GateMsP50 != 50 {
		t.Fatalf("GateMsP50 = %v, want 50", stats.GateMsP50)
	}
	if stats.GateMsP90 != 90 {
		t.Fatalf("GateMsP90 = %v, want 90", stats.GateMsP90)
	}
}

// --- Service/queue integration (acceptance points 1-6) ------------------------

// gateVerdicts is a test helper: list gate verdicts for a scope-agnostic
// report, sorted oldest first (matches ListGateVerdicts).
func gateVerdicts(t *testing.T, store *SQLiteStore) []GateVerdictRecord {
	t.Helper()
	got, err := store.ListGateVerdicts(context.Background())
	if err != nil {
		t.Fatalf("ListGateVerdicts() error = %v", err)
	}
	return got
}

// Acceptance point 1: reviewer == nil must not record any row (the source of
// denominator inflation the task calls out explicitly).
func TestRunRefineGateJobWithoutAReviewerRecordsNothing(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	// No reviewer configured.

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := gateVerdicts(t, store); len(got) != 0 {
		t.Fatalf("gate verdicts recorded without a gate ever running: %+v", got)
	}
}

// Acceptance point 2: approve and reject each record exactly one row with the
// right outcome and rationale.
func TestRunRefineGateJobRecordsApprove(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want 1", len(got))
	}
	if got[0].Outcome != GateOutcomeApprove || got[0].Rationale != "worth keeping" || got[0].ScopeKey != "s1" {
		t.Fatalf("recorded verdict = %+v", got[0])
	}
	if got[0].GateMS < 0 {
		t.Fatalf("GateMS = %d, want >= 0", got[0].GateMS)
	}
}

func TestRunRefineGateJobRecordsReject(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(false, "transient chatter"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want 1", len(got))
	}
	if got[0].Outcome != GateOutcomeReject || got[0].Rationale != "transient chatter" {
		t.Fatalf("recorded verdict = %+v", got[0])
	}
}

// Acceptance point 3: a gate error is its own outcome, and it is excluded
// from the rejection-rate denominator (already unit-tested against
// ComputeGateStats directly above; here we check the recording half).
func TestRunRefineGateJobRecordsErrorOutcome(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	gateErr := errors.New("upstream 503")
	svc.WithReviewer(reviewerFunc(func(context.Context, Document, []models.Message) (RefineReview, error) {
		return RefineReview{}, gateErr
	}))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want 1", len(got))
	}
	if got[0].Outcome != GateOutcomeError || got[0].Rationale != gateErr.Error() {
		t.Fatalf("recorded verdict = %+v", got[0])
	}
}

// Acceptance point 4: one gate call driving both scopes writes exactly one
// row, marked paired.
func TestScheduleRefinePairedGateWritesOneRowMarkedPaired(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want exactly 1 despite driving two scopes: %+v", len(got), got)
	}
	if !got[0].Paired {
		t.Fatal("want Paired=true when a user-scope job shares this verdict")
	}
}

func TestScheduleRefineWithoutAUserScopeIsNotMarkedPaired(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 || got[0].Paired {
		t.Fatalf("want one unpaired row, got %+v", got)
	}
}

// Acceptance point 5: extract_ms/saved are backfilled for an approval, and
// left NULL for a rejection.
func TestRunRefineGateJobBackfillsExtractMsAndSavedOnApprove(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want 1", len(got))
	}
	if got[0].ExtractMS == nil {
		t.Fatal("ExtractMS must be backfilled after an approved extraction")
	}
	if got[0].Saved == nil || !*got[0].Saved {
		t.Fatalf("Saved = %v, want true (the extractor added a fact)", got[0].Saved)
	}
}

func TestRunRefineGateJobLeavesExtractColumnsNullOnReject(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(false, "transient chatter"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	got := gateVerdicts(t, store)
	if len(got) != 1 {
		t.Fatalf("gate verdicts = %d, want 1", len(got))
	}
	if got[0].ExtractMS != nil || got[0].Saved != nil {
		t.Fatalf("rejected verdict must leave extract_ms/saved NULL, got %+v", got[0])
	}
}

// Acceptance point 6: a storage failure while recording the verdict must not
// change gate behavior — extraction still runs (fail-open contract intact).
type failingGateStore struct {
	*SQLiteStore
}

func (f *failingGateStore) InsertGateVerdict(context.Context, GateVerdictRecord) error {
	return errors.New("disk full")
}

func TestGateVerdictRecordingFailureDoesNotBreakFailOpenBehavior(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inner := newRefinementTestStore(t)
	store := &failingGateStore{SQLiteStore: inner}
	svc := NewService(quietLogger(), store, nil)
	t.Cleanup(func() { _ = svc.Close(ctx) })
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	// Extraction must have proceeded normally...
	if got := factCount(t, inner, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1 (extraction must not be affected by a logging failure)", got)
	}
	// ...even though nothing could be recorded.
	if got := gateVerdicts(t, inner); len(got) != 0 {
		t.Fatalf("want no rows recorded when InsertGateVerdict fails, got %+v", got)
	}
}
