package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/bot/channel"
	"github.com/millken/deepai/pkg/commands"
	"github.com/millken/deepai/pkg/schedule"
	"gopkg.in/yaml.v3"
)

type channelsFileConfig struct {
	Channels struct {
		Telegram telegramChannelConfig `yaml:"telegram"`
		Feishu   feishuChannelConfig   `yaml:"feishu"`
	} `yaml:"channels"`
}

type telegramChannelConfig struct {
	Enabled            bool     `yaml:"enabled"`
	BotTokenEnv        string   `yaml:"bot_token_env"`
	BotName            string   `yaml:"bot_name"`
	DefaultBotID       string   `yaml:"default_bot_id"`
	AllowedBots        []string `yaml:"allowed_bots"`
	AllowUsers         []string `yaml:"allow_users"`
	AllowChats         []string `yaml:"allow_chats"`
	AllowSelfTrain     bool     `yaml:"allow_self_train"`
	StreamEditInterval string   `yaml:"stream_edit_interval"`
}

type feishuChannelConfig struct {
	Enabled            bool     `yaml:"enabled"`
	AppIDEnv           string   `yaml:"app_id_env"`
	AppSecretEnv       string   `yaml:"app_secret_env"`
	BotOpenID          string   `yaml:"bot_open_id"`
	DefaultBotID       string   `yaml:"default_bot_id"`
	AllowedBots        []string `yaml:"allowed_bots"`
	AllowUsers         []string `yaml:"allow_users"`
	AllowChats         []string `yaml:"allow_chats"`
	AllowSelfTrain     bool     `yaml:"allow_self_train"`
	StreamEditInterval string   `yaml:"stream_edit_interval"`
}

func loadChannelsConfig(path string) (channelsFileConfig, error) {
	var cfg channelsFileConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func startChannelGateway(ctx context.Context, mgr *bot.Manager, sched *schedule.Service, db *sql.DB, audit *bot.AuditLogger) (*channel.Gateway, error) {
	cfg, err := loadChannelsConfig(commands.ConfigFile())
	if err != nil {
		slog.Warn("channels: parse config.yaml failed", "err", err)
		return nil, nil
	}
	if !cfg.Channels.Telegram.Enabled && !cfg.Channels.Feishu.Enabled {
		return nil, nil
	}

	var bindings channel.BindingStore
	if db != nil {
		b, err := channel.NewSQLiteBindingStore(db)
		if err != nil {
			return nil, fmt.Errorf("init channel bindings: %w", err)
		}
		bindings = b
	} else {
		bindings = channel.NewMemoryBindingStore()
	}

	gw := channel.NewGateway(channel.GatewayOptions{
		Manager:   mgr,
		Schedules: sched,
		Bindings:  bindings,
		Audit:     audit,
	})
	registered := 0

	if tg := cfg.Channels.Telegram; tg.Enabled {
		envName := strings.TrimSpace(tg.BotTokenEnv)
		if envName == "" {
			envName = "TELEGRAM_BOT_TOKEN"
		}
		tok := strings.TrimSpace(os.Getenv(envName))
		if tok == "" {
			slog.Warn("channels: telegram enabled but token env is empty", "env", envName)
		} else {
			interval, _ := time.ParseDuration(strings.TrimSpace(tg.StreamEditInterval))
			gw.Register(channel.NewTelegramAdapter(channel.TelegramConfig{
				Token:   tok,
				BotName: tg.BotName,
			}), channel.AccessPolicy{
				DefaultBotID:       tg.DefaultBotID,
				AllowedBots:        tg.AllowedBots,
				AllowUsers:         tg.AllowUsers,
				AllowChats:         tg.AllowChats,
				AllowSelfTrain:     tg.AllowSelfTrain,
				StreamEditInterval: interval,
			})
			registered++
		}
	}

	if fs := cfg.Channels.Feishu; fs.Enabled {
		idEnv := strings.TrimSpace(fs.AppIDEnv)
		if idEnv == "" {
			idEnv = "FEISHU_APP_ID"
		}
		secEnv := strings.TrimSpace(fs.AppSecretEnv)
		if secEnv == "" {
			secEnv = "FEISHU_APP_SECRET"
		}
		appID := strings.TrimSpace(os.Getenv(idEnv))
		appSecret := strings.TrimSpace(os.Getenv(secEnv))
		if appID == "" || appSecret == "" {
			slog.Warn("channels: feishu enabled but app credentials env is empty", "app_id_env", idEnv, "app_secret_env", secEnv)
		} else {
			interval, _ := time.ParseDuration(strings.TrimSpace(fs.StreamEditInterval))
			gw.Register(channel.NewFeishuAdapter(channel.FeishuConfig{
				AppID:     appID,
				AppSecret: appSecret,
				BotOpenID: fs.BotOpenID,
			}), channel.AccessPolicy{
				DefaultBotID:       fs.DefaultBotID,
				AllowedBots:        fs.AllowedBots,
				AllowUsers:         fs.AllowUsers,
				AllowChats:         fs.AllowChats,
				AllowSelfTrain:     fs.AllowSelfTrain,
				StreamEditInterval: interval,
			})
			registered++
		}
	}

	if registered == 0 {
		return nil, nil
	}
	if err := gw.Start(ctx); err != nil {
		return nil, err
	}
	slog.Info("deepai-bot IM channel gateway started", "adapters", registered)
	return gw, nil
}

func openBotTokenStore() (*bot.SQLiteTokenStore, func(), error) {
	if err := commands.EnsureHome(); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return nil, nil, err
	}
	dsn := botDB() + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	store, err := bot.NewSQLiteTokenStore(db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return store, func() { _ = db.Close() }, nil
}
