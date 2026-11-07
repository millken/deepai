package bot

import (
	"context"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/schedule"
)

// autoAnswerText is what the bot answers on the user's behalf when nobody
// responds within answer_timeout (docs/BOT_SCHEDULER_DESIGN.md §7.1).
const autoAnswerText = "用户暂时无法回应。请按最合理的假设继续完成任务，并在结果中列出你做的假设。"

// answerWaiter is one outstanding ask_clarification question.
type answerWaiter struct {
	timer   *time.Timer
	auto    bool // reserved for an automatic answer
	handled bool // timeout decision already made
}

// scheduleQuestionHooks implements turnHooks for scheduled turns. It owns the
// per-question answer clock, the auto-answer counter and the Run's
// waiting_input ↔ running transitions. The execution clock itself is paused
// by Manager/turnInteraction, so the two clocks can never cancel a turn at
// the same time (design §7.2).
//
// Service writes from these callbacks are safe: every Service operation holds
// its mutex only while starting a turn, never while waiting for one to end.
type scheduleQuestionHooks struct {
	d     *ScheduleDispatcher
	svc   *schedule.Service
	runID string

	answerTimeout  time.Duration
	onTimeout      string
	maxAutoAnswers int

	mu          sync.Mutex
	sessionID   string // may be replaced if a dedicated session was recreated
	pending     map[string]*answerWaiter
	autoAnswers int
	reserved    int // auto-answers decided but not yet settled
	closed      bool
	failure     string // non-empty when answer_timeout ended the run
}

func newScheduleQuestionHooks(d *ScheduleDispatcher, svc *schedule.Service, sch schedule.Schedule, run schedule.Run, sessionID string) *scheduleQuestionHooks {
	cfg := svc.Config()
	answerTimeout := cfg.DefaultAnswerTimeout
	if v, err := time.ParseDuration(sch.Options.AnswerTimeout); err == nil && v > 0 {
		answerTimeout = v
	}
	policy := sch.Options.OnAnswerTimeout
	if policy == "" {
		policy = schedule.OnAnswerTimeoutAssume
	}
	maxAuto := sch.Options.AutoAnswers
	if maxAuto <= 0 {
		maxAuto = cfg.DefaultMaxAutoAnswers
	}
	return &scheduleQuestionHooks{
		d:              d,
		svc:            svc,
		runID:          run.ID,
		sessionID:      sessionID,
		answerTimeout:  answerTimeout,
		onTimeout:      policy,
		maxAutoAnswers: maxAuto,
		pending:        map[string]*answerWaiter{},
	}
}

// setSessionID pins the session actually used (Fire may have had to recreate
// a deleted dedicated session).
func (h *scheduleQuestionHooks) setSessionID(id string) {
	h.mu.Lock()
	h.sessionID = id
	h.mu.Unlock()
}

func (h *scheduleQuestionHooks) QuestionAsked(q Question) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	w := &answerWaiter{}
	h.pending[q.ID] = w
	first := len(h.pending) == 1
	// The callback needs h.mu, which we still hold, so even a zero-duration
	// timer cannot observe a half-registered waiter.
	w.timer = time.AfterFunc(h.answerTimeout, func() { h.onAnswerTimeout(q.ID) })
	h.mu.Unlock()
	if first {
		h.update(schedule.RunWaitingInput, 0)
	}
}

func (h *scheduleQuestionHooks) QuestionSettled(qid, _ string, auto bool) {
	h.settle(qid, auto)
}

func (h *scheduleQuestionHooks) TurnEnded() {
	h.mu.Lock()
	h.closed = true
	for id, w := range h.pending {
		if w.timer != nil {
			w.timer.Stop()
		}
		delete(h.pending, id)
	}
	h.mu.Unlock()
}

// failureReason is read by the Dispatcher once the turn has ended.
func (h *scheduleQuestionHooks) failureReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failure
}

func (h *scheduleQuestionHooks) onAnswerTimeout(qid string) {
	h.mu.Lock()
	w := h.pending[qid]
	if h.closed || w == nil || w.handled {
		h.mu.Unlock()
		return
	}
	w.handled = true
	if w.timer != nil {
		w.timer.Stop()
	}
	assume := h.onTimeout != schedule.OnAnswerTimeoutFail &&
		h.autoAnswers+h.reserved < h.maxAutoAnswers
	sid := h.sessionID
	if assume {
		w.auto = true
		h.reserved++
		h.mu.Unlock()
		if err := h.d.m.answerAuto(sid, qid, autoAnswerText); err != nil {
			// The question was answered or the turn ended in the meantime.
			h.settle(qid, false)
		}
		return
	}
	h.failure = "no answer within " + h.answerTimeout.String()
	h.mu.Unlock()
	_ = h.d.m.cancelWithSource(sid, cancelSourceScheduler)
}

// settle stops a question's answer clock, records an auto-answer and moves
// the Run back to running once every outstanding question is resolved.
func (h *scheduleQuestionHooks) settle(qid string, auto bool) {
	h.mu.Lock()
	w := h.pending[qid]
	delete(h.pending, qid)
	if w == nil {
		h.mu.Unlock()
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	delta := 0
	if w.auto {
		// Release the reservation even if the question disappeared before
		// the automatic answer could be delivered.
		h.reserved--
		if auto {
			h.autoAnswers++
			delta = 1
		}
	}
	last := len(h.pending) == 0
	h.mu.Unlock()
	status := schedule.RunStatus("")
	if last {
		status = schedule.RunRunning
	}
	h.update(status, delta)
}

func (h *scheduleQuestionHooks) update(status schedule.RunStatus, autoDelta int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.svc.UpdateRunInteraction(ctx, h.runID, status, autoDelta); err != nil {
		h.d.log.Warn("bot: update scheduled run interaction", "run", h.runID, "err", err)
	}
}
