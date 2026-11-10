package schedule

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CreateInput is the validated input for Service.Create. Both the REST API
// and the schedule tool build one of these.
type CreateInput struct {
	Name      string
	Prompt    string
	Trigger   Trigger
	Timezone  string
	Options   Options
	CreatedBy CreatedBy
	// State is normally empty (= active); the tool sets pending_approval for
	// nested creations when schedule.allow_nested is on.
	State State
}

// Patch is a partial update. Pointer fields distinguish "not provided" from
// an explicit empty value.
type Patch struct {
	Name     *string
	Prompt   *string
	Trigger  *Trigger
	Timezone *string

	SessionMode     *string
	Model           *string
	Timeout         *string
	Overlap         *string
	Misfire         *string
	Notify          *string
	AnswerTimeout   *string
	OnAnswerTimeout *string

	EndAt          *time.Time
	ClearEndAt     bool
	MaxRuns        *int
	MaxToolCalls   *int
	MaxAutoAnswers *int
}

// Validation is returned by /v1/schedule/validate and by the tool's
// "validate" action.
type Validation struct {
	Trigger  Trigger     `json:"trigger"`
	Timezone string      `json:"timezone"`
	Summary  string      `json:"summary"`
	NextRuns []time.Time `json:"next_runs"`
}

// DueKind distinguishes a slot that is due from a queued Run whose deadline
// has passed; both live in the Scheduler's min-heap.
type DueKind string

const (
	DueSlot     DueKind = "slot"
	DueDeadline DueKind = "deadline"
)

// DueItem is one heap entry.
type DueItem struct {
	At         time.Time
	Kind       DueKind
	ScheduleID string
	RunID      string
}

// Service owns schedule state. All writes are serialized by mu inside this
// process; the Store's UNIQUE constraints guard against a second process.
type Service struct {
	store Store
	cfg   Config
	clock Clock
	log   *slog.Logger

	mu         sync.Mutex
	observer   Observer
	dispatcher Dispatcher
	kick       func()
	mode       string // "active" | "standby"

	// deferred holds interrupted Runs whose recovery was postponed because
	// the previous lease holder may still be executing them (run ID → the
	// time after which they must be reconciled). Guarded by mu.
	deferred map[string]time.Time
}

// ServiceOption configures optional collaborators.
type ServiceOption func(*Service)

// WithClock injects a controllable clock (tests).
func WithClock(c Clock) ServiceOption { return func(s *Service) { s.clock = c } }

// WithObserver attaches the global event publisher.
func WithObserver(o Observer) ServiceOption { return func(s *Service) { s.observer = o } }

// WithLogger attaches a logger.
func WithLogger(l *slog.Logger) ServiceOption { return func(s *Service) { s.log = l } }

// NewService builds a Service and migrates its Store.
func NewService(store Store, cfg Config, opts ...ServiceOption) (*Service, error) {
	if store == nil {
		return nil, errors.New("schedule: Store is required")
	}
	d := DefaultConfig()
	if cfg.Timezone == "" {
		cfg.Timezone = d.Timezone
	}
	if cfg.Confirm == "" {
		cfg.Confirm = d.Confirm
	}
	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = d.ConfirmTimeout
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = d.MinInterval
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = d.DefaultTimeout
	}
	if cfg.DefaultAnswerTimeout <= 0 {
		cfg.DefaultAnswerTimeout = d.DefaultAnswerTimeout
	}
	if cfg.MaxSchedules <= 0 {
		cfg.MaxSchedules = d.MaxSchedules
	}
	if cfg.MaxConcurrentRuns <= 0 {
		cfg.MaxConcurrentRuns = d.MaxConcurrentRuns
	}
	if cfg.MaxRunsPerDay <= 0 {
		cfg.MaxRunsPerDay = d.MaxRunsPerDay
	}
	if cfg.RunHistory <= 0 {
		cfg.RunHistory = d.RunHistory
	}
	if cfg.DefaultMaxAutoAnswers <= 0 {
		cfg.DefaultMaxAutoAnswers = d.DefaultMaxAutoAnswers
	}
	s := &Service{
		store: store, cfg: cfg, clock: RealClock(), log: slog.Default(),
		mode: "active", deferred: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := store.Migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// Config returns the effective (defaulted) configuration.
func (s *Service) Config() Config { return s.cfg }

// Mode reports "active" (this process executes tasks) or "standby".
func (s *Service) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *Service) setMode(m string) {
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
}

// SetDispatcher attaches the Fire/Cancel implementation.
func (s *Service) SetDispatcher(d Dispatcher) {
	s.mu.Lock()
	s.dispatcher = d
	s.mu.Unlock()
}

// SetObserver attaches the event publisher (can be called after construction).
func (s *Service) SetObserver(o Observer) {
	s.mu.Lock()
	s.observer = o
	s.mu.Unlock()
}

// SetKick registers the Scheduler's wake-up callback.
func (s *Service) SetKick(f func()) {
	s.mu.Lock()
	s.kick = f
	s.mu.Unlock()
}

// Kick wakes the Scheduler after an external change. It never blocks.
func (s *Service) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kickLocked()
}

// kickLocked is the variant for callers already holding mu.
func (s *Service) kickLocked() {
	if s.kick != nil {
		s.kick()
	}
}

