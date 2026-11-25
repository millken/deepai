package bot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
)

func signGitHubPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookHTTPEndpointAndToolAllowlistValidation(t *testing.T) {
	ts, _, rt, svc := newMultiBotServer(t)

	// Invalid tool_allowlist on schedule create is rejected with 400 unknown_tool.
	rBadTool := doReq(t, http.MethodPost, ts.URL+"/v1/schedules", "secret", map[string]any{
		"name":           "bad-tools",
		"prompt":         "hi",
		"trigger":        map[string]any{"type": "interval", "every": "1h"},
		"tool_allowlist": []string{"no_such_tool"},
	})
	ae := decodeAPIError(t, rBadTool)
	rBadTool.Body.Close()
	if rBadTool.StatusCode != http.StatusBadRequest || ae.Code != "unknown_tool" {
		t.Fatalf("bad tool_allowlist got (%d, %q), want (400, unknown_tool)", rBadTool.StatusCode, ae.Code)
	}

	// Create a webhook schedule via REST.
	rCreate := doReq(t, http.MethodPost, ts.URL+"/v1/schedules", "secret", map[string]any{
		"name":   "pr-opened",
		"prompt": "Check PR {{.Payload.number}}",
		"trigger": map[string]any{
			"type": "webhook",
			"event_filter": map[string]string{
				"header.X-GitHub-Event": "pull_request",
				"payload.action":        "opened",
			},
		},
		"tool_allowlist": []string{"read_file"},
	})
	if rCreate.StatusCode != http.StatusCreated {
		t.Fatalf("create webhook schedule status = %d", rCreate.StatusCode)
	}
	var env struct {
		Schedule schedule.Schedule `json:"schedule"`
	}
	_ = json.NewDecoder(rCreate.Body).Decode(&env)
	rCreate.Body.Close()
	if env.Schedule.WebhookSecret == "" {
		t.Fatal("expected one-time webhook_secret on create")
	}

	hookURL := ts.URL + "/v1/hooks/" + env.Schedule.ID
	payload := []byte(`{"action":"opened","number":99}`)

	// Call POST /v1/hooks/{id} WITHOUT bot bearer token: bad signature -> 401.
	reqBad, _ := http.NewRequest(http.MethodPost, hookURL, bytes.NewReader(payload))
	reqBad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatal(err)
	}
	respBad.Body.Close()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad webhook sig status = %d, want 401", respBad.StatusCode)
	}

	// Valid GitHub HMAC signature WITHOUT bot bearer token -> 202 and executes turn!
	reqOK, _ := http.NewRequest(http.MethodPost, hookURL, bytes.NewReader(payload))
	reqOK.Header.Set("X-Hub-Signature-256", signGitHubPayload(env.Schedule.WebhookSecret, payload))
	reqOK.Header.Set("X-GitHub-Event", "pull_request")
	reqOK.Header.Set("X-GitHub-Delivery", "gh-deliv-99")
	respOK, err := http.DefaultClient.Do(reqOK)
	if err != nil {
		t.Fatal(err)
	}
	var whRes schedule.WebhookResult
	_ = json.NewDecoder(respOK.Body).Decode(&whRes)
	respOK.Body.Close()
	if respOK.StatusCode != http.StatusAccepted || whRes.Run == nil || whRes.Deduplicated {
		t.Fatalf("valid webhook = (%d, %+v)", respOK.StatusCode, whRes)
	}

	doneRun := waitRunStatus(t, svc, whRes.Run.ID, schedule.RunCompleted)
	if !strings.Contains(doneRun.Output, "Check PR 99") || !strings.Contains(doneRun.Output, "<untrusted_webhook_payload") {
		t.Fatalf("webhook run output = %q", doneRun.Output)
	}
	in := rt.lastInput()
	if len(in.ToolAllowlist) != 1 || in.ToolAllowlist[0] != "read_file" {
		t.Fatalf("webhook run tool_allowlist = %v, want [read_file]", in.ToolAllowlist)
	}

	// Duplicate delivery returns 202 deduplicated=true with same Run ID.
	reqDup, _ := http.NewRequest(http.MethodPost, hookURL, bytes.NewReader(payload))
	reqDup.Header.Set("X-Hub-Signature-256", signGitHubPayload(env.Schedule.WebhookSecret, payload))
	reqDup.Header.Set("X-GitHub-Event", "pull_request")
	reqDup.Header.Set("X-GitHub-Delivery", "gh-deliv-99")
	respDup, err := http.DefaultClient.Do(reqDup)
	if err != nil {
		t.Fatal(err)
	}
	var dupRes schedule.WebhookResult
	_ = json.NewDecoder(respDup.Body).Decode(&dupRes)
	respDup.Body.Close()
	if respDup.StatusCode != http.StatusAccepted || !dupRes.Deduplicated || dupRes.Run == nil || dupRes.Run.ID != whRes.Run.ID {
		t.Fatalf("dup webhook = (%d, %+v)", respDup.StatusCode, dupRes)
	}
}

type scriptOutputRuntime struct {
	mu      sync.Mutex
	outputs []string
	idx     int
	prompts []string
}

