package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/chat"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
)

// botSessionStore is the bot.Store, bot.BotStore and bot.BotRegistry over
// ~/.deepai/bot/bot.db: the CLI's session store implementation (schema,
// migrations, session locks) plus the bots table, bot_id scoping, workdir
// scoping and cascade cleanup only deepai-bot needs.
type botSessionStore struct {
	*chat.SQLiteSessionStore
	db *sql.DB
}

func newBotSessionStore(db *sql.DB) *botSessionStore {
	return &botSessionStore{SQLiteSessionStore: chat.NewSQLiteSessionStoreFromDB(db), db: db}
}

const botsSchemaSQL = `
CREATE TABLE IF NOT EXISTS bots (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    title               TEXT NOT NULL DEFAULT '',
    description         TEXT NOT NULL DEFAULT '',
    instructions        TEXT NOT NULL DEFAULT '',
    default_model       TEXT NOT NULL DEFAULT '',
    tool_allowlist_json TEXT NOT NULL DEFAULT '[]',
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    extra_json          TEXT NOT NULL DEFAULT '{}'
);
`

// Migrate runs base session migrations plus the multi-bot schema upgrades.
func (s *botSessionStore) Migrate() error {
	if err := s.SQLiteSessionStore.Migrate(); err != nil {
		return err
	}
	if _, err := s.db.Exec(botsSchemaSQL); err != nil {
		return fmt.Errorf("migrate bots schema: %w", err)
	}
	if err := s.ensureSessionBotIDColumn(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_bot_cwd_updated ON sessions(bot_id, cwd, updated_at DESC)`); err != nil {
		return fmt.Errorf("create sessions bot index: %w", err)
	}
	return s.EnsureDefault(context.Background())
}

func (s *botSessionStore) ensureSessionBotIDColumn() error {
	rows, err := s.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return fmt.Errorf("inspect sessions columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "bot_id" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`ALTER TABLE sessions ADD COLUMN bot_id TEXT NOT NULL DEFAULT 'default'`); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("add sessions.bot_id: %w", err)
	}
	return nil
}

// ─────────────────────────── bot.BotRegistry ───────────────────────────

func (s *botSessionStore) EnsureDefault(ctx context.Context) error {
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO bots (id, name, enabled, created_at, updated_at)
		VALUES (?, 'Default', 1, ?, ?)`, bot.DefaultBotID, now, now)
	return err
}

func (s *botSessionStore) ListBots(ctx context.Context) ([]bot.BotProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, title, description, instructions, default_model, tool_allowlist_json, enabled, created_at, updated_at, extra_json
		FROM bots
		ORDER BY CASE WHEN id = 'default' THEN 0 ELSE 1 END, created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	defer rows.Close()
	var out []bot.BotProfile
	for rows.Next() {
		b, err := scanBotProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *botSessionStore) GetBot(ctx context.Context, id string) (bot.BotProfile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, title, description, instructions, default_model, tool_allowlist_json, enabled, created_at, updated_at, extra_json
		FROM bots WHERE id = ?`, id)
	b, err := scanBotProfile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return bot.BotProfile{}, fmt.Errorf("%w: %s", bot.ErrUnknownBot, id)
		}
		return bot.BotProfile{}, err
	}
	return b, nil
}

func (s *botSessionStore) CreateBot(ctx context.Context, b bot.BotProfile) (bot.BotProfile, error) {
	allowJSON := marshalAllowlist(b.ToolAllowlist)
	extraJSON := marshalExtensions(b.Extensions)
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bots (id, name, title, description, instructions, default_model, tool_allowlist_json, enabled, created_at, updated_at, extra_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Name, b.Title, b.Description, b.Instructions, b.DefaultModel,
		allowJSON, enabled, b.CreatedAt.UnixMilli(), b.UpdatedAt.UnixMilli(), extraJSON)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "constraint") || strings.Contains(strings.ToLower(err.Error()), "unique") {
			return bot.BotProfile{}, fmt.Errorf("%w: %s", bot.ErrBotExists, b.ID)
		}
		return bot.BotProfile{}, err
	}
	return b.Clone(), nil
}

func (s *botSessionStore) UpdateBot(ctx context.Context, b bot.BotProfile) (bot.BotProfile, error) {
	allowJSON := marshalAllowlist(b.ToolAllowlist)
	extraJSON := marshalExtensions(b.Extensions)
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE bots
		SET name=?, title=?, description=?, instructions=?, default_model=?, tool_allowlist_json=?, enabled=?, updated_at=?, extra_json=?
		WHERE id=?`,
		b.Name, b.Title, b.Description, b.Instructions, b.DefaultModel,
		allowJSON, enabled, b.UpdatedAt.UnixMilli(), extraJSON, b.ID)
	if err != nil {
		return bot.BotProfile{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return bot.BotProfile{}, fmt.Errorf("%w: %s", bot.ErrUnknownBot, b.ID)
	}
	return b.Clone(), nil
}

func (s *botSessionStore) DeleteBot(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bots WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", bot.ErrUnknownBot, id)
	}
	return nil
}

