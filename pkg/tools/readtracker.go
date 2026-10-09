package tools

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
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
	// gitRewriteAt is the completion time of the most recent bash git
	// command that rewrites working-tree files (see NoteGitRewrite). It is
	// only ever used to explain a change the stamp mismatch already proved —
	// never to waive one.
	gitRewriteAt time.Time
}

type readStamp struct {
	mtimeNano int64
	size      int64
	// sum fingerprints the content at stamp time. mtime+size is the fast
	// path; the fingerprint is what lets a content-preserving rewrite (git
	// checkout/rebase rewriting an unchanged file) pass without a re-read
	// while a real change is still caught.
	sum          string
	lastVerified time.Time
}

var ErrNotReadInSession = errors.New("not read in this session")

// StaleReadError reports an on-disk state that no longer matches the stamp
// from the model's last read/write of the file.
type StaleReadError struct {
	RecordedSize    int64
	CurrentSize     int64
	RecordedModTime time.Time
	CurrentModTime  time.Time
	// LikelyGitRewrite is set when a git command that rewrites working-tree
	// files ran after this file's stamp was last verified and the content no
	// longer matches — session telemetry's R2 failures were dominated by the
	// model's own `git checkout`/`git rebase` rewriting files it had read,
	// which an mtime-only gate could only report as an opaque external
	// change.
	LikelyGitRewrite bool
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
// one file. EvalSymlinks alone is not enough — on a relative path it returns
// a still-relative result, so Abs runs on the resolved path.
func absKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
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
	t.stamps[absKey(path)] = readStamp{
		mtimeNano:    info.ModTime().UnixNano(),
		size:         info.Size(),
		sum:          contentSum(path),
		lastVerified: time.Now(),
	}
}

// NoteGitRewrite records that a bash command running a git subcommand that
// rewrites working-tree files (checkout, rebase, merge, ...) just completed.
// It never un-blocks anything: CheckEdit only reads it to decide whether a
// detected change deserves the git-specific explanation.
func (t *ReadTracker) NoteGitRewrite() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gitRewriteAt = time.Now()
}

func contentSum(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum64())
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
	gitRewriteAt := t.gitRewriteAt
	t.mu.Unlock()
	if !ok {
		return ErrNotReadInSession
	}
	info, err := os.Stat(key)
	if err != nil {
		return nil
	}
	if info.ModTime().UnixNano() == stamp.mtimeNano && info.Size() == stamp.size {
		return nil
	}
	// Content-preserving rewrite: the text the model read is still the text
	// on disk, so editing is safe. Re-stamp so later edits take the fast path
	// instead of re-hashing every time.
	if stamp.sum != "" && contentSum(key) == stamp.sum {
		t.mu.Lock()
		if s, ok := t.stamps[key]; ok {
			s.mtimeNano = info.ModTime().UnixNano()
			s.size = info.Size()
			s.lastVerified = time.Now()
			t.stamps[key] = s
		}
		t.mu.Unlock()
		return nil
	}
	return &StaleReadError{
		RecordedSize:     stamp.size,
		CurrentSize:      info.Size(),
		RecordedModTime:  time.Unix(0, stamp.mtimeNano),
		CurrentModTime:   info.ModTime(),
		LikelyGitRewrite: !gitRewriteAt.IsZero() && gitRewriteAt.After(stamp.lastVerified),
	}
}
