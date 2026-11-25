package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
	_ "modernc.org/sqlite"
)

type fakeChannelRuntime struct {
	mu      sync.Mutex
	blockCh chan struct{}
	mode    string
}

func (f *fakeChannelRuntime) Info() bot.Info {
	return bot.Info{Models: []bot.ModelInfo{{Name: "fake"}}, DefaultModel: "fake"}
}
func (f *fakeChannelRuntime) DefaultModel() string   { return "fake" }
func (f *fakeChannelRuntime) HasModel(a string) bool { return a == "fake" }
func (f *fakeChannelRuntime) NewConversation(string) (bot.Conversation, error) {
	return &fakeChannelConv{rt: f}, nil
}

type fakeChannelConv struct{ rt *fakeChannelRuntime }

func (c *fakeChannelConv) Close() error { return nil }
func (c *fakeChannelConv) RunTurn(ctx context.Context, in bot.TurnInput) (*bot.TurnOutput, error) {
	last := in.Messages[len(in.Messages)-1].Content
	reply := func(text string) *bot.TurnOutput {
		msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
			ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: text,
		})
		return &bot.TurnOutput{Messages: msgs, FinalOutput: text, Usage: &agent.Usage{InputTokens: 2, OutputTokens: 2}, LLMTurns: 1}
	}
	if last == "ask" {
		ans, err := in.Interaction.AskQuestion(ctx, "proceed?", []string{"yes", "no"})
		if err != nil {
			return &bot.TurnOutput{Messages: in.Messages}, err
		}
		in.Emit(bot.Event{Type: bot.EventTextDelta, Text: "answered:" + ans})
		return reply("answered:" + ans), nil
	}
	if last == "block" {
		c.rt.mu.Lock()
		ch := c.rt.blockCh
		c.rt.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return &bot.TurnOutput{Messages: in.Messages}, ctx.Err()
		}
		in.Emit(bot.Event{Type: bot.EventTextDelta, Text: "unblocked"})
		return reply("unblocked"), nil
	}
	in.Emit(bot.Event{Type: bot.EventTextDelta, Text: "echo:" + in.BotID + ":" + last})
	return reply("echo:" + in.BotID + ":" + last), nil
}

type mockAdapter struct {
	name      string
	mu        sync.Mutex
	sent      []string
	edited    []string
	questions []bot.Question
	seq       int
}

func (a *mockAdapter) Name() string                                          { return a.name }
func (a *mockAdapter) Start(ctx context.Context, _ func(InboundMessage)) error { <-ctx.Done(); return ctx.Err() }
func (a *mockAdapter) SendText(_ context.Context, _ Destination, text string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	a.sent = append(a.sent, text)
	return "m_" + string(rune('0'+a.seq)), nil
}
func (a *mockAdapter) EditText(_ context.Context, _ Destination, _, text string) error {
	a.mu.Lock()
	a.edited = append(a.edited, text)
	a.mu.Unlock()
	return nil
}
func (a *mockAdapter) SendQuestion(_ context.Context, _ Destination, q bot.Question) (string, error) {
	a.mu.Lock()
	a.questions = append(a.questions, q)
	a.mu.Unlock()
	return "qmsg_1", nil
}
func (a *mockAdapter) SendAttachment(_ context.Context, _ Destination, _, _ string) error { return nil }