// Decorate fills the computed fields a client expects on every response.
func (s *Service) Decorate(v Schedule) Schedule {
	v.Summary = v.Trigger.Summary(v.Timezone)
	v.NextRuns = v.Trigger.NextRuns(s.now(), 3)
	return v
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// ─────────────────────────── CRUD ───────────────────────────

// Create validates and persists a new task.
func (s *Service) Create(ctx context.Context, in CreateInput) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	tz := in.Timezone
	if tz == "" {
		tz = s.cfg.ConfiguredTimezone()
	}
	if _, err := Location(tz); err != nil {
		return Schedule{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 80 {
		return Schedule{}, NewError(CodeBadRequest, "name is required and must be at most 80 characters")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" || len(prompt) > 8<<10 {
		return Schedule{}, NewError(CodeBadRequest, "prompt is required and must be at most 8 KiB")
	}
	tr := in.Trigger
	if err := tr.Normalize(tz, now, s.cfg.MinInterval, false); err != nil {
		return Schedule{}, err
	}
	opts, err := s.normalizeOptions(in.Options, now)
	if err != nil {
		return Schedule{}, err
	}
	state := StateActive
	if in.State == StatePendingApproval {
		state = StatePendingApproval
	}
	if err := s.checkScheduleLimitLocked(ctx); err != nil {
		return Schedule{}, err
	}
	createdBy := in.CreatedBy
	if createdBy.Kind == "" {
		createdBy.Kind = "user"
	}
	v := Schedule{
		ID: newID("sch_"), Name: name, Prompt: prompt, Trigger: tr, Timezone: tz,
		Options: opts, State: state, CreatedBy: createdBy,
		CreatedAt: now, UpdatedAt: now,
	}
	if state == StateActive {
		v.NextRunAt = s.firstFreeSlotLocked(ctx, v, now)
	}
	if err := s.store.InsertSchedule(ctx, v); err != nil {
		return Schedule{}, err
	}
	s.changed(v, ChangeCreated)
	s.kickLocked()
	return s.Decorate(v), nil
}

// List returns schedules, newest-created first.
func (s *Service) List(ctx context.Context, state string, limit int) ([]Schedule, error) {
	list, err := s.store.ListSchedules(ctx, ScheduleFilter{State: State(state), Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, 0, len(list))
	for _, v := range list {
		out = append(out, s.Decorate(v))
	}
	return out, nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, id string) (Schedule, error) {
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	if v.State == StateDeleted {
		return Schedule{}, NewError(CodeNotFound, "schedule %s was deleted", id)
	}
	return s.Decorate(v), nil
}

// Update applies a partial patch and recomputes next_run_at when the rule or
// timezone changed.
func (s *Service) Update(ctx context.Context, id string, p Patch) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	if v.State == StateDeleted || v.State == StateCompleted {
		return Schedule{}, NewError(CodeInvalidState, "schedule %s is %s", id, v.State)
	}
	ruleChanged := p.Trigger != nil || p.Timezone != nil
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" || len([]rune(name)) > 80 {
			return Schedule{}, NewError(CodeBadRequest, "name is required and must be at most 80 characters")
		}
		v.Name = name
	}
	if p.Prompt != nil {
		prompt := strings.TrimSpace(*p.Prompt)
		if prompt == "" || len(prompt) > 8<<10 {
			return Schedule{}, NewError(CodeBadRequest, "prompt is required and must be at most 8 KiB")
		}
		v.Prompt = prompt
	}
	if p.Timezone != nil {
		if _, err := Location(*p.Timezone); err != nil {
			return Schedule{}, err
		}
		v.Timezone = *p.Timezone
	}
	if p.Trigger != nil {
		tr := *p.Trigger
		if err := tr.Normalize(v.Timezone, s.now(), s.cfg.MinInterval, false); err != nil {
			return Schedule{}, err
		}
		v.Trigger = tr
	}
	applyPatch(&v.Options, p)
	opts, err := s.normalizeOptions(v.Options, s.now())
	if err != nil {
		return Schedule{}, err
	}
	v.Options = opts
	if ruleChanged {
		if v.State == StateActive {
			v.NextRunAt = s.firstFreeSlotLocked(ctx, v, s.now())
		} else {
			v.NextRunAt = nil
		}
	}
	v.UpdatedAt = s.now()
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return Schedule{}, err
	}
	s.kickLocked()
	return s.Decorate(v), nil
}

// Pause stops future slots; already running Runs keep going.
func (s *Service) Pause(ctx context.Context, id string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.requireMutable(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	v.State, v.NextRunAt = StatePaused, nil
	v.UpdatedAt = s.now()
	for _, r := range s.pendingRunsLocked(ctx, id) {
		r.Status, r.ErrorCode = RunSkipped, "paused"
		r.EndedAt = timePtr(s.now())
		_ = s.updateRunLocked(ctx, r)
	}
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return Schedule{}, err
	}
	s.kickLocked()
	return s.Decorate(v), nil
}

// checkOnceNotPast rejects activating a one-shot whose trigger time has
// elapsed: firstFreeSlotLocked would return nil, leaving the task active
// forever with no Run and no completion (review M1).
func (s *Service) checkOnceNotPast(v Schedule, now time.Time) error {
	if v.Trigger.Type != TriggerOnce {
		return nil
	}
	at, ok, err := v.Trigger.nextAfter(now.Add(-time.Nanosecond))
	if err != nil || !ok || !at.After(now) {
		return NewError(CodeTimeInPast, "the scheduled time has passed; update the task with a new 'at'")
	}
	return nil
}

// Resume recomputes the next slot from "now"; paused slots are not replayed.
func (s *Service) Resume(ctx context.Context, id string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	if v.State != StatePaused {
		return Schedule{}, NewError(CodeInvalidState, "schedule %s is %s, not paused", id, v.State)
	}
	now := s.now()
	if err := s.checkOnceNotPast(v, now); err != nil {
		return Schedule{}, err
	}
	v.State = StateActive
	v.NextRunAt = s.firstFreeSlotLocked(ctx, v, now)
	v.UpdatedAt = now
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return Schedule{}, err
	}
	s.kickLocked()
	return s.Decorate(v), nil
}

