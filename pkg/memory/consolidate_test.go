package memory

import (
	"context"
	"errors"
	"strings"
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

	groups := PlanConsolidation(facts, "preference", 0.55, "unrelated-target")

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
	groups := PlanConsolidation(facts, "preference", 0.55, "unrelated-target")
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
	if groups := PlanConsolidation(facts, "preference", 0.55, "unrelated-target"); len(groups) != 0 {
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
	groups := PlanConsolidation(facts, "preference", 0.55, userKey)
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

// TestPlanConsolidationRenamesConflictingSurvivorIDs is the dry-run half of
// the most severe bug this file guards against: preference fact IDs are
// LLM-generated as "pref-<short-name>" (see preference.go), so two
// unrelated groups from different scopes can easily pick the same survivor
// ID. Without a rename, both would land in the target document under that
// ID and prepareDocument's dedup-by-ID would keep the first and silently
// drop the second — an entire group of facts vanishing with no error. The
// plan must show the collision (and its resolution) before anything is
// written, in the same order ApplyConsolidation reads it in.
func TestPlanConsolidationRenamesConflictingSurvivorIDs(t *testing.T) {
	t.Parallel()

	targetKey := UserScope("/target").Key()
	facts := []ScopedFact{
		{ScopeKey: "projB", Fact: Fact{ID: "pref-style", Content: "Prefers short professional answers in English", Category: "preference", Confidence: 0.9}},
		{ScopeKey: "projC", Fact: Fact{ID: "pref-style", Content: "Prefers detailed thorough explanations with examples", Category: "preference", Confidence: 0.9}},
		{ScopeKey: "projB", Fact: Fact{ID: "pref-style-alt", Content: "Prefers short professional replies in English", Category: "preference", Confidence: 0.7}},
		{ScopeKey: "projC", Fact: Fact{ID: "pref-style-alt2", Content: "Prefers detailed thorough explanations with samples", Category: "preference", Confidence: 0.7}},
	}

	groups := PlanConsolidation(facts, "preference", 0.55, targetKey)
	if len(groups) != 2 {
		t.Fatalf("the two unrelated survivors must not cluster with each other, got %+v", groups)
	}

	var projBGroup, projCGroup *ConsolidationGroup
	for i := range groups {
		switch groups[i].Survivor.ScopeKey {
		case "projB":
			projBGroup = &groups[i]
		case "projC":
			projCGroup = &groups[i]
		}
	}
	if projBGroup == nil || projCGroup == nil {
		t.Fatalf("expected exactly one group per scope, got %+v", groups)
	}
	if projBGroup.TargetID != "" {
		t.Fatalf("the first group to claim pref-style must keep it unrenamed, got TargetID %q", projBGroup.TargetID)
	}
	if projCGroup.TargetID != "pref-style-2" {
		t.Fatalf("the second group's colliding survivor must be deterministically renamed to pref-style-2, got %q", projCGroup.TargetID)
	}
}

// TestApplyConsolidationRenamesConflictingSurvivorIDs is the applied half of
// the same scenario: reproduces the real bug (`group 0 survivor=pref-style
// (projB) group 1 survivor=pref-style(projC)` -> only one project's fact
// survived) and asserts both facts now exist in the target document under
// distinct IDs, with both source scopes fully drained.
func TestApplyConsolidationRenamesConflictingSurvivorIDs(t *testing.T) {
	t.Parallel()

	store := consolidationTestStore(t)
	ctx := context.Background()
	targetKey := UserScope("/target").Key()

	if err := store.Save(ctx, Document{SessionID: "projB", Facts: []Fact{
		{ID: "pref-style", Content: "Prefers short professional answers in English", Category: "preference", Confidence: 0.9},
		{ID: "pref-style-alt", Content: "Prefers short professional replies in English", Category: "preference", Confidence: 0.7},
	}}); err != nil {
		t.Fatalf("Save(projB): %v", err)
	}
	if err := store.Save(ctx, Document{SessionID: "projC", Facts: []Fact{
		{ID: "pref-style", Content: "Prefers detailed thorough explanations with examples", Category: "preference", Confidence: 0.9},
		{ID: "pref-style-alt2", Content: "Prefers detailed thorough explanations with samples", Category: "preference", Confidence: 0.7},
	}}); err != nil {
		t.Fatalf("Save(projC): %v", err)
	}

	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts(): %v", err)
	}
	groups := PlanConsolidation(facts, "preference", 0.55, targetKey)
	if len(groups) != 2 {
		t.Fatalf("expected two independent groups, got %+v", groups)
	}

	changed, err := store.ApplyConsolidation(ctx, groups, targetKey, time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyConsolidation(): %v", err)
	}
	if changed != 4 { // 2 drops + 2 moves
		t.Fatalf("changed = %d, want 4", changed)
	}

	after, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts() after: %v", err)
	}
	byKey := map[string]ScopedFact{}
	for _, f := range after {
		byKey[f.ScopeKey+"/"+f.ID] = f
	}

	original, ok := byKey[targetKey+"/pref-style"]
	if !ok {
		t.Fatalf("the first survivor must keep its original ID in the target, got %+v", after)
	}
	if !strings.Contains(original.Content, "English") {
		t.Errorf("pref-style in target must be the projB fact, got %+v", original)
	}
	renamed, ok := byKey[targetKey+"/pref-style-2"]
	if !ok {
		t.Fatalf("the second survivor must be renamed to pref-style-2 in target instead of vanishing, got %+v", after)
	}
	if !strings.Contains(renamed.Content, "examples") {
		t.Errorf("pref-style-2 in target must be the projC fact, got %+v", renamed)
	}
	if len(after) != 2 {
		t.Fatalf("4 seeded facts (2 survivors + 2 drops) must collapse to exactly 2 facts in target, got %d: %+v", len(after), after)
	}
}

