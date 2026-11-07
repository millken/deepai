package bot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// MemoryStore is a non-persistent Store, used by tests and by
// `deepai-bot serve --ephemeral`.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
	messages map[string][]models.Message
	cwd      map[string]string
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: map[string]*models.Session{},
		messages: map[string][]models.Message{},
		cwd:      map[string]string{},
	}
}

func (s *MemoryStore) Create(opts models.CreateOpts) (*models.Session, error) {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	now := time.Now().UTC()
	sess := &models.Session{
		ID:        now.Format("20060102_150405") + "_" + hex.EncodeToString(b),
		Title:     opts.Title,
		State:     models.SessionStateActive,
		Metadata:  map[string]string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if opts.Model != "" {
		sess.Metadata["model"] = opts.Model
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	s.cwd[sess.ID] = opts.CWD
	cp := *sess
	return &cp, nil
}

func (s *MemoryStore) Load(id string) (*models.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	cp := *sess
	return &cp, nil
}

func (s *MemoryStore) LoadMessages(id string) ([]models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.Message(nil), s.messages[id]...), nil
}

func (s *MemoryStore) AppendMessage(sessionID string, msg models.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	s.messages[sessionID] = append(s.messages[sessionID], msg)
	sess.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryStore) ListRecentInDir(cwd string, limit int) ([]models.SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.SessionMeta, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if cwd != "" && s.cwd[id] != cwd {
			continue
		}
		out = append(out, models.SessionMeta{
			ID: id, Title: sess.Title, CWD: s.cwd[id], MsgCount: len(s.messages[id]),
			CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) InDir(id, cwd string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false, fmt.Errorf("session %s not found", id)
	}
	return s.cwd[id] != "" && s.cwd[id] == cwd, nil
}

func (s *MemoryStore) Rename(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	sess.Title = title
	return nil
}

func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return fmt.Errorf("session %s not found", id)
	}
	delete(s.sessions, id)
	delete(s.messages, id)
	return nil
}
