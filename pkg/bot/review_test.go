package bot

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/schedule"
)

// Regression tests for the round-1 review of PR #43 (H2, M4, L3, L4).

// H2: when a dedicated session was deleted by the user, Fire recreates it
// and the new ID must be pinned on the Schedule, so later triggers do not
// keep creating a fresh session on every run.
func TestDeletedDedicatedSessionIsRecreatedAndPinned(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)
	sch, err := svc.Create(ctx, schedule.CreateInput{
		Name: "专属会话", Prompt: "检查",
		Trigger:  schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a dedicated session that existed earlier and was deleted.
	if err := svc.SetSessionID(ctx, sch.ID, "sess_deleted"); err != nil {
		t.Fatal(err)
	}

	first, err := svc.RunNow(ctx, sch.ID, "req-recreate-1")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, first.ID, schedule.RunCompleted)
	firstRun, err := svc.GetRun(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstRun.SessionID == "" || firstRun.SessionID == "sess_deleted" {
		t.Fatalf("recreated session = %q", firstRun.SessionID)
	}
	pinned, err := svc.Get(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.SessionID != firstRun.SessionID {
		t.Fatalf("schedule session = %q, want %q", pinned.SessionID, firstRun.SessionID)
	}

	second, err := svc.RunNow(ctx, sch.ID, "req-recreate-2")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, second.ID, schedule.RunCompleted)
	secondRun, err := svc.GetRun(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondRun.SessionID != firstRun.SessionID {
		t.Fatalf("second run used a new session %q, want the pinned %q",
			secondRun.SessionID, firstRun.SessionID)
	}
}

func newHookTestService(t *testing.T) (*schedule.Service, string) {
	t.Helper()
	store := schedule.NewMemoryStore()
	cfg := schedule.DefaultConfig()
	cfg.Confirm = "auto"
	svc, err := schedule.NewService(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := schedule.Run{
		ID: "run_hooks", ScheduleID: "sch_hooks", Seq: 1,
		Trigger: schedule.FireTimer, ScheduledFor: time.Now().UTC(),
		Status: schedule.RunRunning,
	}
	if err := store.InsertRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return svc, run.ID
}

func newTestQuestionHooks(svc *schedule.Service, runID string) *scheduleQuestionHooks {
	return &scheduleQuestionHooks{
		d:              &ScheduleDispatcher{log: slog.Default()},
		svc:            svc,
		runID:          runID,
		answerTimeout:  time.Hour,
		onTimeout:      schedule.OnAnswerTimeoutAssume,
		maxAutoAnswers: 3,
		pending:        map[string]*answerWaiter{},
	}
}

// M4: a slow "running" write from Q1's settle must not land after Q2's
// "waiting_input" write. Both writes are serialized with the pending map.
func TestQuestionHooksSerializeStatusWrites(t *testing.T) {
	ctx := context.Background()
	svc, runID := newHookTestService(t)
	h := newTestQuestionHooks(svc, runID)
	h.write = func(status schedule.RunStatus, delta int) {
		if status == schedule.RunRunning {
			time.Sleep(40 * time.Millisecond)
		}
		h.updateLocked(status, delta)
	}

	h.QuestionAsked(Question{ID: "q1"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.settle("q1", false)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		h.QuestionAsked(Question{ID: "q2"})
	}()
	wg.Wait()
	if len(h.pending) != 1 {
		t.Fatalf("pending questions = %d, want 1", len(h.pending))
	}
	h.TurnEnded()

	got, err := svc.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != schedule.RunWaitingInput {
		t.Fatalf("run status with one unanswered question = %s, want waiting_input", got.Status)
	}
}

// L3: once answer_timeout(fail) decided the run's fate, the ctx.Done
// unwinding of AskQuestion must not emit a misleading "running" write.
func TestAnswerTimeoutFailDoesNotWriteRunning(t *testing.T) {
	ctx := context.Background()
	svc, runID := newHookTestService(t)
	h := newTestQuestionHooks(svc, runID)
	h.QuestionAsked(Question{ID: "q1"})
	h.mu.Lock()
	h.failure = "no answer within 1s"
	h.mu.Unlock()
	h.settle("q1", false)
	h.TurnEnded()

	got, err := svc.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != schedule.RunWaitingInput {
		t.Fatalf("status after a failed answer timeout = %s, want waiting_input", got.Status)
	}
}

// L4: a client cancellation that began before shutdown keeps its own cause;
// only an unattributed cancellation during shutdown is bot_shutdown.
func TestFinalizeOutcomeAttribution(t *testing.T) {
	timeout := 30 * time.Minute
	client := &TurnSummary{Status: TurnCancelled, CancelSource: "client"}
	out := finalizeOutcome(outcomeFromSummary(client), client, true, "", schedule.CancelClient, timeout)
	if out.Status != schedule.RunCancelled || out.ErrorCode != "cancelled" {
		t.Fatalf("client cancel during shutdown = %+v", out)
	}

	shutdown := &TurnSummary{Status: TurnCancelled, CancelSource: "shutdown"}
	out = finalizeOutcome(outcomeFromSummary(shutdown), shutdown, false, "", schedule.CancelClient, timeout)
	if out.Status != schedule.RunFailed || out.ErrorCode != "bot_shutdown" {
		t.Fatalf("shutdown cancel = %+v", out)
	}

	unknown := &TurnSummary{Status: TurnCancelled}
	out = finalizeOutcome(outcomeFromSummary(unknown), unknown, true, "", schedule.CancelClient, timeout)
	if out.Status != schedule.RunFailed || out.ErrorCode != "bot_shutdown" {
		t.Fatalf("unattributed cancel during shutdown = %+v", out)
	}

	answered := &TurnSummary{Status: TurnCancelled, CancelSource: "scheduler"}
	out = finalizeOutcome(outcomeFromSummary(answered), answered, false, "no answer within 1s", schedule.CancelScheduler, timeout)
	if out.Status != schedule.RunFailed || out.ErrorCode != "answer_timeout" {
		t.Fatalf("answer_timeout failure = %+v", out)
	}
}