// Approve activates a pending_approval task created by the model.
func (s *Service) Approve(ctx context.Context, id string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	if v.State != StatePendingApproval {
		return Schedule{}, NewError(CodeInvalidState, "schedule %s is %s, not pending_approval", id, v.State)
	}
	now := s.now()
	if err := s.checkOnceNotPast(v, now); err != nil {
		return Schedule{}, err
	}
	v.State = StateActive
	v.NextRunAt = s.firstFreeSlotLocked(ctx, v, now)
	v.UpdatedAt = now
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return Schedule{}, err
	}
	s.kickLocked()
	return s.Decorate(v), nil
}

// Delete soft-deletes a task. Active Runs must be cancelled explicitly.
func (s *Service) Delete(ctx context.Context, id string, cancelActive bool) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	if v.State == StateDeleted {
		return v, nil
	}
	active := s.activeRunsLocked(ctx, id)
	if len(active) > 0 && !cancelActive {
		return Schedule{}, NewError(CodeRunInProgress, "schedule %s has an active run; pass cancel=true", id)
	}
	for _, r := range active {
		now := s.now()
		r.CancelRequestedAt = &now
		if s.dispatcher != nil {
			_ = s.dispatcher.Cancel(r, CancelClient)
		}
		_ = s.updateRunLocked(ctx, r)
	}
	for _, r := range s.pendingRunsLocked(ctx, id) {
		r.Status, r.ErrorCode = RunCancelled, "schedule_deleted"
		r.EndedAt = timePtr(s.now())
		_ = s.updateRunLocked(ctx, r)
	}
	now := s.now()
	v.State, v.NextRunAt, v.DeletedAt, v.UpdatedAt = StateDeleted, nil, &now, now
	if err := s.updateScheduleLocked(ctx, v, ChangeDeleted); err != nil {
		return Schedule{}, err
	}
	s.kickLocked()
	return v, nil
}

// Validate checks a trigger without creating anything.
func (s *Service) Validate(ctx context.Context, tr Trigger, tz string, n int) (Validation, error) {
	if tz == "" {
		tz = s.cfg.ConfiguredTimezone()
	}
	if _, err := Location(tz); err != nil {
		return Validation{}, err
	}
	if err := tr.Normalize(tz, s.now(), s.cfg.MinInterval, false); err != nil {
		return Validation{}, err
	}
	if n <= 0 {
		n = 3
	}
	return Validation{
		Trigger: tr, Timezone: tz,
		Summary:  tr.Summary(tz),
		NextRuns: tr.NextRuns(s.now(), n),
	}, nil
}

// RunNow triggers one manual execution, deduplicated by request_id.
func (s *Service) RunNow(ctx context.Context, id, requestID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Run{}, notFound(err, "schedule "+id)
	}
	if v.State != StateActive {
		return Run{}, NewError(CodeInvalidState, "schedule %s is %s", id, v.State)
	}
	if requestID == "" {
		requestID = newID("req_")
	}
	if existing, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: id, RequestID: requestID, Limit: 1}); len(existing) > 0 {
		return existing[0], nil
	}
	if err := s.checkDailyLimitLocked(ctx, id); err != nil {
		return Run{}, err
	}
	now := s.now()
	r := Run{
		ID: newID("run_"), ScheduleID: id, Seq: s.nextSeqLocked(ctx, id),
		Trigger: FireManual, ScheduledFor: now, RequestID: requestID, Status: RunPending,
	}
	active := s.activeRunsLocked(ctx, id)
	if len(active) > 0 {
		switch v.Options.Overlap {
		case OverlapQueue, OverlapReplace:
			r.QueuedReason = "overlap"
			r.QueueDeadline = timePtr(now.Add(s.timeoutOf(v)))
			if v.Options.Overlap == OverlapReplace && s.dispatcher != nil {
				_ = s.dispatcher.Cancel(active[0], CancelReplaced)
			}
		default:
			return Run{}, NewError(CodeRunInProgress, "schedule %s already has a run in progress", id)
		}
	}
	if err := s.insertRunLocked(ctx, r); err != nil {
		return Run{}, err
	}
	s.tryStartLocked(ctx, r, v)
	s.kickLocked()
	return s.store.GetRun(ctx, r.ID)
}

// Skip occupies the next slot with a skipped Run so recovery never replays it.
func (s *Service) Skip(ctx context.Context, id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Run{}, notFound(err, "schedule "+id)
	}
	if v.State != StateActive {
		return Run{}, NewError(CodeInvalidState, "schedule %s is %s", id, v.State)
	}
	now := s.now()
	slot := v.NextRunAt
	if slot == nil {
		slot = s.firstFreeSlotLocked(ctx, v, now)
	}
	if slot == nil {
		return Run{}, NewError(CodeInvalidState, "schedule %s has no future slot to skip", id)
	}
	r := Run{
		ID: newID("run_"), ScheduleID: id, Seq: s.nextSeqLocked(ctx, id),
		Trigger: FireSkip, Slot: slot, ScheduledFor: *slot,
		Status: RunSkipped, ErrorCode: "user_skip", EndedAt: &now,
	}
	if err := s.insertRunLocked(ctx, r); err != nil {
		return Run{}, err
	}
	v.NextRunAt = s.firstFreeSlotLocked(ctx, v, *slot)
	v.UpdatedAt = now
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return Run{}, err
	}
	s.kickLocked()
	return r, nil
}

