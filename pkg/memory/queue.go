package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const (
	defaultQueueSize = 128
	queueWorkerCount = 1 // single worker to serialize per-key updates
)

// jobType classifies the kind of async memory update.
type jobType int

const (
	jobUpdateWith             jobType = iota
	jobUpdate                 // uses service's own extractor
	jobUpdateWithFactSource   // update with explicit fact source
	jobRecordSkillUsage       // direct skill usage fact
	jobIncrementRetrieval     // atomic retrieval count bump
	jobUpdateScopeWithSkill   // user-scope update + skill usage combined
)

// updateJob represents a pending async memory operation.
type updateJob struct {
	typ         jobType
	sessionID   string
	messages    []models.Message // cloned
	ext         Extractor
	factSource  string
	skillName   string
	factIDs     []string
	scope       Scope
	seq         uint64 // monotonic sequence for dedup
}

// UpdateQueue serializes and deduplicates memory update operations.
// It replaces fire-and-forget goroutines with a channel-backed worker,
// ensuring that multiple updates for the same session are merged rather
// than racing.
type UpdateQueue struct {
	svc     *Service
	ch      chan updateJob
	done    chan struct{}
	mu      sync.Mutex // protects pendingSeq
	pendingSeq map[string]uint64 // dedup key → latest sequence number
	seq     uint64
}

// newUpdateQueue creates and starts the queue.
func newUpdateQueue(svc *Service, size int) *UpdateQueue {
	if size <= 0 {
		size = defaultQueueSize
	}
	q := &UpdateQueue{
		svc:        svc,
		ch:         make(chan updateJob, size),
		done:       make(chan struct{}),
		pendingSeq: make(map[string]uint64),
	}
	go q.run()
	return q
}

// Close drains the queue and stops the worker. Blocks until all pending
// jobs are processed or the context is cancelled.
func (q *UpdateQueue) Close(ctx context.Context) error {
	close(q.ch)
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// submitTimeout is the maximum time to wait when the queue is full.
const submitTimeout = 2 * time.Second

// submit enqueues a job. If a job with the same dedup key is already pending,
// the new job replaces it (last-writer-wins coalescing).
// Blocks up to submitTimeout if the queue is full to avoid silent data loss.
func (q *UpdateQueue) submit(job updateJob) {
	key := q.dedupKey(job)

	q.mu.Lock()
	q.seq++
	job.seq = q.seq
	if key != "" {
		q.pendingSeq[key] = job.seq
	}
	q.mu.Unlock()

	// Block briefly if full; only drop after timeout.
	select {
	case q.ch <- job:
		return
	case <-time.After(submitTimeout):
		q.svc.logf("update queue submit timeout after %v, dropping job type=%d session=%s", submitTimeout, job.typ, job.sessionID)
		if key != "" {
			q.mu.Lock()
			delete(q.pendingSeq, key)
			q.mu.Unlock()
		}
	}
}

// cancelPending removes any pending dedup entry for the given dedup key,
// so that when the queued job executes, it will be skipped by the worker.
func (q *UpdateQueue) cancelPending(key string) {
	if key == "" {
		return
	}
	q.mu.Lock()
	delete(q.pendingSeq, key)
	q.mu.Unlock()
}

func (q *UpdateQueue) dedupKey(job updateJob) string {
	switch job.typ {
	case jobRecordSkillUsage:
		return "skill:" + job.sessionID + ":" + job.skillName
	case jobIncrementRetrieval:
		// Don't dedup retrieval increments — accumulate them.
		return ""
	default:
		return "update:" + job.sessionID
	}
}

// run is the main worker loop.
func (q *UpdateQueue) run() {
	defer close(q.done)

	for job := range q.ch {
		key := q.dedupKey(job)
		if key != "" {
			q.mu.Lock()
			latest, exists := q.pendingSeq[key]
			if !exists || latest != job.seq {
				// Job cancelled (!exists) or replaced by newer job; skip.
				q.mu.Unlock()
				continue
			}
			delete(q.pendingSeq, key)
			q.mu.Unlock()
		}

		q.execute(job)
	}
}

func (q *UpdateQueue) execute(job updateJob) {
	timeout := q.svc.updateTimeout
	if timeout <= 0 {
		timeout = defaultUpdateTimeout
	}
	switch job.typ {
	case jobUpdateWith:
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := q.svc.UpdateWith(ctx, job.sessionID, job.messages, job.ext); err != nil {
			q.svc.logf("async update with failed for session %s: %v", job.sessionID, err)
		}

	case jobUpdate:
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := q.svc.Update(ctx, job.sessionID, job.messages); err != nil {
			q.svc.logf("async update failed for session %s: %v", job.sessionID, err)
		}

	case jobUpdateWithFactSource:
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := q.svc.UpdateWithFactSource(ctx, job.sessionID, job.messages, job.ext, job.factSource); err != nil {
			q.svc.logf("async update with fact source failed for session %s: %v", job.sessionID, err)
		}

	case jobRecordSkillUsage:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := q.svc.RecordSkillUsage(ctx, job.sessionID, job.skillName); err != nil {
			q.svc.logf("record skill usage failed for session %s skill %s: %v", job.sessionID, job.skillName, err)
		}

	case jobIncrementRetrieval:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.svc.storage.IncrementRetrievalCounts(ctx, job.sessionID, job.factIDs); err != nil {
			q.svc.logf("increment retrieval counts failed for session %s: %v", job.sessionID, err)
		}

	case jobUpdateScopeWithSkill:
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := q.svc.UpdateScopeWithSkillUsage(ctx, job.scope, job.messages, job.ext, job.skillName); err != nil {
			q.svc.logf("async scope+skill update failed for scope %s: %v", job.scope.Key(), err)
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
