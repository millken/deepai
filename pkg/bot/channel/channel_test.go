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

type chunkingRuntime struct {
	fakeChannelRuntime
}

func (r *chunkingRuntime) NewConversation(string) (bot.Conversation, error) {
	return &chunkingConv{}, nil
}

type chunkingConv struct{}

func (c *chunkingConv) Close() error { return nil }
func (c *chunkingConv) RunTurn(_ context.Context, in bot.TurnInput) (*bot.TurnOutput, error) {
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		seg := strings.Repeat(string(rune('A'+i)), 10)
		sb.WriteString(seg)
		in.Emit(bot.Event{Type: bot.EventTextDelta, Text: seg})
		time.Sleep(15 * time.Millisecond)
	}
	full := sb.String()
	msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
		ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: full,
	})
	return &bot.TurnOutput{Messages: msgs, FinalOutput: full, LLMTurns: 1}, nil
}

func TestGatewayLongChunkedStreamNoDuplicates(t *testing.T) {
	m, _ := bot.NewManager(bot.Options{Runtime: &chunkingRuntime{}, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(context.Background())

	gw := NewGateway(GatewayOptions{Manager: m})
	ad := &mockAdapter{name: "telegram"}
	gw.Register(ad, AccessPolicy{
		AllowUsers:         []string{"u1"},
		StreamEditInterval: 10 * time.Millisecond,
		MaxMessageRunes:    20,
	})

	gw.HandleInbound(context.Background(), InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "long"})
	gw.Wait()

	ad.mu.Lock()
	defer ad.mu.Unlock()
	if len(ad.sent) != 5 {
		t.Fatalf("100-rune reply with MaxMessageRunes=20 sent %d messages, want 5: %v", len(ad.sent), ad.sent)
	}
}

func TestGatewaySteerOnIdleModelValidationAndAllowChatsRestriction(t *testing.T) {
	ctx := context.Background()
	rt := &fakeChannelRuntime{}
	m, _ := bot.NewManager(bot.Options{Runtime: rt, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(ctx)
	_, _ = m.CreateBot(ctx, bot.CreateBotRequest{ID: "secret", Name: "Secret"})

	svc, _ := schedule.NewService(schedule.NewMemoryStore(), schedule.DefaultConfig())
	bot.AttachSchedule(m, svc)
	secSch, _ := svc.Create(ctx, schedule.CreateInput{
		BotID:   "secret",
		Name:    "sec-job",
		Prompt:  "p",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		State:   schedule.StatePendingApproval,
	})

	gw := NewGateway(GatewayOptions{Manager: m, Schedules: svc})
	ad := &mockAdapter{name: "telegram"}
	gw.Register(ad, AccessPolicy{
		DefaultBotID:       "default",
		AllowUsers:         []string{"admin_u"},
		AllowChats:         []string{"grp_public"},
		StreamEditInterval: 10 * time.Millisecond,
	})

	// 61-M1: /steer on idle session must attach forwarder and deliver reply.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "admin_u", ChatID: "dm_1", Text: "/steer hello idle"})
	ad.waitForSent(t, "echo:default:hello idle")
	gw.Wait()

	// 61-M7: /model with unknown alias is rejected.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "admin_u", ChatID: "dm_1", Text: "/model nonexistent"})
	ad.waitForSent(t, "未知模型")

	// 61-M2: group member admitted only via AllowChats cannot switch to secret bot or approve secret schedule.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "random_member", ChatID: "grp_public", IsGroup: true, Mentioned: true, Text: "/bot secret"})
	ad.waitForSent(t, "不允许使用该 Bot")
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "random_member", ChatID: "grp_public", IsGroup: true, Mentioned: true, Text: "/approve " + secSch.ID})
	ad.waitForSent(t, "无权批准")
	gotSch, _ := svc.Get(ctx, secSch.ID)
	if gotSch.State != schedule.StatePendingApproval {
		t.Fatalf("group member approved secret schedule: state=%s", gotSch.State)
	}
}

func TestTelegramLazyDownloadGroupMentionAndCallbackIndex(t *testing.T) {
	var mu sync.Mutex
	var getFileCalls int
	var sentQuestionPayload map[string]any

	mockTG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			mu.Lock()
			getFileCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "docs/a.txt"}})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_ = json.NewDecoder(r.Body).Decode(&sentQuestionPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 11}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	defer mockTG.Close()

	tg := NewTelegramAdapter(TelegramConfig{Token: "tok", BotName: "my_bot", APIBaseURL: mockTG.URL})
	m, _ := bot.NewManager(bot.Options{Runtime: &fakeChannelRuntime{}, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(context.Background())

	gw := NewGateway(GatewayOptions{Manager: m})
	gw.Register(tg, AccessPolicy{AllowUsers: []string{"allowed_u"}})

	// 61-M6: unauthorized user sending a document must NOT trigger getFile.
	inMsg, ok := tg.toInbound(context.Background(), tgUpdate{
		UpdateID: 1,
		Message: &tgMessage{
			MessageID: 1,
			From:      &tgUser{ID: 9999},
			Chat:      tgChat{ID: 9999, Type: "private"},
			Document:  &tgDocument{FileID: "evil_file", FileName: "big.bin"},
		},
	})
	if !ok {
		t.Fatal("toInbound returned false")
	}
	gw.HandleInbound(context.Background(), inMsg)
	mu.Lock()
	calls := getFileCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("getFile called %d times for unauthorized user, want 0", calls)
	}

	// 61-M3: group command for another bot is NOT mentioned; /status and @my_bot ARE mentioned.
	otherCmd, _ := tg.toInbound(context.Background(), tgUpdate{
		Message: &tgMessage{Chat: tgChat{ID: -100, Type: "group"}, Text: "/help@other_bot"},
	})
	if otherCmd.Mentioned {
		t.Fatalf("/help@other_bot should not be Mentioned")
	}
	otherSlash, _ := tg.toInbound(context.Background(), tgUpdate{
		Message: &tgMessage{Chat: tgChat{ID: -100, Type: "group"}, Text: "/unknown_cmd"},
	})
	if otherSlash.Mentioned {
		t.Fatalf("/unknown_cmd in group should not be Mentioned")
	}
	ownMention, _ := tg.toInbound(context.Background(), tgUpdate{
		Message: &tgMessage{Chat: tgChat{ID: -100, Type: "group"}, Text: "hi @my_bot"},
	})
	if !ownMention.Mentioned {
		t.Fatalf("@my_bot in group should be Mentioned")
	}

	// 61-L1: long CJK option encodes index in callback_data without byte truncation.
	longOpt := strings.Repeat("非常长的中文选项说明", 5)
	if _, err := tg.SendQuestion(context.Background(), Destination{ChatID: "42"}, bot.Question{
		ID: "q_123456", Text: "选哪个？", Options: []string{longOpt},
	}); err != nil {
		t.Fatal(err)
	}
	rm := sentQuestionPayload["reply_markup"].(map[string]any)
	rows := rm["inline_keyboard"].([]any)
	btn := rows[0].([]any)[0].(map[string]any)
	if cb := btn["callback_data"].(string); cb != "ans:q_123456:#0" {
		t.Fatalf("callback_data = %q, want ans:q_123456:#0", cb)
	}
}

