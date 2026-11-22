package bot

import (
	"context"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/schedule"
)

// PR #43 round-3 M1: a run queued as session_busy must start when the
// blocking turn ENDS (design §4.1: any session's turn_end wakes
// TryStartPending) — not passively wait for the scheduler's next timer
// sweep. In the test environment there is no Scheduler loop at all, so the
// old code leaves the run pending forever: only the turn_end wake chain can
// move it.
func TestSessionBusyRunStartsOnTurnEnd(t *testing.T) {
	m, _, _ := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)

	// The schedule targets the user's console session (session_mode=origin).
	sess, err := m.CreateSession(CreateSessionRequest{Title: "控制台"})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name: "日报", Prompt: "总结今天的工作",
		Trigger:   schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
		Timezone:  "UTC",
		Options:   schedule.Options{SessionMode: schedule.SessionOrigin},
		CreatedBy: schedule.CreatedBy{Kind: "user", SessionID: sess.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A human turn occupies the session (fakeRuntime blocks on "block").
	if _, err := m.Send(sess.ID, SendRequest{Content: "block"}); err != nil {
		t.Fatal(err)
	}

	// The slot fires into the busy session and must queue as session_busy.
	run, err := svc.RunNow(context.Background(), sch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := svc.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.QueuedReason == "session_busy" {
			break
		}
		if r.Status.Terminal() {
			t.Fatalf("run reached terminal %s before queuing: %+v", r.Status, r)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never queued as session_busy: %+v", r)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The human turn ends (cancelled) → the turn_end wake chain must start
	// the queued run and drive it to a terminal state, with NO scheduler
	// timer running in this process.
	if err := m.Cancel(sess.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if finished.SessionID == "" || finished.TurnID == "" {
		t.Fatalf("woken run did not dispatch: %+v", finished)
	}
}
