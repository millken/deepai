// Package channel implements the IM Channel Adapter Gateway for deepai-bot
// (Telegram Long Polling, Feishu/Lark, etc.). It sits strictly on top of
// pkg/bot.Manager and pkg/schedule.Service without coupling to pkg/agent.
//
// See docs/BOT_NEXT_DESIGN.md §5.
package channel

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/bot"
)

const (
	DefaultStreamEditInterval = 800 * time.Millisecond
	DefaultMaxMessageRunes    = 4000
)

// Destination identifies a chat/thread target on an IM platform.
type Destination struct {
	ChatID    string
	ThreadID  string
	ReplyToID string
}

// InboundAttachment carries a downloaded file or image sent by an IM user.
type InboundAttachment struct {
	Name     string
	MimeType string
	Data     []byte
}

// InboundMessage is the normalized message delivered from any IM Adapter to Gateway.
type InboundMessage struct {
	Channel      string // "telegram" | "feishu"
	UserID       string
	ChatID       string
	ThreadID     string // empty for DMs; topic/thread id for group threads
	MessageID    string
	IsGroup      bool
	Mentioned    bool
	Text         string
	CallbackData string // button click payload e.g. "ans:<qid>:<option>"
	Attachments  []InboundAttachment
}

// Adapter connects one IM platform to Gateway (docs/BOT_NEXT_DESIGN.md §5.2).
type Adapter interface {
	Name() string
	Start(ctx context.Context, onInbound func(InboundMessage)) error
	SendText(ctx context.Context, dst Destination, text string) (msgID string, err error)
	EditText(ctx context.Context, dst Destination, msgID, text string) error
	SendQuestion(ctx context.Context, dst Destination, q bot.Question) (msgID string, err error)
	SendAttachment(ctx context.Context, dst Destination, filePath, caption string) error
}

// AccessPolicy enforces default-deny allowlisting per channel (§5.4).
type AccessPolicy struct {
	DefaultBotID       string
	AllowUsers         []string
	AllowChats         []string
	StreamEditInterval time.Duration
	MaxMessageRunes    int
}

// Allowed reports whether (userID, chatID) is permitted. When BOTH AllowUsers
// and AllowChats are empty, every external message is rejected (default-deny).
func (p AccessPolicy) Allowed(userID, chatID string) bool {
	if len(p.AllowUsers) == 0 && len(p.AllowChats) == 0 {
		return false
	}
	userID = strings.TrimSpace(userID)
	chatID = strings.TrimSpace(chatID)
	if userID != "" && slices.Contains(p.AllowUsers, userID) {
		return true
	}
	if chatID != "" && slices.Contains(p.AllowChats, chatID) {
		return true
	}
	return false
}

func (p AccessPolicy) editInterval() time.Duration {
	if p.StreamEditInterval > 0 {
		return p.StreamEditInterval
	}
	return DefaultStreamEditInterval
}

func (p AccessPolicy) maxRunes() int {
	if p.MaxMessageRunes > 0 {
		return p.MaxMessageRunes
	}
	return DefaultMaxMessageRunes
}

func (p AccessPolicy) defaultBot() string {
	if id := strings.TrimSpace(p.DefaultBotID); id != "" {
		return id
	}
	return bot.DefaultBotID
}

// Binding maps one IM conversation thread to a (bot_id, session_id) pair.
type Binding struct {
	Channel   string
	ChatID    string
	ThreadID  string
	BotID     string
	SessionID string
	Model     string
	UpdatedAt time.Time
}

// BindingStore persists channel_bindings in bot.db (or in-memory for tests).
type BindingStore interface {
	GetBinding(ctx context.Context, channel, chatID, threadID string) (Binding, bool, error)
	PutBinding(ctx context.Context, b Binding) error
	DeleteBinding(ctx context.Context, channel, chatID, threadID string) error
}

// ─────────────────────── MemoryBindingStore ───────────────────────

type MemoryBindingStore struct {
	mu   sync.Mutex
	rows map[[3]string]Binding
}

func NewMemoryBindingStore() *MemoryBindingStore {
	return &MemoryBindingStore{rows: map[[3]string]Binding{}}
}

func (s *MemoryBindingStore) GetBinding(_ context.Context, ch, chatID, threadID string) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.rows[[3]string{ch, chatID, threadID}]
	return b, ok, nil
}

func (s *MemoryBindingStore) PutBinding(_ context.Context, b Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now().UTC()
	}
	s.rows[[3]string{b.Channel, b.ChatID, b.ThreadID}] = b
	return nil
}

func (s *MemoryBindingStore) DeleteBinding(_ context.Context, ch, chatID, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, [3]string{ch, chatID, threadID})
	return nil
}

// ─────────────────────── SQLiteBindingStore ───────────────────────

type SQLiteBindingStore struct {
	db *sql.DB
}

func NewSQLiteBindingStore(db *sql.DB) (*SQLiteBindingStore, error) {
	s := &SQLiteBindingStore{db: db}
	if err := s.Migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteBindingStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channel_bindings (
			channel    TEXT NOT NULL,
			chat_id    TEXT NOT NULL,
			thread_id  TEXT NOT NULL DEFAULT '',
			bot_id     TEXT NOT NULL DEFAULT 'default',
			session_id TEXT NOT NULL,
			model      TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (channel, chat_id, thread_id)
		)`)
	return err
}

func (s *SQLiteBindingStore) GetBinding(ctx context.Context, ch, chatID, threadID string) (Binding, bool, error) {
	var b Binding
	var updMS int64
	err := s.db.QueryRowContext(ctx, `
		SELECT channel, chat_id, thread_id, bot_id, session_id, model, updated_at
		FROM channel_bindings WHERE channel = ? AND chat_id = ? AND thread_id = ?`,
		ch, chatID, threadID).Scan(&b.Channel, &b.ChatID, &b.ThreadID, &b.BotID, &b.SessionID, &b.Model, &updMS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Binding{}, false, nil
		}
		return Binding{}, false, err
	}
	b.UpdatedAt = time.UnixMilli(updMS).UTC()
	return b, true, nil
}

func (s *SQLiteBindingStore) PutBinding(ctx context.Context, b Binding) error {
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO channel_bindings (channel, chat_id, thread_id, bot_id, session_id, model, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel, chat_id, thread_id) DO UPDATE SET
			bot_id = excluded.bot_id,
			session_id = excluded.session_id,
			model = excluded.model,
			updated_at = excluded.updated_at`,
		b.Channel, b.ChatID, b.ThreadID, b.BotID, b.SessionID, b.Model, b.UpdatedAt.UnixMilli())
	return err
}

func (s *SQLiteBindingStore) DeleteBinding(ctx context.Context, ch, chatID, threadID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM channel_bindings WHERE channel = ? AND chat_id = ? AND thread_id = ?`,
		ch, chatID, threadID)
	return err
}
