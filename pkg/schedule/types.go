// Package schedule implements timed tasks for deepai-bot: a task definition
// (Schedule), a per-execution record (Run), a Store, a Service with validation
// and a non-blocking Scheduler. One trigger = one ordinary bot turn in a
// session; the package itself never depends on pkg/bot or pkg/agent.
//
// See docs/BOT_SCHEDULER_DESIGN.md.
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Trigger types. cron is deliberately rejected in P1 (see Trigger.Normalize).
const (
	TriggerOnce     = "once"
	TriggerInterval = "interval"
	TriggerCron     = "cron"
)

// State is a Schedule's lifecycle state.
type State string

const (
	StateActive          State = "active"
	StatePaused          State = "paused"
	StatePendingApproval State = "pending_approval"
	StateCompleted       State = "completed"
	StateDeleted         State = "deleted"
)

// RunStatus is one execution's state.
type RunStatus string

const (
	RunPending      RunStatus = "pending"
	RunRunning      RunStatus = "running"
	RunWaitingInput RunStatus = "waiting_input"
	RunCompleted    RunStatus = "completed"
	RunFailed       RunStatus = "failed"
	RunTimedOut     RunStatus = "timed_out"
	RunCancelled    RunStatus = "cancelled"
	RunSkipped      RunStatus = "skipped"
	RunMissed       RunStatus = "missed"
)

// Terminal reports whether no further transition is expected.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunTimedOut, RunCancelled, RunSkipped, RunMissed:
		return true
	}
	return false
}

// Active reports whether the run occupies its schedule's overlap slot.
func (s RunStatus) Active() bool { return s == RunRunning || s == RunWaitingInput }

var activeStatuses = []RunStatus{RunRunning, RunWaitingInput}

// RunTrigger is why a Run exists.
type RunTrigger string

const (
	FireTimer   RunTrigger = "timer"
	FireMisfire RunTrigger = "misfire"
	FireManual  RunTrigger = "manual"
	FireSkip    RunTrigger = "skip"
)

// Option enums.
const (
	SessionDedicated = "dedicated"
	SessionNew       = "new"
	SessionOrigin    = "origin"

	OverlapSkip    = "skip"
	OverlapQueue   = "queue"
	OverlapReplace = "replace"

	MisfireRunOnce = "run_once"
	MisfireSkip    = "skip"

	NotifyAlways    = "always"
	NotifyOnFailure = "on_failure"
	NotifyNever     = "never"

	// What to do when nobody answers an ask_clarification question within
	// answer_timeout (design §7.1).
	OnAnswerTimeoutAssume = "assume"
	OnAnswerTimeoutFail   = "fail"
)

// ChangeKind is the reason an Observer hears about a Schedule.
type ChangeKind string

const (
	ChangeCreated ChangeKind = "created"
	ChangeUpdated ChangeKind = "updated"
	ChangeDeleted ChangeKind = "deleted"
)

// CancelReason is why a Dispatcher is asked to cancel a run.
type CancelReason string

const (
	CancelClient    CancelReason = "client"
	CancelReplaced  CancelReason = "replaced"
	CancelScheduler CancelReason = "scheduler"
	CancelShutdown  CancelReason = "shutdown"
)

