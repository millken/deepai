package memory

import (
	"sort"
	"testing"
	"time"
)

var (
	rollbackNow = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	rollbackOld = rollbackNow.Add(-time.Hour)
)

func fact(id, content string) Fact {
	return Fact{ID: id, Content: content, Category: "style", Confidence: 0.9, CreatedAt: rollbackOld, UpdatedAt: rollbackOld}
}

// record builds a RefinementRecord from the fact set before the refine and the
// fact set immediately after it, which is how RefineAndRecord produces one.
func record(pre, post []Fact) RefinementRecord {
	return RefinementRecord{
		ID:                   "refine_1",
		PreSnapshot:          pre,
		PostFactFingerprints: factFingerprints(post),
	}
}

func contentByID(facts []Fact) map[string]string {
	out := make(map[string]string, len(facts))
	for _, f := range facts {
		out[f.ID] = f.Content
	}
	return out
}

// Each case is one row of the decision table in the design doc: rollback is
// classified by (in PreSnapshot, in PostFactFingerprints, currently exists,
// fingerprint still matches Post). The universe iterated is the union of all
// three sets — a fact the refine evicted appears in none of the current facts.
func TestRollbackFactsDecisionTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		row         string
		pre         []Fact
		post        []Fact
		current     []Fact
		wantContent map[string]string
		wantSkipped []string
	}{
		{
			name:        "refine edited it and nobody else touched it",
			row:         "Pre=Y Post=Y cur=Y match=Y -> restore in place",
			pre:         []Fact{fact("f1", "original")},
			post:        []Fact{fact("f1", "refined")},
			current:     []Fact{fact("f1", "refined")},
			wantContent: map[string]string{"f1": "original"},
		},
		{
			name:        "a third party edited it after the refine",
			row:         "Pre=Y Post=Y cur=Y match=N -> keep current",
			pre:         []Fact{fact("f1", "original")},
			post:        []Fact{fact("f1", "refined")},
			current:     []Fact{fact("f1", "hand edited")},
			wantContent: map[string]string{"f1": "hand edited"},
			wantSkipped: []string{"f1"},
		},
		{
			name:        "a third party deleted it after the refine",
			row:         "Pre=Y Post=Y cur=N -> respect the deletion",
			pre:         []Fact{fact("f1", "original")},
			post:        []Fact{fact("f1", "refined")},
			current:     nil,
			wantContent: map[string]string{},
			wantSkipped: []string{"f1"},
		},
		{
			name:        "the refine evicted it and a third party recreated it",
			row:         "Pre=Y Post=N cur=Y -> keep current",
			pre:         []Fact{fact("f1", "original")},
			post:        nil,
			current:     []Fact{fact("f1", "recreated by agent")},
			wantContent: map[string]string{"f1": "recreated by agent"},
			wantSkipped: []string{"f1"},
		},
		{
			name:        "the refine evicted it and nothing brought it back",
			row:         "Pre=Y Post=N cur=N -> reinsert whole fact",
			pre:         []Fact{fact("f1", "original")},
			post:        nil,
			current:     nil,
			wantContent: map[string]string{"f1": "original"},
		},
		{
			name:        "the refine added it and nobody else touched it",
			row:         "Pre=N Post=Y cur=Y match=Y -> delete",
			pre:         nil,
			post:        []Fact{fact("f2", "added by refine")},
			current:     []Fact{fact("f2", "added by refine")},
			wantContent: map[string]string{},
		},
		{
			name:        "the refine added it and a third party edited it",
			row:         "Pre=N Post=Y cur=Y match=N -> keep current",
			pre:         nil,
			post:        []Fact{fact("f2", "added by refine")},
			current:     []Fact{fact("f2", "then hand edited")},
			wantContent: map[string]string{"f2": "then hand edited"},
			wantSkipped: []string{"f2"},
		},
		{
			name:        "the refine added it and a third party already deleted it",
			row:         "Pre=N Post=Y cur=N -> nothing to do",
			pre:         nil,
			post:        []Fact{fact("f2", "added by refine")},
			current:     nil,
			wantContent: map[string]string{},
		},
		{
			name:        "created after the refine, unrelated to it",
			row:         "Pre=N Post=N cur=Y -> keep current",
			pre:         nil,
			post:        nil,
			current:     []Fact{fact("f3", "brand new")},
			wantContent: map[string]string{"f3": "brand new"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, skipped := RollbackFacts(tc.current, record(tc.pre, tc.post), rollbackNow)

			gotContent := contentByID(got)
			if len(gotContent) != len(tc.wantContent) {
				t.Fatalf("%s\nresult = %v, want %v", tc.row, gotContent, tc.wantContent)
			}
			for id, want := range tc.wantContent {
				if gotContent[id] != want {
					t.Fatalf("%s\nfact %q = %q, want %q", tc.row, id, gotContent[id], want)
				}
			}

			sort.Strings(skipped)
			want := append([]string(nil), tc.wantSkipped...)
			sort.Strings(want)
			if len(skipped) != len(want) {
				t.Fatalf("%s\nskipped = %v, want %v", tc.row, skipped, want)
			}
			for i := range want {
				if skipped[i] != want[i] {
					t.Fatalf("%s\nskipped = %v, want %v", tc.row, skipped, want)
				}
			}
		})
	}
}

