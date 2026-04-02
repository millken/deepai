package proxy

import (
	"context"
	"sync"
)

// MemoryEventStore is an in-memory EventStore implementation.
type MemoryEventStore struct {
	mu          sync.RWMutex
	events      []LogEvent
	byID        map[string][]int // request ID → event indices
	summaries   []RequestSummary
	summaryByID map[string]int // request ID → summary index
}

// NewMemoryEventStore creates a new empty MemoryEventStore.
func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{
		events:      make([]LogEvent, 0),
		byID:        make(map[string][]int),
		summaries:   make([]RequestSummary, 0),
		summaryByID: make(map[string]int),
	}
}

func (s *MemoryEventStore) Append(_ context.Context, events ...LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, evt := range events {
		idx := len(s.events)
		s.events = append(s.events, evt)
		s.byID[evt.ID] = append(s.byID[evt.ID], idx)
		updateSummaryFromEvent(&s.summaries, s.summaryByID, evt)
	}
	return nil
}

func (s *MemoryEventStore) ListRequests(_ context.Context, offset, limit int) ([]RequestSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := len(s.summaries)
	if offset >= n {
		return nil, nil
	}

	end := offset + limit
	if end > n {
		end = n
	}

	out := make([]RequestSummary, end-offset)
	copy(out, s.summaries[offset:end])
	return out, nil
}

func (s *MemoryEventStore) GetTimeline(_ context.Context, id string) ([]LogEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	indices, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}

	out := make([]LogEvent, len(indices))
	for i, idx := range indices {
		out[i] = s.events[idx]
	}
	return out, nil
}

func (s *MemoryEventStore) Close() error { return nil }

// RequestCount returns the total number of tracked requests (for testing).
func (s *MemoryEventStore) RequestCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.summaries)
}

// EventCount returns the total number of stored events (for testing).
func (s *MemoryEventStore) EventCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// updateSummaryFromEvent updates the summaries slice based on event type.
// Shared by MemoryEventStore and FileEventStore to avoid duplication.
func updateSummaryFromEvent(summaries *[]RequestSummary, summaryByID map[string]int, evt LogEvent) {
	switch evt.Type {
	case EventStart:
		summaryByID[evt.ID] = len(*summaries)
		*summaries = append(*summaries, RequestSummary{
			ID:        evt.ID,
			Method:    evt.Method,
			Path:      evt.Path,
			Model:     evt.Model,
			APIFormat: evt.APIFormat,
			Streaming: evt.Streaming,
			CreatedAt: evt.Timestamp,
			ClientID:  evt.ClientID,
		})
	case EventUsage:
		if si, ok := summaryByID[evt.ID]; ok {
			s := &(*summaries)[si]
			if evt.InputTokens > s.InputTokens {
				s.InputTokens = evt.InputTokens
			}
			if evt.OutputTokens > s.OutputTokens {
				s.OutputTokens = evt.OutputTokens
			}
		}
	case EventDone:
		if si, ok := summaryByID[evt.ID]; ok {
			s := &(*summaries)[si]
			s.StatusCode = evt.StatusCode
			s.Duration = evt.Duration
			s.Error = evt.Error
		}
	}
}