// TestApplyConsolidationCreatesMissingTargetWithoutClearingOthers covers the
// ErrNotFound branch of item 3's Load-error handling: a target scope that
// has never been written yet must be created fresh with only the moved
// survivor, and every other affected scope must be left exactly as it was.
//
// The other branch item 3 guards against — a Load error that is NOT
// ErrNotFound aborting the whole apply with nothing written, instead of
// either fabricating an empty target document or silently skipping a
// scope's drops while still counting them as applied — has no clean
// injection point through the public SQLiteStore API; it would need a
// fault-injecting driver wrapper, which this codebase does not have. The
// closest available proxy is TestApplyConsolidationAtomicWhenStoreFails
// below, which forces every Load to fail (via a closed store) and checks
// that nothing was written as a result.
func TestApplyConsolidationCreatesMissingTargetWithoutClearingOthers(t *testing.T) {
	t.Parallel()

	store := consolidationTestStore(t)
	ctx := context.Background()
	targetKey := UserScope("/brand-new-target").Key()

	if err := store.Save(ctx, Document{SessionID: "sess-1", Facts: []Fact{
		{ID: "pref-a", Content: "Prefers concise commit messages", Category: "preference", Confidence: 0.9},
		{ID: "pref-b", Content: "Prefers short commit messages that are concise", Category: "preference", Confidence: 0.7},
		{ID: "keep-me", Content: "Unrelated fact that must not move", Category: "work", Confidence: 0.5},
	}}); err != nil {
		t.Fatalf("Save(sess-1): %v", err)
	}

	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts(): %v", err)
	}
	groups := PlanConsolidation(facts, "preference", 0.55, targetKey)
	if len(groups) != 1 {
		t.Fatalf("expected one group, got %+v", groups)
	}

	if _, err := store.Load(ctx, targetKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("target must not exist yet, got err=%v", err)
	}

	if _, err := store.ApplyConsolidation(ctx, groups, targetKey, time.Now().UTC()); err != nil {
		t.Fatalf("ApplyConsolidation(): %v", err)
	}

	targetDoc, err := store.Load(ctx, targetKey)
	if err != nil {
		t.Fatalf("target must have been created, Load() error = %v", err)
	}
	if len(targetDoc.Facts) != 1 || targetDoc.Facts[0].ID != "pref-a" {
		t.Fatalf("target must contain only the moved survivor, got %+v", targetDoc.Facts)
	}

	sourceDoc, err := store.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load(sess-1): %v", err)
	}
	if len(sourceDoc.Facts) != 1 || sourceDoc.Facts[0].ID != "keep-me" {
		t.Fatalf("the unrelated fact in the source scope must be untouched, got %+v", sourceDoc.Facts)
	}
}

// TestApplyConsolidationAtomicWhenStoreFails exercises item 1's core
// guarantee (read every affected document and compute new fact sets, then
// write all of them in a single transaction) via the closest failure this
// codebase's public API can inject: a store that goes bad before
// ApplyConsolidation runs. This fails at the read phase rather than partway
// through the write transaction (a true "succeeds reading, fails mid-write"
// injection needs a fault-injecting driver wrapper, which does not exist
// here), but it still verifies the invariant that matters: on any error,
// nothing is written anywhere, not even the survivor's source scope.
func TestApplyConsolidationAtomicWhenStoreFails(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/memory.db"
	ctx := context.Background()

	store, err := NewSQLiteStore(ctx, path)
	if err != nil {
		t.Fatalf("NewSQLiteStore(): %v", err)
	}
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate(): %v", err)
	}
	targetKey := UserScope("/atomic-target").Key()
	if err := store.Save(ctx, Document{SessionID: "sess-1", Facts: []Fact{
		{ID: "pref-a", Content: "Prefers concise commit messages", Category: "preference", Confidence: 0.9},
		{ID: "pref-b", Content: "Prefers short commit messages that are concise", Category: "preference", Confidence: 0.7},
	}}); err != nil {
		t.Fatalf("Save(sess-1): %v", err)
	}

	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		t.Fatalf("ListAllFacts(): %v", err)
	}
	groups := PlanConsolidation(facts, "preference", 0.55, targetKey)
	if len(groups) != 1 {
		t.Fatalf("expected one group, got %+v", groups)
	}

	store.Close()
	if _, err := store.ApplyConsolidation(ctx, groups, targetKey, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error from a closed store")
	}

	reopened, err := NewSQLiteStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	sourceDoc, err := reopened.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load(sess-1) after failed apply: %v", err)
	}
	if len(sourceDoc.Facts) != 2 {
		t.Fatalf("source scope must be untouched by a failed apply, got %+v", sourceDoc.Facts)
	}
	if _, err := reopened.Load(ctx, targetKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("target scope must not have been created by a failed apply, got err=%v", err)
	}
}
