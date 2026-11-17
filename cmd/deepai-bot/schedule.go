package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/millken/deepai/pkg/commands"
	"github.com/millken/deepai/pkg/schedule"
	"github.com/millken/deepai/pkg/tools"
)

// scheduleConfig translates the config.yaml `schedule:` section into the
// effective service config. Invalid durations are ignored with a warning so a
// typo in one field never stops the bot from starting.
func scheduleConfig(cfg commands.Config) schedule.Config {
	out := schedule.DefaultConfig()
	y := cfg.Schedule
	if y.Enabled != nil {
		out.Enabled = *y.Enabled
	}
	out.Timezone = y.Timezone
	if y.Confirm != "" {
		out.Confirm = y.Confirm
	}
	out.AllowNested = y.AllowNested
	out.ConfirmTimeout = scheduleDuration(y.ConfirmTimeout, out.ConfirmTimeout)
	out.MinInterval = scheduleDuration(y.MinInterval, out.MinInterval)
	out.DefaultTimeout = scheduleDuration(y.DefaultTimeout, out.DefaultTimeout)
	out.DefaultAnswerTimeout = scheduleDuration(y.DefaultAnswerTimeout, out.DefaultAnswerTimeout)
	if y.MaxSchedules > 0 {
		out.MaxSchedules = y.MaxSchedules
	}
	if y.MaxConcurrentRuns > 0 {
		out.MaxConcurrentRuns = y.MaxConcurrentRuns
	}
	if y.MaxRunsPerDay > 0 {
		out.MaxRunsPerDay = y.MaxRunsPerDay
	}
	if y.RunHistory > 0 {
		out.RunHistory = y.RunHistory
	}
	if y.DefaultMaxAutoAnswers > 0 {
		out.DefaultMaxAutoAnswers = y.DefaultMaxAutoAnswers
	}
	return out
}

// newScheduleService builds the Service on bot.db (or in memory under
// --ephemeral) and registers the single `schedule` tool. A disabled config
// returns (nil, nil, cfg, nil): no tool, no tables, no scheduler.
func newScheduleService(registry *tools.Registry, cfg commands.Config, db *sql.DB) (*schedule.Service, schedule.Store, schedule.Config, error) {
	c := scheduleConfig(cfg)
	if !c.Enabled {
		return nil, nil, c, nil
	}
	var store schedule.Store
	if db != nil {
		store = schedule.NewSQLiteStore(db)
	} else {
		store = schedule.NewMemoryStore()
	}
	svc, err := schedule.NewService(store, c)
	if err != nil {
		return nil, nil, c, fmt.Errorf("init schedule service: %w", err)
	}
	if err := registry.Register(schedule.Tool(svc)); err != nil {
		return nil, nil, c, fmt.Errorf("register schedule tool: %w", err)
	}
	return svc, store, c, nil
}

func scheduleDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		slog.Warn("invalid schedule duration; using default", "value", s, "default", def.String())
		return def
	}
	return d
}

// currentTimeNote is appended to each turn's system prompt so the model can
// resolve relative expressions such as "今晚 10 点" (design §8.4).
func currentTimeNote(cfg schedule.Config) string {
	tz := cfg.ConfiguredTimezone()
	loc, err := schedule.Location(tz)
	if err != nil {
		loc = time.Local
		tz = "Local"
	}
	now := time.Now().In(loc)
	return fmt.Sprintf("当前时间：%s（%s，UTC%s）",
		now.Format("2006-01-02 15:04"), tz, now.Format("-07:00"))
}
