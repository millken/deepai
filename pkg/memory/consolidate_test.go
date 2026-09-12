package memory

import (
	"context"
	"testing"
	"time"
)

func consolidationTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(context.Background(), t.TempDir()+"/memory.db")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	return store
}

// realWorldChineseVariants are the five near-duplicate "communicate in
// Chinese" facts `deepai memory list` actually found in production — the
// dataset that motivated consolidation.
var realWorldChineseVariants = []ScopedFact{
	{ScopeKey: "sess-a", Fact: Fact{ID: "pref-chinese-summaries", Content: "Prefers technical analysis summaries in Chinese", Category: "preference", Confidence: 0.95}},
	{ScopeKey: "sess-b", Fact: Fact{ID: "pref-chinese-conversation", Content: "Prefers communicating in Chinese (中文对话)", Category: "preference", Confidence: 1.0}},
	{ScopeKey: "sess-c", Fact: Fact{ID: "pref-chinese-communication", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 1.0}},
	{ScopeKey: "sess-d", Fact: Fact{ID: "pref-language-chinese", Content: "Prefers communication in Chinese (switched to Chinese mid-conversation with '切到中文')", Category: "preference", Confidence: 1.0}},
	{ScopeKey: "userdoc", Fact: Fact{ID: "pref-chinese-default", Content: "All future documentation, discussions, and code comments should default to Chinese (中文)", Category: "preference", Confidence: 1.0}},
}

func TestPlanConsolidationGroupsRealWorldChineseVariants(t *testing.T) {
	t.Parallel()

	facts := append([]ScopedFact{}, realWorldChineseVariants...)
	facts = append(facts,
		ScopedFact{ScopeKey: "sess-a", Fact: Fact{ID: "pref-feature-branch", Content: "Prefers feature branch workflow with peer review before merge", Category: "preference", Confidence: 0.9}},
		ScopedFact{ScopeKey: "sess-b", Fact: Fact{ID: "work-opencart", Content: "Prefers OpenCart-style architecture over strict modular structures", Category: "work", Confidence: 0.9}},
	)

	groups := PlanConsolidation(facts, "preference", 0.55)

	groupMembers := func(ids ...string) []string {
		var found []string
		for _, g := range groups {
			member := map[string]bool{}
			member[g.Survivor.ID] = true
			for _, f := range g.Dropped {
				member[f.ID] = true
			}
			for _, id := range ids {
				if member[id] {
					found = append(found, id)
				}
			}
		}
		return found
	}

	// The four "communicate in Chinese" phrasings must land in one group...
	chinese := groupMembers("pref-chinese-summaries", "pref-chinese-conversation", "pref-chinese-communication", "pref-language-chinese")
	if len(chinese) != 4 {
		t.Fatalf("the four communication phrasings must cluster together, got %d of 4: %v\nplan: %+v", len(chinese), chinese, groups)
	}
	// ...the broader "everything defaults to Chinese" rule is a different
	// statement and may stay separate...
	if groupMembers("pref-chinese-default", "pref-feature-branch") != nil {
		t.Fatalf("default-to-Chinese and feature-branch must not share a group with anything, plan: %+v", groups)
	}
	// ...unrelated preferences never join the family...
	for _, id := range []string{"pref-feature-branch", "pref-chinese-default"} {
		for _, g := range groups {
			for _, f := range g.Dropped {
				if f.ID == id {
					t.Fatalf("%s must never be a dropped member, plan: %+v", id, groups)
				}
			}
		}
	}
	// ...and the work-category fact is filtered out entirely.
	for _, g := range groups {
		for _, f := range append([]ScopedFact{g.Survivor}, g.Dropped...) {
			if f.ID == "work-opencart" {
				t.Fatalf("work-category fact must be excluded by the category filter, plan: %+v", groups)
			}
		}
	}

	// Survivor of the Chinese family must be a confidence-1.0 member.
	for _, g := range groups {
		if g.Survivor.ID == "pref-chinese-conversation" || g.Survivor.ID == "pref-chinese-communication" || g.Survivor.ID == "pref-language-chinese" {
			if g.Survivor.Confidence != 1.0 {
				t.Fatalf("survivor must be a highest-confidence member, got %+v", g.Survivor)
			}
			if len(g.Dropped) < 3 {
				t.Fatalf("expected the Chinese family group to absorb at least 3 drops, got %+v", g)
			}
		}
	}
}

