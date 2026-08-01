package chat

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// SessionRepository — see models.SessionRepository for interface definition
// ---------------------------------------------------------------------------

// SessionRepository is an alias for models.SessionRepository.
type SessionRepository = models.SessionRepository

// ---------------------------------------------------------------------------
// SQLite implementation
// ---------------------------------------------------------------------------

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    model       TEXT DEFAULT '',
    cwd         TEXT DEFAULT '',
    source      TEXT DEFAULT 'cli',
    state       TEXT DEFAULT 'active',
    created_at  REAL NOT NULL,
    updated_at  REAL NOT NULL,
    metadata    TEXT DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT DEFAULT '',
    tool_calls  TEXT DEFAULT '[]',
    tool_result TEXT DEFAULT '',
    created_at  REAL NOT NULL,
    UNIQUE(session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);

-- FTS5 (messages.id is TEXT PRIMARY KEY, so SQLite assigns implicit rowid).
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content=messages,
    content_rowid=rowid
);

-- FTS sync triggers (only index human/ai messages).
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages WHEN new.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages WHEN old.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au_del AFTER UPDATE ON messages WHEN old.role IN ('human', 'ai') AND new.role NOT IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au_ins AFTER UPDATE ON messages WHEN new.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
`

// SQLiteSessionStore implements SessionRepository backed by SQLite.
type SQLiteSessionStore struct {
	db     *sql.DB
	ownsDB bool
}

// NewSQLiteSessionStore opens (or creates) a SQLite database and runs migrations.
func NewSQLiteSessionStore(dbPath string) (*SQLiteSessionStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	store := &SQLiteSessionStore{db: db, ownsDB: true}
	if err := db.Ping(); err != nil {
		store.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := store.Migrate(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// NewSQLiteSessionStoreFromDB wraps an existing *sql.DB.
// The caller is responsible for managing the DB lifecycle (open/close).
// Migrate() must be called after construction if schema setup is needed.
func NewSQLiteSessionStoreFromDB(db *sql.DB) *SQLiteSessionStore {
	return &SQLiteSessionStore{db: db, ownsDB: false}
}

// Migrate creates tables and runs schema version migrations.
func (s *SQLiteSessionStore) Migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	// Initialize schema_version if empty.
	var v int
	err := s.db.QueryRow("SELECT version FROM schema_version").Scan(&v)
	if err != nil {
		if err == sql.ErrNoRows {
			_, err = s.db.Exec("INSERT INTO schema_version (version) VALUES (1)")
		}
		if err != nil {
			return fmt.Errorf("init schema_version: %w", err)
		}
	}
	return nil
}

func (s *SQLiteSessionStore) Close() error {
	if s != nil && s.db != nil && s.ownsDB {
		return s.db.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Create(opts models.CreateOpts) (*models.Session, error) {
	now := time.Now()
	id := generateSessionID(now)
	metadata := make(map[string]string)
	if opts.Model != "" {
		metadata["model"] = opts.Model
	}
	if opts.CWD != "" {
		metadata["cwd"] = opts.CWD
	}
	metaJSON, _ := json.Marshal(metadata)
	source := opts.Source
	if source == "" {
		source = "cli"
	}

	_, err := s.db.Exec(`
		INSERT INTO sessions (id, title, model, cwd, source, state, created_at, updated_at, metadata)
		VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)
	`, id, opts.Title, opts.Model, opts.CWD, source, unixFrac(now), unixFrac(now), string(metaJSON))
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return &models.Session{
		ID:        id,
		Title:     opts.Title,
		State:     models.SessionStateActive,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Load(id string) (*models.Session, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, title, state, metadata, created_at, updated_at
		FROM sessions WHERE id = ?
	`, id)
	var sess models.Session
	var state, metaJSON sql.NullString
	var createdAt, updatedAt float64
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.Title, &state, &metaJSON, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	sess.State = models.SessionState(state.String)
	sess.CreatedAt = time.Unix(int64(createdAt), 0)
	sess.UpdatedAt = time.Unix(int64(updatedAt), 0)
	if metaJSON.Valid && metaJSON.String != "" {
		_ = json.Unmarshal([]byte(metaJSON.String), &sess.Metadata)
	}
	if sess.Metadata == nil {
		sess.Metadata = make(map[string]string)
	}
	return &sess, nil
}

