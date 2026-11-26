package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestBotSkillsRESTWSInfoSummaryBotNameAndScheduleLagMetric(t *testing.T) {
	ctx := context.Background()
	botDataDir := t.TempDir()
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	auditLog, err := NewAuditLogger(auditPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tokStore := NewMemoryTokenStore()

	m, err := NewManager(Options{
		Runtime:     &recordingRuntime{},
		Store:       NewMemoryStore(),
		Secrets:     NewMemorySecretStore(),
		BotDataDir:  botDataDir,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)

	if _, err := m.CreateBot(ctx, CreateBotRequest{ID: "ops", Name: "Ops Assistant"}); err != nil {
		t.Fatal(err)
	}

	// Seed a per-bot skill on disk under <botDataDir>/ops/skills/triage/SKILL.md.
	skillDir := filepath.Join(botDataDir, "ops", "skills", "triage")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: triage\ndescription: \"Incident triage playbook\"\n---\n\nCheck alerts first.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a session for ops so SessionInfo.BotName and Info.Bots SessionCount are verified.
	sess, err := m.CreateSessionForBot("ops", CreateSessionRequest{Title: "oncall"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.BotID != "ops" || sess.BotName != "Ops Assistant" {
		t.Fatalf("CreateSessionForBot info = %+v, want BotID=ops BotName='Ops Assistant'", sess)
	}
	listed, err := m.ListSessionsForBot("ops", 10)
	if err != nil || len(listed) != 1 || listed[0].BotName != "Ops Assistant" {
		t.Fatalf("ListSessionsForBot = (%+v, %v), want BotName='Ops Assistant'", listed, err)
	}

	// Record a synthetic schedule lag sample for Prometheus verification.
	m.Metrics().ObserveScheduleLag(1500 * time.Millisecond)

	userTok, err := tokStore.CreateToken(ctx, CreateTokenRequest{
		Name:   "ops-user",
		Role:   RoleUser,
		BotIDs: []string{"ops"},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerOptions{
		Manager: m,
		Token:   "master-secret",
		Tokens:  tokStore,
		Audit:   auditLog,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. GET /v1/info returns []BotSummary with session_count.
	rInfo := doReq(t, http.MethodGet, ts.URL+"/v1/info", "master-secret", nil)
	var infoResp Info
	_ = json.NewDecoder(rInfo.Body).Decode(&infoResp)
	rInfo.Body.Close()
	if rInfo.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/info = %d", rInfo.StatusCode)
	}
	var opsSum *BotSummary
	for i := range infoResp.Bots {
		if infoResp.Bots[i].ID == "ops" {
			opsSum = &infoResp.Bots[i]
		}
	}
	if opsSum == nil || opsSum.SessionCount != 1 {
		t.Fatalf("GET /v1/info bots missing ops summary with session_count=1: %+v", infoResp.Bots)
	}

	// 2. Scoped user token can GET /v1/bots/ops/skills, cannot DELETE (admin-only).
	rList := doReq(t, http.MethodGet, ts.URL+"/v1/bots/ops/skills", userTok.Token, nil)
	var skList struct {
		Skills []BotSkillInfo `json:"skills"`
	}
	_ = json.NewDecoder(rList.Body).Decode(&skList)
	rList.Body.Close()
	if rList.StatusCode != http.StatusOK || len(skList.Skills) != 1 {
		t.Fatalf("GET /v1/bots/ops/skills = (%d, %+v)", rList.StatusCode, skList)
	}
	if skList.Skills[0].Name != "triage" || skList.Skills[0].Description != "Incident triage playbook" || skList.Skills[0].Content != "Check alerts first." {
		t.Fatalf("unexpected BotSkillInfo: %+v", skList.Skills[0])
	}

	rUserDel := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ops/skills/triage", userTok.Token, nil)
	rUserDel.Body.Close()
	if rUserDel.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin DELETE skill = %d, want 403", rUserDel.StatusCode)
	}

	// Invalid skill name rejected cleanly (400).
	rBad := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ops/skills/BAD..NAME", "master-secret", nil)
	rBad.Body.Close()
	if rBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid skill name DELETE = %d, want 400", rBad.StatusCode)
	}

	// 3. WS list_bot_skills, set_secret (unified audit action), delete_bot_skill.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=master-secret"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
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

	repList := wsCall(WSRequest{ID: "1", Op: "list_bot_skills", BotID: "ops"})
	if !repList.OK {
		t.Fatalf("WS list_bot_skills failed: %+v", repList.Error)
	}
	repSec := wsCall(WSRequest{ID: "2", Op: "set_secret", BotID: "ops", Params: map[string]any{"name": "PAGER_KEY", "value": "pk_123"}})
	if !repSec.OK {
		t.Fatalf("WS set_secret failed: %+v", repSec.Error)
	}
	repDelSec := wsCall(WSRequest{ID: "3", Op: "delete_secret", BotID: "ops", Params: map[string]any{"name": "PAGER_KEY"}})
	if !repDelSec.OK {
		t.Fatalf("WS delete_secret failed: %+v", repDelSec.Error)
	}
	repDelSkill := wsCall(WSRequest{ID: "4", Op: "delete_bot_skill", BotID: "ops", Params: map[string]any{"name": "triage"}})
	if !repDelSkill.OK {
		t.Fatalf("WS delete_bot_skill failed: %+v", repDelSkill.Error)
	}

	// Deleting again returns 404 skill_not_found.
	rGone := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ops/skills/triage", "master-secret", nil)
	rGone.Body.Close()
	if rGone.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE skill = %d, want 404", rGone.StatusCode)
	}

	// 4. Verify unified audit actions (secret_set, secret_deleted, bot_skill_deleted).
	rawAudit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(rawAudit)
	for _, want := range []string{`"action":"secret_set"`, `"action":"secret_deleted"`, `"action":"bot_skill_deleted"`} {
		if !strings.Contains(auditText, want) {
			t.Errorf("audit.jsonl missing %s:\n%s", want, auditText)
		}
	}

	// 5. Verify /metrics exposes deepai_bot_schedule_lag_seconds.
	rMet := doReq(t, http.MethodGet, ts.URL+"/metrics", "master-secret", nil)
	metBytes, _ := io.ReadAll(rMet.Body)
	rMet.Body.Close()
	metStr := string(metBytes)
	if !strings.Contains(metStr, "deepai_bot_schedule_lag_seconds_count 1") || !strings.Contains(metStr, "deepai_bot_schedule_lag_seconds_sum 1.500000") {
		t.Fatalf("metrics missing schedule_lag_seconds:\n%s", metStr)
	}
}
