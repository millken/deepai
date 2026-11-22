package schedule

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCronParseAndNext(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("America/New_York tzdata not installed")
	}
	taipei, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Skip("Asia/Taipei tzdata not installed")
	}

	cases := []struct {
		name  string
		expr  string
		loc   *time.Location
		after time.Time
		want  string // RFC3339 in loc
	}{
		{
			name:  "daily 22:00 Taipei",
			expr:  "0 22 * * *",
			loc:   taipei,
			after: time.Date(2026, 11, 7, 12, 45, 0, 0, taipei),
			want:  "2026-11-07T22:00:00+08:00",
		},
		{
			name:  "macro @weekly is Monday 00:00",
			expr:  "@weekly",
			loc:   time.UTC,
			after: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), // Saturday
			want:  "2026-11-09T00:00:00Z",                         // Monday
		},
		{
			name:  "macro @monthly",
			expr:  "@monthly",
			loc:   time.UTC,
			after: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			want:  "2026-12-01T00:00:00Z",
		},
		{
			name:  "macro @yearly",
			expr:  "@yearly",
			loc:   time.UTC,
			after: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			want:  "2027-01-01T00:00:00Z",
		},
		{
			name:  "macro @hourly",
			expr:  "@hourly",
			loc:   time.UTC,
			after: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
			want:  "2026-11-07T13:00:00Z",
		},
		{
			name:  "step and range",
			expr:  "*/15 9-17 * * MON-FRI",
			loc:   time.UTC,
			after: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC), // Saturday
			want:  "2026-11-09T09:00:00Z",                         // Monday 09:00
		},
		{
			name:  "month names and Sunday as 7",
			expr:  "30 8 * NOV 7",
			loc:   time.UTC,
			after: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), // Saturday
			want:  "2026-11-08T08:30:00Z",                         // Sunday
		},
		{
			name:  "last day of month L in non-leap February",
			expr:  "0 18 L 2 *",
			loc:   time.UTC,
			after: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			want:  "2026-02-28T18:00:00Z",
		},
		{
			name:  "last day of month L in leap February 2028",
			expr:  "0 18 L 2 *",
			loc:   time.UTC,
			after: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
			want:  "2028-02-29T18:00:00Z",
		},
		{
			name:  "Feb 29 leap year jump",
			expr:  "0 9 29 2 *",
			loc:   time.UTC,
			after: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			want:  "2028-02-29T09:00:00Z",
		},
		{
			name:  "Vixie OR semantics when both DOM and DOW set",
			expr:  "0 9 1 * MON",
			loc:   time.UTC,
			after: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), // Sunday Nov 1
			want:  "2026-11-02T09:00:00Z",                        // Monday Nov 2
		},
		{
			name:  "DST spring forward gap (America/New_York 2026-03-08 02:30 does not exist -> 03:00 EDT)",
			expr:  "30 2 * * *",
			loc:   ny,
			after: time.Date(2026, 3, 8, 1, 0, 0, 0, ny),
			want:  "2026-03-08T03:00:00-04:00",
		},
		{
			name:  "DST spring forward gap from last minute before jump (01:59:00 EST -> 03:00 EDT)",
			expr:  "30 2 * * *",
			loc:   ny,
			after: time.Date(2026, 3, 8, 1, 59, 0, 0, ny),
			want:  "2026-03-08T03:00:00-04:00",
		},
		{
			name:  "DST spring forward gap from 01:59:30 EST -> 03:00 EDT",
			expr:  "30 2 * * *",
			loc:   ny,
			after: time.Date(2026, 3, 8, 1, 59, 30, 0, ny),
			want:  "2026-03-08T03:00:00-04:00",
		},
		{
			name:  "DST fall back overlap (America/New_York 2026-11-01 01:30 fires once on first occurrence EDT)",
			expr:  "30 1 * * *",
			loc:   ny,
			after: time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
			want:  "2026-11-01T01:30:00-04:00",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) err = %v", tc.expr, err)
			}
			got, ok := spec.Next(tc.after, tc.loc)
			if !ok {
				t.Fatalf("Next(%q) returned ok=false", tc.expr)
			}
			if gotStr := got.In(tc.loc).Format(time.RFC3339); gotStr != tc.want {
				t.Fatalf("Next(%q) = %s, want %s", tc.expr, gotStr, tc.want)
			}
		})
	}

	// Verify DST fall-back does NOT fire a second time on the repeated 01:30 EST.
	t.Run("DST fall back does not double fire", func(t *testing.T) {
		spec, err := ParseCron("30 1 * * *")
		if err != nil {
			t.Fatal(err)
		}
		first, ok := spec.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, ny), ny)
		if !ok {
			t.Fatal("missing first occurrence")
		}
		second, ok := spec.Next(first, ny)
		if !ok {
			t.Fatal("missing second occurrence")
		}
		if got := second.In(ny).Format(time.RFC3339); got != "2026-11-02T01:30:00-05:00" {
			t.Fatalf("after first 01:30 EDT, next = %s, want 2026-11-02T01:30:00-05:00", got)
		}
	})
}