func (s *SQLiteSessionStore) LoadMessages(id string) ([]models.Message, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, role, content, tool_calls, tool_result, created_at
		FROM messages WHERE session_id = ? ORDER BY seq ASC
	`, id)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var msgs []models.Message
	for rows.Next() {
		var m models.Message
		var role, toolCalls, toolResult sql.NullString
		var createdAt float64
		if err := rows.Scan(&m.ID, &m.SessionID, &role, &m.Content, &toolCalls, &toolResult, &createdAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Role = models.Role(role.String)
		m.CreatedAt = time.Unix(int64(createdAt), 0)
		if toolCalls.Valid && toolCalls.String != "" && toolCalls.String != "[]" {
			_ = json.Unmarshal([]byte(toolCalls.String), &m.ToolCalls)
		}
		if toolResult.Valid && toolResult.String != "" {
			var tr models.ToolResult
			if err := json.Unmarshal([]byte(toolResult.String), &tr); err == nil {
				m.ToolResult = &tr
			}
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ---------------------------------------------------------------------------
// Save
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Save(sess *models.Session) error {
	metaJSON, _ := json.Marshal(sess.Metadata)
	_, err := s.db.Exec(`
		UPDATE sessions SET state = ?, updated_at = ?, metadata = ?
		WHERE id = ?
	`, string(sess.State), unixFrac(time.Now()), string(metaJSON), sess.ID)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AppendMessage
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) AppendMessage(sessionID string, msg models.Message) error {
	if msg.ID == "" {
		msg.ID = sessionID + "_" + nextSeqID()
	}
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	// Images are not persisted (they can be large base64 blobs). Replace them
	// with a text placeholder so the transcript remains coherent on reload.
	content := msg.Content
	if len(msg.Images) > 0 {
		placeholder := fmt.Sprintf("[%d image(s) attached — not persisted]", len(msg.Images))
		if content == "" {
			content = placeholder
		} else {
			content = content + "\n" + placeholder
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin append message: %w", err)
	}
	defer tx.Rollback()

	// Get next seq within transaction.
	var seq int
	err = tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return fmt.Errorf("get next seq: %w", err)
	}

	toolCallsJSON := "[]"
	if len(msg.ToolCalls) > 0 {
		b, _ := json.Marshal(msg.ToolCalls)
		toolCallsJSON = string(b)
	}
	toolResultJSON := ""
	if msg.ToolResult != nil {
		b, _ := json.Marshal(msg.ToolResult)
		toolResultJSON = string(b)
	}

	_, err = tx.Exec(`
		INSERT INTO messages (id, session_id, seq, role, content, tool_calls, tool_result, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, msg.ID, sessionID, seq, string(msg.Role), content, toolCallsJSON, toolResultJSON, unixFrac(msg.CreatedAt))
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}

	// Update session updated_at.
	_, err = tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, unixFrac(msg.CreatedAt), sessionID)
	if err != nil {
		return fmt.Errorf("update session timestamp: %w", err)
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// DeleteMessagesAfterSeq
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) DeleteMessagesAfterSeq(sessionID string, afterSeq int) error {
	_, err := s.db.Exec(
		`DELETE FROM messages WHERE session_id = ? AND seq > ?`, sessionID, afterSeq,
	)
	return err
}

