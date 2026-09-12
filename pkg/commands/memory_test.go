package commands

import (
	"context"
	"fmt"
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

// seedFacts saves one document per storage key with the given facts, exactly
// the way the async writers do — user-scope keys from memory.UserScope, bare
// session ids from preference extraction.
func seedFacts(t *testing.T, store *memory.SQLiteStore, docs map[string][]memory.Fact) {
	t.Helper()
	ctx := context.Background()
	for key, facts := range docs {
		if err := store.Save(ctx, memory.Document{SessionID: key, Facts: facts}); err != nil {
			t.Fatalf("Save(%q) error = %v", key, err)
		}
	}
}

func TestListFactsReportOnEmptyDatabase(t *testing.T) {
	t.Parallel()

	_, dbPath := gateStatsTestStore(t)

	report, err := listFactsReport(context.Background(), dbPath, "preference", 0.7, 100)
	if err != nil {
		t.Fatalf("listFactsReport() error = %v", err)
	}
	if !strings.Contains(report, "No memory facts recorded for this filter") {
		t.Fatalf("empty report must say so plainly, got:\n%s", report)
	}
	if !strings.Contains(report, "SQL used:") || !strings.Contains(report, memory.ListFactsQuerySQL) {
		t.Fatalf("empty report must print the SQL it used (gate-stats convention), got:\n%s", report)
	}
}

func TestListFactsReportFiltersAcrossScopes(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	userKey := memory.UserScope("/w").Key()
	seedFacts(t, store, map[string][]memory.Fact{
		userKey: {
			{ID: "pref-answers", Content: "用户偏好简短回复", Category: "preference", Confidence: 0.9},
			{ID: "pref-verbose", Content: "低置信度偏好", Category: "preference", Confidence: 0.6},
			{ID: "work-deploy", Content: "部署走 make build", Category: "work", Confidence: 0.8},
		},
		"sess-abc-12345": {
			{ID: "pref-cli", Content: "prefers CLI over GUI", Category: "preference", Confidence: 0.75},
		},
	})

	report, err := listFactsReport(context.Background(), dbPath, "preference", 0.7, 0)
	if err != nil {
		t.Fatalf("listFactsReport() error = %v", err)
	}
	for _, want := range []string{"pref-answers", "pref-cli", "0.90", "0.75"} {
		if !strings.Contains(report, want) {
			t.Fatalf("filtered report must contain %q, got:\n%s", want, report)
		}
	}
	for _, banned := range []string{"pref-verbose", "work-deploy", "0.60"} {
		if strings.Contains(report, banned) {
			t.Fatalf("filtered report must exclude %q (confidence/category filter), got:\n%s", banned, report)
		}
	}
	if !strings.Contains(report, "user") || !strings.Contains(report, "session:sess-abc") {
		t.Fatalf("scope column must distinguish user from session:<id8>, got:\n%s", report)
	}
	if strings.Index(report, "pref-answers") > strings.Index(report, "pref-cli") {
		t.Fatalf("facts must sort by confidence descending (0.90 before 0.75), got:\n%s", report)
	}

	unfiltered, err := listFactsReport(context.Background(), dbPath, "", 0, 0)
	if err != nil {
		t.Fatalf("listFactsReport() unfiltered error = %v", err)
	}
	if !strings.Contains(unfiltered, "work-deploy") {
		t.Fatalf("unfiltered report must contain the work fact, got:\n%s", unfiltered)
	}
}

func TestListFactsReportLimit(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	var facts []memory.Fact
	for i := 0; i < 5; i++ {
		facts = append(facts, memory.Fact{
			ID:         fmt.Sprintf("pref-%02d", i),
			Content:    fmt.Sprintf("fact number %d", i),
			Category:   "preference",
			Confidence: 0.5 + float64(i)*0.1,
		})
	}
	seedFacts(t, store, map[string][]memory.Fact{"sess-1": facts})

	report, err := listFactsReport(context.Background(), dbPath, "preference", 0, 2)
	if err != nil {
		t.Fatalf("listFactsReport() error = %v", err)
	}
	if !strings.Contains(report, "(2 of 5 facts shown") {
		t.Fatalf("limited report must state how many of how many, got:\n%s", report)
	}
	if strings.Contains(report, "pref-02") {
		t.Fatalf("limit=2 must exclude the third-highest-confidence fact, got:\n%s", report)
	}
}

func TestShortScopeLabels(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"sess-abcdef123456":            "session:sess-abc",
		memory.UserScope("/w").Key():   "user",
		memory.AgentScope("coder").Key(): "agent:coder",
		memory.GroupScope("team").Key(): "group:team",
		"":                             "session",
	}
	for key, want := range cases {
		if got := shortScope(key); got != want {
			t.Errorf("shortScope(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestConsolidateReportDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	seedFacts(t, store, map[string][]memory.Fact{
		"sess-1": {
			{ID: "pref-chinese-summaries", Content: "Prefers technical analysis summaries in Chinese", Category: "preference", Confidence: 0.95},
			{ID: "pref-solo", Content: "Prefers table-driven Go tests", Category: "preference", Confidence: 0.9},
		},
		"sess-2": {
			{ID: "pref-chinese-communication", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 1.0},
		},
	})
	before := countFacts(t, store)

	report, err := consolidateReport(context.Background(), dbPath, "preference", 0.55, "/w", false)
	if err != nil {
		t.Fatalf("consolidateReport() dry-run error = %v", err)
	}
	if !strings.Contains(report, "dry run") || !strings.Contains(report, "--apply") {
		t.Fatalf("dry-run report must say it did not write and how to apply, got:\n%s", report)
	}
	if !strings.Contains(report, "drop") {
		t.Fatalf("dry-run report must list the drops, got:\n%s", report)
	}
	if after := countFacts(t, store); after != before {
		t.Fatalf("dry-run must not change the store: %d facts before, %d after", before, after)
	}
}

func TestConsolidateReportApplyCollapsesAndKeepsUnrelated(t *testing.T) {
	t.Parallel()

	store, dbPath := gateStatsTestStore(t)
	seedFacts(t, store, map[string][]memory.Fact{
		"sess-1": {
			{ID: "pref-chinese-summaries", Content: "Prefers technical analysis summaries in Chinese", Category: "preference", Confidence: 0.95},
			{ID: "pref-solo", Content: "Prefers table-driven Go tests", Category: "preference", Confidence: 0.9},
		},
		"sess-2": {
			{ID: "pref-chinese-communication", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 1.0},
		},
	})

	report, err := consolidateReport(context.Background(), dbPath, "preference", 0.55, "/w", true)
	if err != nil {
		t.Fatalf("consolidateReport() apply error = %v", err)
	}
	if !strings.Contains(report, "Consolidation applied") {
		t.Fatalf("applied report must say so, got:\n%s", report)
	}

	facts, err := store.ListAllFacts(context.Background())
	if err != nil {
		t.Fatalf("ListAllFacts() after apply: %v", err)
	}
	ids := map[string]bool{}
	for _, f := range facts {
		ids[f.ID] = true
	}
	if ids["pref-chinese-summaries"] {
		t.Fatalf("near-duplicate must be dropped, remaining: %v", ids)
	}
	if !ids["pref-chinese-communication"] || !ids["pref-solo"] {
		t.Fatalf("survivor and unrelated fact must remain, got: %v", ids)
	}
	if len(facts) != 2 {
		t.Fatalf("3 seeded facts must collapse to 2, got %d: %+v", len(facts), facts)
	}
}

func countFacts(t *testing.T, store *memory.SQLiteStore) int {
	t.Helper()
	facts, err := store.ListAllFacts(context.Background())
	if err != nil {
		t.Fatalf("ListAllFacts(): %v", err)
	}
	return len(facts)
}
