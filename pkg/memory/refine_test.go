package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// The fingerprint is the rollback conflict-detection baseline: it decides
// whether a fact was touched by a third party (the memory builtin tool) after
// a refine landed. It must therefore be stable across the exact normalization
// Save applies (prepareFact), and sensitive to every field rollback overwrites.

func TestFactFingerprintIgnoresSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	// prepareFact TrimSpaces Content/Category on Save, so an in-memory fact and
	// the same fact read back from storage must fingerprint identically.
	raw := Fact{Content: "  uses tabs  ", Category: " style\n", Confidence: 0.9}
	normalized := Fact{Content: "uses tabs", Category: "style", Confidence: 0.9}

	if factFingerprint(raw) != factFingerprint(normalized) {
		t.Fatalf("fingerprint must be whitespace-insensitive: raw=%q normalized=%q",
			factFingerprint(raw), factFingerprint(normalized))
	}
}

func TestFactFingerprintCoversEveryFieldRollbackOverwrites(t *testing.T) {
	t.Parallel()

	base := Fact{Content: "uses tabs", Category: "style", Confidence: 0.9}
	cases := []struct {
		name string
		fact Fact
	}{
		{"content", Fact{Content: "uses spaces", Category: "style", Confidence: 0.9}},
		{"category", Fact{Content: "uses tabs", Category: "preference", Confidence: 0.9}},
		{"confidence", Fact{Content: "uses tabs", Category: "style", Confidence: 0.4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if factFingerprint(base) == factFingerprint(tc.fact) {
				t.Fatalf("changing %s must change the fingerprint, both = %q",
					tc.name, factFingerprint(base))
			}
		})
	}
}

func TestFactFingerprintClampsConfidenceLikePrepareFact(t *testing.T) {
	t.Parallel()

	// prepareFact clamps Confidence into [0,1]; an out-of-range in-memory value
	// and its clamped stored form must agree.
	if factFingerprint(Fact{Content: "c", Confidence: 1.5}) != factFingerprint(Fact{Content: "c", Confidence: 1}) {
		t.Fatal("confidence above 1 must fingerprint as 1")
	}
	if factFingerprint(Fact{Content: "c", Confidence: -0.5}) != factFingerprint(Fact{Content: "c", Confidence: 0}) {
		t.Fatal("confidence below 0 must fingerprint as 0")
	}
}

func TestFactFingerprintsKeysByFactID(t *testing.T) {
	t.Parallel()

	facts := []Fact{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got := factFingerprints(facts)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(got), got)
	}
	if got["a"] != factFingerprint(facts[0]) || got["b"] != factFingerprint(facts[1]) {
		t.Fatalf("fingerprints not keyed by fact ID: %v", got)
	}
}

func TestRefineIDIsUniqueAcrossConsecutiveCalls(t *testing.T) {
	t.Parallel()

	// (session_id, id) is the primary key; a rollback record and the manual
	// refine right after it must not collide.
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := refineID()
		if _, dup := seen[id]; dup {
			t.Fatalf("refineID() collided after %d calls: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestHasRefinementStoreDetectsBackendCapability(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqliteStore, err := NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer sqliteStore.Close()

	if !HasRefinementStore(sqliteStore) {
		t.Fatal("SQLiteStore must implement RefinementStore (production path)")
	}

	// fakeStorage implements Storage but not RefinementStore.
	if HasRefinementStore(&fakeStorage{}) {
		t.Fatal("a plain Storage must not implement RefinementStore; it falls back to UpdateWith")
	}
}

func TestRefinementRecordSummary(t *testing.T) {
	t.Parallel()

	// Everything the UI needs for a "+N ~M -K" summary is already in the record:
	// facts only in the snapshot were removed, facts only in the post state were
	// added, and facts in both whose fingerprint moved were rewritten.
	record := RefinementRecord{
		PreSnapshot: []Fact{
			{ID: "kept", Content: "unchanged"},
			{ID: "edited", Content: "before"},
			{ID: "dropped", Content: "gone"},
		},
		PostFactFingerprints: map[string]string{
			"kept":   factFingerprint(Fact{Content: "unchanged"}),
			"edited": factFingerprint(Fact{Content: "after"}),
			"added":  factFingerprint(Fact{Content: "new"}),
		},
	}

	added, updated, removed := record.Summary()
	if added != 1 || updated != 1 || removed != 1 {
		t.Fatalf("summary = +%d ~%d -%d, want +1 ~1 -1", added, updated, removed)
	}
}

func TestRefinementRecordSummaryOfAFreshSession(t *testing.T) {
	t.Parallel()

	record := RefinementRecord{
		PostFactFingerprints: map[string]string{"a": "x", "b": "y"},
	}
	added, updated, removed := record.Summary()
	if added != 2 || updated != 0 || removed != 0 {
		t.Fatalf("summary = +%d ~%d -%d, want +2 ~0 -0", added, updated, removed)
	}
}
