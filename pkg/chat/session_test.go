package chat

import (
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

func newTestStore(t *testing.T) (*SQLiteSessionStore, func()) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := NewSQLiteSessionStore(dbPath)
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	return store, func() { store.Close() }
}

func TestCRUD_Roundtrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model", CWD: "/tmp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("expected non-empty session ID")
	}

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != sess.ID {
		t.Fatalf("expected ID %q, got %q", sess.ID, loaded.ID)
	}

	if err := store.SetTitle(sess.ID, "My Session"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	loaded2, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load after rename: %v", err)
	}
	if loaded2.Title != "My Session" {
		t.Fatalf("expected title %q, got %q", "My Session", loaded2.Title)
	}
}

func TestAppendMessage_SeqIncrement(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 3; i++ {
		msg := models.Message{
			Role:    models.RoleHuman,
			Content: "msg",
		}
		if err := store.AppendMessage(sess.ID, msg); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Verify ordering by seq via content check (all content is "msg").
	for i, m := range msgs {
		if m.SessionID != sess.ID {
			t.Fatalf("msg[%d]: wrong session_id %q", i, m.SessionID)
		}
		if m.Role != models.RoleHuman {
			t.Fatalf("msg[%d]: expected human role, got %q", i, m.Role)
		}
	}
}

func TestDeleteMessagesAfterSeq_Boundary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "msg"}); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	// Delete messages with seq > 3 (keeps seq 1, 2, 3).
	if err := store.DeleteMessagesAfterSeq(sess.ID, 3); err != nil {
		t.Fatalf("DeleteMessagesAfterSeq: %v", err)
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after delete, got %d", len(msgs))
	}

	// Append after delete should get seq 4.
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleAI, Content: "new"}); err != nil {
		t.Fatalf("AppendMessage after delete: %v", err)
	}
	msgs, err = store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages after append: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
}

func TestSearch_FTS5(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Title: "search-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Append a human message (FTS only indexes human/ai).
	if err := store.AppendMessage(sess.ID, models.Message{Role: models.RoleHuman, Content: "hello world test"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	results, err := store.Search("hello", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 search result for 'hello'")
	}
	found := false
	for _, r := range results {
		if r.ID == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("session not found in search results")
	}
}

func TestPrune_OnlyCompletedExpired(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create an active session (should not be pruned).
	activeSess, err := store.Create(models.CreateOpts{Title: "active-session"})
	if err != nil {
		t.Fatalf("Create active: %v", err)
	}

	// Create a completed session with old updated_at (should be pruned).
	oldSess, err := store.Create(models.CreateOpts{Title: "old-completed"})
	if err != nil {
		t.Fatalf("Create old: %v", err)
	}
	// Directly update updated_at to 120 days ago.
	_, err = store.db.Exec(`UPDATE sessions SET state = 'completed', updated_at = ? WHERE id = ?`,
		time.Now().AddDate(0, 0, -120).Unix(), oldSess.ID)
	if err != nil {
		t.Fatalf("update old session: %v", err)
	}

	count, err := store.Prune(90, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 pruned session, got %d", count)
	}

	// Active session should still exist.
	if _, err := store.Load(activeSess.ID); err != nil {
		t.Fatalf("active session should still exist: %v", err)
	}
	// Old session should be gone.
	if _, err := store.Load(oldSess.ID); err == nil {
		t.Fatal("old completed session should have been pruned")
	}
}

func TestResolveAll_Priority(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create sessions with specific titles.
	s1, _ := store.Create(models.CreateOpts{Title: "Alpha"})
	s2, _ := store.Create(models.CreateOpts{Title: "Alpha Beta"})
	_, _ = store.Create(models.CreateOpts{Title: "Beta Alpha"})

	// Priority 1: ID exact match.
	metas, err := store.ResolveAll(s1.ID)
	if err != nil {
		t.Fatalf("ResolveAll by ID: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s1.ID {
		t.Fatalf("expected single ID match, got %v", metas)
	}

	// Priority 2: Title exact match.
	metas, err = store.ResolveAll("Alpha Beta")
	if err != nil {
		t.Fatalf("ResolveAll exact title: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s2.ID {
		t.Fatalf("expected exact title match for 'Alpha Beta', got %v", metas)
	}

	// Priority 3: Title prefix match.
	metas, err = store.ResolveAll("Alpha B")
	if err != nil {
		t.Fatalf("ResolveAll prefix title: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != s2.ID {
		t.Fatalf("expected prefix match for 'Alpha B', got %v", metas)
	}

	// Priority 4: Fuzzy match (case-insensitive contains).
	metas, err = store.ResolveAll("lpha")
	if err != nil {
		t.Fatalf("ResolveAll fuzzy title: %v", err)
	}
	if len(metas) < 2 {
		t.Fatalf("expected at least 2 fuzzy matches for 'lpha', got %d: %v", len(metas), metas)
	}

	// No match.
	_, err = store.ResolveAll("nonexistent_title_xyz")
	if err == nil {
		t.Fatal("expected error for no match")
	}
}

func TestPrune_DryRun(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Title: "dryrun-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Set as completed and old.
	_, err = store.db.Exec(`UPDATE sessions SET state = 'completed', updated_at = ? WHERE id = ?`,
		time.Now().AddDate(0, 0, -120).Unix(), sess.ID)
	if err != nil {
		t.Fatalf("update session: %v", err)
	}

	count, err := store.Prune(90, true)
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected dry-run count 1, got %d", count)
	}

	// Session should still exist after dry-run.
	if _, err := store.Load(sess.ID); err != nil {
		t.Fatal("session should still exist after dry-run")
	}
}

func TestDeleteLastUserTurn(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, err := store.Create(models.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulates a session whose history diverges from a filtered in-memory view:
	// an orphan assistant (unresolved tool call) sits before a resolved one.
	seqRoles := []struct {
		role    models.Role
		content string
	}{
		{models.RoleHuman, "task A"},                                  // seq 1
		{models.RoleAI, "orphan tool_use"},                            // seq 2
		{models.RoleAI, "resolved work B"},                            // seq 3
		{models.RoleHuman, "task C"},                                  // seq 4 (last human)
		{models.RoleAI, "work D"},                                     // seq 5
	}
	for _, m := range seqRoles {
		if err := store.AppendMessage(sess.ID, models.Message{Role: m.role, Content: m.content}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	removed, err := store.DeleteLastUserTurn(sess.ID)
	if err != nil {
		t.Fatalf("DeleteLastUserTurn: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (task C + work D)", removed)
	}

	msgs, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("remaining = %d, want 3 (task A, orphan, resolved work B)", len(msgs))
	}
	// The resolved work B (before the last human) must survive — the old
	// index==seq undo would have wrongly deleted it.
	if msgs[2].Content != "resolved work B" {
		t.Fatalf("msgs[2] = %q, want 'resolved work B' (must not be deleted)", msgs[2].Content)
	}
}

func TestDeleteLastUserTurn_NothingToUndo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, _ := store.Create(models.CreateOpts{})
	// Only assistant messages, no human turn.
	_ = store.AppendMessage(sess.ID, models.Message{Role: models.RoleAI, Content: "hi"})

	removed, err := store.DeleteLastUserTurn(sess.ID)
	if err != nil {
		t.Fatalf("DeleteLastUserTurn: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (no human turn)", removed)
	}
}