func (r *scriptOutputRuntime) Info() Info {
	return Info{Models: []ModelInfo{{Name: "fake"}}, DefaultModel: "fake"}
}
func (r *scriptOutputRuntime) DefaultModel() string   { return "fake" }
func (r *scriptOutputRuntime) HasModel(a string) bool { return a == "fake" }
func (r *scriptOutputRuntime) NewConversation(string) (Conversation, error) {
	return &scriptOutputConv{rt: r}, nil
}

type scriptOutputConv struct{ rt *scriptOutputRuntime }

func (c *scriptOutputConv) Close() error { return nil }
func (c *scriptOutputConv) RunTurn(_ context.Context, in TurnInput) (*TurnOutput, error) {
	c.rt.mu.Lock()
	c.rt.prompts = append(c.rt.prompts, in.Messages[len(in.Messages)-1].Content)
	out := "ok"
	if c.rt.idx < len(c.rt.outputs) {
		out = c.rt.outputs[c.rt.idx]
		c.rt.idx++
	}
	c.rt.mu.Unlock()
	msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
		ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: out,
	})
	return &TurnOutput{Messages: msgs, FinalOutput: out, LLMTurns: 1}, nil
}

func TestScheduleNotifyIfMarkerSuppressesHookEndToEnd(t *testing.T) {
	rt := &scriptOutputRuntime{outputs: []string{"[NO_NOTIFY] everything looks green", "ALERT: disk usage 95%"}}
	emitter := &recordingEmitter{}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), Hook: emitter, IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	svc, _ := newScheduleTestService(t, m)

	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name:    "disk-watch",
		Prompt:  "check disk usage",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		Options: schedule.Options{
			Notify:   schedule.NotifyAlways,
			NotifyIf: &schedule.NotifyIf{Mode: schedule.NotifyIfMarker},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run 1: outputs [NO_NOTIFY] -> hook must NOT fire.
	r1, err := svc.RunNow(context.Background(), sch.ID, "r1")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, r1.ID, schedule.RunCompleted)
	time.Sleep(80 * time.Millisecond)
	if n := emitter.count(hook.EventScheduleRunEnd); n != 0 {
		t.Fatalf("expected 0 notifications for [NO_NOTIFY], got %d", n)
	}

	// Verify marker convention hint was appended to the prompt sent to the model.
	rt.mu.Lock()
	firstPrompt := rt.prompts[0]
	rt.mu.Unlock()
	if !strings.Contains(firstPrompt, "[NO_NOTIFY]") {
		t.Fatalf("prompt missing [NO_NOTIFY] marker guidance:\n%s", firstPrompt)
	}

	// Run 2: outputs real alert -> hook MUST fire!
	r2, err := svc.RunNow(context.Background(), sch.ID, "r2")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, r2.ID, schedule.RunCompleted)
	evt := emitter.waitFor(t, hook.EventScheduleRunEnd)
	if !strings.Contains(evt.Message, "ALERT: disk usage 95%") {
		t.Fatalf("unexpected hook message: %q", evt.Message)
	}
}

func TestScheduleChainEndToEndAcrossBots(t *testing.T) {
	ts, m, rt, svc := newMultiBotServer(t)
	_ = ts
	ctx := context.Background()

	if _, err := m.CreateBot(ctx, CreateBotRequest{
		ID:           "reviewer",
		Name:         "Reviewer",
		Instructions: "Review upstream outputs.",
	}); err != nil {
		t.Fatal(err)
	}

	schA, err := svc.Create(ctx, schedule.CreateInput{
		BotID:   DefaultBotID,
		Name:    "fetch-logs",
		Prompt:  "collect error logs",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	schB, err := svc.Create(ctx, schedule.CreateInput{
		BotID:   "reviewer",
		Name:    "review-logs",
		Prompt:  "Audit this output: {{prev.output}}",
		Trigger: schedule.Trigger{Type: schedule.TriggerChain, AfterScheduleID: schA.ID, OnStatus: schedule.ChainOnCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}

	runA, err := svc.RunNow(ctx, schA.ID, "chain-e2e")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, runA.ID, schedule.RunCompleted)

	// Wait for downstream chain B to complete automatically under bot "reviewer".
	deadline := time.Now().Add(3 * time.Second)
	var runB schedule.Run
	for time.Now().Before(deadline) {
		runs, _ := svc.Runs(ctx, schB.ID, 5)
		if len(runs) > 0 && runs[0].Status == schedule.RunCompleted {
			runB = runs[0]
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if runB.Status != schedule.RunCompleted {
		t.Fatalf("downstream chain B did not complete: %+v", runB)
	}
	in := rt.lastInput()
	if in.BotID != "reviewer" || in.SystemPromptExtra != "Review upstream outputs." {
		t.Fatalf("downstream chain B did not run under reviewer bot: %+v", in)
	}
	if !strings.Contains(runB.Output, "Audit this output:") || !strings.Contains(runB.Output, "collect error logs") {
		t.Fatalf("downstream chain B output missing upstream context: %q", runB.Output)
	}
}
