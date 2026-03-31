package memory

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
)

const schemaSQL = `
create table if not exists memories (
	session_id text primary key,
	user_memory jsonb not null default '{}'::jsonb,
	history_memory jsonb not null default '{}'::jsonb,
	source text not null default '',
	updated_at timestamptz not null
);

create table if not exists memory_facts (
	session_id text not null references memories(session_id) on delete cascade,
	id text not null,
	content text not null,
	category text not null default '',
	confidence double precision not null default 0,
	created_at timestamptz not null,
	updated_at timestamptz not null,
	primary key (session_id, id)
);

create index if not exists idx_memory_facts_session_updated
	on memory_facts(session_id, updated_at desc, id asc);
`

var ErrNotFound = errors.New("memory record not found")

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

func (s *PostgresStore) AutoMigrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not initialized")
	}
	if _, err := s.db.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate memory schema: %w", err)
	}
	return nil
}

func (s *PostgresStore) Load(ctx context.Context, sessionID string) (Document, error) {
	row := s.db.QueryRow(ctx, `
		select session_id, user_memory, history_memory, source, updated_at
		from memories
		where session_id = $1
	`, sessionID)

	doc, err := scanDocument(row)
	if err != nil {
		return Document{}, fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	facts, err := s.listFacts(ctx, sessionID)
	if err != nil {
		return Document{}, err
	}
	doc.Facts = facts
	return doc, nil
}

func (s *PostgresStore) Save(ctx context.Context, doc Document) error {
	if err := prepareDocument(&doc); err != nil {
		return err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin memory transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := upsertDocument(ctx, tx, doc); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `delete from memory_facts where session_id = $1`, doc.SessionID); err != nil {
		return fmt.Errorf("replace memory facts for %q: %w", doc.SessionID, err)
	}

	for _, fact := range doc.Facts {
		if err := insertFact(ctx, tx, doc.SessionID, fact); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit memory %q: %w", doc.SessionID, err)
	}
	committed = true
	return nil
}

func upsertDocument(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}, doc Document) error {
	userJSON, err := json.Marshal(doc.User)
	if err != nil {
		return fmt.Errorf("marshal user memory: %w", err)
	}
	historyJSON, err := json.Marshal(doc.History)
	if err != nil {
		return fmt.Errorf("marshal history memory: %w", err)
	}

	_, err = q.Exec(ctx, `
		insert into memories (session_id, user_memory, history_memory, source, updated_at)
		values ($1, $2, $3, $4, $5)
		on conflict (session_id) do update
		set user_memory = excluded.user_memory,
			history_memory = excluded.history_memory,
			source = excluded.source,
			updated_at = excluded.updated_at
	`, doc.SessionID, userJSON, historyJSON, doc.Source, doc.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert memory %q: %w", doc.SessionID, err)
	}
	return nil
}

func insertFact(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}, sessionID string, fact Fact) error {
	if err := prepareFact(&fact); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		insert into memory_facts (session_id, id, content, category, confidence, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, sessionID, fact.ID, fact.Content, fact.Category, fact.Confidence, fact.CreatedAt, fact.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert fact %q for session %q: %w", fact.ID, sessionID, err)
	}
	return nil
}

func (s *PostgresStore) listFacts(ctx context.Context, sessionID string) ([]Fact, error) {
	rows, err := s.db.Query(ctx, `
		select id, content, category, confidence, created_at, updated_at
		from memory_facts
		where session_id = $1
		order by updated_at desc, id asc
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list facts for memory %q: %w", sessionID, err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var fact Fact
		if err := rows.Scan(&fact.ID, &fact.Content, &fact.Category, &fact.Confidence, &fact.CreatedAt, &fact.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan facts for memory %q: %w", sessionID, err)
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list facts for memory %q: %w", sessionID, err)
	}
	return facts, nil
}

func scanDocument(row rowScanner) (Document, error) {
	var (
		doc         Document
		userJSON    []byte
		historyJSON []byte
	)
	if err := row.Scan(&doc.SessionID, &userJSON, &historyJSON, &doc.Source, &doc.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Document{}, ErrNotFound
		}
		return Document{}, err
	}
	if len(userJSON) > 0 {
		if err := json.Unmarshal(userJSON, &doc.User); err != nil {
			return Document{}, fmt.Errorf("decode user memory: %w", err)
		}
	}
	if len(historyJSON) > 0 {
		if err := json.Unmarshal(historyJSON, &doc.History); err != nil {
			return Document{}, fmt.Errorf("decode history memory: %w", err)
		}
	}
	return doc, nil
}

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
