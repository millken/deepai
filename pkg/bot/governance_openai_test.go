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
	"time"

	"github.com/millken/deepai/pkg/schedule"
	"golang.org/x/net/websocket"
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

func TestScopedTokenEnforcesBotScopeOnSchedulesRunsChainAndStreams(t *testing.T) {
	ctx := context.Background()
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

	m, err := NewManager(Options{Runtime: &recordingRuntime{}, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)

	if _, err := m.CreateBot(ctx, CreateBotRequest{ID: "reviewer", Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBot(ctx, CreateBotRequest{ID: "secret", Name: "Secret"}); err != nil {
		t.Fatal(err)
	}

	svc, err := schedule.NewService(schedule.NewMemoryStore(), schedule.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	disp := AttachSchedule(m, svc)
	defer disp.Wait(ctx)

	srv := NewServer(ServerOptions{
		Manager:   m,
		Schedules: svc,
		Token:     "master-secret",
		Tokens:    tokStore,
		Audit:     auditLog,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Create secret bot's schedule + run and reviewer bot's pending schedule.
	secretSch, err := svc.Create(ctx, schedule.CreateInput{
		BotID:   "secret",
		Name:    "secret-job",
		Prompt:  "confidential",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	secretRun, err := svc.RunNow(ctx, secretSch.ID, "sec-r1")
	if err != nil {
		t.Fatal(err)
	}
	disp.Wait(ctx)

	revSch, err := svc.Create(ctx, schedule.CreateInput{
		BotID:   "reviewer",
		Name:    "rev-job",
		Prompt:  "review",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		State:   schedule.StatePendingApproval,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Issue a user token scoped strictly to "reviewer".
	revTok, err := tokStore.CreateToken(ctx, CreateTokenRequest{
		Name:   "rev-only",
		Role:   RoleUser,
		BotIDs: []string{"reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 61-H1: flat /v1/schedules & /v1/runs must enforce bot_ids!
	rCreateSec := doReq(t, http.MethodPost, ts.URL+"/v1/schedules", revTok.Token, map[string]any{
		"bot_id": "secret", "name": "hack", "prompt": "p",
		"trigger": map[string]any{"type": "interval", "every": "1h"},
	})
	rCreateSec.Body.Close()
	if rCreateSec.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /v1/schedules for secret bot = %d, want 403", rCreateSec.StatusCode)
	}

	// 60-M2 + 61-H1: chaining reviewer schedule after secret schedule must be 403!
	rChain := doReq(t, http.MethodPost, ts.URL+"/v1/bots/reviewer/schedules", revTok.Token, map[string]any{
		"name": "steal-chain", "prompt": "leak {{prev.output}}",
		"trigger": map[string]any{"type": "chain", "after_schedule_id": secretSch.ID},
	})
	rChain.Body.Close()
	if rChain.StatusCode != http.StatusForbidden {
		t.Fatalf("POST chain after secret schedule = %d, want 403", rChain.StatusCode)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/schedules/" + secretSch.ID},
		{http.MethodPatch, "/v1/schedules/" + secretSch.ID},
		{http.MethodPost, "/v1/schedules/" + secretSch.ID + "/run"},
		{http.MethodPost, "/v1/schedules/" + secretSch.ID + "/pause"},
		{http.MethodDelete, "/v1/schedules/" + secretSch.ID},
		{http.MethodGet, "/v1/schedules/" + secretSch.ID + "/runs"},
		{http.MethodGet, "/v1/runs/" + secretRun.ID},
		{http.MethodPost, "/v1/runs/" + secretRun.ID + "/cancel"},
	} {
		resp := doReq(t, tc.method, ts.URL+tc.path, revTok.Token, map[string]any{"prompt": "pwned"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("scoped token %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}

	// GET /v1/schedules must only list reviewer schedules, never secretSch.
	rList := doReq(t, http.MethodGet, ts.URL+"/v1/schedules", revTok.Token, nil)
	var listRes struct {
		Schedules []schedule.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(rList.Body).Decode(&listRes)
	rList.Body.Close()
	if len(listRes.Schedules) != 1 || listRes.Schedules[0].ID != revSch.ID {
		t.Fatalf("GET /v1/schedules leaked unauthorized schedules: %+v", listRes.Schedules)
	}

	// Approving reviewer's own schedule succeeds and writes schedule_approved audit.
	rApp := doReq(t, http.MethodPost, ts.URL+"/v1/schedules/"+revSch.ID+"/approve", revTok.Token, nil)
	rApp.Body.Close()
	if rApp.StatusCode != http.StatusOK {
		t.Fatalf("approve own schedule = %d, want 200", rApp.StatusCode)
	}

	// 61-H2: GET /v1/events?bot_id=secret -> 403; WS subscribe to secret session -> forbidden; WS subscribe "*" leaks nothing from secret.
	rEvSec := doReq(t, http.MethodGet, ts.URL+"/v1/events?bot_id=secret", revTok.Token, nil)
	rEvSec.Body.Close()
	if rEvSec.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /v1/events?bot_id=secret = %d, want 403", rEvSec.StatusCode)
	}

	secSess, err := m.CreateSessionForBot("secret", CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	revSess, err := m.CreateSessionForBot("reviewer", CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=" + revTok.Token
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Subscribe to secret session must be rejected.
	if err := websocket.JSON.Send(conn, WSRequest{ID: "sub-sec", Op: "subscribe", BotID: "secret", SessionID: secSess.ID}); err != nil {
		t.Fatal(err)
	}
	var rep WSMessage
	if err := websocket.JSON.Receive(conn, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.Error == nil || rep.Error.Code != "forbidden" {
		t.Fatalf("WS subscribe to secret session = %+v, want forbidden", rep)
	}

	// Subscribe "*" without bot_id must filter out secret bot turns while delivering reviewer bot turns.
	zero := int64(0)
	if err := websocket.JSON.Send(conn, WSRequest{ID: "sub-all", Op: "subscribe", SessionID: "*", Since: &zero}); err != nil {
		t.Fatal(err)
	}
	for {
		var m2 WSMessage
		if err := websocket.JSON.Receive(conn, &m2); err != nil {
			t.Fatal(err)
		}
		if m2.Kind == "reply" && m2.ID == "sub-all" {
			if !m2.OK {
				t.Fatalf("subscribe *: %+v", m2.Error)
			}
			break
		}
		if m2.Kind == "event" && m2.Event != nil && m2.Event.BotID == "secret" {
			t.Fatalf("secret event leaked in * backlog: %+v", m2.Event)
		}
	}

	tSec, _ := m.SendForBot("secret", secSess.ID, SendRequest{Content: "TOP SECRET"}, SendOptions{})
	waitTurn(t, tSec)
	tRev, _ := m.SendForBot("reviewer", revSess.ID, SendRequest{Content: "REVIEW OK"}, SendOptions{})
	waitTurn(t, tRev)

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var msg WSMessage
		if err := websocket.JSON.Receive(conn, &msg); err != nil {
			t.Fatalf("waiting for reviewer turn_end: %v", err)
		}
		if msg.Kind != "event" || msg.Event == nil {
			continue
		}
		if msg.Event.BotID == "secret" || msg.Event.SessionID == secSess.ID || strings.Contains(msg.Event.Text, "TOP SECRET") {
			t.Fatalf("secret bot event leaked to reviewer-scoped WS *: %+v", msg.Event)
		}
		if msg.Event.SessionID == revSess.ID && msg.Event.Type == EventTurnEnd {
			break
		}
	}

	// 61-M4: verify bot_updated, schedule_approved, schedule_run_end in audit.jsonl + auth_failed rate cap.
	rPatch := doReq(t, http.MethodPatch, ts.URL+"/v1/bots/reviewer", "master-secret", map[string]any{"title": "Code Reviewer"})
	rPatch.Body.Close()
	for i := 0; i < 80; i++ {
		auditLog.Record(AuditEntry{Action: "auth_failed", Target: "/v1/info", Outcome: "denied"})
	}
	rawAudit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(rawAudit)
	for _, act := range []string{"bot_updated", "schedule_approved", "schedule_run_end"} {
		if !strings.Contains(auditText, `"action":"`+act+`"`) {
			t.Errorf("audit.jsonl missing %q:\n%s", act, auditText)
		}
	}
	if n := strings.Count(auditText, `"action":"auth_failed"`); n > defaultNoisyAuditCap {
		t.Errorf("auth_failed audit count = %d, want <= %d", n, defaultNoisyAuditCap)
	}
}
