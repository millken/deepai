package schedule

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned when a Schedule or Run id is unknown.
	ErrNotFound = errors.New("schedule: not found")
	// ErrConflict is returned when a Run would violate a UNIQUE constraint
	// (schedule_id, slot) or (schedule_id, request_id).
	ErrConflict = errors.New("schedule: conflict")
)

// ScheduleFilter selects schedules for ListSchedules.
type ScheduleFilter struct {
	State          State
	IncludeDeleted bool
	Limit          int
}

// RunFilter selects runs for FindRun.
type RunFilter struct {
	ScheduleID string
	RequestID  string
	Slot       *time.Time
	Statuses   []RunStatus
	Since      *time.Time // scheduled_for >= Since
	// CancelRequested selects runs whose cancel_requested_at is set; the
	// lease holder uses it to relay cancellations written by a standby bot.
	CancelRequested bool
	Limit           int
	// Order is "seq_desc" (default) or "scheduled_for_asc".
	Order string
}

// Store is the persistence contract. Both implementations (memory and
// SQLite) enforce the two UNIQUE constraints on Run insertion; that is the
// second line of defence against double-firing a slot across processes.
type Store interface {
	Migrate(ctx context.Context) error

	InsertSchedule(ctx context.Context, s Schedule) error
	UpdateSchedule(ctx context.Context, s Schedule) error
	GetSchedule(ctx context.Context, id string) (Schedule, error)
	ListSchedules(ctx context.Context, f ScheduleFilter) ([]Schedule, error)

	InsertRun(ctx context.Context, r Run) error
	UpdateRun(ctx context.Context, r Run) error
	GetRun(ctx context.Context, id string) (Run, error)
	FindRun(ctx context.Context, f RunFilter) ([]Run, error)
	// PruneRuns removes terminal Runs beyond the newest `keep` per schedule.
	// Active/pending Runs are never removed.
	PruneRuns(ctx context.Context, scheduleID string, keep int) error

	Close() error
}

// LeaseStore is the optional single-writer lease. A process without the
// lease stays in standby: it keeps serving reads and writes but never Fires.
type LeaseStore interface {
	AcquireLease(ctx context.Context, owner string, now, expiresAt time.Time) (bool, error)
	RenewLease(ctx context.Context, owner string, expiresAt time.Time) (bool, error)
	ReleaseLease(ctx context.Context, owner string) error
}

// MemoryStore is an in-memory Store used by --ephemeral and by tests. It
// implements LeaseStore too, always granting the lease to its first caller.
type MemoryStore struct {
	mu        sync.Mutex
	schedules map[string]Schedule
	runs      map[string]Run
	version   int64
	owner     string
	expiresAt time.Time
}

// NewMemoryStore builds an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{schedules: map[string]Schedule{}, runs: map[string]Run{}}
}

func (s *MemoryStore) Migrate(context.Context) error { return nil }
func (s *MemoryStore) Close() error                  { return nil }

// Version exposes the meta version for tests / standby polling.
func (s *MemoryStore) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *MemoryStore) InsertSchedule(_ context.Context, v Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.schedules[v.ID]; exists {
		return fmt.Errorf("%w: schedule %s", ErrConflict, v.ID)
	}
	s.schedules[v.ID] = v.Clone()
	s.version++
	return nil
}

func (s *MemoryStore) UpdateSchedule(_ context.Context, v Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.schedules[v.ID]; !exists {
		return fmt.Errorf("%w: schedule %s", ErrNotFound, v.ID)
	}
	s.schedules[v.ID] = v.Clone()
	s.version++
	return nil
}

func (s *MemoryStore) GetSchedule(_ context.Context, id string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.schedules[id]
	if !ok {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	return v.Clone(), nil
}

func (s *MemoryStore) ListSchedules(_ context.Context, f ScheduleFilter) ([]Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, 0, len(s.schedules))
	for _, v := range s.schedules {
		if !f.IncludeDeleted && v.State == StateDeleted {
			continue
		}
		if f.State != "" && v.State != f.State {
			continue
		}
		out = append(out, v.Clone())
	}
	sortSchedules(out)
	return limitSchedules(out, f.Limit), nil
}

