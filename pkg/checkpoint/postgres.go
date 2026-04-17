package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/millken/deepai/pkg/models"
)

const schemaSQL = `
create table if not exists sessions (
	id text primary key,
	user_id text not null,
	state text not null default 'active',
	metadata jsonb not null default '{}'::jsonb,
	created_at timestamptz not null,
	updated_at timestamptz not null
);

create table if not exists messages (
	id text primary key,
	session_id text not null references sessions(id) on delete cascade,
	role text not null,
	content text not null,
	tool_calls jsonb,
	tool_result text,
	metadata jsonb not null default '{}'::jsonb,
	created_at timestamptz not null
);

create index if not exists idx_messages_session
	on messages(session_id, created_at);

alter table messages add column if not exists tsv tsvector
	generated always as (to_tsvector('simple', coalesce(content, ''))) stored;
create index if not exists idx_messages_tsv
	on messages using gin(tsv);
`

var ErrNotFound = errors.New("checkpoint record not found")

type (
	Session      = models.Session
	Message      = models.Message
	SessionState = models.SessionState
	Role         = models.Role
	CallStatus   = models.CallStatus
	ToolCall     = models.ToolCall
	ToolResult   = models.ToolResult
)

const (
	SessionStateActive    = models.SessionStateActive
	SessionStateCompleted = models.SessionStateCompleted
	SessionStateArchived  = models.SessionStateArchived

	RoleHuman  = models.RoleHuman
	RoleAI     = models.RoleAI
	RoleSystem = models.RoleSystem
	RoleTool   = models.RoleTool
)

type rowScanner interface {
	Scan(dest ...any) error
}

type rows interface {
	Close()
	Err() error
	Next() bool
	Scan(dest ...any) error
}

type tx interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) rowScanner
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

type db interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) rowScanner
	Begin(ctx context.Context) (tx, error)
}

type pgxPoolDB struct {
	pool *pgxpool.Pool
}

func (p *pgxPoolDB) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return p.pool.Exec(ctx, sql, arguments...)
}

func (p *pgxPoolDB) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p *pgxPoolDB) QueryRow(ctx context.Context, sql string, args ...any) rowScanner {
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p *pgxPoolDB) Begin(ctx context.Context) (tx, error) {
	raw, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTxDB{tx: raw}, nil
}

type pgxTxDB struct {
	tx pgx.Tx
}

func (t *pgxTxDB) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, arguments...)
}

func (t *pgxTxDB) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

func (t *pgxTxDB) QueryRow(ctx context.Context, sql string, args ...any) rowScanner {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t *pgxTxDB) Commit(ctx context.Context) error {
	return t.tx.Commit(ctx)
}

func (t *pgxTxDB) Rollback(ctx context.Context) error {
	return t.tx.Rollback(ctx)
}

type PostgresStore struct {
	db    db
	close func()
}

func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database url is required")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	store := &PostgresStore{
		db:    &pgxPoolDB{pool: pool},
		close: pool.Close,
	}
	if err := store.AutoMigrate(ctx); err != nil {
		store.Close()
		return nil, err
	}

	return store, nil
}

func newPostgresStore(db db) *PostgresStore {
	return &PostgresStore{
		db:    db,
		close: func() {},
	}
}

func (s *PostgresStore) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not initialized")
	}
	if _, err := s.db.Exec(ctx, "select 1"); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	return nil
}

func (s *PostgresStore) AutoMigrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not initialized")
	}
	if _, err := s.db.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate checkpoint schema: %w", err)
	}
	return nil
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	return s.AutoMigrate(ctx)
}

func (s *PostgresStore) CreateSession(ctx context.Context, session Session) error {
	if err := prepareSession(&session); err != nil {
		return err
	}

	metadata, err := json.Marshal(defaultMap(session.Metadata))
	if err != nil {
		return fmt.Errorf("marshal session metadata: %w", err)
	}

	_, err = s.db.Exec(ctx, `
		insert into sessions (id, user_id, state, metadata, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6)
	`, session.ID, session.UserID, session.State, metadata, session.CreatedAt, session.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create session %q: %w", session.ID, err)
	}
	return nil
}

