package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ReadTracker records which files the current session has seen via read_file
// or write_file (and the on-disk state at that moment), so edit_file can
// refuse edits to files the model never read or that changed on disk since —
// the two dominant edit_file failure modes observed in session telemetry
// (old_string retyped from memory, invented hashes).
//
// A nil tracker (or none in ctx) disables the gate entirely: standalone
// handler calls and existing tests keep the legacy behavior.
type ReadTracker struct {
	mu     sync.Mutex
	stamps map[string]readStamp
}

type readStamp struct {
	mtimeNano int64
	size      int64
}

var ErrNotReadInSession = errors.New("not read in this session")

// StaleReadError reports an on-disk state that no longer matches the stamp
// from the model's last read/write of the file.
type StaleReadError struct {
	RecordedSize     int64
	CurrentSize      int64
	RecordedModTime  time.Time
	CurrentModTime   time.Time
}

func (e *StaleReadError) Error() string {
	return "changed on disk since last read"
}

func NewReadTracker() *ReadTracker {
	return &ReadTracker{stamps: make(map[string]readStamp)}
}

const readTrackerContextKey contextKey = "tool_read_tracker"

func WithReadTracker(ctx context.Context, tracker *ReadTracker) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, readTrackerContextKey, tracker)
}

func ReadTrackerFromContext(ctx context.Context) *ReadTracker {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(readTrackerContextKey).(*ReadTracker)
	return t
}

// absKey anchors relative paths so the same file reached as "a.go" and
// "/abs/a.go" lands on one entry. Symlinks are resolved for the same reason:
// reading via a link and editing via the real path (or vice versa) is still
// one file.
func absKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// Record stamps the on-disk state observed right after a successful
// read_file or write_file. Stat-after-I/O (not before) so the stamp matches
// the version the model actually saw.
func (t *ReadTracker) Record(path string, info os.FileInfo) {
	if t == nil || info == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stamps[absKey(path)] = readStamp{mtimeNano: info.ModTime().UnixNano(), size: info.Size()}
}

// CheckEdit returns nil when editing path is allowed: the file was read (or
// written) in this session AND its on-disk state still matches that stamp.
// A file missing from disk is allowed through so the handler's own read
// produces the canonical "read failed" error instead of a gate error.
func (t *ReadTracker) CheckEdit(path string) error {
	if t == nil {
		return nil
	}
	key := absKey(path)
	t.mu.Lock()
	stamp, ok := t.stamps[key]
	t.mu.Unlock()
	if !ok {
		return ErrNotReadInSession
	}
	info, err := os.Stat(key)
	if err != nil {
		return nil
	}
	if info.ModTime().UnixNano() != stamp.mtimeNano || info.Size() != stamp.size {
		return &StaleReadError{
			RecordedSize:    stamp.size,
			CurrentSize:     info.Size(),
			RecordedModTime: time.Unix(0, stamp.mtimeNano),
			CurrentModTime:  info.ModTime(),
		}
	}
	return nil
}
