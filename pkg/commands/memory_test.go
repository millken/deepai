package commands

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/memory"
)

// gateStatsTestStore opens a fresh, migrated memory store at a temp path —
// never the user's real ~/.deepai/deepai.db.
func gateStatsTestStore(t *testing.T) (*memory.SQLiteStore, string) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memory.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	return store, dbPath
}

func TestGateStatsReportOnEmptyDatabase(t *testing.T) {
	t.Parallel()

	_, dbPath := gateStatsTestStore(t)

	report, err := gateStatsReport(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("gateStatsReport() error = %v", err)
	}
	if !strings.Contains(report, "No gate verdicts recorded yet") {
		t.Fatalf("empty report must say so plainly, got:\n%s", report)
	}
	if !strings.Contains(report, "SQL used:") || !strings.Contains(report, memory.GateStatsQuerySQL) {
		t.Fatalf("report must print the SQL it used even when empty, got:\n%s", report)
	}
	// Must not fabricate a rejection rate or any percentage from zero rows.
	if strings.Contains(report, "%") {
		t.Fatalf("empty report must not print any percentage, got:\n%s", report)
	}
}

func TestGateStatsReportOnANewDatabaseWithNoMemoryTablesAtAll(t *testing.T) {
	t.Parallel()

	// A completely fresh path: AutoMigrate must create every table gate-stats
	// needs, including memory_gate_verdicts, from nothing.
	dbPath := filepath.Join(t.TempDir(), "brand-new.db")
	report, err := gateStatsReport(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("gateStatsReport() on a brand-new path must not error, got %v", err)
	}
	if !strings.Contains(report, "No gate verdicts recorded yet") {
		t.Fatalf("want the empty-report message, got:\n%s", report)
	}
}

func TestGateStatsReportWarnsOnASmallSample(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-24 * time.Hour)

	// Well under minGateSampleForConfidence: this must not be presented as a
	// confident verdict, mirroring the n=9 lesson in HANDOFF.md §5.4.
	for i := 0; i < 8; i++ {
		outcome := memory.GateOutcomeApprove
		if i%2 == 0 {
			outcome = memory.GateOutcomeReject
		}
		if err := store.InsertGateVerdict(ctx, memory.GateVerdictRecord{
			ID:        "gatev_" + strconv.Itoa(i),
			ScopeKey:  "s1",
			DecidedAt: base.Add(time.Duration(i) * time.Minute),
			Outcome:   outcome,
			GateMS:    100,
		}); err != nil {
			t.Fatalf("InsertGateVerdict(%d) error = %v", i, err)
		}
	}

	report, err := gateStatsReport(ctx, dbPath)
	if err != nil {
		t.Fatalf("gateStatsReport() error = %v", err)
	}
	if !strings.Contains(report, "CAUTION") {
		t.Fatalf("a sample of 8 must trigger the small-sample caution, got:\n%s", report)
	}
	if !strings.Contains(report, "n = 8 verdicts") {
		t.Fatalf("report must state the sample size, got:\n%s", report)
	}
}

func TestGateStatsReportOnFullDataIsAccurateAndUnambiguous(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)

	// 40 approvals, 60 rejections, 5 errors: rejection rate = 60/100 = 60%,
	// exactly the low edge of the break-even band, and well above the
	// small-sample threshold.
	id := 0
	insert := func(outcome string, extractMS *int64, saved *bool) {
		rec := memory.GateVerdictRecord{
			ID:        "gatev_" + strconv.Itoa(id),
			ScopeKey:  "s1",
			DecidedAt: base.Add(time.Duration(id) * time.Minute),
			Outcome:   outcome,
			GateMS:    int64(80 + id%20),
		}
		id++
		if err := store.InsertGateVerdict(ctx, rec); err != nil {
			t.Fatalf("InsertGateVerdict(%s) error = %v", rec.ID, err)
		}
		if extractMS != nil {
			if err := store.RecordGateExtraction(ctx, rec.ID, *extractMS, *saved); err != nil {
				t.Fatalf("RecordGateExtraction(%s) error = %v", rec.ID, err)
			}
		}
	}

	extractSample := int64(300)
	savedTrue, savedFalse := true, false
	for i := 0; i < 40; i++ {
		saved := &savedTrue
		if i < 10 {
			// A quarter of approvals extracted nothing.
			saved = &savedFalse
		}
		insert(memory.GateOutcomeApprove, &extractSample, saved)
	}
	for i := 0; i < 60; i++ {
		insert(memory.GateOutcomeReject, nil, nil)
	}
	for i := 0; i < 5; i++ {
		insert(memory.GateOutcomeError, &extractSample, &savedTrue)
	}

	report, err := gateStatsReport(ctx, dbPath)
	if err != nil {
		t.Fatalf("gateStatsReport() error = %v", err)
	}
	if strings.Contains(report, "CAUTION") {
		t.Fatalf("105 approve/reject verdicts must not trigger the small-sample caution, got:\n%s", report)
	}
	if !strings.Contains(report, "n = 105 verdicts") {
		t.Fatalf("want the total sample size (approve+reject+error), got:\n%s", report)
	}
	if !strings.Contains(report, "60.0% (60 of 100)") {
		t.Fatalf("want the exact rejection rate excluding the 5 errors from the denominator, got:\n%s", report)
	}
	if !strings.Contains(report, "INSIDE the 60-80% break-even band") {
		t.Fatalf("60%% is the band's own low edge, want it read as inside, got:\n%s", report)
	}
	if !strings.Contains(report, "25.0% (10 of 40)") {
		t.Fatalf("want the no-op rate computed over genuine approvals only (10 of 40), got:\n%s", report)
	}
}
