package memory

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestSelectRelevantFactsSkipsHighSuspect(t *testing.T) {
	now := time.Now().UTC()
	facts := []Fact{
		{ID: "good", Content: "use tabs for indentation", Confidence: 0.9, UpdatedAt: now},
		{ID: "toxic", Content: "use spaces for indentation", Confidence: 0.9, SuspectCount: 3, UpdatedAt: now},
	}
	_, ids := selectRelevantFacts(facts, "", 1000, "")
	for _, id := range ids {
		if id == "toxic" {
			t.Fatalf("fact with SuspectCount>=3 must be filtered, got ids=%v", ids)
		}
	}
	if len(ids) != 1 || ids[0] != "good" {
		t.Fatalf("expected only 'good' fact, got ids=%v", ids)
	}
}

func TestSelectRelevantFactsPenalizesSuspect(t *testing.T) {
	now := time.Now().UTC()
	clean := Fact{ID: "clean", Content: "alpha", Confidence: 0.5, UpdatedAt: now}
	suspect := Fact{ID: "suspect", Content: "alpha", Confidence: 0.9, SuspectCount: 2, UpdatedAt: now}
	// Without suspect penalty, suspect (conf=0.9) would outrank clean (conf=0.5).
	// With penalty 1 - 0.3*2 = 0.4, suspect score = 0.36 < clean score = 0.5.
	_, ids := selectRelevantFacts([]Fact{suspect, clean}, "", 1000, "")
	if len(ids) == 0 || ids[0] != "clean" {
		t.Fatalf("expected clean fact ranked first after penalty, got %v", ids)
	}
}

func TestScheduleSuspectIncrementUpdatesStorage(t *testing.T) {
	store := &fakeStorage{docs: map[string]Document{
		"s1": {
			SessionID: "s1",
			Facts:     []Fact{{ID: "f1", Content: "x", UpdatedAt: time.Now()}},
			UpdatedAt: time.Now(),
		},
	}}
	svc := NewService(slog.Default(), store, nil)

	svc.ScheduleSuspectIncrement("s1", 1, []string{"f1"})

	// Drain queue.
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Facts[0].SuspectCount != 1 {
		t.Fatalf("expected SuspectCount=1, got %d", got.Facts[0].SuspectCount)
	}
}
