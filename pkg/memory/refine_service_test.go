package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// extractorFunc adapts a function to Extractor.
type extractorFunc func(ctx context.Context, current Document, messages []models.Message) (Update, error)

func (f extractorFunc) ExtractUpdate(ctx context.Context, current Document, messages []models.Message) (Update, error) {
	return f(ctx, current, messages)
}

// addFact returns an extractor that always proposes one fact.
func addFact(id, content string) extractorFunc {
	return func(_ context.Context, _ Document, _ []models.Message) (Update, error) {
		return Update{Facts: []Fact{{ID: id, Content: content, Category: "style", Confidence: 0.9}}}, nil
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newRefineService(t *testing.T) (*Service, *SQLiteStore) {
	t.Helper()

	store := newRefinementTestStore(t)
	svc := NewService(quietLogger(), store, nil)
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return svc, store
}

func refineMessages() []models.Message {
	return []models.Message{
		{Role: models.RoleHuman, Content: "always run gofmt before committing"},
		{Role: models.RoleAI, Content: "understood"},
	}
}

func TestRefineAndRecordSavesFactsAndRecordsThem(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)

	record, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "uses gofmt"), RefineMeta{Rationale: "manual"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if !saved {
		t.Fatal("want saved=true")
	}

	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(doc.Facts) != 1 || doc.Facts[0].Content != "uses gofmt" {
		t.Fatalf("facts not saved: %+v", doc.Facts)
	}

	stored, err := store.GetRefinement(ctx, "s1", record.ID)
	if err != nil {
		t.Fatalf("record not persisted: %v", err)
	}
	if stored.Rationale != "manual" || stored.SessionID != "s1" {
		t.Fatalf("record metadata = %+v", stored)
	}
	if len(stored.FactIDsChanged) != 1 || stored.FactIDsChanged[0] != "f1" {
		t.Fatalf("FactIDsChanged = %v", stored.FactIDsChanged)
	}
	if len(stored.PreSnapshot) != 0 {
		t.Fatalf("PreSnapshot must be the state BEFORE the refine (empty here), got %+v", stored.PreSnapshot)
	}
	if stored.PostFactFingerprints["f1"] != factFingerprint(doc.Facts[0]) {
		t.Fatalf("PostFactFingerprints must describe the saved state, got %v", stored.PostFactFingerprints)
	}
}

func TestRefineAndRecordCarriesThePairIDIntoTheRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)

	// One refine writes a record in the session partition and another in the user
	// scope partition. "/refine undo" finds the second from the first by PairID,
	// so it must survive into storage.
	meta := RefineMeta{PairID: "pair-42", Rationale: "gate approved"}
	record, _, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "uses gofmt"), meta)
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if record.PairID != "pair-42" {
		t.Fatalf("returned PairID = %q", record.PairID)
	}
	stored, err := store.GetRefinement(ctx, "s1", record.ID)
	if err != nil {
		t.Fatalf("GetRefinement() error = %v", err)
	}
	if stored.PairID != "pair-42" {
		t.Fatalf("stored PairID = %q", stored.PairID)
	}
}

func TestRefineAndRecordSnapshotsTheStateBeforeTheRefine(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "refined"), RefineMeta{Rationale: "manual"})
	if err != nil || !saved {
		t.Fatalf("RefineAndRecord() saved=%v err=%v", saved, err)
	}
	if len(record.PreSnapshot) != 1 || record.PreSnapshot[0].Content != "original" {
		t.Fatalf("PreSnapshot = %+v, want the pre-refine content", record.PreSnapshot)
	}
}

func TestRefineAndRecordProducesARecordRollbackCanUse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	record, _, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "refined"), RefineMeta{Rationale: "manual"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	after, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// The record and the saved facts must line up well enough for a rollback to
	// recognise the refine's own output and undo it.
	restored, skipped := RollbackFacts(after.Facts, record, time.Now().UTC())
	if len(skipped) != 0 {
		t.Fatalf("nothing was touched externally, so nothing should be skipped: %v", skipped)
	}
	if len(restored) != 1 || restored[0].Content != "original" {
		t.Fatalf("rollback did not restore the pre-refine content: %+v", restored)
	}
}

