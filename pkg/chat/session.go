package chat

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

-- One live writer per session. CREATE TABLE IF NOT EXISTS backfills this onto
-- existing databases on next open, no separate migration step needed.
CREATE TABLE IF NOT EXISTS session_locks (
    session_id   TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    pid          INTEGER NOT NULL,
    host         TEXT NOT NULL DEFAULT '',
    acquired_at  REAL NOT NULL,
    heartbeat_at REAL NOT NULL
);
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
	// _txlock=immediate: every transaction this store opens (session lock
	// acquire, AppendMessage) starts with BEGIN IMMEDIATE instead of the
	// modernc.org/sqlite default of BEGIN DEFERRED. Deferred grabs the write
	// lock only when the transaction later tries to write, by which point it
	// has already pinned a read snapshot; if another connection committed a
	// write in between, upgrading that snapshot fails with SQLITE_BUSY_SNAPSHOT
	// — a variant busy_timeout(5000) does NOT retry, so two real processes
	// racing AcquireSessionLock could get a bare "database is locked" instead
	// of *models.ErrSessionLocked, silently breaking the --fork/--force
	// remedies that key off errors.As(err, &lockErr) (see acquireOrHandleLock
	// in pkg/chat/repl.go). BEGIN IMMEDIATE takes the write lock up front, so
	// a conflict happens at BEGIN and IS retried by busy_timeout.
	//
	// Side effect worth knowing: this also changes AppendMessage, which was
	// already a deferred read-then-write transaction (SELECT MAX(seq)+1, then
	// INSERT). Before this session-lock feature existed, only one deepai
	// process ever touched a given DB at a time, so the race never came up.
	// Now that two processes running concurrently is a supported scenario
	// (that is the entire point of the lock), AppendMessage can occasionally
	// hit a real, expected "database is locked" under genuine concurrent
	// writes — busy_timeout(5000) absorbs the common case by retrying, but a
	// caller writing to a DB two processes are actively fighting over should
	// still expect an occasional error here.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
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
	cwd := normalizeCWD(opts.CWD)
	metadata := make(map[string]string)
	if opts.Model != "" {
		metadata["model"] = opts.Model
	}
	if cwd != "" {
		metadata["cwd"] = cwd
	}
	metaJSON, _ := json.Marshal(metadata)
	source := opts.Source
	if source == "" {
		source = "cli"
	}

	_, err := s.db.Exec(`
		INSERT INTO sessions (id, title, model, cwd, source, state, created_at, updated_at, metadata)
		VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)
	`, id, opts.Title, opts.Model, cwd, source, unixFrac(now), unixFrac(now), string(metaJSON))
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

