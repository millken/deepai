package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore keeps schedules/runs in deepai-bot's own bot.db. The package
// only uses database/sql; the driver is opened by the command that wires the
// bot together (cmd/deepai-bot), so the CLI never touches these tables.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore wraps an already-open bot.db connection.
func NewSQLiteStore(db *sql.DB) *SQLiteStore { return &SQLiteStore{db: db} }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schedules (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prompt TEXT NOT NULL,
  trigger_json TEXT NOT NULL,
  timezone TEXT NOT NULL,
  options_json TEXT NOT NULL,
  session_id TEXT,
  state TEXT NOT NULL,
  created_by_json TEXT NOT NULL,
  next_run_at INTEGER,
  last_run_json TEXT,
  run_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS schedules_next ON schedules(state, next_run_at);

CREATE TABLE IF NOT EXISTS schedule_runs (
  id TEXT PRIMARY KEY,
  schedule_id TEXT NOT NULL REFERENCES schedules(id),
  seq INTEGER NOT NULL,
  trigger TEXT NOT NULL,
  slot INTEGER,
  scheduled_for INTEGER NOT NULL,
  request_id TEXT,
  coalesced INTEGER NOT NULL DEFAULT 0,
  queued_reason TEXT,
  queue_deadline INTEGER,
  started_at INTEGER,
  ended_at INTEGER,
  session_id TEXT,
  turn_id TEXT,
  status TEXT NOT NULL,
  auto_answers INTEGER NOT NULL DEFAULT 0,
  cancel_requested_at INTEGER,
  output TEXT,
  error TEXT,
  error_code TEXT,
  usage_json TEXT,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  UNIQUE(schedule_id, slot),
  UNIQUE(schedule_id, request_id)
);
CREATE INDEX IF NOT EXISTS schedule_runs_by_schedule ON schedule_runs(schedule_id, seq DESC);
CREATE INDEX IF NOT EXISTS schedule_runs_active ON schedule_runs(status);

CREATE TABLE IF NOT EXISTS schedule_lease (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  owner TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS schedule_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT OR IGNORE INTO schedule_meta (id, version) VALUES (1, 0);
`

// Migrate creates the schedule tables and bumps the schema version.
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate schedule schema: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Close() error { return nil } // the connection belongs to the caller

func (s *SQLiteStore) bump(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_meta SET version = version + 1 WHERE id = 1`)
	return err
}

func conflictErr(err error, what string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "constraint") || strings.Contains(msg, "unique") {
		return fmt.Errorf("%w: %s", ErrConflict, what)
	}
	return err
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func msPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func ptrTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func mustTime(v int64) time.Time { return time.UnixMilli(v).UTC() }

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (s *SQLiteStore) InsertSchedule(ctx context.Context, v Schedule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO schedules
		  (id, name, prompt, trigger_json, timezone, options_json, session_id, state,
		   created_by_json, next_run_at, last_run_json, run_count, created_at, updated_at, deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.Name, v.Prompt, jsonString(v.Trigger), v.Timezone, jsonString(v.Options),
		nullableString(v.SessionID), string(v.State), jsonString(v.CreatedBy), msPtr(v.NextRunAt),
		nullableJSON(v.LastRun), v.RunCount, ms(v.CreatedAt), ms(v.UpdatedAt), msPtr(v.DeletedAt))
	if err != nil {
		return conflictErr(err, "schedule "+v.ID)
	}
	return s.bump(ctx)
}

