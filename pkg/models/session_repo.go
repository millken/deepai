package models

import "time"

// SessionMeta is a lightweight summary used for listing sessions.
type SessionMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CWD       string    `json:"cwd"`
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
	// LatestInDir is Latest scoped to sessions created in cwd. Returns
	// (nil, nil) when none match, same empty-result convention as Latest.
	LatestInDir(cwd string) (*Session, error)
	SetTitle(id, title string) error
	// AcquireSessionLock claims the single-writer lock on sessionID for
	// owner. force=true steals a live lock outright (see the SQLite
	// implementation's canAcquireSessionLock for the exact takeover rules);
	// otherwise a live foreign lock yields *ErrSessionLocked, recoverable
	// with errors.As so the caller can render who holds it.
	AcquireSessionLock(sessionID string, owner LockOwner, force bool) error
	// RefreshSessionLock updates the lock's heartbeat so it does not go
	// stale while owner is still actively using sessionID. Errors if owner
	// does not currently hold the lock (e.g. it was force-stolen).
	RefreshSessionLock(sessionID string, owner LockOwner) error
	// ReleaseSessionLock drops the lock, but only if owner still holds it —
	// a mismatched owner (e.g. a stale caller after a force takeover) is a
	// silent no-op so it can never delete someone else's lock.
	ReleaseSessionLock(sessionID string, owner LockOwner) error
	// ForkSession copies id's messages (seq order preserved) into a brand
	// new session and returns it, leaving the original session and its
	// messages byte-for-byte untouched. cwd is the NEW session's cwd (the
	// caller's current working directory) — deliberately not inherited from
	// the original session's stored cwd metadata, which could be stale or
	// belong to a different directory than the one the fork is actually
	// happening in.
	ForkSession(id string, cwd string) (*Session, error)
	// ForkSessionFromMessages is ForkSession's counterpart for a caller
	// that already holds, IN MEMORY, the transcript to copy — the REPL's
	// /fork command, rescuing a turn that never reached storage because
	// the REPL's suspended-writes state (sessionLockState.isSuspended) dropped
	// it after a session-lock loss (see pkg/chat/repl.go's onLockLost). Unlike
	// ForkSession, this never reads
	// origID's stored rows at all: msgs is copied exactly as given, and
	// origTitle/origMetadata are the caller's own in-memory copies of what
	// it already knows about origID. origID is used only to compute the
	// fork's title lineage (forkedTitle) and the forked_from metadata key
	// — it is never loaded from storage, so this is safe to call even
	// after origID's lock (or the row itself) no longer belongs to this
	// process.
	ForkSessionFromMessages(origID, origTitle string, origMetadata map[string]string, cwd string, msgs []Message) (*Session, error)
	Close() error
}
