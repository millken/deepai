package memory

import (
	"strings"
	"testing"
	"time"
)

func TestMergeArchivesConflictingFact(t *testing.T) {
	now := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	current := Document{
		SessionID: "s1",
		Facts: []Fact{
			{ID: "f1", Content: "user prefers dark mode", Confidence: 0.8, UpdatedAt: now.Add(-time.Hour)},
		},
	}
	update := Update{
		Facts: []Fact{
			{ID: "f1", Content: "user prefers light mode", Confidence: 0.9},
		},
	}

	merged := MergeWithFactSource(current, update, "s1", "", now)

	var live, archived *Fact
	for i := range merged.Facts {
		switch {
		case merged.Facts[i].ID == "f1":
			live = &merged.Facts[i]
		case strings.HasPrefix(merged.Facts[i].ID, "f1#prev"):
			archived = &merged.Facts[i]
		}
	}
	if live == nil {
		t.Fatalf("expected live fact f1 to remain, facts=%+v", merged.Facts)
	}
	if live.Content != "user prefers light mode" {
		t.Fatalf("live fact should hold new content, got %q", live.Content)
	}
	if archived == nil {
		t.Fatalf("expected archived prev version, facts=%+v", merged.Facts)
	}
	if archived.Content != "user prefers dark mode" {
		t.Fatalf("archived fact must keep old content, got %q", archived.Content)
	}
	if archived.Confidence > 0.3 {
		t.Fatalf("archived confidence must be demoted, got %v", archived.Confidence)
	}
	if archived.SuspectCount < 2 {
		t.Fatalf("archived SuspectCount must be >=2 to be hard-filtered, got %d", archived.SuspectCount)
	}
}

func TestMergeNoArchiveOnWhitespaceOnlyChange(t *testing.T) {
	now := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	current := Document{
		SessionID: "s1",
		Facts:     []Fact{{ID: "f1", Content: "alpha beta", Confidence: 0.8, UpdatedAt: now.Add(-time.Hour)}},
	}
	update := Update{
		Facts: []Fact{{ID: "f1", Content: "  ALPHA   beta\n", Confidence: 0.8}},
	}
	merged := MergeWithFactSource(current, update, "s1", "", now)
	for _, f := range merged.Facts {
		if strings.HasPrefix(f.ID, "f1#prev") {
			t.Fatalf("whitespace/case-only change must not archive, facts=%+v", merged.Facts)
		}
	}
}
