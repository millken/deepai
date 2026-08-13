package memory

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// refineOnce runs one refinement and returns its record, failing the test if
// nothing was saved.
func refineOnce(t *testing.T, svc *Service, sessionID, factID, content string) RefinementRecord {
	t.Helper()

	record, saved, err := svc.RefineAndRecord(
		context.Background(), sessionID, refineMessages(),
		addFact(factID, content), RefineMeta{Rationale: "manual"},
	)
	if err != nil || !saved {
		t.Fatalf("RefineAndRecord() saved=%v err=%v", saved, err)
	}
	return record
}

func TestRollbackRefinementRestoresFactsAndReplacesTheRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record := refineOnce(t, svc, "s1", "f1", "refined")

	skipped, err := svc.RollbackRefinement(ctx, "s1", record.ID, "pair-1")
	if err != nil {
		t.Fatalf("RollbackRefinement() error = %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("nothing was touched externally, skipped = %v", skipped)
	}

	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(doc.Facts) != 1 || doc.Facts[0].Content != "original" {
		t.Fatalf("facts not restored: %+v", doc.Facts)
	}

	if _, err := store.GetRefinement(ctx, "s1", record.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the rolled-back record must be gone, err = %v", err)
	}
	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("want exactly the rollback record, got %d", len(records))
	}
	got := records[0]
	if got.PairID != "pair-1" {
		t.Fatalf("rollback record PairID = %q", got.PairID)
	}
	if got.Rationale != "Rollback of "+record.ID {
		t.Fatalf("rollback record Rationale = %q", got.Rationale)
	}
	if len(got.PostFactFingerprints) == 0 {
		t.Fatal("rollback record needs PostFactFingerprints, or it cannot itself be rolled back")
	}
}

func TestRollbackOfARollbackRestoresTheRefinedState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record := refineOnce(t, svc, "s1", "f1", "refined")
	if _, err := svc.RollbackRefinement(ctx, "s1", record.ID, "pair-1"); err != nil {
		t.Fatalf("first rollback error = %v", err)
	}

	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	rollbackRecord := records[0]

	// A mistaken rollback has to be undoable, which is the reason rollback is
	// itself recorded as a refinement.
	if _, err := svc.RollbackRefinement(ctx, "s1", rollbackRecord.ID, "pair-2"); err != nil {
		t.Fatalf("second rollback error = %v", err)
	}
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if contentByID(doc.Facts)["f1"] != "refined" {
		t.Fatalf("undoing the rollback did not restore the refined state: %+v", doc.Facts)
	}
}

func TestRollbackHandlesTheArchiveFactMergeCreatesOnRewrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	// Merge keeps an audit copy of a rewritten fact under "<id>#prev<nano>", so a
	// refine that rewrites one fact actually produces two. Rollback must treat
	// that archive as the refine's own output and drop it; restoring the refine
	// must bring it back.
	record := refineOnce(t, svc, "s1", "f1", "refined")

	afterRefine, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	archiveID := ""
	for _, f := range afterRefine.Facts {
		if f.ID != "f1" {
			archiveID = f.ID
		}
	}
	if archiveID == "" {
		t.Fatal("expected Merge to archive the rewritten fact; the rest of this test assumes it")
	}

	if _, err := svc.RollbackRefinement(ctx, "s1", record.ID, "pair-1"); err != nil {
		t.Fatalf("RollbackRefinement() error = %v", err)
	}
	afterRollback, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, present := contentByID(afterRollback.Facts)[archiveID]; present {
		t.Fatalf("the archive was produced by the refine and must be rolled back too: %+v", afterRollback.Facts)
	}
	if len(afterRollback.Facts) != 1 {
		t.Fatalf("want just the restored fact, got %+v", afterRollback.Facts)
	}
}

func TestRollbackRefinementLeavesThirdPartyEditsAloneAndReportsThem(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record := refineOnce(t, svc, "s1", "f1", "refined")

	// The agent edits the same fact through the memory builtin tool after the
	// refine. Rolling that back would silently revert the user's own change.
	edited, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	edited.Facts[0].Content = "hand edited"
	if err := svc.Save(ctx, edited); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	skipped, err := svc.RollbackRefinement(ctx, "s1", record.ID, "pair-1")
	if err != nil {
		t.Fatalf("RollbackRefinement() error = %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "f1" {
		t.Fatalf("skipped = %v, want [f1]", skipped)
	}
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if doc.Facts[0].Content != "hand edited" {
		t.Fatalf("third-party edit was clobbered: %q", doc.Facts[0].Content)
	}
}

func TestRollbackRefinementAppliesOnceUnderConcurrentCalls(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record := refineOnce(t, svc, "s1", "f1", "refined")

	// Read-modify-write plus the record delete are one critical section. Without
	// the session lock both callers would read the record and both would apply
	// the rollback; with it, the loser finds the record already gone.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.RollbackRefinement(ctx, "s1", record.ID, "pair-1")
		}(i)
	}
	wg.Wait()

	failures := 0
	for _, err := range errs {
		if err != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("want exactly one caller to fail, got %d failures: %v", failures, errs)
	}

	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("rollback applied more than once: %d records", len(records))
	}
}

func TestRollbackRefinementReportsAMissingRecord(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	seedDocument(t, store, "s1")

	_, err := svc.RollbackRefinement(context.Background(), "s1", "refine_nope", "pair-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRollbackRefinementFailsWhenBackendCannotStoreHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "mem"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	svc := NewService(quietLogger(), store, nil)
	t.Cleanup(func() { _ = svc.Close(ctx) })

	// Unlike extraction, rollback has no degraded mode: without history there is
	// nothing to roll back to, so this must fail loudly rather than fall back.
	if _, err := svc.RollbackRefinement(ctx, "s1", "refine_1", "pair-1"); err == nil {
		t.Fatal("want an error when the backend keeps no refinement history")
	}
}
