package models

import "time"

// SessionMeta is a lightweight summary used for listing sessions.
type SessionMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	MsgCount  int       `json:"msg_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionStats holds aggregate statistics about all sessions.
type SessionStats struct {
	SessionCount int
	MessageCount int64
	OldestAt     time.Time
	LatestAt     time.Time
}

// SessionExport is a session with its messages for export.
type SessionExport struct {
	Session  Session   `json:"session"`
	Messages []Message `json:"messages"`
}

// CreateOpts holds optional fields for session creation.
type CreateOpts struct {
	Title  string
	Model  string
	CWD    string
	Source string
}

// SessionRepository defines the persistence interface for sessions.
type SessionRepository interface {
	Create(opts CreateOpts) (*Session, error)
	Load(id string) (*Session, error)
	LoadMessages(id string) ([]Message, error)
	Save(sess *Session) error
	AppendMessage(sessionID string, msg Message) error
	DeleteMessagesAfterSeq(sessionID string, afterSeq int) error
	DeleteLastUserTurn(sessionID string) (int, error)
	Delete(id string) error
	Rename(id, title string) error
	ListRecent(limit int) ([]SessionMeta, error)
	Search(query string, limit int) ([]SessionMeta, error)
	Stats() (SessionStats, error)
	Prune(olderThanDays int, dryRun bool) (int, error)
	ExportSession(id string) (*SessionExport, error)
	ExportAll() ([]SessionExport, error)
	Resolve(input string) (*Session, error)
	ResolveAll(input string) ([]SessionMeta, error)
	Latest() (*Session, error)
	SetTitle(id, title string) error
	Close() error
}
