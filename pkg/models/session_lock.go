package models

import (
	"errors"
	"fmt"
	"time"
)

// ErrLockNotHeld is the sentinel RefreshSessionLock returns when the update
// touched zero rows — i.e. this process definitively does not hold the
// session lock it thinks it does (most likely: force-stolen by another
// `deepai --force`, or the row was never created for it in the first
// place). This is deliberately NOT the same signal as a wrapped driver
// error from the UPDATE itself (e.g. SQLITE_BUSY from a concurrent writer
// holding a transaction open elsewhere on the DB) — that is transient
// contention, not lock loss, and pkg/chat/repl.go's heartbeatTick relies on
// errors.Is(err, ErrLockNotHeld) to tell the two apart (H1, session-lock
// review round 2: treating every RefreshSessionLock error as "lock lost"
// killed a live session over a 5-7s SQLITE_BUSY blip caused by perfectly
// ordinary same-DB contention).
var ErrLockNotHeld = errors.New("session lock not held")

// LockOwner identifies the OS process holding a session lock (see
// SessionRepository.AcquireSessionLock). PID and Host together are what let
// two processes on the SAME host tell a live holder from a dead one; across
// hosts a pid is meaningless, so cross-host staleness can only be judged by
// heartbeat age (see the SQLite implementation's canAcquireSessionLock).
type LockOwner struct {
	PID  int
	Host string
}

// ErrSessionLocked is returned by AcquireSessionLock when a session is held
// by a live owner and the caller did not force the takeover. It carries the
// holder's identity so a caller can render a human-readable prompt (who has
// it, since when, how long since its last heartbeat) and use errors.As to
// recover those details rather than parsing Error()'s text.
type ErrSessionLocked struct {
	SessionID   string
	Owner       LockOwner
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}

func (e *ErrSessionLocked) Error() string {
	return fmt.Sprintf("session %s is locked by pid %d@%s (heartbeat %s ago)",
		e.SessionID, e.Owner.PID, e.Owner.Host, time.Since(e.HeartbeatAt).Round(time.Second))
}
