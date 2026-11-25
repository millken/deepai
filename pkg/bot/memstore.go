package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// MemoryStore is a non-persistent Store, BotStore and BotRegistry used by
// tests and by `deepai-bot serve --ephemeral`.
type MemoryStore struct {
	mu       sync.Mutex
	bots     map[string]BotProfile
	sessions map[string]*models.Session
	messages map[string][]models.Message
	cwd      map[string]string
	botID    map[string]string
}

// NewMemoryStore returns a MemoryStore seeded with the system default bot.
func NewMemoryStore() *MemoryStore {
	now := time.Now().UTC()
	return &MemoryStore{
		bots: map[string]BotProfile{
			DefaultBotID: {
				ID:        DefaultBotID,
				Name:      "Default",
				Enabled:   true,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
		sessions: map[string]*models.Session{},
		messages: map[string][]models.Message{},
		cwd:      map[string]string{},
		botID:    map[string]string{},
	}
}

func (s *MemoryStore) EnsureDefault(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bots == nil {
		s.bots = map[string]BotProfile{}
	}
	if _, ok := s.bots[DefaultBotID]; !ok {
		now := time.Now().UTC()
		s.bots[DefaultBotID] = BotProfile{
			ID: DefaultBotID, Name: "Default", Enabled: true, CreatedAt: now, UpdatedAt: now,
		}
	}
	return nil
}

func (s *MemoryStore) ListBots(context.Context) ([]BotProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BotProfile, 0, len(s.bots))
	for _, b := range s.bots {
		out = append(out, b.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == DefaultBotID {
			return true
		}
		if out[j].ID == DefaultBotID {
			return false
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) GetBot(_ context.Context, id string) (BotProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bots[id]
	if !ok {
		return BotProfile{}, fmt.Errorf("%w: %s", ErrUnknownBot, id)
	}
	return b.Clone(), nil
}

func (s *MemoryStore) CreateBot(_ context.Context, b BotProfile) (BotProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bots[b.ID]; exists {
		return BotProfile{}, fmt.Errorf("%w: %s", ErrBotExists, b.ID)
	}
	s.bots[b.ID] = b.Clone()
	return b.Clone(), nil
}

func (s *MemoryStore) UpdateBot(_ context.Context, b BotProfile) (BotProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bots[b.ID]; !exists {
		return BotProfile{}, fmt.Errorf("%w: %s", ErrUnknownBot, b.ID)
	}
	s.bots[b.ID] = b.Clone()
	return b.Clone(), nil
}

func (s *MemoryStore) DeleteBot(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bots[id]; !exists {
		return fmt.Errorf("%w: %s", ErrUnknownBot, id)
	}
	delete(s.bots, id)
	return nil
}

func (s *MemoryStore) Create(opts models.CreateOpts) (*models.Session, error) {
	return s.CreateForBot(DefaultBotID, opts)
}

func (s *MemoryStore) CreateForBot(botID string, opts models.CreateOpts) (*models.Session, error) {
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
	s.botID[sess.ID] = effectiveBotID(botID)
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
	return s.ListRecentForBot(DefaultBotID, cwd, limit)
}

func (s *MemoryStore) ListRecentForBot(botID, cwd string, limit int) ([]models.SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	targetBot := effectiveBotID(botID)
	out := make([]models.SessionMeta, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if cwd != "" && s.cwd[id] != cwd {
			continue
		}
		if effectiveBotID(s.botID[id]) != targetBot {
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
	_, gotCWD, err := s.SessionOwner(id)
	if err != nil {
		return false, err
	}
	return gotCWD != "" && gotCWD == cwd, nil
}

func (s *MemoryStore) SessionOwner(id string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return "", "", fmt.Errorf("session %s not found", id)
	}
	return effectiveBotID(s.botID[id]), s.cwd[id], nil
}

func (s *MemoryStore) CountBotSessions(botID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := effectiveBotID(botID)
	n := 0
	for id := range s.sessions {
		if effectiveBotID(s.botID[id]) == target {
			n++
		}
	}
	return n, nil
}

func (s *MemoryStore) HasExternalBotLock(string, models.LockOwner) (bool, error) {
	return false, nil
}

func (s *MemoryStore) DeleteBotCascade(botID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := effectiveBotID(botID)
	for id := range s.sessions {
		if effectiveBotID(s.botID[id]) == target {
			delete(s.sessions, id)
			delete(s.messages, id)
			delete(s.cwd, id)
			delete(s.botID, id)
		}
	}
	delete(s.bots, target)
	return nil
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
	delete(s.cwd, id)
	delete(s.botID, id)
	return nil
}