// Runs returns a task's history, newest first.
func (s *Service) Runs(ctx context.Context, id string, limit int) ([]Run, error) {
	if _, err := s.store.GetSchedule(ctx, id); err != nil {
		return nil, notFound(err, "schedule "+id)
	}
	if limit <= 0 {
		limit = 20
	}
	return s.store.FindRun(ctx, RunFilter{ScheduleID: id, Limit: limit})
}

// GetRun returns one execution by id.
func (s *Service) GetRun(ctx context.Context, runID string) (Run, error) {
	r, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, notFound(err, "run "+runID)
	}
	return r, nil
}

// CancelRun asks the Dispatcher to cancel a running execution.
func (s *Service) CancelRun(ctx context.Context, runID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, notFound(err, "run "+runID)
	}
	if r.Status.Terminal() {
		return r, nil
	}
	now := s.now()
	if r.Status == RunPending {
		r.Status, r.ErrorCode, r.EndedAt = RunCancelled, "client_cancel", &now
	} else if s.mode != "active" || s.dispatcher == nil {
		// Standby processes cannot cancel a turn running in the holder;
		// record the request and let the holder relay it on its next tick.
		r.CancelRequestedAt = &now
		if s.mode == "active" {
			r.Status, r.ErrorCode, r.EndedAt = RunCancelled, "no_dispatcher", &now
		}
	} else {
		r.CancelRequestedAt = &now
		if err := s.dispatcher.Cancel(r, CancelClient); err != nil {
			r.Status, r.ErrorCode, r.EndedAt = RunCancelled, "cancel_failed", &now
		}
	}
	if err := s.updateRunLocked(ctx, r); err != nil {
		return Run{}, err
	}
	s.kickLocked()
	return r, nil
}

// UpdateRunStatus records an intermediate Run state (waiting_input etc.).
func (s *Service) UpdateRunStatus(ctx context.Context, runID string, status RunStatus) (Run, error) {
	return s.UpdateRunInteraction(ctx, runID, status, 0)
}

// UpdateRunInteraction records the question lifecycle of a running Run:
// running ↔ waiting_input and the number of auto-answers. status may be empty
// when only autoDelta needs recording; terminal Runs are left untouched so a
// late question callback cannot overwrite FinishRun (design §7.3).
func (s *Service) UpdateRunInteraction(ctx context.Context, runID string, status RunStatus, autoDelta int) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, notFound(err, "run "+runID)
	}
	if r.Status.Terminal() {
		return r, nil
	}
	changed := false
	switch status {
	case RunWaitingInput:
		if r.Status == RunRunning {
			r.Status = RunWaitingInput
			changed = true
		}
	case RunRunning:
		if r.Status == RunWaitingInput {
			r.Status = RunRunning
			changed = true
		}
	}
	if autoDelta > 0 {
		r.AutoAnswers += autoDelta
		changed = true
	}
	if !changed {
		return r, nil
	}
	if err := s.updateRunLocked(ctx, r); err != nil {
		return Run{}, err
	}
	return r, nil
}

// FinishRun writes a terminal state reported by the Dispatcher's turn_end
// callback. It is idempotent: a second report for the same Run is ignored.
func (s *Service) FinishRun(ctx context.Context, runID string, out Outcome) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, notFound(err, "run "+runID)
	}
	if r.Status.Terminal() {
		return r, nil
	}
	now := s.now()
	status := out.Status
	if status == "" {
		status = RunFailed
	}
	r.Status = status
	r.EndedAt = &now
	if r.StartedAt == nil {
		r.StartedAt = &now
	}
	r.Output = truncateBytes(out.Output, 16<<10)
	r.Error, r.ErrorCode, r.ToolCalls, r.Usage = out.Error, out.ErrorCode, out.ToolCalls, out.Usage
	if err := s.updateRunLocked(ctx, r); err != nil {
		return Run{}, err
	}
	s.finishEffectsLocked(ctx, r)
	s.tryStartPendingLocked(ctx)
	s.kickLocked()
	return r, nil
}

// SetSessionID pins the dedicated session of a task (used by the Dispatcher
// when it lazily creates one).
func (s *Service) SetSessionID(ctx context.Context, id, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil || v.SessionID == sessionID {
		return err
	}
	v.SessionID, v.UpdatedAt = sessionID, s.now()
	return s.updateScheduleLocked(ctx, v, ChangeUpdated)
}

// ─────────────────── scheduler-facing operations ───────────────────