func TestRollbackFactsPreservesFeedbackCountsOnRestore(t *testing.T) {
	t.Parallel()

	// RetrievalCount/HelpfulCount/SuspectCount are accumulated by direct SQL
	// updates that bypass Document, so the snapshot's copies are stale. Restoring
	// them would erase the feedback signal gathered since the refine.
	pre := []Fact{fact("f1", "original")}
	post := []Fact{fact("f1", "refined")}

	current := fact("f1", "refined")
	current.RetrievalCount = 7
	current.HelpfulCount = 4
	current.SuspectCount = 2

	got, _ := RollbackFacts([]Fact{current}, record(pre, post), rollbackNow)
	if len(got) != 1 {
		t.Fatalf("want 1 fact, got %d", len(got))
	}
	if got[0].Content != "original" {
		t.Fatalf("content not restored: %q", got[0].Content)
	}
	if got[0].RetrievalCount != 7 || got[0].HelpfulCount != 4 || got[0].SuspectCount != 2 {
		t.Fatalf("counts must survive rollback, got r=%d h=%d s=%d",
			got[0].RetrievalCount, got[0].HelpfulCount, got[0].SuspectCount)
	}
}

func TestRollbackFactsReinsertsEvictedFactWithFreshUpdatedAt(t *testing.T) {
	t.Parallel()

	// An evicted fact carries the snapshot's old UpdatedAt. Merge sorts on it and
	// factScore decays relevance by it, so restoring the stale timestamp would
	// make the fact first in line for eviction again — undone immediately.
	evicted := fact("f1", "original")
	evicted.RetrievalCount = 3

	got, _ := RollbackFacts(nil, record([]Fact{evicted}, nil), rollbackNow)
	if len(got) != 1 {
		t.Fatalf("want the evicted fact reinserted, got %d facts", len(got))
	}
	if !got[0].UpdatedAt.Equal(rollbackNow) {
		t.Fatalf("UpdatedAt = %v, want now (%v)", got[0].UpdatedAt, rollbackNow)
	}
	if !got[0].CreatedAt.Equal(rollbackOld) {
		t.Fatalf("CreatedAt must stay as recorded, got %v want %v", got[0].CreatedAt, rollbackOld)
	}
	if got[0].RetrievalCount != 3 {
		t.Fatalf("reinsert keeps the snapshot's counts (no current value exists), got %d", got[0].RetrievalCount)
	}
}

func TestRollbackFactsRestoresEveryOverwrittenField(t *testing.T) {
	t.Parallel()

	// Rollback overwrites Content, Category and Confidence — the same three the
	// fingerprint covers.
	pre := []Fact{{ID: "f1", Content: "original", Category: "style", Confidence: 0.9, UpdatedAt: rollbackOld}}
	post := []Fact{{ID: "f1", Content: "refined", Category: "preference", Confidence: 0.4, UpdatedAt: rollbackNow}}

	got, _ := RollbackFacts(post, record(pre, post), rollbackNow)
	if len(got) != 1 {
		t.Fatalf("want 1 fact, got %d", len(got))
	}
	if got[0].Content != "original" || got[0].Category != "style" || got[0].Confidence != 0.9 {
		t.Fatalf("not all overwritten fields restored: %+v", got[0])
	}
}

// A rollback is itself recorded as a refinement so a mistaken one can be undone.
// That only holds if rolling back the rollback record reproduces the state the
// first rollback discarded.
func TestRollbackOfARollbackIsTheInverse(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name        string
		pre         []Fact
		post        []Fact
		current     []Fact
		wantAfter2x map[string]string
	}{
		{
			name:        "edited fact",
			pre:         []Fact{fact("f1", "original")},
			post:        []Fact{fact("f1", "refined")},
			current:     []Fact{fact("f1", "refined")},
			wantAfter2x: map[string]string{"f1": "refined"},
		},
		{
			name:        "evicted fact",
			pre:         []Fact{fact("f1", "original")},
			post:        nil,
			current:     nil,
			wantAfter2x: map[string]string{},
		},
		{
			name:        "added fact",
			pre:         nil,
			post:        []Fact{fact("f2", "added by refine")},
			current:     []Fact{fact("f2", "added by refine")},
			wantAfter2x: map[string]string{"f2": "added by refine"},
		},
	}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			afterFirst, _ := RollbackFacts(tc.current, record(tc.pre, tc.post), rollbackNow)

			// Service.RollbackRefinement builds this record: the pre-rollback facts
			// become the snapshot, the rolled-back facts become the post state.
			rollbackRecord := RefinementRecord{
				ID:                   "refine_2",
				PreSnapshot:          tc.current,
				PostFactFingerprints: factFingerprints(afterFirst),
			}

			afterSecond, _ := RollbackFacts(afterFirst, rollbackRecord, rollbackNow)

			got := contentByID(afterSecond)
			if len(got) != len(tc.wantAfter2x) {
				t.Fatalf("undoing the rollback = %v, want %v", got, tc.wantAfter2x)
			}
			for id, want := range tc.wantAfter2x {
				if got[id] != want {
					t.Fatalf("fact %q = %q, want %q", id, got[id], want)
				}
			}
		})
	}
}

func TestRollbackFactsIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	pre := []Fact{fact("f1", "original"), fact("f2", "second")}
	post := []Fact{fact("f1", "refined")}
	current := []Fact{fact("f1", "refined"), fact("f3", "unrelated")}

	first, _ := RollbackFacts(current, record(pre, post), rollbackNow)
	second, _ := RollbackFacts(current, record(pre, post), rollbackNow)

	if len(first) != len(second) {
		t.Fatalf("unstable length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("unstable order at %d: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}
	if current[0].Content != "refined" {
		t.Fatalf("input facts were mutated: %q", current[0].Content)
	}
}
