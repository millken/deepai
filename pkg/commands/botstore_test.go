package commands

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
