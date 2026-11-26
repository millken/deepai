package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/models"
)

type deletableMockAdapter struct {
	mockAdapter
	delMu   sync.Mutex
	deleted []string
}

func (a *deletableMockAdapter) DeleteMessage(_ context.Context, _ Destination, msgID string) error {
	a.delMu.Lock()
	a.deleted = append(a.deleted, msgID)
	a.delMu.Unlock()
	return nil
}

func TestGatewaySecretSlashCommandDMOnlyAndRedacted(t *testing.T) {
	ctx := context.Background()
	secrets := bot.NewMemorySecretStore()
	m, _ := bot.NewManager(bot.Options{
		Runtime:     &fakeChannelRuntime{},
		Store:       bot.NewMemoryStore(),
		Secrets:     secrets,
		IdleTimeout: -1,
	})
	defer m.Close(ctx)

	gw := NewGateway(GatewayOptions{Manager: m})
	ad := &deletableMockAdapter{mockAdapter: mockAdapter{name: "telegram"}}
	gw.Register(ad, AccessPolicy{
		AllowUsers: []string{"admin_u"},
		AllowChats: []string{"grp_1"},
	})

	// Group chat /secret must be rejected without storing the credential.
	gw.HandleInbound(ctx, InboundMessage{
		Channel: "telegram", UserID: "admin_u", ChatID: "grp_1", IsGroup: true, Mentioned: true,
		MessageID: "101", Text: "/secret set NOTION_KEY grp_val_123",
	})
	ad.waitForSent(t, "仅限私聊")
	if list, _ := m.ListSecrets(ctx, "default"); len(list) != 0 {
		t.Fatalf("group /secret unexpectedly stored secret: %+v", list)
	}

	// DM /secret set stores sealed secret, deletes source DM message when supported, and advises CLI usage.
	const rawVal = "ntn_dm_private_value_999"
	gw.HandleInbound(ctx, InboundMessage{
		Channel: "telegram", UserID: "admin_u", ChatID: "dm_admin",
		MessageID: "777", Text: "/secret set NOTION_KEY " + rawVal,
	})
	ad.waitForSent(t, "{{secret:NOTION_KEY}}")
	ad.waitForSent(t, "deepai-bot secret set")

	ad.delMu.Lock()
	deletedIDs := append([]string(nil), ad.deleted...)
	ad.delMu.Unlock()
	if len(deletedIDs) != 1 || deletedIDs[0] != "777" {
		t.Fatalf("expected DeleteMessage(777) on /secret set, got %v", deletedIDs)
	}

	ad.mu.Lock()
	for _, s := range ad.sent {
		if strings.Contains(s, rawVal) {
			ad.mu.Unlock()
			t.Fatalf("IM reply echoed raw secret value: %q", s)
		}
	}
	ad.mu.Unlock()

	gotVal, err := secrets.GetSecret(ctx, "default", "NOTION_KEY")
	if err != nil || gotVal != rawVal {
		t.Fatalf("stored secret = (%q, %v), want %q", gotVal, err, rawVal)
	}
	sessions, _ := m.ListSessionsForBot("default", 10)
	for _, s := range sessions {
		_, msgs, _ := m.GetSessionForBot("default", s.ID)
		for _, msg := range msgs {
			if strings.Contains(msg.Content, rawVal) {
				t.Fatalf("session transcript leaked /secret value: %+v", msg)
			}
		}
	}

	// /secret list and /secret delete work in DM.
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "admin_u", ChatID: "dm_admin", Text: "/secret list"})
	ad.waitForSent(t, "NOTION_KEY")
	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "admin_u", ChatID: "dm_admin", Text: "/secret delete NOTION_KEY"})
	ad.waitForSent(t, "已删除 Bot")
	if list, _ := m.ListSecrets(ctx, "default"); len(list) != 0 {
		t.Fatalf("secret still present after /secret delete: %+v", list)
	}
}

type captureSelfTrainRuntime struct {
	fakeChannelRuntime
	mu    sync.Mutex
	flags []bool
}

func (r *captureSelfTrainRuntime) NewConversation(string) (bot.Conversation, error) {
	return &captureSelfTrainConv{rt: r}, nil
}

type captureSelfTrainConv struct{ rt *captureSelfTrainRuntime }

func (c *captureSelfTrainConv) Close() error { return nil }

