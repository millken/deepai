package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const (
	defaultQueueSize = 128
)

// jobType classifies the kind of async memory update.
type jobType int

const (
	jobUpdateWith           jobType = iota
	jobUpdate                       // uses service's own extractor
	jobUpdateWithFactSource         // update with explicit fact source
	jobRecordSkillUsage             // direct skill usage fact
	jobIncrementRetrieval           // atomic retrieval count bump
	jobIncrementHelpful             // atomic helpful count bump
	jobUpdateScopeWithSkill         // user-scope update + skill usage combined
	jobPreferenceUpdate             // preference extraction with dedup key "pref:"
)

// updateJob represents a pending async memory operation.
type updateJob struct {
	typ        jobType
	sessionID  string
	messages   []models.Message // cloned
	ext        Extractor
	factSource string
	skillName  string
	factIDs    []string
	scope      Scope
	turnID     int    // turn number for dedup key
	seq        uint64 // monotonic sequence for dedup
}

// UpdateQueue serializes and deduplicates memory update operations.
// It replaces fire-and-forget goroutines with a channel-backed worker,
// ensuring that multiple updates for the same session are merged rather
// than racing.
type UpdateQueue struct {
	svc          *Service
	ch           chan updateJob
	done         chan struct{}
	cancel       context.CancelFunc // cancels in-flight jobs on Close
	mu           sync.Mutex         // protects pendingSeq
	pendingSeq   map[string]uint64  // dedup key → latest sequence number
	flushVersion sync.Map           // "update:"+sessionID → uint64 ; bumped on sync flush
	seq          uint64
	closed       atomic.Bool
	closeOnce    sync.Once
	closeErr     error // first Close result; returned on subsequent calls
}

