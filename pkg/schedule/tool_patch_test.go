package schedule

import (
	"context"
	"testing"
	"time"
)

// PR #43 round-3 L1: an update's naive end_at (no offset) must be read in
// the TASK's timezone — patch timezone first, then the schedule's own
// Timezone, and only then the global config default (design §3.3: the
// timezone of record is the Schedule's timezone field).
func TestUpdateEndAtTimezonePriority(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, newFakeClock(testNow), func(c *Config) {
		c.Timezone = "UTC" // global default deliberately differs from the task
		c.Confirm = "auto"
	})
	sch, err := svc.Create(ctx, CreateInput{
		Name: "日报", Prompt: "总结",
		Trigger:  Trigger{Type: TriggerOnce, In: "1h"},
		Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}

	est := time.FixedZone("EST", -5*3600)
	cases := []struct {
		name    string
		args    map[string]any
		wantUTC time.Time
	}{
		{
			name:    "inherits schedule timezone",
			args:    map[string]any{"action": "update", "id": sch.ID, "end_at": "2026-11-20T22:00"},
			wantUTC: time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC), // 22:00 EST
		},
		{
			name: "patch timezone wins over schedule timezone",
			args: map[string]any{
				"action": "update", "id": sch.ID,
				"timezone": "Asia/Tokyo", "end_at": "2026-11-20T22:00",
			},
			wantUTC: time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC), // 22:00 JST
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runToolAction(ctx, svc, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := res["schedule"].(Schedule)
			if !ok || got.Options.EndAt == nil {
				t.Fatalf("update result = %#v", res["schedule"])
			}
			if !got.Options.EndAt.Equal(tc.wantUTC) {
				t.Fatalf("EndAt = %s, want %s (interpreted in the wrong timezone)", got.Options.EndAt.UTC(), tc.wantUTC.UTC())
			}
			_ = est
		})
	}

	// No task timezone anywhere (global-config-only task): global default
	// still applies.
	schUTC, err := svc.Create(ctx, CreateInput{
		Name: "UTC 任务", Prompt: "总结",
		Trigger:  Trigger{Type: TriggerOnce, In: "1h"},
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := runToolAction(ctx, svc, map[string]any{
		"action": "update", "id": schUTC.ID, "end_at": "2026-11-20T22:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := res["schedule"].(Schedule)
	if got.Options.EndAt == nil || !got.Options.EndAt.Equal(time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("global-fallback EndAt = %v, want 2026-11-20T22:00Z", got.Options.EndAt)
	}
}
