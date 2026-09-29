// Package hook delivers REPL lifecycle events to user-configured sinks
// (webhook URL or local command) and polls a user-configured control source
// for remote commands (docs/HOOKS.md). Both directions are strictly opt-in:
// with no notifications/control config, nothing runs.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Kind string

const (
	EventTurnStart        Kind = "turn_start"
	EventTurnEnd          Kind = "turn_end"
	EventAsk              Kind = "ask"
	EventMissionEnd       Kind = "mission_end"
	EventPRAwaitingMerge  Kind = "pr_awaiting_merge"
	EventIdle             Kind = "idle"
	EventSessionEnd       Kind = "session_end"
)

// Event is one observable REPL lifecycle moment. Message carries the
// human-readable payload a chat notification will show verbatim.
type Event struct {
	Kind      Kind     `json:"kind"`
	SessionID string   `json:"session_id"`
	WorkDir   string   `json:"work_dir,omitempty"`
	Message   string   `json:"message"`
	Time      time.Time `json:"time"`
}

// Emitter is the outbound side. A nil Emitter (or nil *Dispatcher) must be a
// silent no-op for every call site.
type Emitter interface {
	Fire(ctx context.Context, evt Event)
}

// NotificationConfig is one sink declaration as it appears in config.yaml.
// Events empty means all kinds.
type NotificationConfig struct {
	Events     []string `yaml:"events,omitempty"`
	WebhookURL string   `yaml:"webhook_url,omitempty"`
	Command    []string `yaml:"command,omitempty"`
}

const sinkTimeout = 5 * time.Second

type sinkEntry struct {
	events    []string
	all       bool
	webhook   string
	command   []string
}

// Dispatcher fans an event out to every configured sink asynchronously. Fire
// never blocks the REPL: each sink runs in its own goroutine with a fixed
// timeout, and a failing sink only logs.
type Dispatcher struct {
	entries []sinkEntry
	client  *http.Client
	wg      sync.WaitGroup
}

func NewDispatcher(cfgs []NotificationConfig) *Dispatcher {
	d := &Dispatcher{client: &http.Client{Timeout: sinkTimeout}}
	for _, c := range cfgs {
		if c.WebhookURL == "" && len(c.Command) == 0 {
			continue
		}
		e := sinkEntry{events: c.Events, all: len(c.Events) == 0, webhook: c.WebhookURL}
		for _, a := range c.Command {
			e.command = append(e.command, expandHome(a))
		}
		d.entries = append(d.entries, e)
	}
	return d
}

// wants: an entry with an empty events list subscribes to everything.
func (e *sinkEntry) wants(k Kind) bool {
	if e.all {
		return true
	}
	for _, want := range e.events {
		if want == string(k) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) Fire(ctx context.Context, evt Event) {
	if d == nil {
		return
	}
	if evt.Time.IsZero() {
		evt.Time = time.Now()
	}
	for i := range d.entries {
		e := &d.entries[i]
		if !e.wants(evt.Kind) {
			continue
		}
		d.wg.Add(1)
		go func(e *sinkEntry) {
			defer d.wg.Done()
			d.deliver(ctx, e, evt)
		}(e)
	}
}

// fireNow delivers synchronously; tests only.
func (d *Dispatcher) fireNow(ctx context.Context, evt Event) {
	if d == nil {
		return
	}
	if evt.Time.IsZero() {
		evt.Time = time.Now()
	}
	for i := range d.entries {
		if e := &d.entries[i]; e.wants(evt.Kind) {
			d.deliver(ctx, e, evt)
		}
	}
}

// WaitIdle blocks until every in-flight delivery has settled (or timeout) —
// tests only; production never waits on notifications.
func (d *Dispatcher) WaitIdle(timeout time.Duration) {
	if d == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (d *Dispatcher) deliver(ctx context.Context, e *sinkEntry, evt Event) {
	if e.webhook != "" {
		if err := d.postWebhook(ctx, e.webhook, evt); err != nil {
			slog.Warn("hook webhook delivery failed", "url", e.webhook, "kind", evt.Kind, "err", err)
		}
	}
	if len(e.command) > 0 {
		if err := runCommandSink(ctx, e.command, evt); err != nil {
			slog.Warn("hook command delivery failed", "argv", e.command, "kind", evt.Kind, "err", err)
		}
	}
}

func (d *Dispatcher) postWebhook(ctx context.Context, url string, evt Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %s", resp.Status)
	}
	return nil
}

// runCommandSink pipes the event JSON to the command's stdin and mirrors the
// essential fields into DEEPAI_* env vars so a bash hook needs no jq.
func runCommandSink(ctx context.Context, argv []string, evt Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	// Same bound as CommandSource.Poll: a grandchild inheriting the pipes
	// (script backgrounds curl and exits) must not hold the delivery
	// goroutine for its lifetime.
	cmd.WaitDelay = time.Second
	cmd.Env = append(cmd.Environ(),
		"DEEPAI_EVENT="+string(evt.Kind),
		"DEEPAI_SESSION_ID="+evt.SessionID,
		"DEEPAI_WORKDIR="+evt.WorkDir,
		"DEEPAI_MESSAGE="+evt.Message,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// expandHome rewrites a leading ~ to $HOME; hook paths live in ~/.deepai.
func expandHome(p string) string {
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p[1] == '\\') {
		return os.Getenv("HOME") + p[1:]
	}
	return p
}
