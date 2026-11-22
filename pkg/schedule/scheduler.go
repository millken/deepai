package schedule

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const (
	defaultLeaseTTL     = 60 * time.Second
	defaultSyncInterval = 20 * time.Second
	defaultMaxWake      = time.Minute

	// A standby process rebuilds its heap so it can take over quickly, but a
	// slot that is already due must not spin the loop with a zero delay.
	standbyMinDelay = time.Second
)

// SchedulerOptions configures the Scheduler.
type SchedulerOptions struct {
	Clock        Clock      // default RealClock
	Lease        LeaseStore // nil = single process, always active
	LeaseTTL     time.Duration
	SyncInterval time.Duration
	MaxWake      time.Duration
	Logger       *slog.Logger
}

// Scheduler is one non-blocking loop: a min-heap of due slots/queue deadlines
// and a single timer. It never sleeps, waits for a turn, or holds a lock
// across an external call. Every wait is represented as a pending Run.
type Scheduler struct {
	svc        *Service
	dispatcher Dispatcher
	clock      Clock
	lease      LeaseStore
	owner      string
	leaseTTL   time.Duration
	syncEvery  time.Duration
	maxWake    time.Duration
	log        *slog.Logger

	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	started bool

	// lastRenew is the local time of the last successful lease renewal. It is
	// written by Start (before the loop starts) and by the loop goroutine
	// only, so no extra lock is needed. A transient renewal error must not
	// make us stop firing while the lease we already own is still valid.
	lastRenew time.Time
}

// NewScheduler builds a Scheduler. The Service's Dispatcher and kick callback
// are attached in Start, so the tool/REST layer can be wired up beforehand.
func NewScheduler(svc *Service, d Dispatcher, opts SchedulerOptions) *Scheduler {
	if opts.Clock == nil {
		opts.Clock = RealClock()
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultLeaseTTL
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = defaultSyncInterval
	}
	if opts.MaxWake <= 0 {
		opts.MaxWake = defaultMaxWake
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	host, _ := os.Hostname()
	return &Scheduler{
		svc: svc, dispatcher: d, clock: opts.Clock, lease: opts.Lease,
		owner:    fmt.Sprintf("%d@%s#%d", os.Getpid(), host, time.Now().UnixNano()),
		leaseTTL: opts.LeaseTTL, syncEvery: opts.SyncInterval, maxWake: opts.MaxWake,
		log:  opts.Logger,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start attaches the Dispatcher, takes the lease, recovers persisted state
// and starts the loop. It is safe to call once.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	s.svc.SetDispatcher(s.dispatcher)
	s.svc.SetKick(s.Kick)
	if s.lease != nil {
		now := s.clock.Now()
		ok, err := s.lease.AcquireLease(ctx, s.owner, now, now.Add(s.leaseTTL))
		if err != nil {
			return fmt.Errorf("acquire schedule lease: %w", err)
		}
		if ok {
			s.svc.setMode("active")
			s.lastRenew = now
		} else {
			s.svc.setMode("standby")
			s.log.Info("schedule: another deepai-bot holds the lease; standby mode")
		}
	}
	if s.svc.Mode() == "active" {
		if err := s.svc.Recover(ctx); err != nil {
			s.log.Warn("schedule: startup recovery", "err", err)
		}
		if err := s.svc.TryStartPending(ctx); err != nil {
			s.log.Warn("schedule: resume pending runs", "err", err)
		}
	}
	go s.loop(ctx)
	return nil
}

// Kick wakes the loop after a task/run change. Never blocks.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Stop ends the loop and releases the lease.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.once.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.lease != nil {
		if err := s.lease.ReleaseLease(ctx, s.owner); err != nil {
			s.log.Warn("schedule: release lease", "err", err)
		}
	}
	return nil
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)
	for {
		items, err := s.svc.DueItems(ctx)
		if err != nil {
			s.log.Warn("schedule: reload due items", "err", err)
		}
		h := newDueHeap(items)
		delay := s.maxWake
		if h.Len() > 0 {
			d := (*h)[0].At.Sub(s.clock.Now())
			if d < 0 {
				d = 0
			}
			if d < delay {
				delay = d
			}
		}
		if s.svc.Mode() != "active" && delay < standbyMinDelay {
			delay = standbyMinDelay
		}
		timer := s.clock.After(delay)
		syncTick := s.clock.After(s.syncEvery)
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-s.kick:
			continue // rebuild the heap from the Store
		case <-syncTick:
			s.syncLease(ctx)
			continue
		case <-timer:
			s.processDue(ctx, h)
		}
	}
}