func (s *PostgresStore) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRow(ctx, `
		select id, user_id, state, metadata, created_at, updated_at
		from sessions
		where id = $1
	`, id)

	session, err := scanSession(row)
	if err != nil {
		return Session{}, fmt.Errorf("get session %q: %w", id, err)
	}

	messages, err := s.ListMessages(ctx, id)
	if err != nil {
		return Session{}, fmt.Errorf("get session %q messages: %w", id, err)
	}
	session.Messages = messages
	return session, nil
}

func (s *PostgresStore) UpdateSession(ctx context.Context, session Session) error {
	if err := prepareSession(&session); err != nil {
		return err
	}

	metadata, err := json.Marshal(defaultMap(session.Metadata))
	if err != nil {
		return fmt.Errorf("marshal session metadata: %w", err)
	}

	tag, err := s.db.Exec(ctx, `
		update sessions
		set user_id = $2, state = $3, metadata = $4, updated_at = $5
		where id = $1
	`, session.ID, session.UserID, session.State, metadata, session.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update session %q: %w", session.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update session %q: %w", session.ID, ErrNotFound)
	}
	return nil
}

func (s *PostgresStore) UpdateSessionState(ctx context.Context, sessionID string, state SessionState) error {
	if err := state.Validate(); err != nil {
		return err
	}

	tag, err := s.db.Exec(ctx, `
		update sessions
		set state = $2, updated_at = $3
		where id = $1
	`, sessionID, state, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update session %q state: %w", sessionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update session %q state: %w", sessionID, ErrNotFound)
	}
	return nil
}

func (s *PostgresStore) DeleteSession(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `delete from sessions where id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete session %q: %w", id, ErrNotFound)
	}
	return nil
}

func (s *PostgresStore) SaveSession(ctx context.Context, session Session) error {
	session = normalizeSessionMessages(session)
	if err := prepareSession(&session); err != nil {
		return err
	}
	for _, msg := range session.Messages {
		if err := prepareMessage(&msg); err != nil {
			return err
		}
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin save session transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := upsertSession(ctx, tx, session); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `delete from messages where session_id = $1`, session.ID); err != nil {
		return fmt.Errorf("replace session %q messages: %w", session.ID, err)
	}

	for _, msg := range session.Messages {
		if err := insertMessage(ctx, tx, msg); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit save session %q: %w", session.ID, err)
	}
	committed = true
	return nil
}

func (s *PostgresStore) CreateMessage(ctx context.Context, msg Message) error {
	if err := prepareMessage(&msg); err != nil {
		return err
	}

	toolCalls, err := encodeToolCalls(msg.ToolCalls)
	if err != nil {
		return err
	}
	toolResult, err := encodeToolResult(msg.ToolResult)
	if err != nil {
		return err
	}
	metadata, err := encodeMetadata(msg.Metadata)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `
		insert into messages (id, session_id, role, content, tool_calls, tool_result, metadata, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
	`, msg.ID, msg.SessionID, msg.Role, messageContent(msg), toolCalls, toolResult, metadata, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("create message %q: %w", msg.ID, err)
	}
	return nil
}

func (s *PostgresStore) SaveMessage(ctx context.Context, msg Message) error {
	if err := prepareMessage(&msg); err != nil {
		return err
	}

	toolCalls, err := encodeToolCalls(msg.ToolCalls)
	if err != nil {
		return err
	}
	toolResult, err := encodeToolResult(msg.ToolResult)
	if err != nil {
		return err
	}
	metadata, err := encodeMetadata(msg.Metadata)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `
		insert into messages (id, session_id, role, content, tool_calls, tool_result, metadata, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (id) do update
		set session_id = excluded.session_id,
			role = excluded.role,
			content = excluded.content,
			tool_calls = excluded.tool_calls,
			tool_result = excluded.tool_result,
			metadata = excluded.metadata,
			created_at = excluded.created_at
	`, msg.ID, msg.SessionID, msg.Role, messageContent(msg), toolCalls, toolResult, metadata, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("save message %q: %w", msg.ID, err)
	}
	return nil
}

func (s *PostgresStore) GetMessage(ctx context.Context, id string) (Message, error) {
	row := s.db.QueryRow(ctx, `
		select id, session_id, role, content, tool_calls, tool_result, metadata, created_at
		from messages
		where id = $1
	`, id)

	msg, err := scanMessage(row)
	if err != nil {
		return Message{}, fmt.Errorf("get message %q: %w", id, err)
	}
	return msg, nil
}

func (s *PostgresStore) UpdateMessage(ctx context.Context, msg Message) error {
	if err := prepareMessage(&msg); err != nil {
		return err
	}

	toolCalls, err := encodeToolCalls(msg.ToolCalls)
	if err != nil {
		return err
	}
	toolResult, err := encodeToolResult(msg.ToolResult)
	if err != nil {
		return err
	}
	metadata, err := encodeMetadata(msg.Metadata)
	if err != nil {
		return err
	}

	tag, err := s.db.Exec(ctx, `
		update messages
		set session_id = $2,
			role = $3,
			content = $4,
			tool_calls = $5,
			tool_result = $6,
			metadata = $7,
			created_at = $8
		where id = $1
	`, msg.ID, msg.SessionID, msg.Role, messageContent(msg), toolCalls, toolResult, metadata, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("update message %q: %w", msg.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update message %q: %w", msg.ID, ErrNotFound)
	}
	return nil
}

func (s *PostgresStore) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.Query(ctx, `
		select id, session_id, role, content, tool_calls, tool_result, metadata, created_at
		from messages
		where session_id = $1
		order by created_at asc, id asc
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages for session %q: %w", sessionID, err)
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan messages for session %q: %w", sessionID, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages for session %q: %w", sessionID, err)
	}
	return messages, nil
}

