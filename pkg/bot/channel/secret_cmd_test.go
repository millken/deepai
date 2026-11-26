package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/bot"
)

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
	ad := &mockAdapter{name: "telegram"}
	gw.Register(ad, AccessPolicy{
		AllowUsers: []string{"admin_u"},
		AllowChats: []string{"grp_1"},
	})

	// Group chat /secret must be rejected without storing the credential.
	gw.HandleInbound(ctx, InboundMessage{
		Channel: "telegram", UserID: "admin_u", ChatID: "grp_1", IsGroup: true, Mentioned: true,
		Text: "/secret set NOTION_KEY grp_val_123",
	})
	ad.waitForSent(t, "仅限私聊")
	if list, _ := m.ListSecrets(ctx, "default"); len(list) != 0 {
		t.Fatalf("group /secret unexpectedly stored secret: %+v", list)
	}

	// DM /secret set stores sealed secret without creating a chat message containing the value.
	const rawVal = "ntn_dm_private_value_999"
	gw.HandleInbound(ctx, InboundMessage{
		Channel: "telegram", UserID: "admin_u", ChatID: "dm_admin",
		Text: "/secret set NOTION_KEY " + rawVal,
	})
	ad.waitForSent(t, "{{secret:NOTION_KEY}}")

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