func TestCronRejectsUnsupportedAndMinInterval(t *testing.T) {
	bad := []string{
		"",
		"* * * *",
		"0 0 * * * *",   // 6 fields (seconds)
		"0 0 * * * * *", // 7 fields (year)
		"0 9 ? * MON",   // ?
		"0 9 15W * *",   // W
		"0 9 * * MON#1", // #
		"@reboot",
		"@every 5m",
		"@unknown",
		"60 * * * *",
		"0 24 * * *",
		"0 0 32 * *",
		"0 0 1 13 *",
		"0 0 * * 8",
		"10-5 * * * *",
		"*/0 * * * *",
		"5/10 * * * *",
	}
	for _, expr := range bad {
		if _, err := ParseCron(expr); ErrorCode(err) != CodeInvalidCron {
			t.Errorf("ParseCron(%q) err = %v, want CodeInvalidCron", expr, err)
		}
	}

	// Verify W in non-DOM fields (e.g. hour/dow) gives the same unsupported syntax error.
	for _, expr := range []string{"0 9W * * *", "0 9 * * 1W"} {
		_, err := ParseCron(expr)
		if ErrorCode(err) != CodeInvalidCron || !strings.Contains(err.Error(), "unsupported syntax") {
			t.Errorf("ParseCron(%q) err = %v, want unsupported syntax error", expr, err)
		}
	}
	// WED in DOW remains valid.
	if _, err := ParseCron("0 9 * * WED"); err != nil {
		t.Fatalf("WED in DOW should be valid: %v", err)
	}

	// Impossible date (Feb 31) rejected on Normalize.
	if err := (&Trigger{Type: TriggerCron, Expr: "0 0 31 2 *"}).Normalize("UTC", testNow, 5*time.Minute, false); ErrorCode(err) != CodeInvalidCron {
		t.Fatalf("Feb 31 err = %v, want CodeInvalidCron", err)
	}
	// Every minute violates min_interval=5m.
	if err := (&Trigger{Type: TriggerCron, Expr: "* * * * *"}).Normalize("UTC", testNow, 5*time.Minute, false); ErrorCode(err) != CodeIntervalTooShort {
		t.Fatalf("* * * * * err = %v, want CodeIntervalTooShort", err)
	}
	// Every 5 minutes passes min_interval=5m.
	if err := (&Trigger{Type: TriggerCron, Expr: "*/5 * * * *"}).Normalize("UTC", testNow, 5*time.Minute, false); err != nil {
		t.Fatalf("*/5 * * * * err = %v", err)
	}
}