func (s *PostgresStore) LoadSession(ctx context.Context, sessionID string) ([]Message, error) {
	return s.ListMessages(ctx, sessionID)
}

func (s *PostgresStore) DeleteMessage(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `delete from messages where id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete message %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete message %q: %w", id, ErrNotFound)
	}
	return nil
}

func (s *PostgresStore) DeleteMessages(ctx context.Context, sessionID string) error {
	if _, err := s.db.Exec(ctx, `delete from messages where session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("delete messages for session %q: %w", sessionID, err)
	}
	return nil
}

// SearchMessagesResult holds a single search hit.
type SearchMessagesResult struct {
	SessionID string    `json:"session_id"`
	MessageID string    `json:"message_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// SearchMessages performs full-text search across all messages using the tsvector column.
// It returns messages matching the query, grouped by session, up to limit results.
func (s *PostgresStore) SearchMessages(ctx context.Context, query string, limit int) ([]SearchMessagesResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	tsquery := " plainto_tsquery('simple', $1)"
	sql := `select session_id, id, role, content, created_at
		from messages
		where tsv @@ ` + tsquery + `
		order by ts_rank(tsv, ` + tsquery + `) desc, created_at desc
		limit $2`

	rows, err := s.db.Query(ctx, sql, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchMessagesResult
	for rows.Next() {
		var r SearchMessagesResult
		if err := rows.Scan(&r.SessionID, &r.MessageID, &r.Role, &r.Content, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	return results, nil
}

func upsertSession(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}, session Session) error {
	metadata, err := json.Marshal(defaultMap(session.Metadata))
	if err != nil {
		return fmt.Errorf("marshal session metadata: %w", err)
	}

	_, err = q.Exec(ctx, `
		insert into sessions (id, user_id, state, metadata, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (id) do update
		set user_id = excluded.user_id,
			state = excluded.state,
			metadata = excluded.metadata,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at
	`, session.ID, session.UserID, session.State, metadata, session.CreatedAt, session.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert session %q: %w", session.ID, err)
	}
	return nil
}

func insertMessage(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}, msg Message) error {
	toolCalls, err := encodeToolCalls(msg.ToolCalls)
	if err != nil {
		return err
	}
	toolResult, err := encodeToolResult(msg.ToolResult)
	if err != nil {
		return err
	}
	metadata, err := encodeMetadata(msg.Metadata)
	if err != nil {
		return err
	}

	_, err = q.Exec(ctx, `
		insert into messages (id, session_id, role, content, tool_calls, tool_result, metadata, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
	`, msg.ID, msg.SessionID, msg.Role, messageContent(msg), toolCalls, toolResult, metadata, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert message %q: %w", msg.ID, err)
	}
	return nil
}

func scanSession(row rowScanner) (Session, error) {
	var (
		session  Session
		metadata []byte
	)
	if err := row.Scan(&session.ID, &session.UserID, &session.State, &metadata, &session.CreatedAt, &session.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &session.Metadata); err != nil {
			return Session{}, fmt.Errorf("unmarshal session metadata: %w", err)
		}
	}
	if session.Metadata == nil {
		session.Metadata = map[string]string{}
	}
	return session, nil
}

func scanMessage(row rowScanner) (Message, error) {
	var (
		msg           Message
		toolCallsRaw  []byte
		toolResultRaw *string
		metadataRaw   []byte
	)
	if err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &toolCallsRaw, &toolResultRaw, &metadataRaw, &msg.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, ErrNotFound
		}
		return Message{}, err
	}
	if len(toolCallsRaw) > 0 {
		if err := json.Unmarshal(toolCallsRaw, &msg.ToolCalls); err != nil {
			return Message{}, fmt.Errorf("unmarshal message tool_calls: %w", err)
		}
	}
	if toolResultRaw != nil && strings.TrimSpace(*toolResultRaw) != "" {
		var result ToolResult
		if err := json.Unmarshal([]byte(*toolResultRaw), &result); err != nil {
			return Message{}, fmt.Errorf("unmarshal message tool_result: %w", err)
		}
		msg.ToolResult = &result
	}
	if len(metadataRaw) > 0 {
		if err := json.Unmarshal(metadataRaw, &msg.Metadata); err != nil {
			return Message{}, fmt.Errorf("unmarshal message metadata: %w", err)
		}
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]string{}
	}
	if msg.ToolCalls == nil {
		msg.ToolCalls = []ToolCall{}
	}
	return msg, nil
}

func prepareSession(session *Session) error {
	if session == nil {
		return errors.New("session is nil")
	}
	session.ID = strings.TrimSpace(session.ID)
	session.UserID = strings.TrimSpace(session.UserID)
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	}
	if session.Metadata == nil {
		session.Metadata = map[string]string{}
	}
	if session.Messages == nil {
		session.Messages = []Message{}
	}
	return session.Validate()
}

func prepareMessage(msg *Message) error {
	if msg == nil {
		return errors.New("message is nil")
	}
	msg.ID = strings.TrimSpace(msg.ID)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]string{}
	}
	if msg.ToolCalls == nil {
		msg.ToolCalls = []ToolCall{}
	}
	return msg.Validate()
}

func normalizeSessionMessages(session Session) Session {
	if session.Messages == nil {
		session.Messages = []Message{}
	}
	for i := range session.Messages {
		if strings.TrimSpace(session.Messages[i].SessionID) == "" {
			session.Messages[i].SessionID = session.ID
		}
	}
	return session
}

func messageContent(msg Message) string {
	if msg.Content == "" && (len(msg.ToolCalls) > 0 || msg.ToolResult != nil) {
		return ""
	}
	return msg.Content
}

func encodeToolCalls(toolCalls []ToolCall) ([]byte, error) {
	raw, err := json.Marshal(defaultToolCalls(toolCalls))
	if err != nil {
		return nil, fmt.Errorf("marshal tool calls: %w", err)
	}
	return raw, nil
}

func encodeToolResult(toolResult *ToolResult) (any, error) {
	if toolResult == nil {
		return nil, nil
	}
	raw, err := json.Marshal(toolResult)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return string(raw), nil
}

func encodeMetadata(metadata map[string]string) ([]byte, error) {
	raw, err := json.Marshal(defaultMap(metadata))
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return raw, nil
}

func defaultMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func defaultToolCalls(v []ToolCall) []ToolCall {
	if v == nil {
		return []ToolCall{}
	}
	return v
}