func TestRefineAndRecordSkipsWhenExtractionChangesNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "uses gofmt", Category: "style", Confidence: 0.9})

	// Re-proposing an identical fact is a no-op; recording it would leave a
	// rollback target that undoes nothing.
	_, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "uses gofmt"), RefineMeta{Rationale: "manual"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if saved {
		t.Fatal("want saved=false when the extraction changes nothing")
	}
	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("no-op extraction must not leave a record, got %d", len(records))
	}
}

func TestRefineAndRecordSkipsWithoutMessagesAndDoesNotCallTheExtractor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRefineService(t)

	failing := extractorFunc(func(context.Context, Document, []models.Message) (Update, error) {
		t.Error("extractor must not be called without messages")
		return Update{}, nil
	})

	_, saved, err := svc.RefineAndRecord(ctx, "s1", nil, failing, RefineMeta{Rationale: "manual"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if saved {
		t.Fatal("want saved=false")
	}
}

func TestRefineAndRecordAbandonsWorkSupersededByASyncFlush(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	// Compaction cancels queued work and bumps the flush version while an
	// extraction is in flight. That in-flight result is stale by the time it
	// returns and must not overwrite the synchronous flush.
	racing := extractorFunc(func(_ context.Context, _ Document, _ []models.Message) (Update, error) {
		svc.queue.cancelPending("update:s1")
		return Update{Facts: []Fact{{ID: "f1", Content: "stale", Confidence: 0.9}}}, nil
	})

	_, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), racing, RefineMeta{Rationale: "auto"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if saved {
		t.Fatal("want saved=false for a superseded extraction")
	}
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if doc.Facts[0].Content != "original" {
		t.Fatalf("stale extraction overwrote the flush: %q", doc.Facts[0].Content)
	}
	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("skipped extraction must not leave a ghost record, got %d", len(records))
	}
}

func TestRefineAndRecordHoldsTheSessionLockAcrossExtraction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRefineService(t)

	// The whole point of this method existing: snapshot, extract and save are one
	// critical section, so the memory builtin tool cannot write between them and
	// have its edit attributed to the refine.
	checking := extractorFunc(func(_ context.Context, _ Document, _ []models.Message) (Update, error) {
		if svc.getSessionLock("s1").TryLock() {
			t.Error("session lock is not held while the extractor runs")
		}
		return Update{Facts: []Fact{{ID: "f1", Content: "uses gofmt", Confidence: 0.9}}}, nil
	})

	if _, _, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), checking, RefineMeta{Rationale: "manual"}); err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
}

func TestRefineAndRecordSurfacesExtractorErrorsWithoutWriting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", Category: "style", Confidence: 0.9})

	failing := extractorFunc(func(context.Context, Document, []models.Message) (Update, error) {
		return Update{}, errors.New("upstream 503")
	})

	_, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), failing, RefineMeta{Rationale: "auto"})
	if err == nil {
		t.Fatal("want the extractor error surfaced")
	}
	if saved {
		t.Fatal("want saved=false")
	}
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if doc.Facts[0].Content != "original" {
		t.Fatalf("failed extraction must not write, got %q", doc.Facts[0].Content)
	}
}

func TestRefineAndRecordFallsBackWhenBackendCannotStoreHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// fakeStorage implements Storage but not RefinementStore.
	store := &fakeStorage{}
	svc := NewService(quietLogger(), store, nil)
	t.Cleanup(func() { _ = svc.Close(ctx) })

	// A backend without RefinementStore loses the history, not the extraction.
	// Failing here would silently stop memory extraction on that backend.
	_, _, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), addFact("f1", "uses gofmt"), RefineMeta{Rationale: "manual"})
	if err != nil {
		t.Fatalf("RefineAndRecord() must fall back, got error = %v", err)
	}
	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(doc.Facts) != 1 || doc.Facts[0].Content != "uses gofmt" {
		t.Fatalf("fallback must still extract, got %+v", doc.Facts)
	}
}