// newUpdateQueue creates and starts the queue.
func newUpdateQueue(svc *Service, size int) *UpdateQueue {
	if size <= 0 {
		size = defaultQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &UpdateQueue{
		svc:        svc,
		ch:         make(chan updateJob, size),
		done:       make(chan struct{}),
		cancel:     cancel,
		pendingSeq: make(map[string]uint64),
	}
	go q.run(ctx)
	return q
}

// Close drains the queue and stops the worker. Blocks until all pending
// jobs are processed or the context is cancelled. Safe to call multiple times;
// subsequent calls return the same error as the first invocation.
func (q *UpdateQueue) Close(ctx context.Context) error {
	q.closeOnce.Do(func() {
		q.closed.Store(true)
		q.cancel() // cancel in-flight jobs
		close(q.ch)
		select {
		case <-q.done:
		case <-ctx.Done():
			q.closeErr = ctx.Err()
		}
	})
	return q.closeErr
}

// submitTimeout is the maximum time to wait when the queue is full.
const submitTimeout = 2 * time.Second

// submit enqueues a job. If a job with the same dedup key is already pending,
// the new job replaces it (last-writer-wins coalescing).
// Blocks up to submitTimeout if the queue is full to avoid silent data loss.
// Silently drops the job if the queue is closed or the timeout expires.
func (q *UpdateQueue) submit(job updateJob) {
	if q.closed.Load() {
		return
	}
	key := q.dedupKey(job)

	q.mu.Lock()
	q.seq++
	job.seq = q.seq
	if key != "" {
		q.pendingSeq[key] = job.seq
	}
	q.mu.Unlock()

	// Block briefly if full; only drop after timeout.
	// Use recover() to handle TOCTOU race: closed.Load() may return false
	// just before Close() calls close(q.ch), causing a send-on-closed-channel panic.
	defer func() {
		if r := recover(); r != nil {
			q.svc.logger.Warn("update queue submit recovered panic", "err", r)
			if key != "" {
				q.mu.Lock()
				delete(q.pendingSeq, key)
				q.mu.Unlock()
			}
		}
	}()
	select {
	case q.ch <- job:
		return
	case <-time.After(submitTimeout):
		q.svc.logger.Warn("update queue submit timeout, dropping job", "timeout", submitTimeout, "type", job.typ, "session", job.sessionID)
		if key != "" {
			q.mu.Lock()
			delete(q.pendingSeq, key)
			q.mu.Unlock()
		}
	}
}

// cancelPending removes any pending dedup entry for the given dedup key
// and bumps the flush version so in-flight tasks detect staleness.
func (q *UpdateQueue) cancelPending(key string) {
	if key == "" {
		return
	}
	q.mu.Lock()
	delete(q.pendingSeq, key)
	q.mu.Unlock()
	// Bump flush version so in-flight tasks for this session detect staleness.
	if strings.HasPrefix(key, "update:") {
		for {
			v, loaded := q.flushVersion.LoadOrStore(key, uint64(1))
			if !loaded {
				return
			}
			if q.flushVersion.CompareAndSwap(key, v, v.(uint64)+1) {
				return
			}
		}
	}
}

// captureFlushVersion returns the current flush version for a session.
// Call this at the start of an update, then check isFlushStale before Save.
func (q *UpdateQueue) captureFlushVersion(sessionID string) uint64 {
	key := "update:" + sessionID
	v, _ := q.flushVersion.Load(key)
	if v == nil {
		return 0
	}
	return v.(uint64)
}

// isFlushStale returns true if the flush version has been bumped since capture,
// meaning a newer sync flush has superseded this in-flight update.
func (q *UpdateQueue) isFlushStale(sessionID string, captured uint64) bool {
	key := "update:" + sessionID
	v, _ := q.flushVersion.Load(key)
	if v == nil {
		return captured != 0
	}
	return v.(uint64) != captured
}

func (q *UpdateQueue) dedupKey(job updateJob) string {
	switch job.typ {
	case jobRecordSkillUsage:
		return "skill:" + job.sessionID + ":" + job.skillName
	case jobIncrementHelpful:
		return "helpful:" + job.sessionID + ":" + fmt.Sprintf("%d", job.turnID)
	case jobPreferenceUpdate:
		return "pref:" + job.sessionID
	case jobIncrementRetrieval:
		// Don't dedup retrieval increments — accumulate them.
		return ""
	default:
		return "update:" + job.sessionID
	}
}

// run is the main worker loop.
func (q *UpdateQueue) run(ctx context.Context) {
	defer close(q.done)

	var dropped, processed uint64

	for job := range q.ch {
		key := q.dedupKey(job)
		if key != "" {
			q.mu.Lock()
			latest, exists := q.pendingSeq[key]
			if !exists || latest != job.seq {
				// Job cancelled (!exists) or replaced by newer job; skip.
				q.mu.Unlock()
				dropped++
				continue
			}
			delete(q.pendingSeq, key)
			q.mu.Unlock()
		}

		q.execute(ctx, job)
		processed++
	}

	q.svc.logger.Debug("memory queue worker stopped",
		"processed", processed,
		"dropped", dropped,
	)
}

func (q *UpdateQueue) execute(ctx context.Context, job updateJob) {
	timeout := q.svc.updateTimeout
	if timeout <= 0 {
		timeout = defaultUpdateTimeout
	}
	switch job.typ {
	case jobUpdateWith:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := q.svc.UpdateWith(ctx, job.sessionID, job.messages, job.ext); err != nil {
			q.svc.logger.Warn("async update with extractor failed", "session", job.sessionID, "err", err)
		}

	case jobUpdate:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := q.svc.Update(ctx, job.sessionID, job.messages); err != nil {
			q.svc.logger.Warn("async update failed", "session", job.sessionID, "err", err)
		}

	case jobUpdateWithFactSource:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := q.svc.UpdateWithFactSource(ctx, job.sessionID, job.messages, job.ext, job.factSource); err != nil {
			q.svc.logger.Warn("async update with fact source failed", "session", job.sessionID, "err", err)
		}

	case jobRecordSkillUsage:
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := q.svc.RecordSkillUsage(ctx, job.sessionID, job.skillName); err != nil {
			q.svc.logger.Warn("record skill usage failed", "session", job.sessionID, "skill", job.skillName, "err", err)
		}

	case jobIncrementRetrieval:
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := q.svc.storage.IncrementRetrievalCounts(ctx, job.sessionID, job.factIDs); err != nil {
			q.svc.logger.Warn("increment retrieval counts failed", "session", job.sessionID, "err", err)
		}

	case jobIncrementHelpful:
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := q.svc.storage.IncrementHelpfulCounts(ctx, job.sessionID, job.factIDs)
		if err != nil {
			q.svc.logger.Warn("increment helpful counts failed", "session", job.sessionID, "err", err)
		} else if n > 0 {
			q.svc.logger.Debug("helpful counts incremented",
				"session", job.sessionID,
				"turn", job.turnID,
				"facts_updated", n,
			)
		}

	case jobUpdateScopeWithSkill:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := q.svc.UpdateScopeWithSkillUsage(ctx, job.scope, job.messages, job.ext, job.skillName); err != nil {
			q.svc.logger.Warn("async scope+skill update failed", "scope", job.scope.Key(), "err", err)
		}

	case jobPreferenceUpdate:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := q.svc.UpdateWith(ctx, job.sessionID, job.messages, job.ext); err != nil {
			q.svc.logger.Warn("async preference update failed", "session", job.sessionID, "err", err)
		}
	}
}

// ScheduleUpdateWith enqueues a background memory update with external extractor.
func (s *Service) ScheduleUpdateWith(sessionID string, messages []models.Message, ext Extractor) {
	if s == nil || s.storage == nil || ext == nil || len(messages) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobUpdateWith,
			sessionID: sessionID,
			messages:  cloneMessages(messages),
			ext:       ext,
		})
	}
}