// normalizeCWD canonicalizes a working-directory path for the sessions.cwd
// column (and for LatestInDir's query side): symlinks resolved and the
// result Cleaned, so cd /tmp/x and cd /private/tmp/x (macOS symlinks
// /tmp -> /private/tmp) — or a trailing slash some shells/CI leave in $PWD —
// land on the identical string instead of silently missing each other in a
// cwd = ? lookup. EvalSymlinks requires the path to actually exist and
// errors otherwise (a test using a literal string, or — in principle — a
// session whose original directory was later removed); Create/LatestInDir
// must never hard-fail over that, so this falls back to a plain Clean.
func normalizeCWD(cwd string) string {
	if cwd == "" {
		return cwd
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(cwd)
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

// contentWithImagePlaceholder returns content with a text placeholder
// appended describing images, since images are not persisted (they can be
// large base64 blobs) — the transcript stays coherent on reload even
// though the image bytes themselves are dropped. Shared by AppendMessage
// and forkInto so this rule exists exactly once: N4 (session-lock review,
// mechanical-cleanup pass) found forkInto had silently grown a SECOND,
// divergent copy of this same logic that simply forgot to apply it — see
// this repo's own established practice of never letting behavior like
// this exist twice (a second implementation only ever drifts).
func contentWithImagePlaceholder(content string, images []models.MessageImage) string {
	if len(images) == 0 {
		return content
	}
	placeholder := fmt.Sprintf("[%d image(s) attached — not persisted]", len(images))
	if content == "" {
		return placeholder
	}
	return content + "\n" + placeholder
}

func (s *SQLiteSessionStore) AppendMessage(sessionID string, msg models.Message) error {
	if msg.ID == "" {
		msg.ID = sessionID + "_" + nextSeqID()
	}
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	content := contentWithImagePlaceholder(msg.Content, msg.Images)

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
		if err := rows.Scan(&m.ID, &m.Title, &m.CWD, &m.MsgCount, &createdAt, &updatedAt); err != nil {
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
		SELECT s.id, s.title, s.cwd, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
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
		SELECT s.id, s.title, s.cwd, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
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
		SELECT s.id, s.title, s.cwd, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
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
		SELECT s.id, s.title, s.cwd, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
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
		SELECT s.id, s.title, s.cwd, COUNT(m.id) AS msg_count, s.created_at, s.updated_at
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

// LatestInDir is Latest scoped to sessions whose cwd column matches cwd —
// used by `deepai -c` so continuing in repo A can never silently resume a
// session from repo B (the trigger for the cross-process interleaving
// incident this lock feature exists to fix). Empty-result convention matches
// Latest(): (nil, nil), not an error.
//
// Matches against BOTH normalizeCWD(cwd) and the raw cwd argument as given.
// Create normalizes going forward, so a row written by today's binary is
// found via the normalized form; a row written before this fix (or by any
// other path that stored cwd unnormalized) is still found as long as the
// caller's raw cwd happens to equal what was stored verbatim — cwd is
// os.Getwd()'s output (pkg/commands/chat.go), which does NOT vary within one
// process invocation, so this is the common case. There is no way to also
// match an old row against a DIFFERENT spelling of the same directory
// without rewriting history the row can't be reliably backfilled from.
func (s *SQLiteSessionStore) LatestInDir(cwd string) (*models.Session, error) {
	norm := normalizeCWD(cwd)
	var id string
	err := s.db.QueryRow(`
		SELECT id FROM sessions WHERE cwd = ? OR cwd = ?
		ORDER BY updated_at DESC, id DESC LIMIT 1
	`, norm, cwd).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest session in dir: %w", err)
	}
	return s.Load(id)
}

// ---------------------------------------------------------------------------
// Session locking — one live writer per session (see models.ErrSessionLocked)
// ---------------------------------------------------------------------------

// staleLockAfter is how long a lock may go without a heartbeat before it is
// treated as abandoned. Run()'s heartbeat goroutine refreshes every 15s
// (pkg/chat/repl.go's sessionLockHeartbeatInterval), so 60s tolerates several
// missed beats (GC pause, slow disk, a suspended laptop) without letting a
// genuinely live session get stolen out from under it.
const staleLockAfter = 60 * time.Second

// staleLockAfterPidReuse is how long a SAME-HOST lock may go without a
// heartbeat before processAlive's "yes" is no longer trusted (H2,
// session-lock review round 2). processAlive can only ask "is SOME process
// running at this pid right now" — it cannot ask "is it still the SAME
// deepai that wrote this row". A `kill -9`'d deepai skips its release defer
// entirely (see cmd/deepai/main.go), leaving the row behind; if the OS
// later reassigns that exact pid to an unrelated, long-lived process (pid
// spaces are small — e.g. macOS wraps around roughly 99999 — and churn
// through them quickly under normal use), processAlive would then report
// "alive" forever and the row could never be reclaimed short of --force:
// trading the rare live-lock-stolen incident D1 fixed for a
// guaranteed-reproducible permanent lock-out, which is strictly worse.
//
// A genuinely live deepai heartbeats every sessionLockHeartbeatInterval
// (15s, pkg/chat/repl.go). Nothing on D1's legitimate-staleness list
// (GC pause, a suspended laptop, SIGSTOP, a debugger breakpoint) plausibly
// withholds EVERY heartbeat for ten minutes straight while the process
// keeps running — past that distance, "abandoned, pid since reused by
// something else" is simply the more likely explanation than "still the
// same live deepai". 10x staleLockAfter is the number: generous enough
// that no merely-slow live holder ever crosses it, but finite so a
// killed-and-reused pid does not lock a directory out permanently.
const staleLockAfterPidReuse = 10 * staleLockAfter

// sessionLockRow is a snapshot of one session_locks row, read inside the
// Acquire transaction before the takeover decision is made.
type sessionLockRow struct {
	Owner       models.LockOwner
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}

// canAcquireSessionLock decides whether owner may take (or keep) the lock
// currently described by existing (nil = unlocked). now is injected so tests
// can control staleness without sleeping.
//
// Same-host liveness is checked BEFORE staleness, and it is a VETO up to
// staleLockAfterPidReuse: a live pid on the same host means "no", never
// mind how old the heartbeat looks, UNTIL the heartbeat gap grows so large
// (10x staleLockAfter) that "still the same live deepai" stops being the
// likely explanation and "pid reused after a kill -9" takes over — see
// staleLockAfterPidReuse's doc comment (H2, session-lock review round 2).
// Getting the ORDER backwards (staleness first, no liveness veto at all) is
// a real incident, not a hypothetical — heartbeat_at is wall-clock and the
// heartbeat goroutine is just a 15s time.Ticker, so a suspended laptop
// (monotonic clock frozen, no ticks fire while asleep), a SIGSTOP'd process
// (Ctrl+Z), a debugger breakpoint, or a long GC/IO stall can all put a
// genuinely live holder's heartbeat well past staleLockAfter while the
// process itself keeps running and keeps writing. Judging staleness first
// (with no floor at all) would hand its lock to a second process out from
// under it — the exact interleaving-writers bug this whole feature exists
// to prevent, now self-inflicted. processAlive is a cheap, reliable signal
// on the same host (signal-0 probe, see its doc comment) and must always
// get to answer before staleness is even considered.
//
// A lock recorded on a DIFFERENT host than owner can only ever be judged by
// heartbeat freshness: owner.PID means nothing to os.FindProcess here, since
// it names a process table this machine does not have. Cross-host is the
// only path that falls through to the staleness check below.
func canAcquireSessionLock(existing *sessionLockRow, owner models.LockOwner, force bool, now time.Time) bool {
	if existing == nil {
		return true // nothing to contend with
	}
	if existing.Owner == owner {
		return true // reentrant: the caller already holds this lock
	}
	if force {
		return true // explicit takeover; the caller has been warned by resolveSession
	}
	if existing.Owner.Host == owner.Host {
		if !processAlive(existing.Owner.PID) {
			return true
		}
		// H2: the pid looks alive, but D1's veto is not unconditional past
		// staleLockAfterPidReuse — see that constant's doc comment for why
		// a heartbeat gap that large means the pid was almost certainly
		// reused by an unrelated process, not that the original deepai is
		// still running. Below that threshold, D1 still applies in full:
		// a merely-stale heartbeat on a live pid must NOT be reclaimable.
		return now.Sub(existing.HeartbeatAt) > staleLockAfterPidReuse
	}
	// Cross-host: no pid to probe, so heartbeat age is the only signal.
	return now.Sub(existing.HeartbeatAt) > staleLockAfter
}

// processAlive reports whether pid names a live process on THIS host, using
// the standard "signal 0" liveness probe: sending no actual signal, just
// checking whether the kernel would let us. ESRCH (no such process) means
// dead; EPERM (exists, but owned by someone else) and nil (exists, ours)
// both mean alive. os.ErrProcessDone is a second, Go-runtime-level "dead":
// for a child this same program started and already Wait()'d on, the os
// package answers from its own bookkeeping instead of re-asking the kernel
// (observed in testing — a reaped exec.Cmd child's pid reports this instead
// of ESRCH), so it must be treated the same as ESRCH. Any other error is
// "can't tell, assume alive" so a transient probe failure can never cause an
// unwarranted takeover.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	switch err := proc.Signal(syscall.Signal(0)); {
	case err == nil:
		return true
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false
	default:
		return true
	}
}

// AcquireSessionLock claims the lock on sessionID for owner. The read of the
// existing row and the write of the new one happen in a single transaction —
// otherwise two processes racing AcquireSessionLock could both read "no
// lock" and both write themselves in as the holder, which is exactly the
// bug this feature exists to close.
func (s *SQLiteSessionStore) AcquireSessionLock(sessionID string, owner models.LockOwner, force bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin acquire session lock: %w", err)
	}
	defer tx.Rollback()

	var existing *sessionLockRow
	var pid int
	var host string
	var acquiredAt, heartbeatAt float64
	err = tx.QueryRow(`SELECT pid, host, acquired_at, heartbeat_at FROM session_locks WHERE session_id = ?`, sessionID).
		Scan(&pid, &host, &acquiredAt, &heartbeatAt)
	switch {
	case err == nil:
		existing = &sessionLockRow{
			Owner:       models.LockOwner{PID: pid, Host: host},
			AcquiredAt:  time.Unix(int64(acquiredAt), 0),
			HeartbeatAt: time.Unix(int64(heartbeatAt), 0),
		}
	case err == sql.ErrNoRows:
		existing = nil
	default:
		return fmt.Errorf("read session lock: %w", err)
	}

	now := time.Now()
	if !canAcquireSessionLock(existing, owner, force, now) {
		return &models.ErrSessionLocked{
			SessionID:   sessionID,
			Owner:       existing.Owner,
			AcquiredAt:  existing.AcquiredAt,
			HeartbeatAt: existing.HeartbeatAt,
		}
	}

	// Reentrant acquisition keeps the original acquired_at — it is still the
	// same holding, just refreshed — everything else (stale takeover, dead
	// pid, force, brand new) starts a fresh holding.
	acquiredAt = unixFrac(now)
	if existing != nil && existing.Owner == owner {
		acquiredAt = unixFrac(existing.AcquiredAt)
	}

	_, err = tx.Exec(`
		INSERT INTO session_locks (session_id, pid, host, acquired_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			pid = excluded.pid, host = excluded.host,
			acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at
	`, sessionID, owner.PID, owner.Host, acquiredAt, unixFrac(now))
	if err != nil {
		return fmt.Errorf("write session lock: %w", err)
	}

	return tx.Commit()
}