func (a *mockAdapter) waitForSent(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, s := range append(append([]string(nil), a.sent...), a.edited...) {
			if strings.Contains(s, substr) {
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		time.Sleep(15 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t.Fatalf("timed out waiting for %q; sent=%v edited=%v", substr, a.sent, a.edited)
}

func TestGatewayAllowlistDefaultDenyAndAudit(t *testing.T) {
	rt := &fakeChannelRuntime{}
	m, _ := bot.NewManager(bot.Options{Runtime: rt, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(context.Background())

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	auditLog, err := bot.NewAuditLogger(auditPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	gw := NewGateway(GatewayOptions{Manager: m, Audit: auditLog})
	ad := &mockAdapter{name: "telegram"}
	// Empty allowlist => default-deny EVERY external user.
	gw.Register(ad, AccessPolicy{})

	gw.HandleInbound(context.Background(), InboundMessage{
		Channel: "telegram", UserID: "999", ChatID: "999", Text: "hello",
	})
	if len(ad.sent) != 0 {
		t.Fatalf("default-deny allowlist allowed message: %v", ad.sent)
	}
	raw, err := os.ReadFile(auditPath)
	if err != nil || !strings.Contains(string(raw), `"action":"channel_rejected"`) {
		t.Fatalf("audit log missing channel_rejected: %q (err=%v)", string(raw), err)
	}
}

func TestGatewayConversationCommandsQuestionAndApprove(t *testing.T) {
	ctx := context.Background()
	rt := &fakeChannelRuntime{blockCh: make(chan struct{})}
	m, _ := bot.NewManager(bot.Options{Runtime: rt, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(ctx)
	_, _ = m.CreateBot(ctx, bot.CreateBotRequest{ID: "reviewer", Name: "Reviewer"})

	cfg := schedule.DefaultConfig()
	cfg.Timezone = "UTC"
	svc, _ := schedule.NewService(schedule.NewMemoryStore(), cfg)
	bot.AttachSchedule(m, svc)
	pendingSch, err := svc.Create(ctx, schedule.CreateInput{
		Name:    "nightly",
		Prompt:  "check",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		State:   schedule.StatePendingApproval,
	})
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bindings, err := NewSQLiteBindingStore(db)
	if err != nil {
		t.Fatal(err)
	}

	gw := NewGateway(GatewayOptions{Manager: m, Schedules: svc, Bindings: bindings})
	ad := &mockAdapter{name: "telegram"}
	gw.Register(ad, AccessPolicy{AllowUsers: []string{"u1"}, StreamEditInterval: 20 * time.Millisecond})

	// 1. Group message without mention is ignored.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "grp1", IsGroup: true, Mentioned: false, Text: "chatter"})
	if len(ad.sent) != 0 {
		t.Fatalf("unmentioned group message should be ignored: %v", ad.sent)
	}

	// 2. Normal DM message creates SQLite binding and streams response.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "hello"})
	ad.waitForSent(t, "echo:default:hello")
	gw.Wait()

	// 3. Switch bot via /bot reviewer and verify next message hits reviewer.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "/bot reviewer"})
	ad.waitForSent(t, "已切换至 Bot")
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "review PR"})
	ad.waitForSent(t, "echo:reviewer:review PR")
	gw.Wait()

	// 4. Question callback flow: ask -> inline button callback -> answer delivered.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "ask"})
	deadline := time.Now().Add(3 * time.Second)
	var qID string
	for time.Now().Before(deadline) {
		ad.mu.Lock()
		if len(ad.questions) > 0 {
			qID = ad.questions[0].ID
		}
		ad.mu.Unlock()
		if qID != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if qID == "" {
		t.Fatal("adapter never received SendQuestion")
	}
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", CallbackData: "ans:" + qID + ":yes"})
	ad.waitForSent(t, "answered:yes")
	gw.Wait()

	// 5. Concurrent message queues while turn is running.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "block"})
	time.Sleep(40 * time.Millisecond)
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "follow up"})
	ad.waitForSent(t, "已加入队列（第 1 位）")
	close(rt.blockCh)
	ad.waitForSent(t, "echo:reviewer:follow up")
	gw.Wait()

	// 6. /approve <schedule_id> approves pending schedule from IM.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "/approve " + pendingSch.ID})
	ad.waitForSent(t, "已批准任务")
	gotSch, err := svc.Get(ctx, pendingSch.ID)
	if err != nil || gotSch.State != schedule.StateActive {
		t.Fatalf("schedule state after /approve = %s (err=%v), want active", gotSch.State, err)
	}
}

func TestTelegramAdapterLongPollingRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var pollCount int
	var sentTexts []string

	mockTG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			pollCount++
			n := pollCount
			mu.Unlock()
			if n == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"result": []map[string]any{{
						"update_id": 100,
						"message": map[string]any{
							"message_id": 1,
							"from":       map[string]any{"id": 42},
							"chat":       map[string]any{"id": 42, "type": "private"},
							"text":       "ping from tg",
						},
					}},
				})
				return
			}
			// Block subsequent polls until context cancels.
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sentTexts = append(sentTexts, body["text"].(string))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 10}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	defer mockTG.Close()

	rt := &fakeChannelRuntime{}
	m, _ := bot.NewManager(bot.Options{Runtime: rt, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(context.Background())

	tg := NewTelegramAdapter(TelegramConfig{Token: "test-tok", APIBaseURL: mockTG.URL})
	gw := NewGateway(GatewayOptions{Manager: m})
	gw.Register(tg, AccessPolicy{AllowUsers: []string{"42"}, StreamEditInterval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = gw.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, s := range sentTexts {
			if strings.Contains(s, "echo:default:ping from tg") {
				found = true
			}
		}
		mu.Unlock()
		if found {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	gw.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(sentTexts) == 0 || !strings.Contains(sentTexts[0], "echo:default:ping from tg") {
		t.Fatalf("telegram sentTexts = %v, want echo reply", sentTexts)
	}
}

func TestFeishuAdapterParseAndOutbound(t *testing.T) {
	var mu sync.Mutex
	var gotMsgTypes []string
	mockFS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t-123", "expire": 7200})
		case strings.HasSuffix(r.URL.Path, "/im/v1/messages"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			gotMsgTypes = append(gotMsgTypes, body["msg_type"])
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"message_id": "om_1"}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockFS.Close()

	rawEvt := []byte(`{
		"header": {"event_type": "im.message.receive_v1"},
		"event": {
			"sender": {"sender_id": {"open_id": "ou_alice"}},
			"message": {"message_id": "om_in", "chat_id": "oc_chat1", "chat_type": "p2p", "message_type": "text", "content": "{\"text\":\"hello feishu\"}"}
		}
	}`)
	inMsg, ok := ParseFeishuEvent(rawEvt)
	if !ok || inMsg.UserID != "ou_alice" || inMsg.Text != "hello feishu" {
		t.Fatalf("ParseFeishuEvent = (%+v, %v)", inMsg, ok)
	}

	fs := NewFeishuAdapter(FeishuConfig{AppID: "cli_1", AppSecret: "sec_1", APIBaseURL: mockFS.URL})
	if id, err := fs.SendText(context.Background(), Destination{ChatID: "oc_chat1"}, "hi"); err != nil || id != "om_1" {
		t.Fatalf("SendText = (%q, %v)", id, err)
	}
	if _, err := fs.SendQuestion(context.Background(), Destination{ChatID: "oc_chat1"}, bot.Question{ID: "q1", Text: "pick?", Options: []string{"A", "B"}}); err != nil {
		t.Fatalf("SendQuestion: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotMsgTypes) != 2 || gotMsgTypes[0] != "text" || gotMsgTypes[1] != "interactive" {
		t.Fatalf("feishu msg_types = %v, want [text interactive]", gotMsgTypes)
	}
}
