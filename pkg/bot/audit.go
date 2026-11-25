package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultAuditMaxBytes int64 = 10 << 20 // 10 MiB before simple .1 rotation
	defaultNoisyAuditCap       = 60       // max unauthenticated failure records per minute per action
)

// AuditEntry is one line in ~/.deepai/bot/audit.jsonl (docs/BOT_NEXT_DESIGN.md §6.3).
type AuditEntry struct {
	Time    time.Time `json:"time"`
	Action  string    `json:"action"`
	Actor   string    `json:"actor,omitempty"`
	Role    string    `json:"role,omitempty"`
	BotID   string    `json:"bot_id,omitempty"`
	Target  string    `json:"target,omitempty"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

// AuditLogger appends structured JSONL security & governance events with
// size-based rotation and rate limiting for noisy unauthenticated failures.
// Safe for concurrent use; nil receiver is a no-op.
type AuditLogger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	noisy    map[string][]time.Time
}

// NewAuditLogger configures a JSONL audit log at path. maxBytes <= 0 defaults
// to 10 MiB.
func NewAuditLogger(path string, maxBytes int64) (*AuditLogger, error) {
	if maxBytes <= 0 {
		maxBytes = defaultAuditMaxBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &AuditLogger{path: path, maxBytes: maxBytes, noisy: map[string][]time.Time{}}, nil
}

func noisyAuditKey(e AuditEntry) string {
	if e.Action == "auth_failed" {
		return "auth_failed"
	}
	if e.Action == "webhook_triggered" && e.Outcome == "error" {
		return "webhook_error"
	}
	return ""
}

// Record appends one AuditEntry as a single JSON line.
func (a *AuditLogger) Record(e AuditEntry) {
	if a == nil || a.path == "" {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if key := noisyAuditKey(e); key != "" {
		if a.noisy == nil {
			a.noisy = map[string][]time.Time{}
		}
		cutoff := e.Time.Add(-time.Minute)
		hits := a.noisy[key]
		kept := hits[:0]
		for _, ts := range hits {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		if len(kept) >= defaultNoisyAuditCap {
			a.noisy[key] = kept
			return
		}
		a.noisy[key] = append(kept, e.Time)
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	raw = append(raw, '\n')

	if st, err := os.Stat(a.path); err == nil && st.Size()+int64(len(raw)) > a.maxBytes {
		_ = os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(raw)
	_ = f.Close()
}
