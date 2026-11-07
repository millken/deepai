package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/commands"
	"github.com/millken/deepai/pkg/schedule"
	"github.com/millken/deepai/pkg/tools"
)

func TestScheduleConfigDefaultsAndOverrides(t *testing.T) {
	def := scheduleConfig(commands.Config{})
	if !def.Enabled || def.Confirm != "ask" || def.MinInterval != 5*time.Minute ||
		def.MaxConcurrentRuns != 2 || def.MaxSchedules != 50 || def.DefaultTimeout != 30*time.Minute {
		t.Fatalf("defaults = %+v", def)
	}

	off := false
	cfg := commands.Config{Schedule: commands.ScheduleYAML{
		Enabled:              &off,
		Timezone:             "Asia/Taipei",
		Confirm:              "auto",
		MinInterval:          "1m",
		ConfirmTimeout:       "2m",
		DefaultTimeout:       "45m",
		MaxSchedules:         7,
		MaxConcurrentRuns:    3,
		AllowNested:          true,
		DefaultAnswerTimeout: "15m",
	}}
	got := scheduleConfig(cfg)
	if got.Enabled || got.Timezone != "Asia/Taipei" || got.Confirm != "auto" ||
		got.MinInterval != time.Minute || got.ConfirmTimeout != 2*time.Minute ||
		got.DefaultTimeout != 45*time.Minute || got.MaxSchedules != 7 ||
		got.MaxConcurrentRuns != 3 || !got.AllowNested || got.DefaultAnswerTimeout != 15*time.Minute {
		t.Fatalf("overrides = %+v", got)
	}

	bad := scheduleConfig(commands.Config{Schedule: commands.ScheduleYAML{MinInterval: "not-a-duration"}})
	if bad.MinInterval != 5*time.Minute {
		t.Fatalf("invalid duration should fall back, got %s", bad.MinInterval)
	}
}

func TestNewScheduleServiceRegistersTool(t *testing.T) {
	reg := tools.NewRegistry()
	svc, store, cfg, err := newScheduleService(reg, commands.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if svc == nil || store == nil || !cfg.Enabled {
		t.Fatalf("service = %v, store = %v, cfg = %+v", svc, store, cfg)
	}
	if reg.Get("schedule") == nil {
		t.Fatal("schedule tool was not registered in the bot's tool registry")
	}
	if _, ok := store.(schedule.LeaseStore); !ok {
		t.Fatal("memory store must implement LeaseStore")
	}
	if svc.Mode() != "active" {
		t.Fatalf("mode = %s", svc.Mode())
	}

	off := false
	disabledReg := tools.NewRegistry()
	svc2, store2, _, err := newScheduleService(disabledReg, commands.Config{
		Schedule: commands.ScheduleYAML{Enabled: &off},
	}, nil)
	if err != nil || svc2 != nil || store2 != nil || disabledReg.Get("schedule") != nil {
		t.Fatalf("disabled = %v, %v, %v, %v", svc2, store2, err, disabledReg.Get("schedule"))
	}
}

func TestNewScheduleServiceSQLiteStore(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := tools.NewRegistry()
	svc, store, _, err := newScheduleService(reg, commands.Config{}, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*schedule.SQLiteStore); !ok {
		t.Fatalf("store type = %T", store)
	}
	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name: "持久化", Prompt: "检查", Timezone: "UTC",
		Trigger: schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), sch.ID)
	if err != nil || got.Name != "持久化" {
		t.Fatalf("sqlite schedule = %+v, %v", got, err)
	}
}

func TestCurrentTimeNote(t *testing.T) {
	note := currentTimeNote(schedule.DefaultConfig())
	if !strings.Contains(note, "当前时间：") {
		t.Fatalf("note = %q", note)
	}
}
