package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadChannelsConfigYAML(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := `
channels:
  telegram:
    enabled: true
    bot_token_env: TELEGRAM_BOT_TOKEN
    default_bot_id: assistant
    allow_users: ["123456789"]
    allow_chats: ["-100999"]
    stream_edit_interval: 800ms
  feishu:
    enabled: false
    app_id_env: FEISHU_APP_ID
    app_secret_env: FEISHU_APP_SECRET
    default_bot_id: default
    allow_users: ["ou_xxxx"]
`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadChannelsConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadChannelsConfig: %v", err)
	}
	tg := cfg.Channels.Telegram
	if !tg.Enabled || tg.BotTokenEnv != "TELEGRAM_BOT_TOKEN" || tg.DefaultBotID != "assistant" ||
		!slices.Equal(tg.AllowUsers, []string{"123456789"}) || tg.StreamEditInterval != "800ms" {
		t.Fatalf("unexpected telegram config: %+v", tg)
	}
	fs := cfg.Channels.Feishu
	if fs.Enabled || fs.AppIDEnv != "FEISHU_APP_ID" || !slices.Equal(fs.AllowUsers, []string{"ou_xxxx"}) {
		t.Fatalf("unexpected feishu config: %+v", fs)
	}
}
