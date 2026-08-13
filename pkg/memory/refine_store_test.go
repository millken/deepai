package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newRefinementTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	ctx := context.Background()
	store, err := NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	return store
}

// seedDocument creates the memories row that memory_refinements references.
func seedDocument(t *testing.T, store *SQLiteStore, sessionID string, facts ...Fact) Document {
	t.Helper()

	doc := Document{SessionID: sessionID, Facts: facts, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), doc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return doc
}

func testRecord(id string, at time.Time) RefinementRecord {
	return RefinementRecord{
		ID:        id,
		PairID:    "pair-" + id,
		Rationale: "captured a stable preference",
		SessionID: "s1",
		PreSnapshot: []Fact{
			{ID: "f1", Content: "uses tabs", Category: "style", Confidence: 0.9, CreatedAt: at, UpdatedAt: at},
		},
		PostFactFingerprints: map[string]string{"f1": "abc123", "f2": "def456"},
		FactIDsChanged:       []string{"f2"},
		CreatedAt:            at,
	}
}

func TestSQLiteRefinementRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")

	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	want := testRecord("refine_1", at)
	if err := store.InsertRefinement(ctx, "s1", want); err != nil {
		t.Fatalf("InsertRefinement() error = %v", err)
	}

	got, err := store.GetRefinement(ctx, "s1", "refine_1")
	if err != nil {
		t.Fatalf("GetRefinement() error = %v", err)
	}
	if got.PairID != want.PairID || got.Rationale != want.Rationale {
		t.Fatalf("scalar fields lost: %+v", got)
	}
	if len(got.PreSnapshot) != 1 || got.PreSnapshot[0].Content != "uses tabs" {
		t.Fatalf("PreSnapshot lost: %+v", got.PreSnapshot)
	}
	if got.PostFactFingerprints["f1"] != "abc123" || got.PostFactFingerprints["f2"] != "def456" {
		t.Fatalf("PostFactFingerprints lost: %v", got.PostFactFingerprints)
	}
	if len(got.FactIDsChanged) != 1 || got.FactIDsChanged[0] != "f2" {
		t.Fatalf("FactIDsChanged lost: %v", got.FactIDsChanged)
	}
}

func TestSQLiteGetRefinementMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")

	if _, err := store.GetRefinement(context.Background(), "s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSQLiteListRefinementsNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"refine_1", "refine_2", "refine_3"} {
		if err := store.InsertRefinement(ctx, "s1", testRecord(id, base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("InsertRefinement(%s) error = %v", id, err)
		}
	}

	got, err := store.ListRefinements(ctx, "s1", 2)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit not honored: got %d records", len(got))
	}
	if got[0].ID != "refine_3" || got[1].ID != "refine_2" {
		t.Fatalf("want newest first, got %s then %s", got[0].ID, got[1].ID)
	}
}

func TestSQLiteListRefinementsIsScopedToSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")
	seedDocument(t, store, "__scope__:user:alice:")

	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if err := store.InsertRefinement(ctx, "s1", testRecord("refine_1", at)); err != nil {
		t.Fatalf("InsertRefinement(session) error = %v", err)
	}
	if err := store.InsertRefinement(ctx, "__scope__:user:alice:", testRecord("refine_2", at)); err != nil {
		t.Fatalf("InsertRefinement(user scope) error = %v", err)
	}

	got, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "refine_1" {
		t.Fatalf("user-scope record leaked into session partition: %+v", got)
	}
}

func TestSQLiteDeleteRefinement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")

	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if err := store.InsertRefinement(ctx, "s1", testRecord("refine_1", at)); err != nil {
		t.Fatalf("InsertRefinement() error = %v", err)
	}
	if err := store.DeleteRefinement(ctx, "s1", "refine_1"); err != nil {
		t.Fatalf("DeleteRefinement() error = %v", err)
	}
	if _, err := store.GetRefinement(ctx, "s1", "refine_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record still present after delete, err = %v", err)
	}
}