func (s *SQLiteStore) UpdateSchedule(ctx context.Context, v Schedule) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE schedules SET name=?, prompt=?, trigger_json=?, timezone=?, options_json=?,
		  session_id=?, state=?, created_by_json=?, next_run_at=?, last_run_json=?,
		  run_count=?, created_at=?, updated_at=?, deleted_at=? WHERE id=?`,
		v.Name, v.Prompt, jsonString(v.Trigger), v.Timezone, jsonString(v.Options),
		nullableString(v.SessionID), string(v.State), jsonString(v.CreatedBy), msPtr(v.NextRunAt),
		nullableJSON(v.LastRun), v.RunCount, ms(v.CreatedAt), ms(v.UpdatedAt), msPtr(v.DeletedAt), v.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: schedule %s", ErrNotFound, v.ID)
	}
	return s.bump(ctx)
}

func (s *SQLiteStore) GetSchedule(ctx context.Context, id string) (Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id))
}

const scheduleColumns = `id, name, prompt, trigger_json, timezone, options_json, session_id, state,
  created_by_json, next_run_at, last_run_json, run_count, created_at, updated_at, deleted_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanSchedule(row rowScanner) (Schedule, error) {
	var v Schedule
	var triggerJSON, optionsJSON, createdByJSON string
	var sessionID, lastRunJSON sql.NullString
	var nextRunAt, deletedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&v.ID, &v.Name, &v.Prompt, &triggerJSON, &v.Timezone, &optionsJSON,
		&sessionID, &v.State, &createdByJSON, &nextRunAt, &lastRunJSON,
		&v.RunCount, &createdAt, &updatedAt, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, fmt.Errorf("%w: schedule", ErrNotFound)
		}
		return Schedule{}, err
	}
	_ = json.Unmarshal([]byte(triggerJSON), &v.Trigger)
	_ = json.Unmarshal([]byte(optionsJSON), &v.Options)
	_ = json.Unmarshal([]byte(createdByJSON), &v.CreatedBy)
	if lastRunJSON.Valid {
		_ = json.Unmarshal([]byte(lastRunJSON.String), &v.LastRun)
	}
	v.SessionID = sessionID.String
	v.NextRunAt = ptrTime(nextRunAt)
	v.DeletedAt = ptrTime(deletedAt)
	v.CreatedAt = mustTime(createdAt)
	v.UpdatedAt = mustTime(updatedAt)
	return v, nil
}

