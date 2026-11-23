package schedule

import (
	"context"
	"testing"
	"time"
)

// Update ignored EndAt/ClearEndAt when deciding whether to recompute
// NextRunAt (issue #52): moving EndAt before the current NextRunAt left
// a slot scheduled past the deadline, and clearing EndAt on a task whose
// NextRunAt was nil never revived scheduling.
func TestUpdate_EndAtRecomputesNextRunAt(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testNow)
	svc, _ := newTestService(t, clock, nil)

	// Every hour on the hour; first slot 13:00, EndAt initially far away.
	sch, err := svc.Create(ctx, CreateInput{
		Name: "每小时", Prompt: "tick",
		Trigger:  Trigger{Type: TriggerCron, Expr: "0 * * * *"},
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sch.NextRunAt == nil || !sch.NextRunAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("initial NextRunAt = %v, want %v", sch.NextRunAt, testNow.Add(time.Hour))
	}

	// 1. EndAt moved before the current NextRunAt: NextRunAt must drop to
	//    nil (no slot fits before the deadline), not stay past it.
	endAt := testNow.Add(30 * time.Minute)
	sch, err = svc.Update(ctx, sch.ID, Patch{EndAt: &endAt})
	if err != nil {
		t.Fatal(err)
	}
	if sch.NextRunAt != nil {
		t.Fatalf("RED: NextRunAt = %v after moving EndAt to %v, want nil — scheduler would fire past end_at",
			sch.NextRunAt, endAt)
	}

	// 2. ClearEndAt on a task with NextRunAt == nil: scheduling must resume.
	sch, err = svc.Update(ctx, sch.ID, Patch{ClearEndAt: true})
	if err != nil {
		t.Fatal(err)
	}
	if sch.NextRunAt == nil || !sch.NextRunAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("RED: NextRunAt = %v after ClearEndAt, want %v — task never resumes",
			sch.NextRunAt, testNow.Add(time.Hour))
	}

	// 3. EndAt moved to a later slot that still fits: NextRunAt is kept at
	//    the first free slot, unchanged.
	later := testNow.Add(90 * time.Minute)
	sch, err = svc.Update(ctx, sch.ID, Patch{EndAt: &later})
	if err != nil {
		t.Fatal(err)
	}
	if sch.NextRunAt == nil || !sch.NextRunAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("NextRunAt = %v with EndAt %v, want %v", sch.NextRunAt, later, testNow.Add(time.Hour))
	}
	_ = clock
}