func TestFeishuCardActionTriggerPutEditAndInboundImage(t *testing.T) {
	var mu sync.Mutex
	var editMethod string
	var resourceCalls int

	mockFS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t-123", "expire": 7200})
		case strings.Contains(r.URL.Path, "/resources/"):
			mu.Lock()
			resourceCalls++
			mu.Unlock()
			_, _ = w.Write([]byte("fake-image-bytes"))
		case strings.Contains(r.URL.Path, "/im/v1/messages/om_edit"):
			mu.Lock()
			editMethod = r.Method
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockFS.Close()

	// 61-H5: official card.action.trigger uses event.operator.open_id & event.context.open_chat_id.
	cardEvt := []byte(`{
		"header": {"event_type": "card.action.trigger"},
		"event": {
			"operator": {"open_id": "ou_clicker"},
			"context": {"open_chat_id": "oc_chat99", "open_message_id": "om_card1"},
			"action": {"value": {"callback_data": "ans:q1:yes"}}
		}
	}`)
	inCard, ok := ParseFeishuEvent(cardEvt)
	if !ok || inCard.UserID != "ou_clicker" || inCard.ChatID != "oc_chat99" || inCard.CallbackData != "ans:q1:yes" {
		t.Fatalf("card.action.trigger parsed = (%+v, %v)", inCard, ok)
	}

	fs := NewFeishuAdapter(FeishuConfig{AppID: "cli_1", AppSecret: "sec_1", BotOpenID: "ou_mybot", APIBaseURL: mockFS.URL})

	// 61-H6: EditText uses PUT (not PATCH) and respects feishuMaxMessageEdits cap.
	if err := fs.EditText(context.Background(), Destination{ChatID: "oc_chat99"}, "om_edit", "v1"); err != nil {
		t.Fatalf("EditText: %v", err)
	}
	mu.Lock()
	gotMethod := editMethod
	mu.Unlock()
	if gotMethod != http.MethodPut {
		t.Fatalf("EditText HTTP method = %q, want PUT", gotMethod)
	}

	// 61-M3: group mention checks BotOpenID when configured.
	grpOtherMention := []byte(`{
		"header": {"event_type": "im.message.receive_v1"},
		"event": {
			"sender": {"sender_id": {"open_id": "ou_u1"}},
			"message": {
				"message_id": "om_2", "chat_id": "oc_grp", "chat_type": "group", "message_type": "text",
				"content": "{\"text\":\"@_user_1 hi\"}",
				"mentions": [{"key": "@_user_1", "id": {"open_id": "ou_someone_else"}}]
			}
		}
	}`)
	if msg, ok := fs.parseEvent(grpOtherMention); !ok || msg.Mentioned {
		t.Fatalf("group mention of another user should not set Mentioned: (%+v, %v)", msg, ok)
	}

	// 61-M8: inbound image message parses with lazy Download (not called until invoked).
	imgEvt := []byte(`{
		"header": {"event_type": "im.message.receive_v1"},
		"event": {
			"sender": {"sender_id": {"open_id": "ou_u1"}},
			"message": {
				"message_id": "om_img1", "chat_id": "oc_dm", "chat_type": "p2p", "message_type": "image",
				"content": "{\"image_key\":\"img_v2_abc\"}"
			}
		}
	}`)
	imgMsg, ok := fs.parseEvent(imgEvt)
	if !ok || len(imgMsg.Attachments) != 1 || imgMsg.Attachments[0].Download == nil {
		t.Fatalf("image event parsed = (%+v, %v)", imgMsg, ok)
	}
	mu.Lock()
	before := resourceCalls
	mu.Unlock()
	if before != 0 {
		t.Fatalf("resource downloaded eagerly before auth")
	}
	data, err := imgMsg.Attachments[0].Download(context.Background())
	if err != nil || string(data) != "fake-image-bytes" {
		t.Fatalf("lazy download = (%q, %v)", string(data), err)
	}

	// 61-L5: without BotOpenID, group mentions must NOT blindly trigger.
	if msg, ok := ParseFeishuEvent(grpOtherMention); !ok || msg.Mentioned {
		t.Fatalf("ParseFeishuEvent without BotOpenID should not set Mentioned on group mention: (%+v, %v)", msg, ok)
	}
}