func (s *SQLiteStore) ListSchedules(ctx context.Context, f ScheduleFilter) ([]Schedule, error) {
	query := `SELECT ` + scheduleColumns + ` FROM schedules WHERE 1=1`
	var args []any
	if !f.IncludeDeleted {
		query += ` AND state != ?`
		args = append(args, string(StateDeleted))
	}
	if f.State != "" {
		query += ` AND state = ?`
		args = append(args, string(f.State))
	}
	query += ` ORDER BY created_at ASC, id ASC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		v, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) InsertRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule_runs
		  (id, schedule_id, seq, trigger, slot, scheduled_for, request_id, coalesced,
		   queued_reason, queue_deadline, started_at, ended_at, session_id, turn_id,
		   status, auto_answers, cancel_requested_at, output, error, error_code, usage_json, tool_calls)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ScheduleID, r.Seq, string(r.Trigger), msPtr(r.Slot), ms(r.ScheduledFor),
		nullableString(r.RequestID), r.Coalesced, nullableString(r.QueuedReason),
		msPtr(r.QueueDeadline), msPtr(r.StartedAt), msPtr(r.EndedAt), nullableString(r.SessionID),
		nullableString(r.TurnID), string(r.Status), r.AutoAnswers, msPtr(r.CancelRequestedAt),
		nullableString(r.Output), nullableString(r.Error), nullableString(r.ErrorCode),
		nullableJSON(r.Usage), r.ToolCalls)
	if err != nil {
		return conflictErr(err, "run "+r.ID)
	}
	return s.bump(ctx)
}

func (s *SQLiteStore) UpdateRun(ctx context.Context, r Run) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE schedule_runs SET schedule_id=?, seq=?, trigger=?, slot=?, scheduled_for=?,
		  request_id=?, coalesced=?, queued_reason=?, queue_deadline=?, started_at=?, ended_at=?,
		  session_id=?, turn_id=?, status=?, auto_answers=?, cancel_requested_at=?, output=?,
		  error=?, error_code=?, usage_json=?, tool_calls=? WHERE id=?`,
		r.ScheduleID, r.Seq, string(r.Trigger), msPtr(r.Slot), ms(r.ScheduledFor),
		nullableString(r.RequestID), r.Coalesced, nullableString(r.QueuedReason),
		msPtr(r.QueueDeadline), msPtr(r.StartedAt), msPtr(r.EndedAt), nullableString(r.SessionID),
		nullableString(r.TurnID), string(r.Status), r.AutoAnswers, msPtr(r.CancelRequestedAt),
		nullableString(r.Output), nullableString(r.Error), nullableString(r.ErrorCode),
		nullableJSON(r.Usage), r.ToolCalls, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: run %s", ErrNotFound, r.ID)
	}
	return s.bump(ctx)
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM schedule_runs WHERE id = ?`, id))
}

const runColumns = `id, schedule_id, seq, trigger, slot, scheduled_for, request_id, coalesced,
  queued_reason, queue_deadline, started_at, ended_at, session_id, turn_id, status,
  auto_answers, cancel_requested_at, output, error, error_code, usage_json, tool_calls`

func scanRun(row rowScanner) (Run, error) {
	var r Run
	var requestID, queuedReason, sessionID, turnID, output, errText, errCode, usageJSON sql.NullString
	var slot, queueDeadline, startedAt, endedAt, cancelAt sql.NullInt64
	var scheduledFor int64
	if err := row.Scan(&r.ID, &r.ScheduleID, &r.Seq, &r.Trigger, &slot, &scheduledFor,
		&requestID, &r.Coalesced, &queuedReason, &queueDeadline, &startedAt, &endedAt,
		&sessionID, &turnID, &r.Status, &r.AutoAnswers, &cancelAt, &output, &errText,
		&errCode, &usageJSON, &r.ToolCalls); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, fmt.Errorf("%w: run", ErrNotFound)
		}
		return Run{}, err
	}
	r.Slot, r.QueueDeadline = ptrTime(slot), ptrTime(queueDeadline)
	r.StartedAt, r.EndedAt = ptrTime(startedAt), ptrTime(endedAt)
	r.CancelRequestedAt = ptrTime(cancelAt)
	r.ScheduledFor = mustTime(scheduledFor)
	r.RequestID, r.QueuedReason, r.SessionID, r.TurnID = requestID.String, queuedReason.String, sessionID.String, turnID.String
	r.Output, r.Error, r.ErrorCode = output.String, errText.String, errCode.String
	if usageJSON.Valid && usageJSON.String != "" {
		_ = json.Unmarshal([]byte(usageJSON.String), &r.Usage)
	}
	return r, nil
}

func (s *SQLiteStore) FindRun(ctx context.Context, f RunFilter) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM schedule_runs WHERE 1=1`
	var args []any
	if f.ScheduleID != "" {
		query += ` AND schedule_id = ?`
		args = append(args, f.ScheduleID)
	}
	if f.RequestID != "" {
		query += ` AND request_id = ?`
		args = append(args, f.RequestID)
	}
	if f.Slot != nil {
		query += ` AND slot = ?`
		args = append(args, ms(*f.Slot))
	}
	if f.Since != nil {
		query += ` AND scheduled_for >= ?`
		args = append(args, ms(*f.Since))
	}
	if len(f.Statuses) > 0 {
		query += ` AND status IN (` + strings.Repeat(",?", len(f.Statuses))[1:] + `)`
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	if f.CancelRequested {
		query += ` AND cancel_requested_at IS NOT NULL`
	}
	if f.Order == "scheduled_for_asc" {
		query += ` ORDER BY scheduled_for ASC, seq ASC`
	} else {
		query += ` ORDER BY seq DESC`
	}
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneRuns keeps at most `keep` terminal runs per schedule. The OFFSET form
// deletes everything after the newest `keep`, ordered by seq.
func (s *SQLiteStore) PruneRuns(ctx context.Context, scheduleID string, keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM schedule_runs WHERE schedule_id = ? AND id IN (
			SELECT id FROM schedule_runs
			WHERE schedule_id = ? AND status IN ('completed','failed','timed_out','cancelled','skipped','missed')
			ORDER BY seq DESC LIMIT -1 OFFSET ?
		)`, scheduleID, scheduleID, keep)
	if err != nil {
		return err
	}
	return s.bump(ctx)
}

// AcquireLease atomically takes (or steals an expired) single-writer lease.
func (s *SQLiteStore) AcquireLease(ctx context.Context, owner string, now, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule_lease (id, owner, expires_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at
		WHERE schedule_lease.owner = excluded.owner OR schedule_lease.expires_at <= ?`,
		owner, ms(expiresAt), ms(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQLiteStore) RenewLease(ctx context.Context, owner string, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE schedule_lease SET expires_at = ? WHERE id = 1 AND owner = ?`, ms(expiresAt), owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQLiteStore) ReleaseLease(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_lease WHERE id = 1 AND owner = ?`, owner)
	return err
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(v any) any {
	if v == nil {
		return nil
	}
	return jsonString(v)
}