func TestPlanConsolidationIdenticalContentCollapses(t *testing.T) {
	t.Parallel()

	facts := []ScopedFact{
		{ScopeKey: "sess-1", Fact: Fact{ID: "a", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 0.8}},
		{ScopeKey: "sess-2", Fact: Fact{ID: "b", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 0.6}},
		{ScopeKey: "sess-3", Fact: Fact{ID: "c", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 0.7}},
	}
	groups := PlanConsolidation(facts, "preference", 0.55)
	if len(groups) != 1 || len(groups[0].Dropped) != 2 {
		t.Fatalf("identical content must collapse to one group with 2 drops, got %+v", groups)
	}
	if groups[0].Survivor.ID != "a" {
		t.Fatalf("survivor must be the highest-confidence fact, got %+v", groups[0].Survivor)
	}
}

func TestPlanConsolidationAllDistinctReturnsNoGroups(t *testing.T) {
	t.Parallel()

	facts := []ScopedFact{
		{ScopeKey: "s1", Fact: Fact{ID: "p1", Content: "Prefers CLI tools over GUI applications", Category: "preference", Confidence: 0.9}},
		{ScopeKey: "s2", Fact: Fact{ID: "p2", Content: "Wants unit tests written before implementation code", Category: "preference", Confidence: 0.8}},
		{ScopeKey: "s3", Fact: Fact{ID: "p3", Content: "Reviews pull requests with per-item disposition tables", Category: "preference", Confidence: 0.7}},
	}
	if groups := PlanConsolidation(facts, "preference", 0.55); len(groups) != 0 {
		t.Fatalf("distinct facts must produce no groups, got %+v", groups)
	}
}

func TestApplyConsolidationMovesSurvivorAndDropsLosers(t *testing.T) {
	t.Parallel()

	store := consolidationTestStore(t)
	ctx := context.Background()
	userKey := UserScope("/w").Key()

	docs := map[string][]Fact{
		"sess-1": {
			{ID: "pref-chinese-summaries", Content: "Prefers technical analysis summaries in Chinese", Category: "preference", Confidence: 0.95},
			{ID: "keep-unrelated", Content: "Prefers table-driven Go tests", Category: "preference", Confidence: 0.9},
		},
		"sess-2": {
			{ID: "pref-chinese-conversation", Content: "Prefers communicating in Chinese (中文对话)", Category: "preference", Confidence: 1.0},
		},
		"sess-3": {
			{ID: "pref-chinese-communication", Content: "Prefers communication in Chinese", Category: "preference", Confidence: 1.0},
			{ID: "keep-other", Content: "Deploys via make build", Category: "work", Confidence: 0.8},
		},
	}
	for key, facts := range docs {
		if err := store.Save(ctx, Document{SessionID: key, Facts: facts}); err != nil {
			t.Fatalf("Save(%q): %v", key, err)
		}
	}

	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts(): %v", err)
	}
	groups := PlanConsolidation(facts, "preference", 0.55)
	if len(groups) != 1 {
		t.Fatalf("expected exactly the Chinese group, got %+v", groups)
	}

	changed, err := store.ApplyConsolidation(ctx, groups, userKey, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ApplyConsolidation(): %v", err)
	}
	if changed != 3 { // 2 drops + 1 move
		t.Fatalf("changed = %d, want 3 (2 drops + 1 move)", changed)
	}

	after, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts() after: %v", err)
	}
	got := map[string]ScopedFact{}
	for _, f := range after {
		got[f.ScopeKey+"/"+f.ID] = f
	}
	for _, banned := range []string{"sess-1/pref-chinese-summaries", "sess-3/pref-chinese-communication", "sess-2/pref-chinese-conversation"} {
		if _, ok := got[banned]; ok {
			t.Errorf("%s must be gone after consolidation", banned)
		}
	}
	survivor, ok := got[userKey+"/pref-chinese-communication"]
	if !ok {
		// Tie between the two confidence-1.0 facts breaks on UpdatedAt; the
		// later-saved communication fact wins over conversation.
		t.Fatalf("survivor must live under the user-scope key after consolidation, got %+v", after)
	}
	if !survivor.UpdatedAt.Equal(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("moved survivor must have UpdatedAt bumped, got %+v", survivor)
	}
	for _, kept := range []string{"sess-1/keep-unrelated", "sess-3/keep-other"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("unrelated fact %s must be untouched", kept)
		}
	}
}

func TestApplyConsolidationNoGroupsIsNoop(t *testing.T) {
	t.Parallel()

	store := consolidationTestStore(t)
	changed, err := store.ApplyConsolidation(context.Background(), nil, UserScope("/w").Key(), time.Now())
	if err != nil {
		t.Fatalf("ApplyConsolidation(nil): %v", err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0", changed)
	}
}