// ScheduleUpdate enqueues a background memory update using the service extractor.
func (s *Service) ScheduleUpdate(sessionID string, messages []models.Message) {
	if s == nil || s.storage == nil || s.extractor == nil || len(messages) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobUpdate,
			sessionID: sessionID,
			messages:  cloneMessages(messages),
		})
	}
}

// ScheduleUpdateWithFactSource enqueues a background update with explicit fact source.
func (s *Service) ScheduleUpdateWithFactSource(sessionID string, messages []models.Message, ext Extractor, factSource string) {
	if s == nil || s.storage == nil || ext == nil || len(messages) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:        jobUpdateWithFactSource,
			sessionID:  sessionID,
			messages:   cloneMessages(messages),
			ext:        ext,
			factSource: factSource,
		})
	}
}

// ScheduleRecordSkillUsage enqueues a skill usage recording.
func (s *Service) ScheduleRecordSkillUsage(sessionID string, skillName string) {
	if s == nil || s.storage == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(skillName) == "" {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobRecordSkillUsage,
			sessionID: sessionID,
			skillName: skillName,
		})
	}
}

// scheduleRetrievalIncrement enqueues a retrieval count increment.
func (s *Service) scheduleRetrievalIncrement(sessionID string, factIDs []string) {
	if s == nil || s.storage == nil || len(factIDs) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobIncrementRetrieval,
			sessionID: sessionID,
			factIDs:   factIDs,
		})
	}
}

// ScheduleHelpfulIncrement enqueues a helpful count increment with turn-based dedup.
func (s *Service) ScheduleHelpfulIncrement(sessionID string, turnID int, factIDs []string) {
	if s == nil || s.storage == nil || len(factIDs) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobIncrementHelpful,
			sessionID: sessionID,
			factIDs:   factIDs,
			turnID:    turnID,
		})
	}
}

// ScheduleScopeUpdateWithSkill enqueues a user-scope update combined with skill usage.
func (s *Service) ScheduleScopeUpdateWithSkill(scope Scope, messages []models.Message, ext Extractor, skillName string) {
	if s == nil || s.storage == nil || len(messages) == 0 {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobUpdateScopeWithSkill,
			sessionID: scope.Key(),
			messages:  cloneMessages(messages),
			ext:       ext,
			skillName: skillName,
			scope:     scope,
		})
	}
}
