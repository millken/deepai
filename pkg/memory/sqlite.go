package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteSchemaSQL = `
pragma foreign_keys = on;

create table if not exists memories (
	session_id text primary key,
	user_memory text not null default '{}',
	history_memory text not null default '{}',
	source text not null default '',
	updated_at text not null
);

create table if not exists memory_facts (
	session_id text not null references memories(session_id) on delete cascade,
	id text not null,
	content text not null,
	category text not null default '',
	confidence real not null default 0,
	created_at text not null,
	updated_at text not null,
	primary key (session_id, id)
);

create index if not exists idx_memory_facts_session_updated
	on memory_facts(session_id, updated_at desc, id asc);
`

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	store := &SQLiteStore{db: db}
	if err := db.PingContext(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := store.AutoMigrate(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() {
	if s != nil && s.db != nil {
		_ = s.db.Close()
	}
}

func (s *SQLiteStore) AutoMigrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite store is not initialized")
	}
	if _, err := s.db.ExecContext(ctx, sqliteSchemaSQL); err != nil {
		return fmt.Errorf("migrate sqlite memory schema: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Load(ctx context.Context, sessionID string) (Document, error) {
	row := s.db.QueryRowContext(ctx, `
		select session_id, user_memory, history_memory, source, updated_at
		from memories
		where session_id = ?
	`, sessionID)
	doc, err := scanSQLiteDocument(row)
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

func (s *SQLiteStore) Save(ctx context.Context, doc Document) error {
	if err := prepareDocument(&doc); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin memory transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := s.upsertDocument(ctx, tx, doc); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from memory_facts where session_id = ?`, doc.SessionID); err != nil {
		return fmt.Errorf("replace memory facts for %q: %w", doc.SessionID, err)
	}
	for _, fact := range doc.Facts {
		if err := s.insertFact(ctx, tx, doc.SessionID, fact); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory %q: %w", doc.SessionID, err)
	}
	committed = true
	return nil
}

func (s *SQLiteStore) upsertDocument(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, doc Document) error {
	userJSON, err := json.Marshal(doc.User)
	if err != nil {
		return fmt.Errorf("marshal user memory: %w", err)
	}
	historyJSON, err := json.Marshal(doc.History)
	if err != nil {
		return fmt.Errorf("marshal history memory: %w", err)
	}
	_, err = execer.ExecContext(ctx, `
		insert into memories (session_id, user_memory, history_memory, source, updated_at)
		values (?, ?, ?, ?, ?)
		on conflict (session_id) do update set
			user_memory = excluded.user_memory,
			history_memory = excluded.history_memory,
			source = excluded.source,
			updated_at = excluded.updated_at
	`, doc.SessionID, string(userJSON), string(historyJSON), doc.Source, formatDBTime(doc.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert memory %q: %w", doc.SessionID, err)
	}
	return nil
}

func (s *SQLiteStore) insertFact(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, sessionID string, fact Fact) error {
	if err := prepareFact(&fact); err != nil {
		return err
	}
	_, err := execer.ExecContext(ctx, `
		insert into memory_facts (session_id, id, content, category, confidence, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)
	`, sessionID, fact.ID, fact.Content, fact.Category, fact.Confidence, formatDBTime(fact.CreatedAt), formatDBTime(fact.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert fact %q for session %q: %w", fact.ID, sessionID, err)
	}
	return nil
}

func (s *SQLiteStore) listFacts(ctx context.Context, sessionID string) ([]Fact, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, content, category, confidence, created_at, updated_at
		from memory_facts
		where session_id = ?
		order by updated_at desc, id asc
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list facts for memory %q: %w", sessionID, err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var (
			fact      Fact
			createdAt string
			updatedAt string
		)
		if err := rows.Scan(&fact.ID, &fact.Content, &fact.Category, &fact.Confidence, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan facts for memory %q: %w", sessionID, err)
		}
		fact.CreatedAt, err = parseDBTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse fact created_at for memory %q: %w", sessionID, err)
		}
		fact.UpdatedAt, err = parseDBTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse fact updated_at for memory %q: %w", sessionID, err)
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list facts for memory %q: %w", sessionID, err)
	}
	return facts, nil
}

type sqliteScanner interface{ Scan(dest ...any) error }

func scanSQLiteDocument(row sqliteScanner) (Document, error) {
	var (
		doc         Document
		userJSON    string
		historyJSON string
		updatedAt   string
	)
	if err := row.Scan(&doc.SessionID, &userJSON, &historyJSON, &doc.Source, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Document{}, ErrNotFound
		}
		return Document{}, err
	}
	if strings.TrimSpace(userJSON) != "" {
		if err := json.Unmarshal([]byte(userJSON), &doc.User); err != nil {
			return Document{}, fmt.Errorf("decode user memory: %w", err)
		}
	}
	if strings.TrimSpace(historyJSON) != "" {
		if err := json.Unmarshal([]byte(historyJSON), &doc.History); err != nil {
			return Document{}, fmt.Errorf("decode history memory: %w", err)
		}
	}
	var err error
	doc.UpdatedAt, err = parseDBTime(updatedAt)
	if err != nil {
		return Document{}, fmt.Errorf("parse memory updated_at: %w", err)
	}
	return doc, nil
}

func formatDBTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseDBTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
