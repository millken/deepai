package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
)

// questionRuntime asks one or more questions on every turn. It stands in for
// a model calling ask_clarification inside a scheduled run.
type questionRuntime struct {
	*fakeRuntime
	mu               sync.Mutex
	questions        int
	blockAfterAnswer bool
	lastMaxToolCalls int
}

func (r *questionRuntime) NewConversation(string) (Conversation, error) {
	return &questionConv{rt: r}, nil
}

type questionConv struct{ rt *questionRuntime }

func (c *questionConv) Close() error { return nil }

func (c *questionConv) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	c.rt.mu.Lock()
	c.rt.lastMaxToolCalls = in.MaxToolCalls
	c.rt.mu.Unlock()
	answers := make([]string, 0, c.rt.questions)
	for i := 0; i < c.rt.questions; i++ {
		ans, err := in.Interaction.AskQuestion(ctx, fmt.Sprintf("question %d?", i), nil)
		if err != nil {
			return &TurnOutput{Messages: in.Messages}, err
		}
		answers = append(answers, ans)
	}
	if c.rt.blockAfterAnswer {
		<-ctx.Done()
		return &TurnOutput{Messages: in.Messages}, ctx.Err()
	}
	text := strings.Join(answers, "|")
	msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
		ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID,
		Role: models.RoleAI, Content: text,
	})
	return &TurnOutput{Messages: msgs, FinalOutput: text}, nil
}

func newQuestionTestEnv(t *testing.T, questions int, tweak func(*schedule.Config)) (*Manager, *schedule.Service, *questionRuntime) {
	t.Helper()
	rt := &questionRuntime{fakeRuntime: &fakeRuntime{}, questions: questions}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	cfg := schedule.DefaultConfig()
	cfg.Timezone = "UTC"
	cfg.MinInterval = time.Second
	cfg.Confirm = "auto"
	cfg.DefaultTimeout = 5 * time.Second
	if tweak != nil {
		tweak(&cfg)
	}
	svc, err := schedule.NewService(schedule.NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	disp := AttachSchedule(m, svc)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = disp.Wait(ctx)
	})
	return m, svc, rt
}

func createQuestionSchedule(t *testing.T, svc *schedule.Service, opts schedule.Options) schedule.Schedule {
	t.Helper()
	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name: "问答任务", Prompt: "请按需提问",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC", Options: opts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func waitGlobalRunStatus(t *testing.T, sub *Subscription, want schedule.RunStatus) schedule.Run {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.C:
			if e.Run != nil && e.Run.Status == want {
				return *e.Run
			}
		case <-deadline:
			t.Fatalf("no global event with run status %s", want)
			return schedule.Run{}
		}
	}
}

// waitGlobalRunUpdate asserts that an intermediate transition uses the
// schedule_run_updated event rather than pretending the run started again.
func waitGlobalRunUpdate(t *testing.T, sub *Subscription, want schedule.RunStatus) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.C:
			if e.Type == EventScheduleRunUpdated && e.Run != nil && e.Run.Status == want {
				return e
			}
		case <-deadline:
			t.Fatalf("no schedule_run_updated event with run status %s", want)
			return Event{}
		}
	}
}

// sessionQuestionID looks for the question event in the session replay buffer
// first (a client connecting late still sees pending questions), then waits
// for a live one.
func sessionQuestionID(t *testing.T, m *Manager, sessionID string) string {
	t.Helper()
	sub, backlog, _, err := m.Subscribe(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for _, e := range backlog {
		if e.Type == EventQuestion && e.Question != nil {
			return e.Question.ID
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.C:
			if e.Type == EventQuestion && e.Question != nil {
				return e.Question.ID
			}
		case <-deadline:
			t.Fatal("no question event on the session bus")
			return ""
		}
	}
}

func waitAutoAnsweredEvent(t *testing.T, m *Manager, sessionID string) Question {
	t.Helper()
	sub, backlog, _, err := m.Subscribe(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	check := func(evts []Event) (Question, bool) {
		for _, e := range evts {
			if e.Type == EventAnswered && e.Question != nil && e.Question.Auto {
				return *e.Question, true
			}
		}
		return Question{}, false
	}
	if q, ok := check(backlog); ok {
		return q
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.C:
			if q, ok := check([]Event{e}); ok {
				return q
			}
		case <-deadline:
			t.Fatal("no auto question_answered event")
			return Question{}
		}
	}
}

func TestScheduleAutoAnswerOnQuestion(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 60 * time.Millisecond
		c.DefaultMaxAutoAnswers = 3
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{})
	run, err := svc.RunNow(context.Background(), sch.ID, "auto-answer")
	if err != nil {
		t.Fatal(err)
	}
	waiting := waitGlobalRunUpdate(t, sub, schedule.RunWaitingInput).Run
	if waiting.ID != run.ID || waiting.SessionID == "" {
		t.Fatalf("waiting run = %+v", *waiting)
	}
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if finished.AutoAnswers != 1 || !strings.Contains(finished.Output, "最合理的假设") {
		t.Fatalf("finished run = %+v", finished)
	}
	if q := waitAutoAnsweredEvent(t, m, waiting.SessionID); q.Auto != true || !strings.Contains(q.Answer, "最合理的假设") {
		t.Fatalf("auto answer event = %+v", q)
	}
}