func (s *MemoryStore) InsertRun(_ context.Context, r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[r.ID]; exists {
		return fmt.Errorf("%w: run %s", ErrConflict, r.ID)
	}
	for _, existing := range s.runs {
		if existing.ScheduleID != r.ScheduleID {
			continue
		}
		if r.Slot != nil && existing.Slot != nil && existing.Slot.Equal(*r.Slot) {
			return fmt.Errorf("%w: slot %s", ErrConflict, r.Slot.Format(time.RFC3339))
		}
		if r.RequestID != "" && existing.RequestID == r.RequestID {
			return fmt.Errorf("%w: request_id %s", ErrConflict, r.RequestID)
		}
	}
	s.runs[r.ID] = r.Clone()
	s.version++
	return nil
}

func (s *MemoryStore) UpdateRun(_ context.Context, r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[r.ID]; !exists {
		return fmt.Errorf("%w: run %s", ErrNotFound, r.ID)
	}
	s.runs[r.ID] = r.Clone()
	s.version++
	return nil
}

func (s *MemoryStore) GetRun(_ context.Context, id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.runs[id]
	if !ok {
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return v.Clone(), nil
}

func (s *MemoryStore) FindRun(_ context.Context, f RunFilter) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.runs))
	for _, v := range s.runs {
		if f.ScheduleID != "" && v.ScheduleID != f.ScheduleID {
			continue
		}
		if f.RequestID != "" && v.RequestID != f.RequestID {
			continue
		}
		if f.Slot != nil && (v.Slot == nil || !v.Slot.Equal(*f.Slot)) {
			continue
		}
		if f.Since != nil && v.ScheduledFor.Before(*f.Since) {
			continue
		}
		if len(f.Statuses) > 0 && !containsStatus(f.Statuses, v.Status) {
			continue
		}
		if f.CancelRequested && v.CancelRequestedAt == nil {
			continue
		}
		out = append(out, v.Clone())
	}
	if f.Order == "scheduled_for_asc" {
		sort.Slice(out, func(i, j int) bool {
			if out[i].ScheduledFor.Equal(out[j].ScheduledFor) {
				return out[i].Seq < out[j].Seq
			}
			return out[i].ScheduledFor.Before(out[j].ScheduledFor)
		})
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	}
	return limitRuns(out, f.Limit), nil
}

func (s *MemoryStore) PruneRuns(_ context.Context, scheduleID string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keep <= 0 {
		return nil
	}
	ordered := make([]Run, 0, len(s.runs))
	for _, v := range s.runs {
		if v.ScheduleID == scheduleID && v.Status.Terminal() {
			ordered = append(ordered, v)
		}
	}
	if len(ordered) <= keep {
		return nil
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Seq > ordered[j].Seq })
	for _, v := range ordered[keep:] {
		delete(s.runs, v.ID)
	}
	s.version++
	return nil
}

func (s *MemoryStore) AcquireLease(_ context.Context, owner string, now, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != "" && s.owner != owner && s.expiresAt.After(now) {
		return false, nil
	}
	s.owner, s.expiresAt = owner, expiresAt
	return true, nil
}

func (s *MemoryStore) RenewLease(_ context.Context, owner string, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != owner {
		return false, nil
	}
	s.expiresAt = expiresAt
	return true, nil
}

func (s *MemoryStore) ReleaseLease(_ context.Context, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == owner {
		s.owner, s.expiresAt = "", time.Time{}
	}
	return nil
}

func containsStatus(list []RunStatus, s RunStatus) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortSchedules(list []Schedule) {
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
}

func limitSchedules(list []Schedule, n int) []Schedule {
	if n > 0 && len(list) > n {
		return list[:n]
	}
	return list
}

func limitRuns(list []Run, n int) []Run {
	if n > 0 && len(list) > n {
		return list[:n]
	}
	return list
}