func TestCronHumanSummary(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"0 22 * * *", "每天 22:00（Asia/Taipei）"},
		{"0 9 * * MON", "每周一 09:00（Asia/Taipei）"},
		{"30 8 15 * *", "每月 15 日 08:30（Asia/Taipei）"},
		{"0 18 L * *", "每月最后一天 18:00（Asia/Taipei）"},
		{"@hourly", "每小时整点（Asia/Taipei）"},
	}
	for _, tc := range cases {
		tr := Trigger{Type: TriggerCron, Expr: tc.expr}
		if err := tr.Normalize("Asia/Taipei", testNow, time.Minute, false); err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		if got := tr.Summary("Asia/Taipei"); got != tc.want {
			t.Errorf("Summary(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

func TestMisfireRecoveryPolicies(t *testing.T) {
	ctx := context.Background()

	t.Run("run_once within grace creates coalesced pending misfire run", func(t *testing.T) {
		clock := newFakeClock(testNow)
		svc, _ := newTestService(t, clock, func(c *Config) { c.MinInterval = time.Minute })
		d := &fakeDispatcher{}
		svc.SetDispatcher(d)

		sch, err := svc.Create(ctx, CreateInput{
			Name: "每小时巡检", Prompt: "检查服务",
			Trigger:  Trigger{Type: TriggerInterval, Every: "1h"},
			Timezone: "UTC",
			Options:  Options{Misfire: MisfireRunOnce, MisfireGrace: "1h"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Advance 2h30m: slots at +1h and +2h were missed; last missed is +2h (30m ago <= 1h grace).
		clock.Advance(2*time.Hour + 30*time.Minute)
		if err := svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		runs, err := svc.Runs(ctx, sch.ID, 10)
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs = %+v, %v", runs, err)
		}
		if runs[0].Trigger != FireMisfire || runs[0].Status != RunPending || runs[0].Coalesced != 1 {
			t.Fatalf("misfire run = %+v, want trigger=misfire status=pending coalesced=1", runs[0])
		}
		if err := svc.TryStartPending(ctx); err != nil {
			t.Fatal(err)
		}
		if d.count() != 1 {
			t.Fatalf("dispatched = %d, want 1", d.count())
		}
	})

	t.Run("run_once beyond grace marks missed", func(t *testing.T) {
		clock := newFakeClock(testNow)
		svc, _ := newTestService(t, clock, nil)

		sch, err := svc.Create(ctx, CreateInput{
			Name: "一次性任务", Prompt: "发报告",
			Trigger:  Trigger{Type: TriggerOnce, In: "1h"},
			Timezone: "UTC",
			Options:  Options{Misfire: MisfireRunOnce, MisfireGrace: "30m"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Advance 2h: once slot (+1h) is 1h ago > 30m grace.
		clock.Advance(2 * time.Hour)
		if err := svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ := svc.Runs(ctx, sch.ID, 10)
		if len(runs) != 1 || runs[0].Status != RunMissed || runs[0].ErrorCode != "misfire_grace_expired" {
			t.Fatalf("runs = %+v", runs)
		}
		got, _ := svc.Get(ctx, sch.ID)
		if got.State != StateCompleted {
			t.Fatalf("expired once state = %s, want completed", got.State)
		}
	})

	t.Run("misfire=skip always marks missed even within grace", func(t *testing.T) {
		clock := newFakeClock(testNow)
		svc, _ := newTestService(t, clock, nil)

		sch, err := svc.Create(ctx, CreateInput{
			Name: "跳过补跑", Prompt: "巡检",
			Trigger:  Trigger{Type: TriggerInterval, Every: "1h"},
			Timezone: "UTC",
			Options:  Options{Misfire: MisfireSkip},
		})
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(1*time.Hour + 5*time.Minute)
		if err := svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ := svc.Runs(ctx, sch.ID, 10)
		if len(runs) != 1 || runs[0].Status != RunMissed || !strings.Contains(runs[0].ErrorCode, "misfire_skip") {
			t.Fatalf("runs = %+v", runs)
		}
	})
}
