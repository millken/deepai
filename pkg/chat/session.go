package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// Session holds conversation history for a single chat session.
type Session struct {
	ID        string
	CreatedAt time.Time
	Title     string
	Messages  []models.Message
	Metadata  map[string]string
}

// SessionMeta is a lightweight summary used for listing sessions.
type SessionMeta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Title     string    `json:"title"`
	MsgCount  int       `json:"msg_count"`
}

// SessionStore persists sessions as JSON files in ~/.deepai/sessions/.
type SessionStore struct {
	dir string
}

// NewSessionStore creates a session store backed by the given directory.
func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	return &SessionStore{dir: dir}, nil
}

// Create makes a new session with a timestamp-based ID.
func (s *SessionStore) Create() (*Session, error) {
	now := time.Now()
	id := now.Format("20060102_150405")
	return &Session{
		ID:        id,
		CreatedAt: now,
		Metadata:  make(map[string]string),
	}, nil
}

// Load reads a session from disk.
func (s *SessionStore) Load(id string) (*Session, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", id, err)
	}
	return &sess, nil
}

// Save persists a session to disk.
func (s *SessionStore) Save(sess *Session) error {
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	return os.WriteFile(s.path(sess.ID), data, 0600)
}

// Latest returns the most recent session, or nil if none exist.
func (s *SessionStore) Latest() (*Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil
	}
	if len(entries) == 0 {
		return nil, nil
	}

	// Sort by name (timestamp-based, so lexical = chronological).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	return s.Load(names[len(names)-1])
}

// ListRecent returns up to n most recent session summaries.
func (s *SessionStore) ListRecent(n int) ([]SessionMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil
	}

	var metas []SessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		metas = append(metas, SessionMeta{
			ID:        sess.ID,
			CreatedAt: sess.CreatedAt,
			Title:     sess.Title,
			MsgCount:  len(sess.Messages),
		})
		if len(metas) >= n {
			break
		}
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.After(metas[j].CreatedAt)
	})
	return metas, nil
}

func (s *SessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}