func (s *SQLiteSessionStore) DeleteLastUserTurn(sessionID string) (int, error) {
	res, err := s.db.Exec(`
		DELETE FROM messages
		WHERE session_id = ?
		  AND seq >= (SELECT MAX(seq) FROM messages WHERE session_id = ? AND role = ?)
	`, sessionID, sessionID, string(models.RoleHuman))
	if err != nil {
		return 0, fmt.Errorf("delete last user turn: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Delete(id string) error {
	// messages cascade via ON DELETE CASCADE
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rename
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Rename(id, title string) error {
	res, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// SetTitle (used by auto-title generation)
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) SetTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, id)
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func scanSessionMetas(rows *sql.Rows) ([]models.SessionMeta, error) {
	var metas []models.SessionMeta
	for rows.Next() {
		var m models.SessionMeta
		var createdAt, updatedAt float64
		if err := rows.Scan(&m.ID, &m.Title, &m.MsgCount, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session meta: %w", err)
		}
		m.CreatedAt = time.Unix(int64(createdAt), 0)
		m.UpdatedAt = time.Unix(int64(updatedAt), 0)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// ---------------------------------------------------------------------------
// ListRecent
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) ListRecent(limit int) ([]models.SessionMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT s.id, s.title, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		GROUP BY s.id ORDER BY s.updated_at DESC, s.id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionMetas(rows)
}

// ---------------------------------------------------------------------------
// Search (FTS5)
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Search(query string, limit int) ([]models.SessionMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	sanitized := sanitizeFTS5(query)
	rows, err := s.db.Query(`
		SELECT s.id, s.title, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		JOIN messages_fts fts ON fts.rowid = m.rowid
		WHERE messages_fts MATCH ?
		GROUP BY s.id ORDER BY rank LIMIT ?
	`, sanitized, limit)
	if err != nil {
		return nil, fmt.Errorf("search sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionMetas(rows)
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Stats() (models.SessionStats, error) {
	var stats models.SessionStats
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&stats.SessionCount)
	if err != nil {
		return stats, fmt.Errorf("session count: %w", err)
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&stats.MessageCount)
	if err != nil {
		return stats, fmt.Errorf("message count: %w", err)
	}
	var oldestAt, latestAt sql.NullFloat64
	if err := s.db.QueryRow(`SELECT MIN(created_at), MAX(updated_at) FROM sessions`).Scan(&oldestAt, &latestAt); err != nil {
		return stats, fmt.Errorf("session time range: %w", err)
	}
	if oldestAt.Valid && oldestAt.Float64 > 0 {
		stats.OldestAt = time.Unix(int64(oldestAt.Float64), 0)
	}
	if latestAt.Valid && latestAt.Float64 > 0 {
		stats.LatestAt = time.Unix(int64(latestAt.Float64), 0)
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// Prune
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Prune(olderThanDays int, dryRun bool) (int, error) {
	cutoff := time.Now().AddDate(0, 0, -olderThanDays).Unix()
	if dryRun {
		var count int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE updated_at < ? AND state = 'completed'`, cutoff).Scan(&count)
		return count, err
	}
	res, err := s.db.Exec(`DELETE FROM sessions WHERE updated_at < ? AND state = 'completed'`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) ExportSession(id string) (*models.SessionExport, error) {
	sess, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	msgs, err := s.LoadMessages(id)
	if err != nil {
		return nil, err
	}
	return &models.SessionExport{Session: *sess, Messages: msgs}, nil
}

func (s *SQLiteSessionStore) ExportAll() ([]models.SessionExport, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list all sessions for export: %w", err)
	}
	defer rows.Close()

	var exports []models.SessionExport
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		exp, err := s.ExportSession(id)
		if err != nil {
			slog.Warn("export session failed", "id", id, "err", err)
			continue
		}
		exports = append(exports, *exp)
	}
	return exports, rows.Err()
}

// ---------------------------------------------------------------------------
// Resolve (ID/TITLE unified matching)
// ---------------------------------------------------------------------------

var idPattern = regexp.MustCompile(`^\d{8}_\d{6}`)

// ResolveAll returns all sessions matching input (ID or title fuzzy match).
func (s *SQLiteSessionStore) ResolveAll(input string) ([]models.SessionMeta, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("empty session identifier")
	}

	// Priority 1: ID exact match.
	if idPattern.MatchString(input) {
		if _, err := s.Load(input); err == nil {
			return []models.SessionMeta{{ID: input}}, nil
		}
	}

	// Priority 2: Title exact match (SQL).
	rows, err := s.db.Query(`
		SELECT s.id, s.title, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		WHERE s.title = ?
		GROUP BY s.id LIMIT 1`, input)
	if err != nil {
		return nil, fmt.Errorf("resolve exact title: %w", err)
	}
	metas, scanErr := scanSessionMetas(rows)
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("resolve exact title scan: %w", scanErr)
	}
	if len(metas) > 0 {
		return metas, nil
	}

	// Priority 3: Title prefix match (SQL).
	escaped := escapeLIKE(input)
	rows, err = s.db.Query(`
		SELECT s.id, s.title, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		WHERE s.title LIKE ? || '%' ESCAPE '\'
		GROUP BY s.id LIMIT 1`, escaped)
	if err != nil {
		return nil, fmt.Errorf("resolve prefix title: %w", err)
	}
	metas, scanErr = scanSessionMetas(rows)
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("resolve prefix title scan: %w", scanErr)
	}
	if len(metas) > 0 {
		return metas, nil
	}

	// Priority 4: Fuzzy (contains) match (SQL, case-insensitive).
	rows, err = s.db.Query(`
		SELECT s.id, s.title, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		WHERE LOWER(s.title) LIKE '%' || LOWER(?) || '%' ESCAPE '\'
		GROUP BY s.id LIMIT 20`, escaped)
	if err != nil {
		return nil, fmt.Errorf("resolve fuzzy title: %w", err)
	}
	metas, scanErr = scanSessionMetas(rows)
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("resolve fuzzy title scan: %w", scanErr)
	}
	if len(metas) > 0 {
		return metas, nil
	}

	return nil, fmt.Errorf("no session matching %q", input)
}

// Resolve returns the first session matching input.
func (s *SQLiteSessionStore) Resolve(input string) (*models.Session, error) {
	metas, err := s.ResolveAll(input)
	if err != nil {
		return nil, err
	}
	return s.Load(metas[0].ID)
}

// ---------------------------------------------------------------------------
// Latest
// ---------------------------------------------------------------------------

func (s *SQLiteSessionStore) Latest() (*models.Session, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM sessions ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest session: %w", err)
	}
	return s.Load(id)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func generateSessionID(t time.Time) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", t.Format("20060102_150405"), hex.EncodeToString(b))
}

func unixFrac(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func nextSeqID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sanitizeFTS5(query string) string {
	// Strip characters that could break FTS5 syntax.
	query = strings.ReplaceAll(query, `"`, "")
	query = strings.ReplaceAll(query, "(", "")
	query = strings.ReplaceAll(query, ")", "")
	query = strings.ReplaceAll(query, "*", "")
	query = strings.TrimSpace(query)
	return query
}

// escapeLIKE escapes SQL LIKE wildcards (% and _) using backslash as escape char.
func escapeLIKE(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