// Schedule is a task definition. Times are UTC on the wire.
type Schedule struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prompt    string    `json:"prompt"`
	Trigger   Trigger   `json:"trigger"`
	Timezone  string    `json:"timezone"`
	Options   Options   `json:"options"`
	State     State     `json:"state"`
	CreatedBy CreatedBy `json:"created_by"`

	SessionID string      `json:"session_id,omitempty"`
	NextRunAt *time.Time  `json:"next_run_at,omitempty"`
	LastRun   *RunSummary `json:"last_run,omitempty"`
	RunCount  int         `json:"run_count"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	DeletedAt *time.Time  `json:"deleted_at,omitempty"`

	// Computed views, never persisted.
	Summary  string      `json:"summary,omitempty"`
	NextRuns []time.Time `json:"next_runs,omitempty"`
}

// Clone copies a Schedule, including its pointer fields, so callers can
// mutate a value read from a Store without racing other readers.
func (s Schedule) Clone() Schedule {
	out := s
	out.NextRuns = append([]time.Time(nil), s.NextRuns...)
	if s.NextRunAt != nil {
		t := *s.NextRunAt
		out.NextRunAt = &t
	}
	if s.DeletedAt != nil {
		t := *s.DeletedAt
		out.DeletedAt = &t
	}
	if s.LastRun != nil {
		r := *s.LastRun
		out.LastRun = &r
	}
	return out
}

// RunSummary is the short view of the most recent execution.
type RunSummary struct {
	ID      string     `json:"id"`
	Status  RunStatus  `json:"status"`
	EndedAt *time.Time `json:"ended_at,omitempty"`
}

// Options are the per-task settings. Durations are duration strings such as
// "30m" so they survive JSON/YAML round trips unchanged; Service.ParseOptions
// validates and resolves them to Defaults.
type Options struct {
	SessionMode     string     `json:"session_mode,omitempty"`
	Model           string     `json:"model,omitempty"`
	Timeout         string     `json:"timeout,omitempty"`
	Overlap         string     `json:"overlap,omitempty"`
	Misfire         string     `json:"misfire,omitempty"`
	Notify          string     `json:"notify,omitempty"`
	AnswerTimeout   string     `json:"answer_timeout,omitempty"`
	OnAnswerTimeout string     `json:"on_answer_timeout,omitempty"`
	EndAt           *time.Time `json:"end_at,omitempty"`
	MaxRuns         int        `json:"max_runs,omitempty"`
	MaxToolCalls    int        `json:"max_tool_calls,omitempty"`
	AutoAnswers     int        `json:"max_auto_answers,omitempty"`
}

// CreatedBy records who asked for the task.
type CreatedBy struct {
	Kind      string `json:"kind"` // "model" | "user"
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	Client    string `json:"client,omitempty"`
}

// Run is one execution of a Schedule.
type Run struct {
	ID                string         `json:"id"`
	ScheduleID        string         `json:"schedule_id"`
	Seq               int            `json:"seq"`
	Trigger           RunTrigger     `json:"trigger"`
	Slot              *time.Time     `json:"slot,omitempty"`
	ScheduledFor      time.Time      `json:"scheduled_for"`
	RequestID         string         `json:"request_id,omitempty"`
	Coalesced         int            `json:"coalesced,omitempty"`
	QueuedReason      string         `json:"queued_reason,omitempty"`
	QueueDeadline     *time.Time     `json:"queue_deadline,omitempty"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	EndedAt           *time.Time     `json:"ended_at,omitempty"`
	SessionID         string         `json:"session_id,omitempty"`
	TurnID            string         `json:"turn_id,omitempty"`
	Status            RunStatus      `json:"status"`
	AutoAnswers       int            `json:"auto_answers,omitempty"`
	CancelRequestedAt *time.Time     `json:"cancel_requested_at,omitempty"`
	Output            string         `json:"output,omitempty"`
	Error             string         `json:"error,omitempty"`
	ErrorCode         string         `json:"error_code,omitempty"`
	ToolCalls         int            `json:"tool_calls,omitempty"`
	Usage             map[string]any `json:"usage,omitempty"`
}

// Clone copies a Run including pointer fields.
func (r Run) Clone() Run {
	out := r
	for _, p := range []**time.Time{&out.Slot, &out.QueueDeadline, &out.StartedAt, &out.EndedAt, &out.CancelRequestedAt} {
		if *p != nil {
			t := **p
			*p = &t
		}
	}
	return out
}

// Outcome is what a Dispatcher reports once a turn has ended.
type Outcome struct {
	Status    RunStatus
	Output    string
	Error     string
	ErrorCode string
	ToolCalls int
	Usage     map[string]any
}

// Dispatcher starts and cancels executions. Fire must return immediately:
// it starts a turn and never waits for the turn to finish; the final state
// comes back through Service.FinishRun. ErrBusy means the target session is
// busy, in which case the Run stays pending.
type Dispatcher interface {
	Fire(ctx context.Context, sch Schedule, run Run) (sessionID, turnID string, err error)
	Cancel(run Run, reason CancelReason) error
}

