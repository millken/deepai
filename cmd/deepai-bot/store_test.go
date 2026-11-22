package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/models"
)

func TestBotSessionStoreScopesByDir(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bot.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newBotSessionStore(db)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	var _ bot.Store = store
	if _, ok := any(store).(bot.Locker); !ok {
		t.Fatal("botSessionStore must keep the session locks")
	}

	a, _ := store.Create(models.CreateOpts{CWD: "/proj/a", Source: bot.SessionSource})
	b, _ := store.Create(models.CreateOpts{CWD: "/proj/b", Source: bot.SessionSource})
	none, _ := store.Create(models.CreateOpts{Source: bot.SessionSource})

	metas, err := store.ListRecentInDir("/proj/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != a.ID || metas[0].CWD != "/proj/a" {
		t.Fatalf("ListRecentInDir(/proj/a) = %+v, want only %s", metas, a.ID)
	}
	if all, _ := store.ListRecentInDir("", 10); len(all) != 3 {
		t.Fatalf("ListRecentInDir(\"\") = %d sessions, want 3", len(all))
	}

	for _, c := range []struct {
		id   string
		want bool
	}{{a.ID, true}, {b.ID, false}, {none.ID, false}} {
		if ok, err := store.InDir(c.id, "/proj/a"); err != nil || ok != c.want {
			t.Errorf("InDir(%s) = %v, %v; want %v", c.id, ok, err, c.want)
		}
	}
	if _, err := store.InDir("missing", "/proj/a"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("InDir(missing) err = %v, want not found", err)
	}
}

// PR #49 round-2 M1: message metadata (including schedule run_id/source) must
// survive SQLite persistence and reload, including upgrading a pre-existing DB
// that was created without the messages.metadata column.
func TestBotSessionStorePersistsMessageMetadataAndMigratesOldDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate an older DB whose messages table lacks the metadata column.
	_, err = db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (1);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, user_id TEXT DEFAULT '', title TEXT NOT NULL DEFAULT '',
			model TEXT DEFAULT '', cwd TEXT DEFAULT '', source TEXT DEFAULT 'bot',
			state TEXT DEFAULT 'active', created_at REAL NOT NULL, updated_at REAL NOT NULL, metadata TEXT DEFAULT '{}'
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL, role TEXT NOT NULL, content TEXT DEFAULT '',
			tool_calls TEXT DEFAULT '[]', tool_result TEXT DEFAULT '', created_at REAL NOT NULL,
			UNIQUE(session_id, seq)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	store := newBotSessionStore(db)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate old db: %v", err)
	}
	sess, err := store.Create(models.CreateOpts{CWD: "/proj/a", Source: bot.SessionSource})
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{
		"source":      "schedule",
		"schedule_id": "sch_123",
		"run_id":      "run_456",
	}
	if err := store.AppendMessage(sess.ID, models.Message{
		Role:     models.RoleHuman,
		Content:  "[定时任务] 检查",
		Metadata: meta,
	}); err != nil {
		t.Fatal(err)
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("loaded %d messages, want 1", len(msgs))
	}
	if got := msgs[0].Metadata["run_id"]; got != "run_456" {
		t.Fatalf("reloaded metadata = %+v, want run_id=run_456", msgs[0].Metadata)
	}
	if got := msgs[0].Metadata["source"]; got != "schedule" {
		t.Fatalf("reloaded metadata = %+v, want source=schedule", msgs[0].Metadata)
	}
}