// FireSlot is called by the Scheduler when a task's next_run_at is due. It
// persists the Run first and only then calls the Dispatcher; tryStartLocked
// additionally reserves the Run as running before the turn starts, so a
// crash can lose a run but can never duplicate one.
func (s *Service) FireSlot(ctx context.Context, id string, slot time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != "active" {
		return nil // standby process may read/write, but never fires
	}
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil || v.State != StateActive {
		return nil
	}
	if taken, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: id, Slot: &slot, Limit: 1}); len(taken) > 0 {
		v.NextRunAt = s.firstFreeSlotLocked(ctx, v, slot)
		_ = s.updateScheduleLocked(ctx, v, ChangeUpdated)
		return nil
	}
	now := s.now()
	if err := s.checkDailyLimitLocked(ctx, id); err != nil {
		// Occupy the slot so recovery does not treat it as missed.
		limitRun := Run{
			ID: newID("run_"), ScheduleID: id, Seq: s.nextSeqLocked(ctx, id),
			Trigger: FireTimer, Slot: &slot, ScheduledFor: slot,
			Status: RunSkipped, ErrorCode: "limit_exceeded", EndedAt: &now,
		}
		if insErr := s.insertRunLocked(ctx, limitRun); insErr != nil && !errors.Is(insErr, ErrConflict) {
			s.log.Warn("schedule: record limit_exceeded run", "schedule", id, "err", insErr)
		}
		v.NextRunAt = s.firstFreeSlotLocked(ctx, v, slot)
		v.UpdatedAt = now
		_ = s.updateScheduleLocked(ctx, v, ChangeUpdated)
		s.kickLocked()
		return nil
	}
	r := Run{
		ID: newID("run_"), ScheduleID: id, Seq: s.nextSeqLocked(ctx, id),
		Trigger: FireTimer, Slot: &slot, ScheduledFor: slot, Status: RunPending,
	}
	if err := s.insertRunLocked(ctx, r); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil // another process won the slot; UNIQUE did its job
		}
		// Only a UNIQUE conflict means "already done". A disk/lock failure
		// must be visible: swallowing it leaves the slot due forever with no
		// log trace (review L1).
		s.log.Warn("schedule: insert run", "schedule", id, "err", err)
		return err
	}
	v.NextRunAt = s.firstFreeSlotLocked(ctx, v, slot)
	v.UpdatedAt = now
	if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
		return err
	}
	if active := s.activeRunsLocked(ctx, id); len(active) > 0 {
		switch v.Options.Overlap {
		case OverlapSkip:
			r.Status, r.ErrorCode, r.EndedAt = RunSkipped, "overlap", &now
		case OverlapQueue:
			queued := 0
			for _, p := range s.pendingRunsLocked(ctx, id) {
				if p.ID != r.ID && p.QueuedReason == "overlap" {
					queued++
				}
			}
			if queued > 0 {
				var firstQueued string
				for _, p := range s.pendingRunsLocked(ctx, id) {
					if p.ID != r.ID && p.QueuedReason == "overlap" {
						firstQueued = p.ID
						break
					}
				}
				r.Status, r.ErrorCode, r.EndedAt = RunSkipped, "coalesced_into="+firstQueued, &now
			} else {
				r.QueuedReason = "overlap"
				r.QueueDeadline = timePtr(slot.Add(s.timeoutOf(v)))
			}
		case OverlapReplace:
			if s.dispatcher != nil {
				_ = s.dispatcher.Cancel(active[0], CancelReplaced)
			}
			r.QueuedReason = "overlap"
			r.QueueDeadline = timePtr(slot.Add(s.timeoutOf(v)))
		}
		if err := s.updateRunLocked(ctx, r); err != nil {
			return err
		}
	}
	if r.Status == RunPending {
		s.tryStartLocked(ctx, r, v)
	}
	s.kickLocked()
	return nil
}