type botRowScanner interface{ Scan(dest ...any) error }

func scanBotProfile(row botRowScanner) (bot.BotProfile, error) {
	var b bot.BotProfile
	var allowJSON, extraJSON string
	var enabled int
	var createdAt, updatedAt int64
	if err := row.Scan(&b.ID, &b.Name, &b.Title, &b.Description, &b.Instructions,
		&b.DefaultModel, &allowJSON, &enabled, &createdAt, &updatedAt, &extraJSON); err != nil {
		return bot.BotProfile{}, err
	}
	b.Enabled = enabled != 0
	b.CreatedAt = time.UnixMilli(createdAt).UTC()
	b.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	if allowJSON != "" && allowJSON != "[]" {
		_ = json.Unmarshal([]byte(allowJSON), &b.ToolAllowlist)
	}
	if extraJSON != "" && extraJSON != "{}" {
		_ = json.Unmarshal([]byte(extraJSON), &b.Extensions)
	}
	return b, nil
}

func marshalExtensions(ext bot.BotExtensions) string {
	b, err := json.Marshal(ext)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func marshalAllowlist(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ─────────────────────────── bot.Store / bot.BotStore ───────────────────────────

func (s *botSessionStore) CreateForBot(botID string, opts models.CreateOpts) (*models.Session, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		botID = bot.DefaultBotID
	}
	now := time.Now()
	var randBytes [2]byte
	_, _ = rand.Read(randBytes[:])
	id := fmt.Sprintf("%s_%s", now.Format("20060102_150405"), hex.EncodeToString(randBytes[:]))
	cwd := normalizeSessionCWD(opts.CWD)
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
		source = bot.SessionSource
	}
	ts := float64(now.UnixNano()) / 1e9
	if _, err := s.db.Exec(`
		INSERT INTO sessions (id, title, model, cwd, source, state, created_at, updated_at, metadata, bot_id)
		VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
		id, opts.Title, opts.Model, cwd, source, ts, ts, string(metaJSON), botID); err != nil {
		return nil, fmt.Errorf("create session for bot %s: %w", botID, err)
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

func normalizeSessionCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(cwd)
}

func (s *botSessionStore) ListRecentInDir(cwd string, limit int) ([]models.SessionMeta, error) {
	return s.ListRecentForBot(bot.DefaultBotID, cwd, limit)
}

func (s *botSessionStore) ListRecentForBot(botID, cwd string, limit int) ([]models.SessionMeta, error) {
	if botID == "" {
		botID = bot.DefaultBotID
	}
	query := `
		SELECT s.id, s.title, s.cwd, COUNT(m.id), s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		WHERE s.bot_id = ?`
	args := []any{botID}
	if cwd != "" {
		query += ` AND s.cwd = ?`
		args = append(args, cwd)
	}
	query += ` GROUP BY s.id ORDER BY s.updated_at DESC, s.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions for bot: %w", err)
	}
	defer rows.Close()
	var out []models.SessionMeta
	for rows.Next() {
		var m models.SessionMeta
		var createdAt, updatedAt float64
		if err := rows.Scan(&m.ID, &m.Title, &m.CWD, &m.MsgCount, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		m.CreatedAt = time.Unix(int64(createdAt), 0)
		m.UpdatedAt = time.Unix(int64(updatedAt), 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *botSessionStore) InDir(id, cwd string) (bool, error) {
	_, gotCWD, err := s.SessionOwner(id)
	if err != nil {
		return false, err
	}
	return gotCWD != "" && gotCWD == cwd, nil
}

func (s *botSessionStore) SessionOwner(id string) (string, string, error) {
	var gotBot, gotCWD sql.NullString
	if err := s.db.QueryRow(`SELECT bot_id, cwd FROM sessions WHERE id = ?`, id).Scan(&gotBot, &gotCWD); err != nil {
		if err == sql.ErrNoRows {
			return "", "", fmt.Errorf("session %s not found", id)
		}
		return "", "", fmt.Errorf("session owner: %w", err)
	}
	b := gotBot.String
	if b == "" {
		b = bot.DefaultBotID
	}
	return b, gotCWD.String, nil
}

func (s *botSessionStore) CountBotSessions(botID string) (int, error) {
	if botID == "" {
		botID = bot.DefaultBotID
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE bot_id = ?`, botID).Scan(&n)
	return n, err
}

func (s *botSessionStore) HasExternalBotLock(botID string, self models.LockOwner) (bool, error) {
	if botID == "" {
		botID = bot.DefaultBotID
	}
	// Matches pkg/chat/session.go staleLockAfter (60s).
	cutoff := float64(time.Now().Add(-60 * time.Second).Unix())
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM session_locks l JOIN sessions s ON s.id = l.session_id
		WHERE s.bot_id = ? AND l.heartbeat_at >= ? AND (l.pid != ? OR l.host != ?)`,
		botID, cutoff, self.PID, self.Host).Scan(&n)
	return n > 0, err
}

func (s *botSessionStore) DeleteBotCascade(botID string) error {
	botID = strings.TrimSpace(botID)
	if botID == "" || botID == bot.DefaultBotID {
		return bot.ErrBotDeleteForbidden
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clean memory tables if they exist in bot.db (user scope "bot:<id>" + every session of this bot).
	userKey := memory.UserScope("bot:" + botID).Key()
	if tableExistsTx(tx, "memories") {
		if _, err := tx.Exec(`DELETE FROM memories WHERE session_id = ? OR session_id IN (SELECT id FROM sessions WHERE bot_id = ?)`, userKey, botID); err != nil {
			return fmt.Errorf("delete bot memories: %w", err)
		}
	}
	if tableExistsTx(tx, "memory_gate_verdicts") {
		if _, err := tx.Exec(`DELETE FROM memory_gate_verdicts WHERE scope_key = ? OR scope_key IN (SELECT id FROM sessions WHERE bot_id = ?)`, userKey, botID); err != nil {
			return fmt.Errorf("delete bot memory verdicts: %w", err)
		}
	}
	if tableExistsTx(tx, "channel_bindings") {
		if _, err := tx.Exec(`DELETE FROM channel_bindings WHERE bot_id = ? OR session_id IN (SELECT id FROM sessions WHERE bot_id = ?)`, botID, botID); err != nil {
			return fmt.Errorf("delete bot channel bindings: %w", err)
		}
	}
	if tableExistsTx(tx, "bot_secrets") {
		if _, err := tx.Exec(`DELETE FROM bot_secrets WHERE bot_id = ?`, botID); err != nil {
			return fmt.Errorf("delete bot secrets: %w", err)
		}
	}
	if tableExistsTx(tx, "schedules") {
		if tableExistsTx(tx, "schedule_metas") {
			rows, err := tx.Query(`SELECT DISTINCT work_dir FROM schedules WHERE bot_id = ?`, botID)
			if err != nil {
				return fmt.Errorf("query bot schedule workdirs: %w", err)
			}
			var dirs []string
			for rows.Next() {
				var wd string
				if err := rows.Scan(&wd); err != nil {
					rows.Close()
					return err
				}
				dirs = append(dirs, wd)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			for _, wd := range dirs {
				if _, err := tx.Exec(`
					INSERT INTO schedule_metas (work_dir, version) VALUES (?, 1)
					ON CONFLICT(work_dir) DO UPDATE SET version = version + 1`, wd); err != nil {
					return fmt.Errorf("bump schedule metas: %w", err)
				}
			}
		}
		if tableExistsTx(tx, "schedule_runs") {
			if _, err := tx.Exec(`DELETE FROM schedule_runs WHERE schedule_id IN (SELECT id FROM schedules WHERE bot_id = ?)`, botID); err != nil {
				return fmt.Errorf("delete bot schedule runs: %w", err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM schedules WHERE bot_id = ?`, botID); err != nil {
			return fmt.Errorf("delete bot schedules: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE bot_id = ?`, botID); err != nil {
		return fmt.Errorf("delete bot sessions: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM bots WHERE id = ?`, botID); err != nil {
		return fmt.Errorf("delete bot: %w", err)
	}
	return tx.Commit()
}

func tableExistsTx(tx *sql.Tx, name string) bool {
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}
