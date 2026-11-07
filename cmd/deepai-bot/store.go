package main

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/millken/deepai/pkg/chat"
	"github.com/millken/deepai/pkg/models"
)

// botSessionStore is the bot.Store over bot.db: the CLI's session store
// implementation (schema, migrations, session locks) plus the workdir-scoped
// queries only the bot needs. cwd values are compared verbatim, so callers
// pass the same symlink-resolved absolute workdir they create sessions with.
type botSessionStore struct {
	*chat.SQLiteSessionStore
	db *sql.DB
}

func newBotSessionStore(db *sql.DB) *botSessionStore {
	return &botSessionStore{SQLiteSessionStore: chat.NewSQLiteSessionStoreFromDB(db), db: db}
}

func (s *botSessionStore) ListRecentInDir(cwd string, limit int) ([]models.SessionMeta, error) {
	if cwd == "" {
		return s.ListRecent(limit)
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT s.id, s.title, s.cwd, COUNT(m.id), s.created_at, s.updated_at
		FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
		WHERE s.cwd = ?
		GROUP BY s.id ORDER BY s.updated_at DESC, s.id DESC LIMIT ?
	`, cwd, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions in dir: %w", err)
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
	var got sql.NullString
	if err := s.db.QueryRow(`SELECT cwd FROM sessions WHERE id = ?`, id).Scan(&got); err != nil {
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("session %s not found", id)
		}
		return false, fmt.Errorf("session cwd: %w", err)
	}
	return got.String != "" && got.String == cwd, nil
}
