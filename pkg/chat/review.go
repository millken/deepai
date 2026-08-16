package chat

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// Turn-boundary worktree snapshots for the adversarial-review gate
// (docs/ADVERSARIAL_REVIEW_DESIGN.md §4.1-C). A snapshot taken before a
// turn (S0) and another at gate time (S1) attribute to the turn every file
// that is new or changed in S1 relative to S0. This catches edits made
// through bash (go fmt, sed -i, scripts) that the edit_file/write_file
// records are blind to, while the user's own between-turn modifications
// stay in the S0 baseline and are never attributed to the agent.
//
// Attribution is (status, size, mtime)-based rather than porcelain-status-
// based alone: a file that was already dirty in S0 and was modified again
// during the turn keeps the same "M" status in both snapshots, and only
// the stat fingerprint reveals the change. Known residual (accepted in the
// design): a file the user edits externally while the agent's turn is
// running is misattributed to the turn.

// gitCommandTimeout bounds each git invocation so a hung git (e.g. a stale
// index lock) degrades the snapshot to "unavailable" instead of blocking
// the REPL goroutine.
const gitCommandTimeout = 5 * time.Second

// worktreeSnapshot is the dirty state of a git worktree at one instant.
// The zero value (and any snapshot with root == "") means "no snapshot":
// not a git worktree, git missing, or a git invocation failed. changedSince
// on such a snapshot returns nil — the gate then falls back to tool-record
// attribution only, per the design's non-git degradation.
type worktreeSnapshot struct {
	root    string // absolute worktree toplevel
	entries map[string]fileStamp
}

// fileStamp fingerprints one dirty path. size/modTime are zero for paths
// that cannot be stat'ed (e.g. deleted from the worktree); the porcelain
// status still participates in comparison so a deletion that happens
// during a turn (M → D) is attributed.
type fileStamp struct {
	status  string
	size    int64
	modTime int64
}

// takeWorktreeSnapshot captures the dirty state of the git worktree
// containing dir. It never fails hard: any error yields the zero snapshot.
func takeWorktreeSnapshot(dir string) worktreeSnapshot {
	root, ok := gitToplevel(dir)
	if !ok {
		return worktreeSnapshot{}
	}
	out, err := runGit(dir, "status", "--porcelain", "-z")
	if err != nil {
		return worktreeSnapshot{}
	}
	entries := make(map[string]fileStamp)
	for _, e := range parsePorcelainZ(out) {
		stamp := fileStamp{status: e.status}
		if info, err := os.Stat(filepath.Join(root, e.path)); err == nil && !info.IsDir() {
			stamp.size = info.Size()
			stamp.modTime = info.ModTime().UnixNano()
		}
		entries[e.path] = stamp
	}
	return worktreeSnapshot{root: root, entries: entries}
}

// changedSince returns the absolute paths of files that are new or changed
// in s relative to prev, sorted. Either snapshot being unavailable yields
// nil: without a trustworthy baseline, snapshot attribution would blame
// the user's entire dirty tree on the turn, which is worse than degrading
// to tool-record attribution alone.
func (s worktreeSnapshot) changedSince(prev worktreeSnapshot) []string {
	if s.root == "" || prev.root == "" || s.root != prev.root {
		return nil
	}
	var changed []string
	for path, stamp := range s.entries {
		if before, ok := prev.entries[path]; !ok || before != stamp {
			changed = append(changed, filepath.Join(s.root, path))
		}
	}
	sort.Strings(changed)
	return changed
}

type porcelainEntry struct {
	status string
	path   string
}

// parsePorcelainZ parses `git status --porcelain -z` output: NUL-separated
// "XY path" records, where rename/copy records (R or C in either column)
// carry a second NUL-terminated field holding the origin path — consumed
// and ignored here, since only the current path exists in the worktree.
func parsePorcelainZ(out []byte) []porcelainEntry {
	fields := bytes.Split(out, []byte{0})
	var entries []porcelainEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 || f[2] != ' ' {
			continue
		}
		status := string(f[:2])
		entries = append(entries, porcelainEntry{status: status, path: string(f[3:])})
		if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
			i++ // skip the origin-path field
		}
	}
	return entries
}

func gitToplevel(dir string) (string, bool) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := string(bytes.TrimSpace(out))
	return root, root != ""
}

func runGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stderr = nil
	return cmd.Output()
}