// RefreshSessionLock updates the heartbeat so Run()'s 15s ticker keeps a
// live session from ever crossing staleLockAfter. It only touches a row
// owner actually holds, so a process that lost its lock (force-stolen by
// someone else) finds out here rather than silently refreshing a lock that
// is no longer its own.
func (s *SQLiteSessionStore) RefreshSessionLock(sessionID string, owner models.LockOwner) error {
	res, err := s.db.Exec(`
		UPDATE session_locks SET heartbeat_at = ?
		WHERE session_id = ? AND pid = ? AND host = ?
	`, unixFrac(time.Now()), sessionID, owner.PID, owner.Host)
	if err != nil {
		// The UPDATE itself failed (e.g. SQLITE_BUSY: another writer on
		// this DB — a second deepai -c, `deepai analyze`, memory's Save, a
		// WAL checkpoint — is mid-transaction past busy_timeout(5000)).
		// This is transient contention, NOT proof the lock is gone —
		// deliberately NOT wrapped in ErrLockNotHeld. See that sentinel's
		// doc comment and heartbeatTick (pkg/chat/repl.go), the only
		// caller that acts on the distinction (H1).
		return fmt.Errorf("refresh session lock: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// The UPDATE ran fine and touched nothing: no row for
		// (sessionID, owner) exists. This IS definitive — owner does not
		// hold this lock, full stop, no retry will ever change that.
		return fmt.Errorf("%w: no session lock held by pid %d@%s on session %s", models.ErrLockNotHeld, owner.PID, owner.Host, sessionID)
	}
	return nil
}

// ReleaseSessionLock drops the lock — but only the row owner actually holds.
// A mismatched owner is a silent no-op: Run()'s deferred release must never
// be able to delete a lock some other process (e.g. a --force takeover)
// legitimately holds now.
func (s *SQLiteSessionStore) ReleaseSessionLock(sessionID string, owner models.LockOwner) error {
	_, err := s.db.Exec(`
		DELETE FROM session_locks WHERE session_id = ? AND pid = ? AND host = ?
	`, sessionID, owner.PID, owner.Host)
	if err != nil {
		return fmt.Errorf("release session lock: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ForkSession
// ---------------------------------------------------------------------------

// ForkSession copies id's session (model) and all of its messages, in seq
// order, into a brand new session and returns it. The original session is
// read-only to this call — Load/LoadMessages only — so it and its messages
// are byte-for-byte untouched; this is the --fork remedy for a locked
// session (see resolveSession), which must never mutate the session someone
// else is actively using.
//
// cwd is the new session's cwd (normalized — see normalizeCWD), always the
// CALLER's current working directory, deliberately not orig's stored cwd
// metadata: the fork is happening in the caller's directory right now, and
// inheriting a possibly stale/different original cwd would make the forked
// session invisible to a later LatestInDir in the directory the fork
// actually continues in.
func (s *SQLiteSessionStore) ForkSession(id string, cwd string) (*models.Session, error) {
	orig, err := s.Load(id)
	if err != nil {
		return nil, fmt.Errorf("load session to fork: %w", err)
	}
	msgs, err := s.LoadMessages(id)
	if err != nil {
		return nil, fmt.Errorf("load messages to fork: %w", err)
	}
	return s.forkInto(id, orig.Title, orig.Metadata, cwd, msgs)
}

// ForkSessionFromMessages is the SessionRepository interface method of the
// same name (see its doc comment there for why it never reads origID's
// stored rows): it shares ForkSession's exact session-creation logic via
// forkInto, but with msgs/origTitle/origMetadata supplied directly by the
// caller instead of read back from storage.
func (s *SQLiteSessionStore) ForkSessionFromMessages(origID, origTitle string, origMetadata map[string]string, cwd string, msgs []models.Message) (*models.Session, error) {
	return s.forkInto(origID, origTitle, origMetadata, cwd, msgs)
}

// forkInto is ForkSession and ForkSessionFromMessages' shared
// implementation: create a brand-new session whose title is derived from
// (origTitle, origID) via forkedTitle, whose metadata is origMetadata plus
// forked_from/cwd, and whose messages are msgs (seq order preserved,
// renumbered 1..N so gaps from a prior /undo on the source don't leak
// through). Deliberately takes no origID *session* to read from — only the
// plain values a caller may already have in memory — so it can never touch
// storage for anything other than the brand-new row itself.
func (s *SQLiteSessionStore) forkInto(origID, origTitle string, origMetadata map[string]string, cwd string, msgs []models.Message) (*models.Session, error) {
	normCWD := normalizeCWD(cwd)
	title := forkedTitle(origTitle, origID)

	metadata := make(map[string]string, len(origMetadata)+1)
	for k, v := range origMetadata {
		metadata[k] = v
	}
	metadata["forked_from"] = origID
	if normCWD != "" {
		metadata["cwd"] = normCWD
	} else {
		delete(metadata, "cwd")
	}
	metaJSON, _ := json.Marshal(metadata)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin fork session: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	newID := generateSessionID(now)
	_, err = tx.Exec(`
		INSERT INTO sessions (id, title, model, cwd, source, state, created_at, updated_at, metadata)
		VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)
	`, newID, title, metadata["model"], normCWD,
		"cli", unixFrac(now), unixFrac(now), string(metaJSON))
	if err != nil {
		return nil, fmt.Errorf("create forked session: %w", err)
	}

	// msgs is already ordered by seq (LoadMessages: ORDER BY seq ASC for
	// ForkSession's caller; ForkSessionFromMessages' caller — the REPL's
	// in-memory r.sess.Messages — is append-ordered the same way), so a
	// simple 1..N renumbering preserves relative order exactly even if the
	// original had gaps (e.g. from a prior /undo).
	for i, m := range msgs {
		toolCallsJSON := "[]"
		if len(m.ToolCalls) > 0 {
			b, _ := json.Marshal(m.ToolCalls)
			toolCallsJSON = string(b)
		}
		toolResultJSON := ""
		if m.ToolResult != nil {
			b, _ := json.Marshal(m.ToolResult)
			toolResultJSON = string(b)
		}
		// N4: ForkSessionFromMessages' caller (the REPL's in-memory
		// r.sess.Messages) can contain a message runTurn built without
		// ever setting CreatedAt (repl.go's userMsg) — AppendMessage
		// defaults that to time.Now() on the ORIGINAL write, but msgs here
		// bypasses AppendMessage entirely, so forkInto must apply the same
		// fallback itself. Falling back to `now` (this fork's own
		// creation time, already computed above) rather than a fresh
		// time.Now() per message keeps every message in a single fork
		// call consistently timestamped, same as a real AppendMessage
		// call would have been close to at the time.
		createdAt := m.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		content := contentWithImagePlaceholder(m.Content, m.Images)
		newMsgID := newID + "_" + nextSeqID()
		_, err = tx.Exec(`
			INSERT INTO messages (id, session_id, seq, role, content, tool_calls, tool_result, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, newMsgID, newID, i+1, string(m.Role), content, toolCallsJSON, toolResultJSON, unixFrac(createdAt))
		if err != nil {
			return nil, fmt.Errorf("copy message %d to forked session: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit fork session: %w", err)
	}

	return s.Load(newID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// forkSuffixPattern strips a trailing " (forked from <id>)" so forkedTitle
// can compute the base title without stacking suffixes on repeated forks.
var forkSuffixPattern = regexp.MustCompile(`\s*\(forked from [^()]*\)\s*$`)

// forkedTitle computes a forked session's title from the original's.
//
// Two things it deliberately avoids:
//   - origTitle == "" used to fall back to origID, which for an untitled
//     session produced "20260831_235721_55e9 (forked from
//     20260831_235721_55e9)" — the same id twice, once as a fake "title"
//     and once naming the session it was forked from. An untitled fork has
//     no base worth keeping, so the base is simply omitted.
//   - Forking a session that is ITSELF already a fork used to keep
//     stacking suffixes without bound: "T (forked from A) (forked from B)
//     (forked from C) ...". forkSuffixPattern strips exactly one trailing
//     "(forked from ...)" from the base before appending the new one, so
//     the title always names only the immediate parent — the full lineage
//     is still recoverable via the chain of metadata["forked_from"] values,
//     it just isn't spelled out in every title.
func forkedTitle(origTitle, origID string) string {
	base := strings.TrimSpace(forkSuffixPattern.ReplaceAllString(origTitle, ""))
	if base == "" {
		return fmt.Sprintf("(forked from %s)", origID)
	}
	return fmt.Sprintf("%s (forked from %s)", base, origID)
}

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
