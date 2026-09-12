package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConsolidationGroup is one near-duplicate cluster and its survivor.
type ConsolidationGroup struct {
	Survivor ScopedFact
	Dropped  []ScopedFact

	// TargetID is the ID the survivor is filed under once it lands in the
	// target document, when that differs from Survivor.ID. It is set only
	// when Survivor.ID collides with an ID already occupied in the target
	// scope (an existing fact there, or another survivor already assigned in
	// this same plan) — otherwise it is empty and Survivor.ID is used as-is.
	// Preference fact IDs are LLM-generated as "pref-<short-name>" (see
	// preference.go), so the same name recurring across unrelated scopes is
	// expected, not a bug in the extractor.
	TargetID string
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
// targetKey is the scope survivors will be moved into by ApplyConsolidation.
// PlanConsolidation needs it up front (not just at apply time) so a dry-run
// plan can show the caller a rename before anything is written: two
// unrelated groups whose survivors happen to share an ID (common, since
// preference IDs are "pref-<short-name>" and not globally unique) would
// otherwise both land in the target document under the same ID, and
// prepareDocument's dedup-by-ID silently drops the second one — quietly
// destroying an entire group's worth of facts. See TargetID.
func PlanConsolidation(facts []ScopedFact, category string, threshold float64, targetKey string) []ConsolidationGroup {
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
	groups = out

	// occupied starts as every ID already live in the target scope, across
	// all categories (a document's IDs must be unique regardless of
	// category), minus whatever this plan is about to drop from there.
	dropped := make(map[string]struct{}, len(groups)) // scopeKey\x00id
	for _, g := range groups {
		for _, f := range g.Dropped {
			dropped[f.ScopeKey+"\x00"+f.ID] = struct{}{}
		}
	}
	occupied := make(map[string]struct{})
	for _, f := range facts {
		if f.ScopeKey != targetKey {
			continue
		}
		if _, gone := dropped[f.ScopeKey+"\x00"+f.ID]; gone {
			continue
		}
		occupied[f.ID] = struct{}{}
	}

	// Assign a conflict-free TargetID to every survivor moving into
	// targetKey, in group order (already deterministic: candidates are
	// sorted by confidence/updatedAt/id above). A survivor already living in
	// targetKey keeps its ID untouched — it occupies its own slot already.
	for i := range groups {
		g := &groups[i]
		if g.Survivor.ScopeKey == targetKey {
			continue
		}
		id := g.Survivor.ID
		if _, taken := occupied[id]; !taken {
			occupied[id] = struct{}{}
			continue
		}
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("%s-%d", id, n)
			if _, taken := occupied[candidate]; !taken {
				g.TargetID = candidate
				occupied[candidate] = struct{}{}
				break
			}
		}
	}
	return groups
}

// ApplyConsolidation rewrites the affected documents: dropped facts vanish,
// survivors living outside targetKey move into the target document (bumping
// UpdatedAt — the fact changed scope — and renamed to g.TargetID when the
// plan assigned one to dodge an ID collision), survivors already in
// targetKey stay untouched. Every affected document is read AND its new fact
// set written inside one transaction (not read-then-transact): two separate
// steps would leave a window between the read and the write where another
// writer on the same document — this command is meant to run against a live
// chat session's database, and auto-refine writes the same user-scope
// document roughly every 5 turns — could commit new facts that this call
// would then silently overwrite when it saves the document it read before
// that write happened. The DSN's _txlock=immediate makes BeginTx take the
// write lock immediately, so reading inside the transaction gets exactly the
// same up-to-date, un-raceable view the write needs.
//
// Before this, the read-then-transact split also had a plain crash-safety
// gap: a half-applied consolidation (survivor deleted from its source scope
// but never written to the target, because some later Save in the old
// per-key loop failed) silently destroyed facts, and this repo has seen
// that happen under SQLITE_BUSY from a concurrent writer.
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
			mv := g.Survivor
			if g.TargetID != "" {
				mv.ID = g.TargetID
			}
			move[g.Survivor.ScopeKey+"\x00"+g.Survivor.ID] = mv
			affected[g.Survivor.ScopeKey] = struct{}{}
		}
	}
	if len(drop) == 0 && len(move) == 0 {
		return 0, nil
	}
	affected[targetKey] = struct{}{}

	err := s.inTx(ctx, targetKey, func(tx *sql.Tx) error {
		docs := make(map[string]Document, len(affected))
		for key := range affected {
			doc, err := s.loadWith(ctx, tx, key)
			if err != nil {
				// A target key that was never written yet still needs creating
				// when survivors move in; any other missing key has nothing to
				// do. Any error that is not "no such document" is not safe to
				// treat as either — that would either fabricate an empty target
				// document (wiping out its existing facts on save) or silently
				// skip a source scope's drops while still counting them as
				// applied, so surface it and abort the whole transaction.
				if !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("load %q for consolidation: %w", key, err)
				}
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
			// Save() normally does this; here the write happens directly via
			// saveDocumentTx below, so prepareDocument's trim/dedup/timestamp
			// defaults have to be applied explicitly or they are silently lost.
			if err := prepareDocument(&doc); err != nil {
				return fmt.Errorf("prepare %q for consolidation: %w", key, err)
			}
			docs[key] = doc
		}
		for _, doc := range docs {
			if err := s.saveDocumentTx(ctx, tx, doc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("apply consolidation: %w", err)
	}
	return len(drop) + len(move), nil
}
