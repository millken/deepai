package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db     *sql.DB
	ownsDB bool
}

// NewSQLiteStore opens (or creates) a SQLite database at the given path.
func NewSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	store := &SQLiteStore{db: db, ownsDB: true}
	if err := db.PingContext(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return store, nil
}

// NewSQLiteStoreFromDB wraps an existing *sql.DB (e.g. from SessionStore).
// The returned store will not close the underlying DB on Close().
func NewSQLiteStoreFromDB(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db, ownsDB: false}
}

func (s *SQLiteStore) Close() {
	if s != nil && s.db != nil && s.ownsDB {
		_ = s.db.Close()
	}
}

// AutoMigrate creates memory tables (if not already created by SessionStore)
// and runs incremental column migrations.
func (s *SQLiteStore) AutoMigrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite store is not initialized")
	}
	// Ensure base tables exist (idempotent; SessionStore.Migrate may have created them).
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS memories (
			session_id TEXT PRIMARY KEY,
			user_memory TEXT NOT NULL DEFAULT '{}',
			history_memory TEXT NOT NULL DEFAULT '{}',
			source TEXT NOT NULL DEFAULT '',
			updated_at REAL NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memory_facts (
			session_id TEXT NOT NULL REFERENCES memories(session_id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			content TEXT NOT NULL,
			category TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT '',
			retrieval_count INTEGER NOT NULL DEFAULT 0,
			helpful_count INTEGER NOT NULL DEFAULT 0,
			suspect_count INTEGER NOT NULL DEFAULT 0,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			PRIMARY KEY (session_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_facts_session_updated
			ON memory_facts(session_id, updated_at DESC, id ASC)`,
	} {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create memory table: %w", err)
		}
	}
	// Incremental column additions (ignore errors if column already exists).
	for _, ddl := range []string{
		"alter table memory_facts add column source text not null default ''",
		"alter table memory_facts add column retrieval_count integer not null default 0",
		"alter table memory_facts add column helpful_count integer not null default 0",
		"alter table memory_facts add column suspect_count integer not null default 0",
	} {
		_, _ = s.db.ExecContext(ctx, ddl)
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

func (s *SQLiteStore) IncrementRetrievalCounts(ctx context.Context, sessionID string, factIDs []string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(factIDs) == 0 {
		return nil
	}
	placeholders := strings.Repeat(",?", len(factIDs)-1)
	args := make([]any, 0, 1+len(factIDs))
	args = append(args, sessionID)
	for _, id := range factIDs {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`update memory_facts set retrieval_count = retrieval_count + 1 where session_id = ? and id in (?`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("increment retrieval counts for session %q: %w", sessionID, err)
	}
	return nil
}

func (s *SQLiteStore) IncrementHelpfulCounts(ctx context.Context, sessionID string, factIDs []string) (int, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(factIDs) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat(",?", len(factIDs)-1)
	args := make([]any, 0, 1+len(factIDs))
	args = append(args, sessionID)
	for _, id := range factIDs {
		args = append(args, id)
	}
	result, err := s.db.ExecContext(ctx,
		`update memory_facts set helpful_count = helpful_count + 1 where session_id = ? and id in (?`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return 0, fmt.Errorf("increment helpful counts for session %q: %w", sessionID, err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) IncrementSuspectCounts(ctx context.Context, sessionID string, factIDs []string) (int, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(factIDs) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat(",?", len(factIDs)-1)
	args := make([]any, 0, 1+len(factIDs))
	args = append(args, sessionID)
	for _, id := range factIDs {
		args = append(args, id)
	}
	result, err := s.db.ExecContext(ctx,
		`update memory_facts set suspect_count = suspect_count + 1 where session_id = ? and id in (?`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return 0, fmt.Errorf("increment suspect counts for session %q: %w", sessionID, err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
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
		insert into memory_facts (session_id, id, content, category, confidence, source, retrieval_count, helpful_count, suspect_count, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, fact.ID, fact.Content, fact.Category, fact.Confidence, fact.Source, fact.RetrievalCount, fact.HelpfulCount, fact.SuspectCount, formatDBTime(fact.CreatedAt), formatDBTime(fact.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert fact %q for session %q: %w", fact.ID, sessionID, err)
	}
	return nil
}

func (s *SQLiteStore) listFacts(ctx context.Context, sessionID string) ([]Fact, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, content, category, confidence, source, retrieval_count, helpful_count, suspect_count, created_at, updated_at
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
			createdAt float64
			updatedAt float64
		)
		if err := rows.Scan(&fact.ID, &fact.Content, &fact.Category, &fact.Confidence, &fact.Source, &fact.RetrievalCount, &fact.HelpfulCount, &fact.SuspectCount, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan facts for memory %q: %w", sessionID, err)
		}
		fact.CreatedAt = parseDBTime(createdAt)
		fact.UpdatedAt = parseDBTime(updatedAt)
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
		updatedAt   float64
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
	doc.UpdatedAt = parseDBTime(updatedAt)
	return doc, nil
}

// formatDBTime converts time.Time to Unix timestamp for REAL columns.
func formatDBTime(t time.Time) float64 {
	return float64(t.UTC().Unix())
}

// parseDBTime converts Unix timestamp (REAL) back to time.Time.
func parseDBTime(value float64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(int64(value), 0).UTC()
}
