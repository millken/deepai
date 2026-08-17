package memory

import (
	"errors"
	"strings"
	"time"
)

var ErrNotFound = errors.New("memory record not found")

func prepareDocument(doc *Document) error {
	if doc == nil {
		return errors.New("memory document is nil")
	}
	doc.SessionID = strings.TrimSpace(doc.SessionID)
	if doc.SessionID == "" {
		return errors.New("memory session_id is required")
	}
	doc.Source = strings.TrimSpace(doc.Source)
	if doc.Source == "" {
		doc.Source = doc.SessionID
	}
	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = time.Now().UTC()
	}
	seen := make(map[string]struct{}, len(doc.Facts))
	facts := make([]Fact, 0, len(doc.Facts))
	for _, fact := range doc.Facts {
		if err := prepareFact(&fact); err != nil {
			return err
		}
		if _, ok := seen[fact.ID]; ok {
			continue
		}
		seen[fact.ID] = struct{}{}
		facts = append(facts, fact)
	}
	doc.Facts = facts
	return nil
}

func prepareFact(fact *Fact) error {
	if fact == nil {
		return errors.New("memory fact is nil")
	}
	fact.ID = strings.TrimSpace(fact.ID)
	fact.Content = strings.TrimSpace(fact.Content)
	fact.Category = strings.TrimSpace(fact.Category)
	fact.Source = strings.TrimSpace(fact.Source)
	if fact.ID == "" {
		return errors.New("memory fact id is required")
	}
	if fact.Content == "" {
		return errors.New("memory fact content is required")
	}
	if fact.Confidence < 0 {
		fact.Confidence = 0
	}
	if fact.Confidence > 1 {
		fact.Confidence = 1
	}
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = time.Now().UTC()
	}
	if fact.UpdatedAt.IsZero() {
		fact.UpdatedAt = fact.CreatedAt
	}
	return nil
}