func TestScheduleAnswerTimeoutFailsRun(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 60 * time.Millisecond
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{OnAnswerTimeout: schedule.OnAnswerTimeoutFail})
	run, err := svc.RunNow(context.Background(), sch.ID, "timeout-fail")
	if err != nil {
		t.Fatal(err)
	}
	waitGlobalRunStatus(t, sub, schedule.RunWaitingInput)
	finished := waitRunStatus(t, svc, run.ID, schedule.RunFailed)
	if finished.ErrorCode != "answer_timeout" || !strings.Contains(finished.Error, "no answer within") {
		t.Fatalf("finished run = %+v", finished)
	}
	if finished.AutoAnswers != 0 {
		t.Fatalf("auto_answers = %d, want 0", finished.AutoAnswers)
	}
}

func TestScheduleAutoAnswersAreCapped(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 2, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 50 * time.Millisecond
		c.DefaultMaxAutoAnswers = 1
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{})
	run, err := svc.RunNow(context.Background(), sch.ID, "capped")
	if err != nil {
		t.Fatal(err)
	}
	waitGlobalRunStatus(t, sub, schedule.RunWaitingInput)
	finished := waitRunStatus(t, svc, run.ID, schedule.RunFailed)
	if finished.ErrorCode != "answer_timeout" || finished.AutoAnswers != 1 {
		t.Fatalf("finished run = %+v", finished)
	}
}

func TestScheduleClientAnswerResumesRun(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 2 * time.Second
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{})
	run, err := svc.RunNow(context.Background(), sch.ID, "client-answer")
	if err != nil {
		t.Fatal(err)
	}
	waiting := waitGlobalRunUpdate(t, sub, schedule.RunWaitingInput).Run
	qid := sessionQuestionID(t, m, waiting.SessionID)
	if err := m.Answer(waiting.SessionID, qid, "human"); err != nil {
		t.Fatal(err)
	}
	waitGlobalRunUpdate(t, sub, schedule.RunRunning)
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if finished.Output != "human" || finished.AutoAnswers != 0 {
		t.Fatalf("finished run = %+v", finished)
	}
}

func TestScheduleCancelWhileWaitingForAnswer(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 2 * time.Second
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{})
	run, err := svc.RunNow(context.Background(), sch.ID, "cancel-waiting")
	if err != nil {
		t.Fatal(err)
	}
	waitGlobalRunUpdate(t, sub, schedule.RunWaitingInput)
	if _, err := svc.CancelRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCancelled)
	if finished.ErrorCode == "answer_timeout" || finished.AutoAnswers != 0 {
		t.Fatalf("cancelled run = %+v", finished)
	}
}

func TestScheduleExecutionClockPausesWhileWaiting(t *testing.T) {
	m, svc, _ := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 2 * time.Second
	})
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	// 120ms execution budget, but the question stays open far longer.
	sch := createQuestionSchedule(t, svc, schedule.Options{Timeout: "120ms"})
	run, err := svc.RunNow(context.Background(), sch.ID, "clock-pause")
	if err != nil {
		t.Fatal(err)
	}
	waiting := waitGlobalRunStatus(t, sub, schedule.RunWaitingInput)
	time.Sleep(320 * time.Millisecond)
	still, err := svc.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != schedule.RunWaitingInput {
		t.Fatalf("run = %s, want still waiting_input (execution clock leaked)", still.Status)
	}
	qid := sessionQuestionID(t, m, waiting.SessionID)
	if err := m.Answer(waiting.SessionID, qid, "human"); err != nil {
		t.Fatal(err)
	}
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if finished.Output != "human" {
		t.Fatalf("finished run = %+v", finished)
	}
}

func TestScheduleExecutionClockResumesAfterAnswer(t *testing.T) {
	m, svc, rt := newQuestionTestEnv(t, 1, func(c *schedule.Config) {
		c.DefaultAnswerTimeout = 2 * time.Second
	})
	rt.blockAfterAnswer = true
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()
	sch := createQuestionSchedule(t, svc, schedule.Options{Timeout: "120ms"})
	run, err := svc.RunNow(context.Background(), sch.ID, "clock-resume")
	if err != nil {
		t.Fatal(err)
	}
	waiting := waitGlobalRunStatus(t, sub, schedule.RunWaitingInput)
	time.Sleep(250 * time.Millisecond)
	qid := sessionQuestionID(t, m, waiting.SessionID)
	if err := m.Answer(waiting.SessionID, qid, "human"); err != nil {
		t.Fatal(err)
	}
	// The turn keeps running after the answer, so the remaining ~120ms must
	// still expire: the clock resumed with its unspent budget.
	finished := waitRunStatus(t, svc, run.ID, schedule.RunTimedOut)
	if finished.ErrorCode != "timed_out" {
		t.Fatalf("finished run = %+v", finished)
	}
}

func TestScheduleMaxToolCallsOverridesTurn(t *testing.T) {
	m, _, rt := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)
	sch := createQuestionSchedule(t, svc, schedule.Options{MaxToolCalls: 7})
	run, err := svc.RunNow(context.Background(), sch.ID, "max-tools")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	rt.mu.Lock()
	got := rt.lastMaxToolCalls
	rt.mu.Unlock()
	if got != 7 {
		t.Fatalf("TurnInput.MaxToolCalls = %d, want 7", got)
	}
}