// ExpireQueued marks pending Runs whose queue deadline has passed as skipped.
func (s *Service) ExpireQueued(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.allPendingLocked(ctx) {
		if r.QueueDeadline == nil || r.QueueDeadline.After(now) {
			continue
		}
		r.Status, r.ErrorCode, r.EndedAt = RunSkipped, "queue_timeout", timePtr(now)
		if err := s.updateRunLocked(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// TryStartPending re-attempts every queued Run; called after a Run finishes,
// a turn ends, or a Scheduler wake-up.
func (s *Service) TryStartPending(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tryStartPendingLocked(ctx)
	return nil
}

// ProcessCancelRequests relays cancel requests written by a standby bot to
// the Dispatcher of the lease holder. The terminal state still arrives via
// FinishRun, as for a local cancellation.
func (s *Service) ProcessCancelRequests(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != "active" || s.dispatcher == nil {
		return nil
	}
	for _, r := range s.findRunsLocked(ctx, RunFilter{Statuses: activeStatuses, CancelRequested: true}) {
		if err := s.dispatcher.Cancel(r, CancelClient); err != nil {
			s.log.Warn("schedule: relay cancel", "run", r.ID, "err", err)
		}
	}
	return nil
}

// Recover runs at startup: interrupted Runs become failed, expired queues
// become skipped, and missed slots are recorded (never replayed in P1).
func (s *Service) Recover(ctx context.Context) error {
	return s.recoverWithGrace(ctx, 0)
}

// RecoverAfterTakeover is used when a standby process acquires an expired
// lease. The previous holder may still be alive (a transient renew error),
// so Runs started within leaseTTL + their execution timeout are deferred
// instead of being declared dead while their turn is still in flight
// (review H1).
func (s *Service) RecoverAfterTakeover(ctx context.Context, leaseTTL time.Duration) error {
	return s.recoverWithGrace(ctx, leaseTTL)
}

func (s *Service) recoverWithGrace(ctx context.Context, leaseTTL time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deferred == nil {
		s.deferred = map[string]time.Time{}
	}
	now := s.now()
	for _, r := range s.findRunsLocked(ctx, RunFilter{Statuses: activeStatuses}) {
		if deadline, ok := s.interruptedDeadlineLocked(ctx, r, now, leaseTTL); ok {
			s.deferred[r.ID] = deadline
			continue
		}
		delete(s.deferred, r.ID)
		r.Status, r.ErrorCode, r.Error = RunFailed, "bot_restarted", "bot restarted while run was active"
		r.EndedAt = timePtr(now)
		if err := s.updateRunLocked(ctx, r); err != nil {
			return err
		}
		// A one-shot whose only run was interrupted must not stay active
		// forever with no next slot.
		s.finishEffectsLocked(ctx, r)
	}
	for _, r := range s.allPendingLocked(ctx) {
		if r.QueueDeadline != nil && !r.QueueDeadline.After(now) {
			r.Status, r.ErrorCode, r.EndedAt = RunSkipped, "queue_timeout", timePtr(now)
			_ = s.updateRunLocked(ctx, r)
		}
	}
	list, err := s.store.ListSchedules(ctx, ScheduleFilter{State: StateActive})
	if err != nil {
		return err
	}
	for _, v := range list {
		if v.NextRunAt == nil || !v.NextRunAt.Before(now) {
			continue
		}
		var last time.Time
		count := 0
		base := v.NextRunAt.Add(-time.Nanosecond)
		for count < 10000 {
			slot, ok, err := v.Trigger.nextAfter(base)
			if err != nil || !ok || slot.After(now) {
				break
			}
			last, count, base = slot, count+1, slot
		}
		if count > 0 {
			r := Run{
				ID: newID("run_"), ScheduleID: v.ID, Seq: s.nextSeqLocked(ctx, v.ID),
				Trigger: FireTimer, Slot: &last, ScheduledFor: last, Coalesced: count - 1,
				Status: RunMissed, ErrorCode: "misfire_not_supported", EndedAt: &now,
			}
			if err := s.insertRunLocked(ctx, r); err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		}
		v.NextRunAt = s.firstFreeSlotLocked(ctx, v, now)
		if v.Trigger.Type == TriggerOnce && count > 0 {
			// A missed one-shot has no future slot; leaving it active would
			// strand it forever.
			v.State = StateCompleted
		}
		v.UpdatedAt = now
		if err := s.updateScheduleLocked(ctx, v, ChangeUpdated); err != nil {
			return err
		}
	}
	return nil
}

// interruptedDeadlineLocked reports whether an interrupted Run may still be
// owned by a live lease holder, and when it must be reconciled if not. A Run
// without StartedAt has no anchor and is reconciled immediately.
func (s *Service) interruptedDeadlineLocked(ctx context.Context, r Run, now time.Time, leaseTTL time.Duration) (time.Time, bool) {
	if leaseTTL <= 0 || r.StartedAt == nil {
		return time.Time{}, false
	}
	grace := leaseTTL
	if v, err := s.store.GetSchedule(ctx, r.ScheduleID); err == nil {
		grace += s.timeoutOf(v)
	}
	deadline := r.StartedAt.Add(grace)
	return deadline, deadline.After(now)
}

// Reconcile fails deferred interrupted Runs whose grace period has expired.
// The lease holder calls it on every sync tick; a late FinishRun wins
// because terminal Runs are skipped.
func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deferred) == 0 {
		return nil
	}
	now := s.now()
	for id, deadline := range s.deferred {
		if deadline.After(now) {
			continue
		}
		r, err := s.store.GetRun(ctx, id)
		if err != nil || r.Status.Terminal() {
			delete(s.deferred, id)
			continue
		}
		r.Status, r.ErrorCode, r.Error = RunFailed, "bot_restarted", "bot restarted while run was active"
		r.EndedAt = timePtr(now)
		if err := s.updateRunLocked(ctx, r); err != nil {
			return err
		}
		delete(s.deferred, id)
		s.finishEffectsLocked(ctx, r)
	}
	return nil
}

// DueItems rebuilds the Scheduler's heap from persisted state.
func (s *Service) DueItems(ctx context.Context) ([]DueItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.store.ListSchedules(ctx, ScheduleFilter{State: StateActive})
	if err != nil {
		return nil, err
	}
	out := make([]DueItem, 0, len(list))
	for _, v := range list {
		if v.NextRunAt != nil {
			out = append(out, DueItem{At: *v.NextRunAt, Kind: DueSlot, ScheduleID: v.ID})
		}
	}
	for _, r := range s.allPendingLocked(ctx) {
		if r.QueueDeadline != nil {
			out = append(out, DueItem{At: *r.QueueDeadline, Kind: DueDeadline, ScheduleID: r.ScheduleID, RunID: r.ID})
		}
	}
	return out, nil
}

// ─────────────────────────── internals ───────────────────────────

func (s *Service) checkScheduleLimitLocked(ctx context.Context) error {
	list, err := s.store.ListSchedules(ctx, ScheduleFilter{})
	if err != nil {
		return err
	}
	n := 0
	for _, v := range list {
		if v.State != StateDeleted && v.State != StateCompleted {
			n++
		}
	}
	if n >= s.cfg.MaxSchedules {
		return NewError(CodeLimitExceeded, "at most %d active schedules are allowed", s.cfg.MaxSchedules)
	}
	return nil
}

func (s *Service) checkDailyLimitLocked(ctx context.Context, id string) error {
	since := s.now().Add(-24 * time.Hour)
	runs, err := s.store.FindRun(ctx, RunFilter{ScheduleID: id, Since: &since})
	if err != nil {
		return err
	}
	if len(runs) >= s.cfg.MaxRunsPerDay {
		return NewError(CodeLimitExceeded, "schedule %s reached %d runs in 24h", id, s.cfg.MaxRunsPerDay)
	}
	return nil
}

func (s *Service) normalizeOptions(o Options, now time.Time) (Options, error) {
	if o.SessionMode == "" {
		o.SessionMode = SessionDedicated
	}
	if o.Overlap == "" {
		o.Overlap = OverlapSkip
	}
	if o.Misfire == "" {
		o.Misfire = MisfireRunOnce
	}
	if o.Notify == "" {
		o.Notify = NotifyAlways
	}
	switch o.SessionMode {
	case SessionDedicated, SessionNew, SessionOrigin:
	default:
		return o, NewError(CodeBadRequest, "invalid session_mode %q", o.SessionMode)
	}
	switch o.Overlap {
	case OverlapSkip, OverlapQueue, OverlapReplace:
	default:
		return o, NewError(CodeBadRequest, "invalid overlap %q", o.Overlap)
	}
	switch o.Misfire {
	case MisfireRunOnce, MisfireSkip:
	default:
		return o, NewError(CodeBadRequest, "invalid misfire %q", o.Misfire)
	}
	switch o.Notify {
	case NotifyAlways, NotifyOnFailure, NotifyNever:
	default:
		return o, NewError(CodeBadRequest, "invalid notify %q", o.Notify)
	}
	if o.OnAnswerTimeout == "" {
		o.OnAnswerTimeout = OnAnswerTimeoutAssume
	}
	switch o.OnAnswerTimeout {
	case OnAnswerTimeoutAssume, OnAnswerTimeoutFail:
	default:
		return o, NewError(CodeBadRequest, "invalid on_answer_timeout %q", o.OnAnswerTimeout)
	}
	timeout := s.cfg.DefaultTimeout
	if o.Timeout != "" {
		d, err := parsePositiveDuration(o.Timeout)
		if err != nil {
			return o, NewError(CodeBadRequest, "invalid timeout %q", o.Timeout)
		}
		timeout = d
	}
	if timeout > 4*time.Hour {
		return o, NewError(CodeBadRequest, "timeout may not exceed 4h")
	}
	o.Timeout = timeout.String()
	if o.AnswerTimeout == "" {
		o.AnswerTimeout = s.cfg.DefaultAnswerTimeout.String()
	} else if _, err := parsePositiveDuration(o.AnswerTimeout); err != nil {
		return o, NewError(CodeBadRequest, "invalid answer_timeout %q", o.AnswerTimeout)
	}
	if o.AutoAnswers == 0 {
		o.AutoAnswers = s.cfg.DefaultMaxAutoAnswers
	}
	if o.MaxRuns < 0 || o.MaxToolCalls < 0 || o.AutoAnswers < 0 {
		return o, NewError(CodeBadRequest, "max_runs, max_tool_calls and max_auto_answers must be non-negative")
	}
	if o.EndAt != nil && !o.EndAt.After(now) {
		return o, NewError(CodeTimeInPast, "end_at must be in the future")
	}
	return o, nil
}

func applyPatch(o *Options, p Patch) {
	if p.SessionMode != nil {
		o.SessionMode = *p.SessionMode
	}
	if p.Model != nil {
		o.Model = *p.Model
	}
	if p.Timeout != nil {
		o.Timeout = *p.Timeout
	}
	if p.Overlap != nil {
		o.Overlap = *p.Overlap
	}
	if p.Misfire != nil {
		o.Misfire = *p.Misfire
	}
	if p.Notify != nil {
		o.Notify = *p.Notify
	}
	if p.EndAt != nil {
		t := *p.EndAt
		o.EndAt = &t
	}
	if p.ClearEndAt {
		o.EndAt = nil
	}
	if p.MaxRuns != nil {
		o.MaxRuns = *p.MaxRuns
	}
	if p.MaxToolCalls != nil {
		o.MaxToolCalls = *p.MaxToolCalls
	}
	if p.AnswerTimeout != nil {
		o.AnswerTimeout = *p.AnswerTimeout
	}
	if p.OnAnswerTimeout != nil {
		o.OnAnswerTimeout = *p.OnAnswerTimeout
	}
	if p.MaxAutoAnswers != nil {
		o.AutoAnswers = *p.MaxAutoAnswers
	}
}

func (s *Service) timeoutOf(v Schedule) time.Duration {
	d, err := time.ParseDuration(v.Options.Timeout)
	if err != nil || d <= 0 {
		return s.cfg.DefaultTimeout
	}
	return d
}

// firstFreeSlotLocked finds the first slot after `after` that has no Run yet.
func (s *Service) firstFreeSlotLocked(ctx context.Context, v Schedule, after time.Time) *time.Time {
	cur := after
	for i := 0; i < 1000; i++ {
		slot, ok, err := v.Trigger.nextAfter(cur)
		if err != nil || !ok {
			return nil
		}
		if v.Options.EndAt != nil && slot.After(*v.Options.EndAt) {
			return nil
		}
		taken, err := s.store.FindRun(ctx, RunFilter{ScheduleID: v.ID, Slot: &slot, Limit: 1})
		if err == nil && len(taken) == 0 {
			return &slot
		}
		cur = slot
	}
	return nil
}

func (s *Service) nextSeqLocked(ctx context.Context, id string) int {
	runs, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: id, Limit: 1})
	if len(runs) == 0 {
		return 1
	}
	return runs[0].Seq + 1
}