func (c *captureSelfTrainConv) RunTurn(_ context.Context, in bot.TurnInput) (*bot.TurnOutput, error) {
	c.rt.mu.Lock()
	c.rt.flags = append(c.rt.flags, in.DisableSelfTrain)
	c.rt.mu.Unlock()
	last := in.Messages[len(in.Messages)-1].Content
	reply := fmt.Sprintf("flag=%v:%s", in.DisableSelfTrain, last)
	in.Emit(bot.Event{Type: bot.EventTextDelta, Text: reply})
	msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
		ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: reply,
	})
	return &bot.TurnOutput{Messages: msgs, FinalOutput: reply, LLMTurns: 1}, nil
}

func TestChannelSelfTrainingPolicyDefaultOffAndDMOnly(t *testing.T) {
	ctx := context.Background()
	rt := &captureSelfTrainRuntime{}
	m, _ := bot.NewManager(bot.Options{Runtime: rt, Store: bot.NewMemoryStore(), IdleTimeout: -1})
	defer m.Close(ctx)

	gw := NewGateway(GatewayOptions{Manager: m})
	tgDefault := &mockAdapter{name: "telegram"}
	fsOptIn := &mockAdapter{name: "feishu"}

	// 1. Default policy (AllowSelfTrain=false): even AllowUsers DM disables self-training.
	gw.Register(tgDefault, AccessPolicy{
		AllowUsers:         []string{"u_admin"},
		StreamEditInterval: 10 * time.Millisecond,
	})
	// 2. Opt-in policy (AllowSelfTrain=true): allows private DM from AllowUsers, blocks group chat.
	gw.Register(fsOptIn, AccessPolicy{
		AllowUsers:         []string{"u_admin"},
		AllowChats:         []string{"grp_shared"},
		AllowSelfTrain:     true,
		StreamEditInterval: 10 * time.Millisecond,
	})

	gw.HandleInbound(ctx, InboundMessage{Channel: "telegram", UserID: "u_admin", ChatID: "dm_1", Text: "try train"})
	tgDefault.waitForSent(t, "flag=true:try train")
	gw.Wait()

	gw.HandleInbound(ctx, InboundMessage{Channel: "feishu", UserID: "u_admin", ChatID: "dm_2", Text: "dm train"})
	fsOptIn.waitForSent(t, "flag=false:dm train")
	gw.Wait()

	gw.HandleInbound(ctx, InboundMessage{Channel: "feishu", UserID: "u_admin", ChatID: "grp_shared", IsGroup: true, Mentioned: true, Text: "group train"})
	fsOptIn.waitForSent(t, "flag=true:group train")
	gw.Wait()
}

func TestTelegramAndFeishuDeleteMessage(t *testing.T) {
	var tgDeletedChat any
	var tgDeletedMsg any
	mockTG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deleteMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			tgDeletedChat = body["chat_id"]
			tgDeletedMsg = body["message_id"]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}))
	defer mockTG.Close()

	tg := NewTelegramAdapter(TelegramConfig{Token: "tok", APIBaseURL: mockTG.URL})
	if err := tg.DeleteMessage(context.Background(), Destination{ChatID: "42"}, "99"); err != nil {
		t.Fatalf("Telegram DeleteMessage: %v", err)
	}
	if tgDeletedChat != "42" || tgDeletedMsg != float64(99) {
		t.Fatalf("Telegram deleteMessage payload = chat:%v msg:%v", tgDeletedChat, tgDeletedMsg)
	}

	var fsDeleteMethod, fsDeletePath string
	var fsCardContent string
	mockFS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t-1", "expire": 7200})
		case strings.HasSuffix(r.URL.Path, "/im/v1/messages/om_secret"):
			fsDeleteMethod = r.Method
			fsDeletePath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		case strings.HasSuffix(r.URL.Path, "/im/v1/messages"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			fsCardContent = body["content"]
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"message_id": "om_q"}})
		}
	}))
	defer mockFS.Close()

	fs := NewFeishuAdapter(FeishuConfig{AppID: "cli", AppSecret: "sec", APIBaseURL: mockFS.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fs.DeleteMessage(ctx, Destination{ChatID: "oc_1"}, "om_secret"); err != nil {
		t.Fatalf("Feishu DeleteMessage: %v", err)
	}
	if fsDeleteMethod != http.MethodDelete || !strings.HasSuffix(fsDeletePath, "/im/v1/messages/om_secret") {
		t.Fatalf("Feishu DeleteMessage = %s %s", fsDeleteMethod, fsDeletePath)
	}
	if _, err := fs.SendQuestion(ctx, Destination{ChatID: "oc_1"}, bot.Question{ID: "q_free", Text: "free text?"}); err != nil {
		t.Fatalf("Feishu SendQuestion without options: %v", err)
	}
	if strings.Contains(fsCardContent, `"tag":"action"`) {
		t.Fatalf("expected free-text Feishu question card to omit empty action element: %s", fsCardContent)
	}
}