func TestSQLiteRefinementsKeepLastN(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	seedDocument(t, store, "s1")

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	for i := 0; i < maxRefinementsPerSession+5; i++ {
		rec := testRecord(refineID(), base.Add(time.Duration(i)*time.Second))
		if err := store.InsertRefinement(ctx, "s1", rec); err != nil {
			t.Fatalf("InsertRefinement(%d) error = %v", i, err)
		}
	}

	got, err := store.ListRefinements(ctx, "s1", 1000)
	if err != nil {
		t.Fatalf("ListRefinements() error = %v", err)
	}
	if len(got) != maxRefinementsPerSession {
		t.Fatalf("want history trimmed to %d, got %d", maxRefinementsPerSession, len(got))
	}
	// The survivors must be the newest ones.
	oldest := base.Add(5 * time.Second)
	if got[len(got)-1].CreatedAt.Before(oldest) {
		t.Fatalf("trimmed the wrong end: oldest survivor = %v, want >= %v", got[len(got)-1].CreatedAt, oldest)
	}
}

func TestSQLiteSaveWithRefinementCommitsBothOrNeither(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "original", CreatedAt: at, UpdatedAt: at})

	doc := Document{
		SessionID: "s1",
		Facts:     []Fact{{ID: "f1", Content: "refined", CreatedAt: at, UpdatedAt: at}},
		UpdatedAt: at,
	}
	if err := store.SaveWithRefinement(ctx, doc, testRecord("refine_1", at)); err != nil {
		t.Fatalf("SaveWithRefinement() error = %v", err)
	}
	loaded, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Facts[0].Content != "refined" {
		t.Fatalf("document not saved: %+v", loaded.Facts)
	}
	if _, err := store.GetRefinement(ctx, "s1", "refine_1"); err != nil {
		t.Fatalf("record not inserted: %v", err)
	}

	// Re-using the same record ID violates the (session_id, id) primary key.
	// The document write in the same transaction must roll back with it,
	// otherwise facts change with no record and the refine is unrollbackable.
	doc.Facts = []Fact{{ID: "f1", Content: "should not persist", CreatedAt: at, UpdatedAt: at}}
	if err := store.SaveWithRefinement(ctx, doc, testRecord("refine_1", at)); err == nil {
		t.Fatal("duplicate record ID must fail")
	}
	loaded, err = store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Facts[0].Content != "refined" {
		t.Fatalf("failed insert must roll back the document write, got %q", loaded.Facts[0].Content)
	}
}

func TestSQLiteSaveWithRollbackAppliesAllThreeEffects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRefinementTestStore(t)
	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	seedDocument(t, store, "s1", Fact{ID: "f1", Content: "refined", CreatedAt: at, UpdatedAt: at})

	if err := store.InsertRefinement(ctx, "s1", testRecord("refine_1", at)); err != nil {
		t.Fatalf("InsertRefinement() error = %v", err)
	}

	restored := Document{
		SessionID: "s1",
		Facts:     []Fact{{ID: "f1", Content: "original", CreatedAt: at, UpdatedAt: at}},
		UpdatedAt: at,
	}
	rollbackRecord := testRecord("refine_2", at.Add(time.Minute))
	rollbackRecord.Rationale = "Rollback of refine_1"

	if err := store.SaveWithRollback(ctx, restored, rollbackRecord, "refine_1"); err != nil {
		t.Fatalf("SaveWithRollback() error = %v", err)
	}

	loaded, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Facts[0].Content != "original" {
		t.Fatalf("document not restored: %+v", loaded.Facts)
	}
	if _, err := store.GetRefinement(ctx, "s1", "refine_2"); err != nil {
		t.Fatalf("rollback record not inserted: %v", err)
	}
	if _, err := store.GetRefinement(ctx, "s1", "refine_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("original record not deleted, err = %v", err)
	}
}

func TestSQLiteInsertRefinementRequiresMemoriesRow(t *testing.T) {
	t.Parallel()

	store := newRefinementTestStore(t)
	at := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	// foreign_keys(1) is on in the DSN. This is why SaveWithRefinement writes
	// the document before the record rather than the other way round.
	err := store.InsertRefinement(context.Background(), "never-saved", testRecord("refine_1", at))
	if err == nil {
		t.Fatal("insert without a memories row must violate the foreign key")
	}
}