// ErrBusy is the non-blocking "try again later" signal from a Dispatcher.
var ErrBusy = errors.New("schedule: target session is busy")

// Observer receives every Schedule/Run change; bot uses it to publish the
// global schedule_* event stream.
type Observer interface {
	ScheduleChanged(Schedule, ChangeKind)
	RunChanged(Run)
}

// Clock is injectable so Scheduler tests never sleep.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now().UTC() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the production Clock.
func RealClock() Clock { return realClock{} }

// Error codes are stable across REST, WebSocket and tool results.
type Code string

const (
	CodeNotFound              Code = "not_found"
	CodeBadRequest            Code = "bad_request"
	CodeInvalidTrigger        Code = "invalid_trigger"
	CodeTriggerNotSupported   Code = "trigger_not_supported"
	CodeInvalidTimezone       Code = "invalid_timezone"
	CodeIntervalTooShort      Code = "interval_too_short"
	CodeTimeInPast            Code = "time_in_past"
	CodeLimitExceeded         Code = "limit_exceeded"
	CodeNotAllowedInScheduled Code = "not_allowed_in_scheduled_run"
	CodeConfirmationMissing   Code = "confirmation_not_received"
	CodeRunInProgress         Code = "run_in_progress"
	CodeInvalidState          Code = "invalid_state"
	CodeConflict              Code = "conflict"
	CodeInternal              Code = "internal"
)

// Error is a structured schedule error.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError builds a structured error.
func NewError(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode extracts a stable code from err.
func ErrorCode(err error) Code {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e.Code
	case errors.Is(err, ErrBusy):
		return CodeRunInProgress
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrConflict):
		return CodeConflict
	}
	return CodeInternal
}

// HTTPStatus maps an error onto an HTTP status and a wire code.
func HTTPStatus(err error) int {
	switch ErrorCode(err) {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeRunInProgress, CodeConflict, CodeInvalidState:
		return http.StatusConflict
	case CodeLimitExceeded:
		return http.StatusTooManyRequests
	case CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

// Config is the `schedule:` section of ~/.deepai/config.yaml.
type Config struct {
	Enabled               bool
	Timezone              string
	Confirm               string // "ask" | "auto"
	ConfirmTimeout        time.Duration
	AllowNested           bool
	MinInterval           time.Duration
	MaxSchedules          int
	MaxConcurrentRuns     int
	MaxRunsPerDay         int
	RunHistory            int
	DefaultTimeout        time.Duration
	DefaultAnswerTimeout  time.Duration
	DefaultMaxAutoAnswers int
}

// DefaultConfig returns the documented defaults. Timezone empty means the
// process's local zone, resolved by Location.
func DefaultConfig() Config {
	return Config{
		Enabled:               true,
		Confirm:               "ask",
		ConfirmTimeout:        10 * time.Minute,
		MinInterval:           5 * time.Minute,
		MaxSchedules:          50,
		MaxConcurrentRuns:     2,
		MaxRunsPerDay:         48,
		RunHistory:            200,
		DefaultTimeout:        30 * time.Minute,
		DefaultAnswerTimeout:  30 * time.Minute,
		DefaultMaxAutoAnswers: 3,
	}
}

// Location resolves a task's IANA timezone. An empty/unknown "Local" falls
// back to the process zone.
func Location(tz string) (*time.Location, error) {
	if tz == "" || tz == "Local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, NewError(CodeInvalidTimezone, "unknown timezone %q: %v", tz, err)
	}
	return loc, nil
}

// SystemTimezone is the default written into new tasks when config does not
// specify one.
func SystemTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	return "Local"
}

// ConfiguredTimezone returns cfg.Timezone or the system zone.
func (c Config) ConfiguredTimezone() string {
	if c.Timezone != "" {
		return c.Timezone
	}
	return SystemTimezone()
}

// id helpers. IDs are short, opaque and collision-safe enough for a local
// single-user daemon; the Store's primary key is the final arbiter.
func newID(prefix string) string {
	return prefix + randomHex(8)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand cannot fail on supported platforms; a time-based
		// fallback still beats a duplicate empty id.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
