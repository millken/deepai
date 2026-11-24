package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
	"github.com/millken/deepai/pkg/tools"
	"golang.org/x/net/websocket"
)

// recordingRuntime wraps fakeRuntime, records every TurnInput, and implements
// ToolValidator so bot CRUD validation tests exercise allowlist checks.
type recordingRuntime struct {
	fakeRuntime
	knownTools []string

	recMu  sync.Mutex
	inputs []TurnInput
}

func makeStubTool(name string) models.Tool {
	return models.Tool{
		Name:        name,
		Description: "stub " + name,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}
}

func (r *recordingRuntime) ValidateToolAllowlist(allowlist []string) []string {
	reg := tools.NewRegistry()
	for _, name := range r.knownTools {
		_ = reg.Register(makeStubTool(name))
	}
	return reg.ValidateAllowlist(allowlist)
}

func (r *recordingRuntime) NewConversation(sessionID string) (Conversation, error) {
	return &recordingConv{rt: r}, nil
}

func (r *recordingRuntime) lastInput() TurnInput {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if len(r.inputs) == 0 {
		return TurnInput{}
	}
	return r.inputs[len(r.inputs)-1]
}

type recordingConv struct{ rt *recordingRuntime }

func (c *recordingConv) Close() error { return nil }

func (c *recordingConv) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	c.rt.recMu.Lock()
	cp := in
	cp.BotToolAllowlist = append([]string(nil), in.BotToolAllowlist...)
	cp.ToolAllowlist = append([]string(nil), in.ToolAllowlist...)
	c.rt.inputs = append(c.rt.inputs, cp)
	c.rt.recMu.Unlock()
	fc := &fakeConv{rt: &c.rt.fakeRuntime}
	return fc.RunTurn(ctx, in)
}