func TestTelegramRedactsTokenInTransportErrors(t *testing.T) {
	// Closed server guarantees a transport-level *url.Error carrying the URL.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	const secretTok = "123456:SUPER_SECRET_TG_TOKEN"
	tg := NewTelegramAdapter(TelegramConfig{Token: secretTok, APIBaseURL: closedURL})

	if _, err := tg.SendText(context.Background(), Destination{ChatID: "1"}, "hi"); err == nil {
		t.Fatal("expected transport error")
	} else if strings.Contains(err.Error(), secretTok) {
		t.Fatalf("SendText error leaked telegram token: %v", err)
	}
	if _, err := tg.getUpdates(context.Background(), 0, 1); err == nil {
		t.Fatal("expected transport error")
	} else if strings.Contains(err.Error(), secretTok) {
		t.Fatalf("getUpdates error leaked telegram token: %v", err)
	}
}

type toolEdgeRuntime struct {
	fakeChannelRuntime
}

func (r *toolEdgeRuntime) NewConversation(string) (bot.Conversation, error) {
	return &toolEdgeConv{}, nil
}

type toolEdgeConv struct{}

func (c *toolEdgeConv) Close() error { return nil }
func (c *toolEdgeConv) RunTurn(_ context.Context, in bot.TurnInput) (*bot.TurnOutput, error) {
	last := in.Messages[len(in.Messages)-1].Content
	if last == "fail_me" {
		time.Sleep(40 * time.Millisecond)
		return &bot.TurnOutput{Messages: in.Messages}, context.DeadlineExceeded
	}
	text := strings.Repeat("Z", 18) // 18 runes with maxRunes=20
	in.Emit(bot.Event{Type: bot.EventTextDelta, Text: text})
	in.Emit(bot.Event{Type: bot.EventToolCallStart, Tool: &agent.ToolCallEvent{Name: "bash"}})
	time.Sleep(35 * time.Millisecond)
	in.Emit(bot.Event{Type: bot.EventToolCallEnd, Tool: &agent.ToolCallEvent{Name: "bash"}})
	msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
		ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: text,
	})
	return &bot.TurnOutput{Messages: msgs, FinalOutput: text, LLMTurns: 1}, nil
}

func TestGatewayToolIndicatorNoOrphanChunkAndFailedQueueExit(t *testing.T) {
	m, _ := bot.NewManager(bot.Options{Runtime: &toolEdgeRuntime{}, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(context.Background())

	gw := NewGateway(GatewayOptions{Manager: m})
	ad := &mockAdapter{name: "telegram"}
	gw.Register(ad, AccessPolicy{
		AllowUsers:         []string{"u1"},
		StreamEditInterval: 10 * time.Millisecond,
		MaxMessageRunes:    20,
	})

	// 61-L6: 18-rune reply + active tool indicator must not create a 2nd orphan message.
	gw.HandleInbound(context.Background(), InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c1", Text: "run tool"})
	gw.Wait()
	ad.mu.Lock()
	sentCount := len(ad.sent)
	ad.mu.Unlock()
	if sentCount != 1 {
		t.Fatalf("expected 1 message (no orphan tool chunk), got %d: %v", sentCount, ad.sent)
	}

	// 61-L7: failed/timed-out turn with preserved queue must let stream forwarder exit cleanly.
	gw.HandleInbound(context.Background(), InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c2", Text: "fail_me"})
	time.Sleep(10 * time.Millisecond)
	gw.HandleInbound(context.Background(), InboundMessage{Channel: "telegram", UserID: "u1", ChatID: "c2", Text: "queued behind failure"})

	done := make(chan struct{})
	go func() {
		gw.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream forwarder leaked after failed turn with preserved queue")
	}
}
