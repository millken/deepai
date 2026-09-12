package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConsolidationGroup is one near-duplicate cluster and its survivor.
type ConsolidationGroup struct {
	Survivor ScopedFact
	Dropped  []ScopedFact
}

// DroppedCount is the number of facts the group removes if applied.
func (g ConsolidationGroup) DroppedCount() int { return len(g.Dropped) }

// foldTerm truncates latin tokens to a 6-char prefix so morphological
// variants count as the same term ("communicating"/"communication" →
// "commun"). Near-duplicate LLM-extracted facts differ mainly by word form,
// and without folding the canonical Chinese-preference pair scores 0.53 —
// below the default threshold — while folding lifts the real pairs to
// 0.55-0.71 and leaves unrelated pairs under 0.21.
func foldTerm(token string) string {
	// extractTerms only yields latin/digit runs or single Han runes; Han runes
	// are 3 bytes and never exceed the length gate, so a byte-slice prefix is
	// safe here.
	if len(token) > 6 {
		for _, r := range token {
			if r >= 0x80 {
				return token
			}
		}
		return token[:6]
	}
	return token
}

// consolidationTerms is extractTerms plus foldTerm. Separate from
// extractTerms so consolidation's folding never changes injection scoring.
func consolidationTerms(text string) map[string]float64 {
	terms := extractTerms(text)
	folded := make(map[string]float64, len(terms))
	for token, tf := range terms {
		folded[foldTerm(token)] += tf
	}
	return folded
}

// PlanConsolidation clusters same-category facts across all scopes by content
// similarity and picks one survivor per group: highest confidence, then most
// recently updated, then id. Grouping is greedy against the current survivor
// (never fact-to-fact), so a chain of pairwise-similar facts cannot pull in
// an outlier the survivor itself does not match. Similarity is the same
// cosine-over-terms measure injection uses (plus prefix folding), which keeps
// "what clusters together" consistent with "what gets recalled together".
func PlanConsolidation(facts []ScopedFact, category string, threshold float64) []ConsolidationGroup {
	category = strings.TrimSpace(category)
	byID := make(map[string]ScopedFact, len(facts))
	candidates := make([]ScopedFact, 0, len(facts))
	for _, f := range facts {
		if category != "" && f.Category != category {
			continue
		}
		if strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Content) == "" {
			continue
		}
		key := f.ScopeKey + "\x00" + f.ID
		if _, dup := byID[key]; dup {
			continue
		}
		byID[key] = f
		candidates = append(candidates, f)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Confidence != candidates[j].Confidence {
			return candidates[i].Confidence > candidates[j].Confidence
		}
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})

	var groups []ConsolidationGroup
	for _, f := range candidates {
		best := -1
		bestSim := threshold
		for i := range groups {
			sim := cosineSimilarity(consolidationTerms(f.Content), consolidationTerms(groups[i].Survivor.Content))
			if sim >= bestSim {
				best = i
				bestSim = sim
			}
		}
		if best < 0 {
			groups = append(groups, ConsolidationGroup{Survivor: f})
			continue
		}
		groups[best].Dropped = append(groups[best].Dropped, f)
	}

	// Only groups that actually collapse something are a plan.
	out := groups[:0]
	for _, g := range groups {
		if len(g.Dropped) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ApplyConsolidation rewrites the affected documents: dropped facts vanish,
// survivors living outside targetKey move into the target document (bumping
// UpdatedAt — the fact changed scope), survivors already in targetKey stay
// untouched. Documents are rewritten wholesale via Save, matching
// saveDocumentTx's delete-then-insert semantics.
func (s *SQLiteStore) ApplyConsolidation(ctx context.Context, groups []ConsolidationGroup, targetKey string, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("sqlite store is not initialized")
	}

	drop := make(map[string]struct{}) // scopeKey \x00 id -> drop
	move := make(map[string]ScopedFact)
	affected := make(map[string]struct{})
	for _, g := range groups {
		for _, f := range g.Dropped {
			drop[f.ScopeKey+"\x00"+f.ID] = struct{}{}
			affected[f.ScopeKey] = struct{}{}
		}
		if g.Survivor.ScopeKey != targetKey {
			move[g.Survivor.ScopeKey+"\x00"+g.Survivor.ID] = g.Survivor
			affected[g.Survivor.ScopeKey] = struct{}{}
		}
	}
	if len(drop) == 0 && len(move) == 0 {
		return 0, nil
	}
	affected[targetKey] = struct{}{}

	for key := range affected {
		doc, err := s.Load(ctx, key)
		if err != nil {
			// A target key that was never written yet still needs creating
			// when survivors move in; any other missing key has nothing to do.
			if key != targetKey {
				continue
			}
			doc = Document{SessionID: key}
		}
		kept := make([]Fact, 0, len(doc.Facts))
		for _, f := range doc.Facts {
			id := key + "\x00" + f.ID
			if _, ok := drop[id]; ok {
				continue
			}
			if _, ok := move[id]; ok {
				continue // survivors leaving this key; re-added under targetKey below
			}
			kept = append(kept, f)
		}
		if key == targetKey {
			for _, mv := range move {
				if mv.ScopeKey == key {
					continue // already handled above
				}
				mv.UpdatedAt = now
				kept = append(kept, mv.Fact)
			}
		}
		doc.Facts = kept
		if err := s.Save(ctx, doc); err != nil {
			return 0, fmt.Errorf("apply consolidation to %q: %w", key, err)
		}
	}
	return len(drop) + len(move), nil
}