func (s *Service) findRunsLocked(ctx context.Context, f RunFilter) []Run {
	runs, err := s.store.FindRun(ctx, f)
	if err != nil {
		s.log.Warn("schedule: find runs", "err", err)
		return nil
	}
	return runs
}

func (s *Service) activeRunsLocked(ctx context.Context, id string) []Run {
	return s.findRunsLocked(ctx, RunFilter{ScheduleID: id, Statuses: activeStatuses})
}

func (s *Service) pendingRunsLocked(ctx context.Context, id string) []Run {
	return s.findRunsLocked(ctx, RunFilter{ScheduleID: id, Statuses: []RunStatus{RunPending}, Order: "scheduled_for_asc"})
}

func (s *Service) allPendingLocked(ctx context.Context) []Run {
	return s.findRunsLocked(ctx, RunFilter{Statuses: []RunStatus{RunPending}, Order: "scheduled_for_asc"})
}

func (s *Service) globalActiveLocked(ctx context.Context) int {
	return len(s.findRunsLocked(ctx, RunFilter{Statuses: activeStatuses}))
}

func (s *Service) tryStartPendingLocked(ctx context.Context) {
	for _, r := range s.allPendingLocked(ctx) {
		v, err := s.store.GetSchedule(ctx, r.ScheduleID)
		if err != nil || v.State != StateActive {
			continue
		}
		s.tryStartLocked(ctx, r, v)
	}
}

