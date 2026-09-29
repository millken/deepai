package hook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcher_WebhookSinkPostsJSON(t *testing.T) {
	var mu sync.Mutex
	var posts int
	var body Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var got Event
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		mu.Lock()
		posts++
		body = got
		mu.Unlock()
	}))
	defer srv.Close()

	d := NewDispatcher([]NotificationConfig{{Events: []string{string(EventAsk)}, WebhookURL: srv.URL}})
	d.Fire(context.Background(), Event{Kind: EventAsk, SessionID: "sess-1", WorkDir: "/w", Message: "Pick one: A or B?"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := posts >= 1
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("posts = %d, want exactly 1", posts)
	}
	if body.Kind != EventAsk || body.SessionID != "sess-1" {
		t.Fatalf("body = %+v, want kind=%s session=sess-1", body, EventAsk)
	}
	if !strings.Contains(body.Message, "Pick one: A or B?") {
		t.Fatalf("message = %q, want the question verbatim", body.Message)
	}
}

func TestDispatcher_EventFilterSkipsUnsubscribed(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
	}))
	defer srv.Close()

	d := NewDispatcher([]NotificationConfig{{Events: []string{string(EventAsk)}, WebhookURL: srv.URL}})
	d.Fire(context.Background(), Event{Kind: EventTurnEnd, SessionID: "s", Message: "done"})
	d.WaitIdle(200 * time.Millisecond)
	if n := posts.Load(); n != 0 {
		t.Fatalf("posts = %d, want 0 for a kind the entry does not subscribe to", n)
	}
}

func TestDispatcher_EmptyEventsMeansAll(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
	}))
	defer srv.Close()

	d := NewDispatcher([]NotificationConfig{{WebhookURL: srv.URL}})
	d.Fire(context.Background(), Event{Kind: EventIdle, SessionID: "s", Message: "idle"})
	d.WaitIdle(200 * time.Millisecond)
	if n := posts.Load(); n != 1 {
		t.Fatalf("posts = %d, want 1 (empty events = all events)", n)
	}
}

func TestDispatcher_CommandSinkGetsStdinAndEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "hook.env")
	script := "#!/bin/sh\nprintf '%s' \"$DEEPAI_EVENT $DEEPAI_SESSION_ID\" > " + out + "\ncat > " + out + ".json\n"
	scriptPath := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher([]NotificationConfig{{Events: []string{string(EventTurnEnd)}, Command: []string{scriptPath}}})
	d.Fire(context.Background(), Event{Kind: EventTurnEnd, SessionID: "sess-42", WorkDir: "/w", Message: "turn done"})

	var env string
	var stdinGot map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(out + ".json"); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &stdinGot)
			if data, err := os.ReadFile(out); err == nil {
				env = string(data)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if env != "turn_end sess-42" {
		t.Fatalf("env capture = %q, want %q", env, "turn_end sess-42")
	}
	if stdinGot == nil {
		t.Fatal("command sink never wrote its stdin JSON")
	}
	if stdinGot["kind"] != string(EventTurnEnd) || stdinGot["session_id"] != "sess-42" {
		t.Fatalf("stdin JSON = %+v, want kind=%s session_id=sess-42", stdinGot, EventTurnEnd)
	}
}

func TestDispatcher_CommandSinkExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	script := "#!/bin/sh\ntrue\n"
	path := filepath.Join(home, "n.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher([]NotificationConfig{{Command: []string{"~/n.sh"}}})
	if got := d.entries[0].command[0]; got != path {
		t.Fatalf("expanded = %q, want %q", got, path)
	}
}

func TestDispatcher_FireIsNonBlockingOnDeadAndStuckSinks(t *testing.T) {
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer stuck.Close()
	defer close(release)
	dead := "http://127.0.0.1:1"

	d := NewDispatcher([]NotificationConfig{
		{WebhookURL: dead},
		{WebhookURL: stuck.URL},
	})
	start := time.Now()
	d.Fire(context.Background(), Event{Kind: EventIdle, SessionID: "s", Message: "x"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Fire blocked for %v on slow sinks, want immediate return", elapsed)
	}
	d.WaitIdle(2 * time.Second)
}

func TestDispatcher_WebhookSinkTimesOut(t *testing.T) {
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer stuck.Close()
	defer close(release)

	d := NewDispatcher([]NotificationConfig{{WebhookURL: stuck.URL}})
	start := time.Now()
	d.fireNow(context.Background(), Event{Kind: EventIdle, SessionID: "s", Message: "x"})
	elapsed := time.Since(start)
	if elapsed >= 2*sinkTimeout {
		t.Fatalf("webhook sink not bounded by sinkTimeout: %v", elapsed)
	}
}

func TestDispatcher_SinkErrorIsIsolated(t *testing.T) {
	var posts atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
	}))
	defer ok.Close()

	d := NewDispatcher([]NotificationConfig{
		{WebhookURL: "http://127.0.0.1:1"},
		{WebhookURL: ok.URL},
	})
	d.fireNow(context.Background(), Event{Kind: EventIdle, SessionID: "s", Message: "x"})
	if n := posts.Load(); n != 1 {
		t.Fatalf("healthy sink skipped after a failing sibling: posts=%d, want 1", n)
	}
}
