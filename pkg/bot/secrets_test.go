package bot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
	_ "modernc.org/sqlite"
)

func TestSQLiteSecretStoreSealRevealAndIsolation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewSQLiteSecretStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := store.SetSecret(ctx, "alpha", "bad-hyphen", "val"); err == nil {
		t.Fatal("expected invalid secret name error")
	}

	const rawCredential = "cred_alpha_test_value_9988"
	meta, err := store.SetSecret(ctx, "alpha", "SERVICE_KEY", rawCredential)
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if meta.BotID != "alpha" || meta.Name != "SERVICE_KEY" {
		t.Fatalf("meta = %+v", meta)
	}

	var sealed string
	if err := db.QueryRow(`SELECT ciphertext FROM bot_secrets WHERE bot_id = 'alpha' AND name = 'SERVICE_KEY'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:") || strings.Contains(sealed, rawCredential) {
		t.Fatalf("cipher_val at rest = %q, expected sealed blob", sealed)
	}

	got, err := store.GetSecret(ctx, "alpha", "SERVICE_KEY")
	if err != nil || got != rawCredential {
		t.Fatalf("GetSecret(alpha) = (%q, %v), want %q", got, err, rawCredential)
	}
	if _, err := store.GetSecret(ctx, "beta", "SERVICE_KEY"); err == nil {
		t.Fatal("secret leaked across bots (alpha -> beta)")
	}

	list, err := store.ListSecrets(ctx, "alpha")
	if err != nil || len(list) != 1 || list[0].Name != "SERVICE_KEY" {
		t.Fatalf("ListSecrets(alpha) = (%+v, %v)", list, err)
	}
	rawListJSON, _ := json.Marshal(list)
	if strings.Contains(string(rawListJSON), rawCredential) {
		t.Fatalf("ListSecrets leaked value: %s", string(rawListJSON))
	}

	if err := store.DeleteAllSecretsForBot(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteAllSecretsForBot: %v", err)
	}
	if after, _ := store.ListSecrets(ctx, "alpha"); len(after) != 0 {
		t.Fatalf("secrets remained after DeleteAllSecretsForBot: %+v", after)
	}
}

func TestServerAndWebSocketBotSecretsRBAC(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), Secrets: NewMemorySecretStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)
	_, _ = m.CreateBot(ctx, CreateBotRequest{ID: "ops", Name: "Ops"})
	_, _ = m.CreateBot(ctx, CreateBotRequest{ID: "other", Name: "Other"})

	tokStore := NewMemoryTokenStore()
	userOpsTok, _ := tokStore.CreateToken(ctx, CreateTokenRequest{Name: "ops-user", Role: RoleUser, BotIDs: []string{"ops"}})

	srv := httptest.NewServer(NewServer(ServerOptions{
		Manager: m,
		Token:   "master-admin-tok",
		Tokens:  tokStore,
	}))
	defer srv.Close()

	doReq := func(method, path, bearer, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp, buf.String()
	}

	resp, _ := doReq(http.MethodPut, "/v1/bots/ops/secrets/API_KEY", userOpsTok.Token, `{"value":"demo_val_1"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user PUT secret status = %d, want 403", resp.StatusCode)
	}

	resp, raw := doReq(http.MethodPut, "/v1/bots/ops/secrets/API_KEY", "master-admin-tok", `{"value":"demo_val_1"}`)
	if resp.StatusCode != http.StatusOK || strings.Contains(raw, "demo_val_1") {
		t.Fatalf("admin PUT secret = (%d, %s)", resp.StatusCode, raw)
	}

	resp, raw = doReq(http.MethodGet, "/v1/bots/ops/secrets", userOpsTok.Token, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"API_KEY"`) || strings.Contains(raw, "demo_val_1") {
		t.Fatalf("scoped user GET /v1/bots/ops/secrets = (%d, %s)", resp.StatusCode, raw)
	}
	resp, _ = doReq(http.MethodGet, "/v1/bots/other/secrets", userOpsTok.Token, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped user GET /v1/bots/other/secrets = %d, want 403", resp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/ws"
	wsCfg, _ := websocket.NewConfig(wsURL, srv.URL)
	wsCfg.Header.Set("Authorization", "Bearer master-admin-tok")
	conn, err := websocket.DialConfig(wsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	wsCall := func(req WSRequest) WSMessage {
		t.Helper()
		if err := websocket.JSON.Send(conn, req); err != nil {
			t.Fatal(err)
		}
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "reply" && msg.ID == req.ID {
				return msg
			}
		}
	}

	setReply := wsCall(WSRequest{ID: "1", Op: "set_secret", BotID: "ops", Params: map[string]any{"name": "WS_SEC", "value": "ws_demo_val"}})
	if !setReply.OK {
		t.Fatalf("ws set_secret failed: %+v", setReply.Error)
	}
	listReply := wsCall(WSRequest{ID: "2", Op: "list_secrets", BotID: "ops"})
	listBytes, _ := json.Marshal(listReply.Data)
	if !listReply.OK || !strings.Contains(string(listBytes), "WS_SEC") || strings.Contains(string(listBytes), "ws_demo_val") {
		t.Fatalf("ws list_secrets = %s", string(listBytes))
	}
	delReply := wsCall(WSRequest{ID: "3", Op: "delete_secret", BotID: "ops", Params: map[string]any{"name": "WS_SEC"}})
	if !delReply.OK {
		t.Fatalf("ws delete_secret failed: %+v", delReply.Error)
	}
}