func (s *Scheduler) processDue(ctx context.Context, h *dueHeap) {
	if s.svc.Mode() != "active" {
		return // standby keeps the heap warm but never fires
	}
	now := s.clock.Now()
	for h.Len() > 0 && !(*h)[0].At.After(now) {
		item := heap.Pop(h).(DueItem)
		if item.Kind == DueSlot {
			if err := s.svc.FireSlot(ctx, item.ScheduleID, item.At); err != nil {
				s.log.Warn("schedule: fire slot", "schedule", item.ScheduleID, "err", err)
			}
		}
	}
	if err := s.svc.ExpireQueued(ctx, now); err != nil {
		s.log.Warn("schedule: expire queued runs", "err", err)
	}
	if err := s.svc.TryStartPending(ctx); err != nil {
		s.log.Warn("schedule: try start pending", "err", err)
	}
}

// syncLease renews the single-writer lease, or tries to take over an expired
// one. A standby process keeps reloading the heap but never Fires.
func (s *Scheduler) syncLease(ctx context.Context) {
	if s.lease == nil {
		return
	}
	now := s.clock.Now()
	if s.svc.Mode() == "active" {
		ok, err := s.lease.RenewLease(ctx, s.owner, now.Add(s.leaseTTL))
		if err == nil && ok {
			s.lastRenew = now
			// A standby bot may have written cancel_requested_at while we hold
			// the lease; relay it and reload the heap on the next iteration.
			if err := s.svc.ProcessCancelRequests(ctx); err != nil {
				s.log.Warn("schedule: process cancel requests", "err", err)
			}
			// Runs whose recovery was deferred during a takeover are
			// reconciled once their original owner's grace period expires.
			if err := s.svc.Reconcile(ctx); err != nil {
				s.log.Warn("schedule: reconcile interrupted runs", "err", err)
			}
			return
		}
		if err != nil && now.Sub(s.lastRenew) < s.leaseTTL {
			// SQLITE_BUSY, a brief disk lock, etc. We still own the lease
			// until it expires, so keep firing and retry on the next sync.
			// Downgrading here would let a standby process take over and
			// execute the same slot while our turn is still running.
			s.log.Warn("schedule: renew lease failed; retrying while lease is valid", "err", err)
			return
		}
		s.svc.setMode("standby")
		s.log.Warn("schedule: lost lease; standby mode", "confirmed_takeover", err == nil && !ok, "err", err)
		return
	}
	ok, err := s.lease.AcquireLease(ctx, s.owner, now, now.Add(s.leaseTTL))
	if err != nil {
		s.log.Warn("schedule: acquire lease", "err", err)
		return
	}
	if ok {
		s.svc.setMode("active")
		s.lastRenew = now
		s.log.Info("schedule: lease acquired; active mode")
		// The previous holder may still be alive (a transient renew error),
		// so its recently started Runs get a grace window instead of being
		// declared dead immediately (design §4.2).
		if err := s.svc.RecoverAfterTakeover(ctx, s.leaseTTL); err != nil {
			s.log.Warn("schedule: recovery after takeover", "err", err)
		}
		s.Kick()
	}
}

// dueHeap is a min-heap over DueItem.At.
type dueHeap []DueItem

func newDueHeap(items []DueItem) *dueHeap {
	h := &dueHeap{}
	*h = append(*h, items...)
	heap.Init(h)
	return h
}

func (h dueHeap) Len() int           { return len(h) }
func (h dueHeap) Less(i, j int) bool { return h[i].At.Before(h[j].At) }
func (h dueHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dueHeap) Push(x any)        { *h = append(*h, x.(DueItem)) }
func (h *dueHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
