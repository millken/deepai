package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/memory"
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

func TestBotSessionStoreMultiBotCRUDAndLegacyMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-multi-bot schema without bots table or sessions.bot_id column.
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
			metadata TEXT DEFAULT '{}', UNIQUE(session_id, seq)
		);
		INSERT INTO sessions (id, title, cwd, created_at, updated_at) VALUES ('legacy_1', 'Old', '/proj/a', 1, 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	store := newBotSessionStore(db)
	var _ bot.BotRegistry = store
	var _ bot.BotStore = store
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureDefault(ctx); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}

	// Legacy session defaults to bot_id = "default".
	ownerBot, ownerCWD, err := store.SessionOwner("legacy_1")
	if err != nil || ownerBot != bot.DefaultBotID || ownerCWD != "/proj/a" {
		t.Fatalf("SessionOwner(legacy_1) = (%q, %q, %v)", ownerBot, ownerCWD, err)
	}

	// Create, get, update bot.
	created, err := store.CreateBot(ctx, bot.BotProfile{
		ID:            "reviewer",
		Name:          "Code Reviewer",
		Title:         "PR Check",
		Instructions:  "Be thorough",
		DefaultModel:  "sonnet",
		ToolAllowlist: []string{"read_file", "mcp__gh__*"},
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	if !slices.Equal(created.ToolAllowlist, []string{"read_file", "mcp__gh__*"}) {
		t.Fatalf("ToolAllowlist = %v", created.ToolAllowlist)
	}
	created.Title = "Updated"
	created.Enabled = false
	updated, err := store.UpdateBot(ctx, created)
	if err != nil || updated.Title != "Updated" || updated.Enabled {
		t.Fatalf("UpdateBot = (%+v, %v)", updated, err)
	}

	// Per-bot session creation and listing.
	revSess, err := store.CreateForBot("reviewer", models.CreateOpts{CWD: "/proj/a", Source: bot.SessionSource})
	if err != nil {
		t.Fatalf("CreateForBot: %v", err)
	}
	defMetas, err := store.ListRecentForBot(bot.DefaultBotID, "/proj/a", 20)
	if err != nil || len(defMetas) != 1 || defMetas[0].ID != "legacy_1" {
		t.Fatalf("ListRecentForBot(default) = %+v, err=%v", defMetas, err)
	}
	revMetas, err := store.ListRecentForBot("reviewer", "/proj/a", 20)
	if err != nil || len(revMetas) != 1 || revMetas[0].ID != revSess.ID {
		t.Fatalf("ListRecentForBot(reviewer) = %+v, err=%v", revMetas, err)
	}
	if n, err := store.CountBotSessions("reviewer"); err != nil || n != 1 {
		t.Fatalf("CountBotSessions(reviewer) = (%d, %v), want (1, nil)", n, err)
	}
}

func TestBotSessionStoreExternalLockAndDeleteCascade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newBotSessionStore(db)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	// Also initialize memory schema on the same bot.db so DeleteBotCascade cleans it.
	ctx := context.Background()
	memStore := memory.NewSQLiteStoreFromDB(db)
	if err := memStore.AutoMigrate(ctx); err != nil {
		t.Fatal(err)
	}

	_ = store.EnsureDefault(ctx)
	_, err = store.CreateBot(ctx, bot.BotProfile{ID: "reviewer", Name: "Reviewer", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defSess, err := store.CreateForBot(bot.DefaultBotID, models.CreateOpts{CWD: "/proj/a", Source: bot.SessionSource})
	if err != nil {
		t.Fatal(err)
	}
	revSess, err := store.CreateForBot("reviewer", models.CreateOpts{CWD: "/proj/a", Source: bot.SessionSource})
	if err != nil {
		t.Fatal(err)
	}

	// Insert memory rows for bot:reviewer, reviewer's session, and bot:default.
	revScope := memory.UserScope("bot:reviewer").Key()
	defScope := memory.UserScope("bot:default").Key()
	sessScope := memory.SessionScope(revSess.ID).Key()
	now := float64(time.Now().Unix())
	for _, sk := range []string{revScope, defScope, sessScope} {
		if _, err := db.Exec(`INSERT INTO memories (session_id, updated_at) VALUES (?, ?)`, sk, now); err != nil {
			t.Fatalf("insert memory %s: %v", sk, err)
		}
		if _, err := db.Exec(`INSERT INTO memory_gate_verdicts (id, scope_key, decided_at, outcome) VALUES (?, ?, ?, 'pass')`, "v_"+sk, sk, now); err != nil {
			t.Fatalf("insert gate verdict %s: %v", sk, err)
		}
	}

	// External lock detection.
	self := models.LockOwner{PID: 1001, Host: "host-a"}
	if locked, err := store.HasExternalBotLock("reviewer", self); err != nil || locked {
		t.Fatalf("unlocked HasExternalBotLock = (%v, %v), want (false, nil)", locked, err)
	}
	// Own lock is ignored.
	_, err = db.Exec(`INSERT INTO session_locks (session_id, pid, host, acquired_at, heartbeat_at) VALUES (?, ?, ?, ?, ?)`,
		revSess.ID, self.PID, self.Host, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := store.HasExternalBotLock("reviewer", self); err != nil || locked {
		t.Fatalf("own lock HasExternalBotLock = (%v, %v), want (false, nil)", locked, err)
	}
	// Fresh lock from another PID is detected.
	_, err = db.Exec(`UPDATE session_locks SET pid = 2002, host = 'host-b', heartbeat_at = ? WHERE session_id = ?`, now, revSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := store.HasExternalBotLock("reviewer", self); err != nil || !locked {
		t.Fatalf("external lock HasExternalBotLock = (%v, %v), want (true, nil)", locked, err)
	}
	// Stale lock (>60s old) is ignored.
	_, err = db.Exec(`UPDATE session_locks SET heartbeat_at = ? WHERE session_id = ?`, now-120, revSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := store.HasExternalBotLock("reviewer", self); err != nil || locked {
		t.Fatalf("stale lock HasExternalBotLock = (%v, %v), want (false, nil)", locked, err)
	}

	// Cascade delete cleans reviewer bot, sessions, and memory scopes, leaving default untouched.
	if err := store.DeleteBotCascade("reviewer"); err != nil {
		t.Fatalf("DeleteBotCascade: %v", err)
	}

	countRow := func(query string, arg any) int {
		var n int
		if err := db.QueryRow(query, arg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countRow(`SELECT COUNT(*) FROM bots WHERE id = ?`, "reviewer"); n != 0 {
		t.Fatalf("reviewer bot row still exists")
	}
	if n := countRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, revSess.ID); n != 0 {
		t.Fatalf("reviewer session still exists")
	}
	if n := countRow(`SELECT COUNT(*) FROM memories WHERE session_id = ?`, revScope); n != 0 {
		t.Fatalf("reviewer user-scope memory still exists")
	}
	if n := countRow(`SELECT COUNT(*) FROM memories WHERE session_id = ?`, sessScope); n != 0 {
		t.Fatalf("reviewer session-scope memory still exists")
	}
	if n := countRow(`SELECT COUNT(*) FROM memory_gate_verdicts WHERE scope_key = ?`, revScope); n != 0 {
		t.Fatalf("reviewer gate verdict still exists")
	}
	// Default bot and its data remain intact.
	if n := countRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, defSess.ID); n != 1 {
		t.Fatalf("default session was unexpectedly deleted")
	}
	if n := countRow(`SELECT COUNT(*) FROM memories WHERE session_id = ?`, defScope); n != 1 {
		t.Fatalf("default user-scope memory was unexpectedly deleted")
	}
}