func doReq(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeAPIError(t *testing.T, resp *http.Response) apiError {
	t.Helper()
	var env struct {
		Error apiError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	return env.Error
}

func newMultiBotServer(t *testing.T) (*httptest.Server, *Manager, *recordingRuntime, *schedule.Service) {
	t.Helper()
	rt := &recordingRuntime{
		knownTools: []string{"read_file", "grep_search", "bash", "mcp__gh__list_prs"},
	}
	store := NewMemoryStore()
	m, err := NewManager(Options{Runtime: rt, Store: store, IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	svc, _ := newScheduleTestService(t, m)

	srv := NewServer(ServerOptions{Manager: m, Token: "secret", Schedules: svc})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, m, rt, svc
}

func TestMultiBotRESTCRUDAndValidation(t *testing.T) {
	ts, _, _, _ := newMultiBotServer(t)

	// GET /v1/info includes default_bot_id and bots.
	resp := doReq(t, http.MethodGet, ts.URL+"/v1/info", "secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/info status = %d", resp.StatusCode)
	}
	var info Info
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.DefaultBotID != DefaultBotID || len(info.Bots) != 1 || info.Bots[0].ID != DefaultBotID {
		t.Fatalf("unexpected info: default=%q bots=%+v", info.DefaultBotID, info.Bots)
	}

	// Validation errors on POST /v1/bots.
	badCases := []struct {
		name string
		body map[string]any
		code string
		want int
	}{
		{"reserved default", map[string]any{"id": "default", "name": "Dup"}, "bad_request", http.StatusBadRequest},
		{"invalid id", map[string]any{"id": "Bad!ID", "name": "Bad"}, "bad_request", http.StatusBadRequest},
		{"unknown model", map[string]any{"id": "r1", "name": "R1", "default_model": "nope"}, "unknown_model", http.StatusBadRequest},
		{"unknown tool", map[string]any{"id": "r2", "name": "R2", "tool_allowlist": []string{"no_such_tool"}}, "unknown_tool", http.StatusBadRequest},
		{"oversized instructions", map[string]any{"id": "r3", "name": "R3", "instructions": strings.Repeat("x", MaxBotInstructionsBytes+1)}, "bad_request", http.StatusBadRequest},
	}
	for _, tc := range badCases {
		resp := doReq(t, http.MethodPost, ts.URL+"/v1/bots", "secret", tc.body)
		ae := decodeAPIError(t, resp)
		resp.Body.Close()
		if resp.StatusCode != tc.want || ae.Code != tc.code {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", tc.name, resp.StatusCode, ae.Code, tc.want, tc.code)
		}
	}

	// Create valid bot.
	resp = doReq(t, http.MethodPost, ts.URL+"/v1/bots", "secret", map[string]any{
		"id":             "reviewer",
		"name":           "Code Reviewer",
		"title":          "PR Specialist",
		"instructions":   "Be strict.",
		"default_model":  "other",
		"tool_allowlist": []string{"read_file", "mcp__gh__*"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/bots status = %d", resp.StatusCode)
	}
	var created BotProfile
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID != "reviewer" || !created.Enabled || created.DefaultModel != "other" {
		t.Fatalf("unexpected created bot: %+v", created)
	}

	// Duplicate ID -> 409 bot_exists.
	resp = doReq(t, http.MethodPost, ts.URL+"/v1/bots", "secret", map[string]any{"id": "reviewer", "name": "Dup"})
	ae := decodeAPIError(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || ae.Code != "bot_exists" {
		t.Fatalf("duplicate create got (%d, %q), want (409, bot_exists)", resp.StatusCode, ae.Code)
	}

	// Disabling default bot is forbidden.
	resp = doReq(t, http.MethodPatch, ts.URL+"/v1/bots/default", "secret", map[string]any{"enabled": false})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disable default status = %d, want 400", resp.StatusCode)
	}

	// Deleting default bot returns 405 method_not_allowed.
	resp = doReq(t, http.MethodDelete, ts.URL+"/v1/bots/default", "secret", nil)
	ae = decodeAPIError(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || ae.Code != "method_not_allowed" {
		t.Fatalf("delete default got (%d, %q), want (405, method_not_allowed)", resp.StatusCode, ae.Code)
	}
}

func TestMultiBotSessionIsolationAndHotReload(t *testing.T) {
	ts, m, rt, _ := newMultiBotServer(t)

	resp := doReq(t, http.MethodPost, ts.URL+"/v1/bots", "secret", map[string]any{
		"id":             "reviewer",
		"name":           "Reviewer",
		"instructions":   "v1 instructions",
		"default_model":  "other",
		"tool_allowlist": []string{"read_file"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bot status = %d", resp.StatusCode)
	}

	// Create one session under default, one under reviewer.
	rDef := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", "secret", map[string]any{})
	var defSess SessionInfo
	_ = json.NewDecoder(rDef.Body).Decode(&defSess)
	rDef.Body.Close()

	rRev := doReq(t, http.MethodPost, ts.URL+"/v1/bots/reviewer/sessions", "secret", map[string]any{})
	var revSess SessionInfo
	_ = json.NewDecoder(rRev.Body).Decode(&revSess)
	rRev.Body.Close()
	if rRev.StatusCode != http.StatusCreated {
		t.Fatalf("create reviewer session status = %d", rRev.StatusCode)
	}
	if revSess.BotID != "reviewer" || revSess.Model != "other" {
		t.Fatalf("unexpected reviewer session: %+v", revSess)
	}

	// Flat /v1/sessions/{reviewerSession} must 404 (strictly default-bot only).
	rFlat := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+revSess.ID, "secret", nil)
	rFlat.Body.Close()
	if rFlat.StatusCode != http.StatusNotFound {
		t.Fatalf("flat GET reviewer session status = %d, want 404", rFlat.StatusCode)
	}

	// Scoped GET succeeds.
	rScoped := doReq(t, http.MethodGet, ts.URL+"/v1/bots/reviewer/sessions/"+revSess.ID, "secret", nil)
	rScoped.Body.Close()
	if rScoped.StatusCode != http.StatusOK {
		t.Fatalf("scoped GET reviewer session status = %d, want 200", rScoped.StatusCode)
	}

	// Listing is strictly isolated per bot.
	defList, err := m.ListSessionsForBot(DefaultBotID, 50)
	if err != nil || len(defList) != 1 || defList[0].ID != defSess.ID {
		t.Fatalf("default sessions = %+v, err=%v", defList, err)
	}
	revList, err := m.ListSessionsForBot("reviewer", 50)
	if err != nil || len(revList) != 1 || revList[0].ID != revSess.ID {
		t.Fatalf("reviewer sessions = %+v, err=%v", revList, err)
	}

	// First turn sees v1 profile.
	turn1, err := m.SendForBot("reviewer", revSess.ID, SendRequest{Content: "hello"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn1)
	in1 := rt.lastInput()
	if in1.BotID != "reviewer" || in1.MemoryUserID != "bot:reviewer" || in1.SystemPromptExtra != "v1 instructions" {
		t.Fatalf("turn1 input mismatch: %+v", in1)
	}
	if !slices.Equal(in1.BotToolAllowlist, []string{"read_file"}) {
		t.Fatalf("turn1 allowlist = %v", in1.BotToolAllowlist)
	}

	// PATCH bot profile -> next turn immediately hot-reloads new instructions & allowlist.
	rPatch := doReq(t, http.MethodPatch, ts.URL+"/v1/bots/reviewer", "secret", map[string]any{
		"instructions":   "v2 instructions",
		"tool_allowlist": []string{"grep_search"},
	})
	rPatch.Body.Close()
	if rPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH bot status = %d", rPatch.StatusCode)
	}

	turn2, err := m.SendForBot("reviewer", revSess.ID, SendRequest{Content: "second"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn2)
	in2 := rt.lastInput()
	if in2.SystemPromptExtra != "v2 instructions" || !slices.Equal(in2.BotToolAllowlist, []string{"grep_search"}) {
		t.Fatalf("turn2 did not hot-reload bot profile: %+v", in2)
	}
}

func TestMultiBotDisabledScheduleSkipAndForceDelete(t *testing.T) {
	ts, m, _, svc := newMultiBotServer(t)

	// Create bot ops.
	resp := doReq(t, http.MethodPost, ts.URL+"/v1/bots", "secret", map[string]any{
		"id":   "ops",
		"name": "Ops",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ops status = %d", resp.StatusCode)
	}

	// Create session and schedule under ops.
	rSess := doReq(t, http.MethodPost, ts.URL+"/v1/bots/ops/sessions", "secret", map[string]any{})
	var opsSess SessionInfo
	_ = json.NewDecoder(rSess.Body).Decode(&opsSess)
	rSess.Body.Close()

	rSch := doReq(t, http.MethodPost, ts.URL+"/v1/bots/ops/schedules", "secret", map[string]any{
		"name":    "ops-check",
		"prompt":  "check health",
		"trigger": map[string]any{"type": "interval", "every": "1h"},
	})
	if rSch.StatusCode != http.StatusCreated {
		t.Fatalf("create ops schedule status = %d", rSch.StatusCode)
	}
	var schEnv struct {
		Schedule schedule.Schedule `json:"schedule"`
	}
	_ = json.NewDecoder(rSch.Body).Decode(&schEnv)
	rSch.Body.Close()
	if schEnv.Schedule.BotID != "ops" {
		t.Fatalf("schedule bot_id = %q, want ops", schEnv.Schedule.BotID)
	}

	// Scoped list_schedules sees it; default bot list does not.
	opsSchedules, err := svc.ListForBot(context.Background(), "ops", "", 50)
	if err != nil || len(opsSchedules) != 1 {
		t.Fatalf("ops schedules = %+v, err=%v", opsSchedules, err)
	}
	defSchedules, err := svc.ListForBot(context.Background(), DefaultBotID, "", 50)
	if err != nil || len(defSchedules) != 0 {
		t.Fatalf("default schedules = %+v, err=%v", defSchedules, err)
	}

	// Disable ops bot.
	rDis := doReq(t, http.MethodPatch, ts.URL+"/v1/bots/ops", "secret", map[string]any{"enabled": false})
	rDis.Body.Close()
	if rDis.StatusCode != http.StatusOK {
		t.Fatalf("disable ops status = %d", rDis.StatusCode)
	}

	// Creating session / sending / creating schedule on disabled bot -> 403 bot_disabled.
	r1 := doReq(t, http.MethodPost, ts.URL+"/v1/bots/ops/sessions", "secret", map[string]any{})
	ae1 := decodeAPIError(t, r1)
	r1.Body.Close()
	if r1.StatusCode != http.StatusForbidden || ae1.Code != "bot_disabled" {
		t.Fatalf("disabled create session got (%d, %q)", r1.StatusCode, ae1.Code)
	}
	r2 := doReq(t, http.MethodPost, ts.URL+"/v1/bots/ops/sessions/"+opsSess.ID+"/messages", "secret", map[string]any{"content": "hi"})
	ae2 := decodeAPIError(t, r2)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden || ae2.Code != "bot_disabled" {
		t.Fatalf("disabled send got (%d, %q)", r2.StatusCode, ae2.Code)
	}
	r3 := doReq(t, http.MethodPost, ts.URL+"/v1/bots/ops/schedules", "secret", map[string]any{
		"name": "blocked", "prompt": "hi", "trigger": map[string]any{"type": "interval", "every": "1h"},
	})
	ae3 := decodeAPIError(t, r3)
	r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden || ae3.Code != "bot_disabled" {
		t.Fatalf("disabled create schedule got (%d, %q)", r3.StatusCode, ae3.Code)
	}

	// Firing ops's schedule while disabled records RunSkipped(bot_disabled).
	run, err := svc.RunNow(context.Background(), schEnv.Schedule.ID, "req-disabled")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var finalRun schedule.Run
	for time.Now().Before(deadline) {
		got, err := svc.GetRun(context.Background(), run.ID)
		if err == nil && got.Status.Terminal() {
			finalRun = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalRun.Status != schedule.RunSkipped || finalRun.QueuedReason != "bot_disabled" {
		t.Fatalf("disabled run got status=%s reason=%q, want skipped(bot_disabled)", finalRun.Status, finalRun.QueuedReason)
	}

	// Non-forced DELETE returns 409 bot_has_sessions.
	rDel := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ops", "secret", nil)
	aeDel := decodeAPIError(t, rDel)
	rDel.Body.Close()
	if rDel.StatusCode != http.StatusConflict || aeDel.Code != "bot_has_sessions" {
		t.Fatalf("non-forced delete got (%d, %q), want (409, bot_has_sessions)", rDel.StatusCode, aeDel.Code)
	}

	// Re-enable, start a blocking turn, then force delete -> cancels turn & cascades.
	rEn := doReq(t, http.MethodPatch, ts.URL+"/v1/bots/ops", "secret", map[string]any{"enabled": true})
	rEn.Body.Close()
	blockingTurn, err := m.SendForBot("ops", opsSess.ID, SendRequest{Content: "block"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rForce := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ops?force=1", "secret", nil)
	rForce.Body.Close()
	if rForce.StatusCode != http.StatusNoContent {
		t.Fatalf("force delete status = %d, want 204", rForce.StatusCode)
	}
	sum := waitTurn(t, blockingTurn)
	if sum.Status != TurnCancelled {
		t.Fatalf("blocking turn status = %s, want cancelled", sum.Status)
	}

	// Bot, sessions, and schedules are all gone.
	if _, err := m.GetBot(context.Background(), "ops"); !errors.Is(err, ErrUnknownBot) {
		t.Fatalf("GetBot(ops) err = %v, want ErrUnknownBot", err)
	}
	if left, _ := svc.ListForBot(context.Background(), "ops", "", 50); len(left) != 0 {
		t.Fatalf("ops schedules still exist: %+v", left)
	}
}

func TestMultiBotWebSocketOpsAndEvents(t *testing.T) {
	ts, _, _, _ := newMultiBotServer(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=secret"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var events []Event
	sendOp := func(req WSRequest) WSMessage {
		t.Helper()
		if err := websocket.JSON.Send(conn, req); err != nil {
			t.Fatal(err)
		}
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "event" && msg.Event != nil {
				events = append(events, *msg.Event)
				continue
			}
			if msg.Kind == "reply" && msg.ID == req.ID {
				return msg
			}
		}
	}

	if rep := sendOp(WSRequest{ID: "1", Op: "subscribe", SessionID: "*"}); !rep.OK {
		t.Fatalf("subscribe *: %+v", rep.Error)
	}
	if rep := sendOp(WSRequest{ID: "2", Op: "create_bot", Params: map[string]any{"id": "writer", "name": "Writer"}}); !rep.OK {
		t.Fatalf("create_bot: %+v", rep.Error)
	}
	if rep := sendOp(WSRequest{ID: "3", Op: "update_bot", BotID: "writer", Params: map[string]any{"title": "Docs"}}); !rep.OK {
		t.Fatalf("update_bot: %+v", rep.Error)
	}
	if rep := sendOp(WSRequest{ID: "4", Op: "create_session", BotID: "writer"}); !rep.OK {
		t.Fatalf("create_session(writer): %+v", rep.Error)
	}
	if rep := sendOp(WSRequest{ID: "5", Op: "delete_bot", BotID: "writer", Force: true}); !rep.OK {
		t.Fatalf("delete_bot(writer): %+v", rep.Error)
	}

	// Drain any trailing events briefly until bot_deleted is seen.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	hasEvent := func(typ EventType) bool {
		for _, e := range events {
			if e.Type == typ && e.Bot != nil && e.Bot.ID == "writer" {
				return true
			}
		}
		return false
	}
	for !hasEvent(EventBotDeleted) {
		var msg WSMessage
		if err := websocket.JSON.Receive(conn, &msg); err != nil {
			break
		}
		if msg.Kind == "event" && msg.Event != nil {
			events = append(events, *msg.Event)
		}
	}
	for _, want := range []EventType{EventBotCreated, EventBotUpdated, EventBotDeleted} {
		if !hasEvent(want) {
			t.Errorf("missing lifecycle event %s; got %v", want, eventTypes(events))
		}
	}
}

func TestAgentRuntimeTurnInjectionAndToolIntersection(t *testing.T) {
	type capturedReq struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}

	var mu sync.Mutex
	var reqs []capturedReq
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cr capturedReq
		if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		reqs = append(reqs, cr)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
	}))
	defer mockLLM.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	modelReg, err := llm.NewModelRegistry([]llm.ModelDef{{
		Name:     "mock",
		Provider: "openai-compat",
		Model:    "mock-model",
		BaseURL:  mockLLM.URL,
	}}, "mock")
	if err != nil {
		t.Fatal(err)
	}

	toolReg := tools.NewRegistry()
	for _, name := range []string{"read_file", "grep_search", "bash", "mcp__gh__list_prs"} {
		_ = toolReg.Register(makeStubTool(name))
	}

	tick := "2026-11-24 20:00"
	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:             modelReg,
		Tools:              toolReg,
		SystemPrompt:       "Base system prompt.",
		SystemPromptSuffix: func() string { return "Current time: " + tick },
		WorkDir:            t.TempDir(),
		SandboxBaseDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	conv, err := rt.NewConversation("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()

	run := func(prompt string) {
		t.Helper()
		_, err := conv.RunTurn(context.Background(), TurnInput{
			BotID:             "reviewer",
			SessionID:         "sess-1",
			TurnID:            "t-" + prompt,
			Model:             "mock",
			SystemPromptExtra: "Always review carefully.",
			MemoryUserID:      "bot:reviewer",
			BotToolAllowlist:  []string{"read_file", "grep_search", "mcp__gh__*"},
			ToolAllowlist:     []string{"read_file", "bash", "mcp__gh__*"},
			Messages:          []models.Message{{ID: "m1", SessionID: "sess-1", Role: models.RoleHuman, Content: prompt}},
			Emit:              func(Event) {},
		})
		if err != nil {
			t.Fatalf("RunTurn(%s): %v", prompt, err)
		}
	}

	run("turn1")
	tick = "2026-11-24 20:05"
	run("turn2")

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("captured %d LLM requests, want 2", len(reqs))
	}
	sysOf := func(c capturedReq) string {
		for _, m := range c.Messages {
			if m.Role == "system" {
				s, _ := m.Content.(string)
				return s
			}
		}
		return ""
	}
	sys1, sys2 := sysOf(reqs[0]), sysOf(reqs[1])
	if sys1 == "" || sys1 != sys2 {
		t.Fatalf("system prompt drifted across turns:\n1: %q\n2: %q", sys1, sys2)
	}
	if !strings.Contains(sys1, "## Bot Instructions") || !strings.Contains(sys1, "Always review carefully.") {
		t.Fatalf("system prompt missing Bot Instructions:\n%s", sys1)
	}
	if strings.Contains(sys1, "2026-11-24") {
		t.Fatalf("dynamic SystemPromptSuffix leaked into system prompt:\n%s", sys1)
	}

	// Verify dynamic suffix landed in the trailing turn injection user message.
	raw1, _ := json.Marshal(reqs[0].Messages)
	raw2, _ := json.Marshal(reqs[1].Messages)
	if !strings.Contains(string(raw1), "Current time: 2026-11-24 20:00") {
		t.Fatalf("turn1 missing dynamic suffix in messages: %s", raw1)
	}
	if !strings.Contains(string(raw2), "Current time: 2026-11-24 20:05") {
		t.Fatalf("turn2 missing updated dynamic suffix in messages: %s", raw2)
	}

	// Verify tool list is the exact intersection: read_file & mcp__gh__list_prs.
	var gotTools []string
	for _, tl := range reqs[0].Tools {
		gotTools = append(gotTools, tl.Function.Name)
	}
	slices.Sort(gotTools)
	wantTools := []string{"mcp__gh__list_prs", "read_file"}
	if !slices.Equal(gotTools, wantTools) {
		t.Fatalf("tools sent to LLM = %v, want intersection %v", gotTools, wantTools)
	}
}

func TestMultiBotWSGlobalFilterExcludesOtherBotTurns(t *testing.T) {
	ts, m, _, _ := newMultiBotServer(t)

	if _, err := m.CreateBot(context.Background(), CreateBotRequest{ID: "reviewer", Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	defSess, err := m.CreateSessionForBot(DefaultBotID, CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	revSess, err := m.CreateSessionForBot("reviewer", CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=secret"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Subscribe "*" filtered strictly to bot_id="reviewer".
	if err := websocket.JSON.Send(conn, WSRequest{ID: "1", Op: "subscribe", SessionID: "*", BotID: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	var rep WSMessage
	for {
		if err := websocket.JSON.Receive(conn, &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Kind == "reply" && rep.ID == "1" {
			if !rep.OK {
				t.Fatalf("subscribe failed: %+v", rep.Error)
			}
			break
		}
	}

	// Run a turn on default bot (must NOT leak to reviewer's "*" feed),
	// followed by a turn on reviewer (MUST appear).
	tDef, err := m.SendForBot(DefaultBotID, defSess.ID, SendRequest{Content: "default msg"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, tDef)

	tRev, err := m.SendForBot("reviewer", revSess.ID, SendRequest{Content: "reviewer msg"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, tRev)

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var seenRevStart, seenRevEnd bool
	for !(seenRevStart && seenRevEnd) {
		var msg WSMessage
		if err := websocket.JSON.Receive(conn, &msg); err != nil {
			t.Fatalf("waiting for reviewer events: %v", err)
		}
		if msg.Kind != "event" || msg.Event == nil || msg.Event.Type == EventHeartbeat {
			continue
		}
		e := msg.Event
		if e.SessionID == defSess.ID || (e.BotID != "" && e.BotID != "reviewer") {
			t.Fatalf("cross-bot event leaked into reviewer-filtered * stream: %+v", e)
		}
		if e.SessionID == revSess.ID && e.Type == EventTurnStart {
			seenRevStart = true
		}
		if e.SessionID == revSess.ID && e.Type == EventTurnEnd {
			seenRevEnd = true
		}
	}
}

func TestMultiBotDeletePurgesSoftDeletedSchedules(t *testing.T) {
	ts, m, _, svc := newMultiBotServer(t)
	ctx := context.Background()

	if _, err := m.CreateBot(ctx, CreateBotRequest{ID: "ephemeral", Name: "Ephemeral"}); err != nil {
		t.Fatal(err)
	}
	sch, err := svc.Create(ctx, schedule.CreateInput{
		BotID:   "ephemeral",
		Name:    "temp-task",
		Prompt:  "ping",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Soft-delete the schedule so active CountByBot is 0, leaving a soft-deleted row.
	if _, err := svc.Delete(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}

	// Non-forced DELETE /v1/bots/ephemeral succeeds and must hard-purge the soft-deleted schedule.
	resp := doReq(t, http.MethodDelete, ts.URL+"/v1/bots/ephemeral", "secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE ephemeral status = %d, want 204", resp.StatusCode)
	}
	if list, err := svc.ListForBot(ctx, "ephemeral", "all", 50); err != nil || len(list) != 0 {
		t.Fatalf("soft-deleted schedule leaked after DeleteBot: %+v, err=%v", list, err)
	}
}

func TestScheduleDispatcherUnknownBotDoesNotMarkBotDisabled(t *testing.T) {
	m, _, _ := newTestManager(t)
	svc, disp := newScheduleTestService(t, m)

	_, _, err := disp.Fire(context.Background(), schedule.Schedule{
		ID:    "sch_orphan",
		BotID: "ghost",
		Name:  "ghost task",
	}, schedule.Run{ID: "run_orphan", ScheduleID: "sch_orphan"})
	_ = svc
	if err == nil {
		t.Fatal("expected error for unknown bot, got nil")
	}
	if errors.Is(err, schedule.ErrBotDisabled) {
		t.Fatalf("unknown bot must not be mapped to schedule.ErrBotDisabled: %v", err)
	}
	if !errors.Is(err, ErrUnknownBot) {
		t.Fatalf("err = %v, want wrapped ErrUnknownBot", err)
	}
}
