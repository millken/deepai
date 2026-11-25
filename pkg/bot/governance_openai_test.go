package bot

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestScopedTokensRBACAndAuditLog(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokStore, err := NewSQLiteTokenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	auditLog, err := NewAuditLogger(auditPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	rt := &recordingRuntime{}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{
		Manager: m,
		Token:   "master-secret",
		Tokens:  tokStore,
		Audit:   auditLog,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Master creates a second bot "reviewer".
	rBot := doReq(t, http.MethodPost, ts.URL+"/v1/bots", "master-secret", map[string]any{"id": "reviewer", "name": "Reviewer"})
	rBot.Body.Close()
	if rBot.StatusCode != http.StatusCreated {
		t.Fatalf("create bot status = %d", rBot.StatusCode)
	}

	// Master issues a readonly token and a reviewer-scoped user token.
	rRO := doReq(t, http.MethodPost, ts.URL+"/v1/tokens", "master-secret", map[string]any{
		"name": "viewer", "role": "readonly",
	})
	var roTok TokenRecord
	_ = json.NewDecoder(rRO.Body).Decode(&roTok)
	rRO.Body.Close()
	if rRO.StatusCode != http.StatusCreated || roTok.Token == "" {
		t.Fatalf("create readonly token = (%d, %+v)", rRO.StatusCode, roTok)
	}

	rUser := doReq(t, http.MethodPost, ts.URL+"/v1/tokens", "master-secret", map[string]any{
		"name": "ipad", "role": "user", "bot_ids": []string{"reviewer"}, "ttl": "24h",
	})
	var userTok TokenRecord
	_ = json.NewDecoder(rUser.Body).Decode(&userTok)
	rUser.Body.Close()
	if rUser.StatusCode != http.StatusCreated || userTok.Token == "" {
		t.Fatalf("create user token = (%d, %+v)", rUser.StatusCode, userTok)
	}

	// GET /v1/tokens lists tokens without exposing plaintext secret.
	rList := doReq(t, http.MethodGet, ts.URL+"/v1/tokens", "master-secret", nil)
	var listEnv struct {
		Tokens []TokenRecord `json:"tokens"`
	}
	_ = json.NewDecoder(rList.Body).Decode(&listEnv)
	rList.Body.Close()
	if len(listEnv.Tokens) != 2 || listEnv.Tokens[0].Token != "" || listEnv.Tokens[1].Token != "" {
		t.Fatalf("list tokens = %+v", listEnv.Tokens)
	}

	// Readonly can GET /v1/info, but cannot POST /v1/sessions -> 403.
	rInfo := doReq(t, http.MethodGet, ts.URL+"/v1/info", roTok.Token, nil)
	rInfo.Body.Close()
	if rInfo.StatusCode != http.StatusOK {
		t.Fatalf("readonly GET /v1/info = %d, want 200", rInfo.StatusCode)
	}
	rROMut := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", roTok.Token, map[string]any{})
	rROMut.Body.Close()
	if rROMut.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly POST /v1/sessions = %d, want 403", rROMut.StatusCode)
	}

	// Reviewer-scoped user token: allowed on /v1/bots/reviewer/sessions, forbidden on /v1/sessions, /v1/bots, /v1/tokens, /metrics.
	rAllow := doReq(t, http.MethodPost, ts.URL+"/v1/bots/reviewer/sessions", userTok.Token, map[string]any{})
	rAllow.Body.Close()
	if rAllow.StatusCode != http.StatusCreated {
		t.Fatalf("scoped user POST reviewer session = %d, want 201", rAllow.StatusCode)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/sessions"},
		{http.MethodPost, "/v1/bots"},
		{http.MethodGet, "/v1/tokens"},
		{http.MethodGet, "/metrics"},
	} {
		resp := doReq(t, tc.method, ts.URL+tc.path, userTok.Token, map[string]any{"id": "x", "name": "X"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("scoped user %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}

	// Revoke user token -> subsequent request gets 401 and logs auth_failed.
	rRev := doReq(t, http.MethodDelete, ts.URL+"/v1/tokens/"+userTok.ID, "master-secret", nil)
	rRev.Body.Close()
	if rRev.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", rRev.StatusCode)
	}
	rAfter := doReq(t, http.MethodGet, ts.URL+"/v1/bots/reviewer/sessions", userTok.Token, nil)
	rAfter.Body.Close()
	if rAfter.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", rAfter.StatusCode)
	}

	// Verify audit.jsonl recorded bot_created, token_created, token_revoked, auth_failed.
	rawAudit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(rawAudit)
	for _, action := range []string{"bot_created", "token_created", "token_revoked", "auth_failed"} {
		if !strings.Contains(auditText, `"action":"`+action+`"`) {
			t.Errorf("audit.jsonl missing action %q:\n%s", action, auditText)
		}
	}
}

func TestOpenAICompatAndPrometheusMetrics(t *testing.T) {
	rt := &recordingRuntime{}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	if _, err := m.CreateBot(context.Background(), CreateBotRequest{
		ID:           "reviewer",
		Name:         "Reviewer",
		Instructions: "Review code concisely.",
		DefaultModel: "other",
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. GET /v1/models lists aliases + bot:default + bot:reviewer.
	rMods := doReq(t, http.MethodGet, ts.URL+"/v1/models", "secret", nil)
	bodyMods, _ := io.ReadAll(rMods.Body)
	rMods.Body.Close()
	if rMods.StatusCode != http.StatusOK || !strings.Contains(string(bodyMods), `"id":"bot:reviewer"`) {
		t.Fatalf("GET /v1/models = (%d, %s)", rMods.StatusCode, string(bodyMods))
	}

	// 2. Non-streaming POST /v1/chat/completions targeting bot:reviewer with multimodal image_url.
	pngB64 := base64.StdEncoding.EncodeToString(makeTestPNG(t, 16, 16))
	rComp := doReq(t, http.MethodPost, ts.URL+"/v1/chat/completions", "secret", map[string]any{
		"model": "bot:reviewer",
		"user":  "alice",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "check screenshot"},
				{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + pngB64}},
			},
		}},
	})
	var comp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.NewDecoder(rComp.Body).Decode(&comp)
	sessHdr := rComp.Header.Get("X-DeepAI-Session")
	rComp.Body.Close()
	if rComp.StatusCode != http.StatusOK || comp.Object != "chat.completion" || sessHdr == "" {
		t.Fatalf("chat.completions = (%d, %+v, sess=%q)", rComp.StatusCode, comp, sessHdr)
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message.Content != "echo: check screenshot" {
		t.Fatalf("unexpected completion choices: %+v", comp.Choices)
	}
	in := rt.lastInput()
	if in.BotID != "reviewer" || len(in.Messages[len(in.Messages)-1].Images) != 1 {
		t.Fatalf("reviewer bot / multimodal image not forwarded: bot=%q imgs=%d", in.BotID, len(in.Messages[len(in.Messages)-1].Images))
	}

	// 3. Streaming POST /v1/chat/completions reuses user "alice" session and emits SSE chunks + [DONE].
	rStream := doReq(t, http.MethodPost, ts.URL+"/v1/chat/completions", "secret", map[string]any{
		"model":  "bot:reviewer/fake",
		"user":   "alice",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "stream test"},
		},
	})
	defer rStream.Body.Close()
	if rStream.StatusCode != http.StatusOK || rStream.Header.Get("X-DeepAI-Session") != sessHdr {
		t.Fatalf("stream status=%d sess=%q want %q", rStream.StatusCode, rStream.Header.Get("X-DeepAI-Session"), sessHdr)
	}
	var sawDelta, sawDone bool
	sc := bufio.NewScanner(rStream.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			sawDone = true
		}
		if strings.Contains(line, `"content":"echo: "`) || strings.Contains(line, `"content":"stream test"`) {
			sawDelta = true
		}
	}
	if !sawDelta || !sawDone {
		t.Fatalf("stream missing delta (%v) or [DONE] (%v)", sawDelta, sawDone)
	}

	// 4. GET /metrics exposes Prometheus counters for turns & tokens.
	rMet := doReq(t, http.MethodGet, ts.URL+"/metrics", "secret", nil)
	metBody, _ := io.ReadAll(rMet.Body)
	rMet.Body.Close()
	metStr := string(metBody)
	if rMet.StatusCode != http.StatusOK || !strings.Contains(metStr, `deepai_bot_turns_total{bot_id="reviewer",status="completed"} 2`) {
		t.Fatalf("GET /metrics = (%d)\n%s", rMet.StatusCode, metStr)
	}
	if !strings.Contains(metStr, `deepai_bot_tokens_total{bot_id="reviewer"`) {
		t.Fatalf("metrics missing tokens_total:\n%s", metStr)
	}
}