// tryStartLocked is idempotent and never waits: every reason it cannot start
// is recorded on the Run as a pending state, and a later wake-up retries.
func (s *Service) tryStartLocked(ctx context.Context, r Run, v Schedule) {
	if r.Status != RunPending || s.mode == "standby" || s.dispatcher == nil {
		return
	}
	now := s.now()
	if len(s.activeRunsLocked(ctx, v.ID)) > 0 {
		r.QueuedReason = "overlap"
	} else if s.globalActiveLocked(ctx) >= s.cfg.MaxConcurrentRuns {
		r.QueuedReason = "concurrency"
	} else {
		// Reserve the Run as "running" BEFORE starting the turn. If the
		// process dies between Fire and the final write, recovery sees a
		// running (not pending) Run and cannot replay the same message into
		// the dedicated session. The quiet write avoids a spurious
		// run_started event when Fire then returns ErrBusy (review M3).
		origDeadline := r.QueueDeadline
		r.Status, r.StartedAt = RunRunning, &now
		r.QueuedReason, r.QueueDeadline = "", nil
		if err := s.store.UpdateRun(ctx, r); err != nil {
			s.log.Warn("schedule: reserve run before dispatch", "run", r.ID, "err", err)
			return
		}
		sid, tid, err := s.dispatcher.Fire(ctx, v, r)
		switch {
		case err == nil:
			r.SessionID, r.TurnID = sid, tid
			// The Dispatcher recreates a deleted dedicated session lazily;
			// pin the new ID on EVERY run, not just the first one, so the
			// next trigger does not start from a stale session again
			// (review H2).
			if v.Options.SessionMode == SessionDedicated && sid != "" && v.SessionID != sid {
				v.SessionID, v.UpdatedAt = sid, now
				_ = s.updateScheduleLocked(ctx, v, ChangeUpdated)
			}
		case errors.Is(err, ErrBusy):
			r.Status, r.StartedAt = RunPending, nil
			r.QueuedReason, r.QueueDeadline = "session_busy", origDeadline
		default:
			r.Status, r.EndedAt = RunFailed, &now
			r.Error, r.ErrorCode = err.Error(), "dispatch_failed"
			s.log.Warn("schedule: dispatch failed", "run", r.ID, "err", err)
		}
	}
	if r.Status == RunPending && r.QueueDeadline == nil {
		r.QueueDeadline = timePtr(r.ScheduledFor.Add(s.timeoutOf(v)))
	}
	if err := s.updateRunLocked(ctx, r); err != nil {
		s.log.Warn("schedule: update run", "run", r.ID, "err", err)
		return
	}
	if r.Status == RunFailed {
		s.finishEffectsLocked(ctx, r)
	}
}

// finishEffectsLocked updates the parent Schedule after a Run reached a
// terminal state (counter, last_run, completed transitions).
func (s *Service) finishEffectsLocked(ctx context.Context, r Run) {
	v, err := s.store.GetSchedule(ctx, r.ScheduleID)
	if err != nil {
		return
	}
	now := s.now()
	if r.Status != RunSkipped && r.Status != RunMissed {
		v.RunCount++
		v.LastRun = &RunSummary{ID: r.ID, Status: r.Status, EndedAt: r.EndedAt}
	}
	if v.Trigger.Type == TriggerOnce && r.Status != RunSkipped && r.Status != RunMissed {
		v.State = StateCompleted
	}
	if v.Options.MaxRuns > 0 && v.RunCount >= v.Options.MaxRuns {
		v.State = StateCompleted
	}
	if v.Options.EndAt != nil && !now.Before(*v.Options.EndAt) {
		v.State = StateCompleted
	}
	if v.State == StateCompleted {
		v.NextRunAt = nil
	}
	v.UpdatedAt = now
	_ = s.updateScheduleLocked(ctx, v, ChangeUpdated)
	if err := s.store.PruneRuns(ctx, v.ID, s.cfg.RunHistory); err != nil {
		s.log.Warn("schedule: prune run history", "schedule", v.ID, "err", err)
	}
}

func (s *Service) requireMutable(ctx context.Context, id string) (Schedule, error) {
	v, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, notFound(err, "schedule "+id)
	}
	switch v.State {
	case StateActive, StatePaused, StatePendingApproval:
		return v, nil
	default:
		return Schedule{}, NewError(CodeInvalidState, "schedule %s is %s", id, v.State)
	}
}

func (s *Service) insertRunLocked(ctx context.Context, r Run) error {
	if err := s.store.InsertRun(ctx, r); err != nil {
		return err
	}
	s.runChanged(r)
	return nil
}

func (s *Service) updateRunLocked(ctx context.Context, r Run) error {
	if err := s.store.UpdateRun(ctx, r); err != nil {
		return err
	}
	s.runChanged(r)
	return nil
}

func (s *Service) updateScheduleLocked(ctx context.Context, v Schedule, kind ChangeKind) error {
	if err := s.store.UpdateSchedule(ctx, v); err != nil {
		return err
	}
	s.changed(v, kind)
	return nil
}

func (s *Service) changed(v Schedule, kind ChangeKind) {
	if s.observer != nil {
		payload := v
		if kind == ChangeDeleted {
			payload = Schedule{ID: v.ID, State: StateDeleted}
		}
		s.observer.ScheduleChanged(payload, kind)
	}
}

func (s *Service) runChanged(r Run) {
	if s.observer != nil {
		s.observer.RunChanged(r)
	}
}

func notFound(err error, what string) error {
	if errors.Is(err, ErrNotFound) {
		return NewError(CodeNotFound, "%s not found", what)
	}
	return err
}

func timePtr(t time.Time) *time.Time { return &t }

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