func TestDiffFactIDsReportsAddedRemovedAndChanged(t *testing.T) {
	t.Parallel()

	before := []Fact{
		{ID: "keep", Content: "same"},
		{ID: "edit", Content: "before"},
		{ID: "drop", Content: "gone soon"},
	}
	after := []Fact{
		{ID: "keep", Content: "same"},
		{ID: "edit", Content: "after"},
		{ID: "add", Content: "new"},
	}

	got := diffFactIDs(before, after)
	want := map[string]bool{"edit": true, "drop": true, "add": true}
	if len(got) != len(want) {
		t.Fatalf("diffFactIDs = %v, want keys %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected id %q in %v", id, got)
		}
	}
}

func TestServiceListAndGetRefinementDelegateToStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRefineService(t)
	record := refineOnce(t, svc, "s1", "f1", "uses gofmt")

	listed, err := svc.ListRefinements(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("ListRefinements() = %+v", listed)
	}

	got, err := svc.GetRefinement(ctx, "s1", record.ID)
	if err != nil {
		t.Fatalf("GetRefinement() error = %v", err)
	}
	if got.ID != record.ID {
		t.Fatalf("GetRefinement() = %+v", got)
	}
}

func TestServiceRefinementAccessorsOnABackendWithoutHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// fakeStorage implements Storage but not RefinementStore.
	store := &fakeStorage{}
	svc := NewService(quietLogger(), store, nil)
	t.Cleanup(func() { _ = svc.Close(ctx) })

	// Reading history from a backend that keeps none is an empty history, not a
	// failure: /refine list should say "nothing recorded", not print an error.
	listed, err := svc.ListRefinements(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("want an empty history, got %+v", listed)
	}
	if _, err := svc.GetRefinement(ctx, "s1", "refine_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestNewPairIDIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := NewPairID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewPairID() collided after %d calls: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestRefineAndRecordPersistsNarrativeMemoryWithoutFactChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "uses gofmt", Category: "style", Confidence: 0.9})

	// User/History are merged into the document and injected into the prompt just
	// like facts are. An extraction that refreshes TopOfMind without touching a
	// fact still has something worth keeping; skipping the save because no fact
	// moved would discard it.
	narrative := extractorFunc(func(context.Context, Document, []models.Message) (Update, error) {
		return Update{User: UserMemory{TopOfMind: "shipping the refine feature"}}, nil
	})

	_, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), narrative, RefineMeta{Rationale: "auto"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if !saved {
		t.Fatal("a narrative-only update is still an update")
	}

	doc, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if doc.User.TopOfMind != "shipping the refine feature" {
		t.Fatalf("narrative update was discarded: %+v", doc.User)
	}

	// There is nothing fact-shaped to restore, so there is nothing to roll back.
	records, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("a narrative-only update needs no rollback target, got %d records", len(records))
	}
}

func TestRefineAndRecordSkipsWhenNothingAtAllChanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "uses gofmt", Category: "style", Confidence: 0.9})

	empty := extractorFunc(func(context.Context, Document, []models.Message) (Update, error) {
		return Update{}, nil
	})

	_, saved, err := svc.RefineAndRecord(ctx, "s1", refineMessages(), empty, RefineMeta{Rationale: "auto"})
	if err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if saved {
		t.Fatal("want saved=false when neither facts nor narrative moved")
	}
}

func TestRefineAndRecordBoundsTheTrajectoryItSendsToTheExtractor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRefineService(t)

	var long []models.Message
	for i := 0; i < 200; i++ {
		long = append(long, models.Message{Role: models.RoleHuman, Content: "turn"})
		long = append(long, models.Message{Role: models.RoleAI, Content: "reply"})
	}

	// The async path bounds this before enqueueing, but a synchronous caller
	// (manual /refine) hands over the whole session. Without a bound here, a long
	// session overflows the model's context instead of extracting.
	var got int
	counting := extractorFunc(func(_ context.Context, _ Document, messages []models.Message) (Update, error) {
		got = len(messages)
		return Update{}, nil
	})
	if _, _, err := svc.RefineAndRecord(ctx, "s1", long, counting, RefineMeta{Rationale: "manual"}); err != nil {
		t.Fatalf("RefineAndRecord() error = %v", err)
	}
	if got == 0 || got > maxAsyncUpdateMessages {
		t.Fatalf("extractor received %d messages, want 1..%d", got, maxAsyncUpdateMessages)
	}
}
