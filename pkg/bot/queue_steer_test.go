package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
	"github.com/millken/deepai/pkg/tools"
	"golang.org/x/net/websocket"
)

// gatedRuntime gives deterministic control over turn progress for queue/steer tests.
type gatedRuntime struct {
	mu      sync.Mutex
	started chan string
	release chan struct{}
	mode    string // "gate", "gate_fail", "steer_wait", "wrapup_gate", "ask"
	ran     []string
	steered []string
}

func newGatedRuntime(mode string) *gatedRuntime {
	return &gatedRuntime{
		started: make(chan string, 16),
		release: make(chan struct{}),
		mode:    mode,
	}
}

func (g *gatedRuntime) Info() Info {
	return Info{Models: []ModelInfo{{Name: "fake"}}, DefaultModel: "fake"}
}
func (g *gatedRuntime) DefaultModel() string   { return "fake" }
func (g *gatedRuntime) HasModel(a string) bool { return a == "fake" }
func (g *gatedRuntime) NewConversation(string) (Conversation, error) {
	return &gatedConv{rt: g}, nil
}

type gatedConv struct{ rt *gatedRuntime }

func (c *gatedConv) Close() error { return nil }

func (c *gatedConv) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	last := in.Messages[len(in.Messages)-1].Content
	c.rt.mu.Lock()
	mode := c.rt.mode
	c.rt.ran = append(c.rt.ran, last)
	c.rt.mu.Unlock()

	c.rt.started <- last

	reply := func(text string) *TurnOutput {
		msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
			ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: text,
		})
		return &TurnOutput{Messages: msgs, FinalOutput: text, LLMTurns: 1}
	}

	switch mode {
	case "gate":
		select {
		case <-c.rt.release:
		case <-ctx.Done():
			return &TurnOutput{Messages: in.Messages}, ctx.Err()
		}
		return reply("done:" + last), nil

	case "gate_fail":
		select {
		case <-c.rt.release:
		case <-ctx.Done():
			return &TurnOutput{Messages: in.Messages}, ctx.Err()
		}
		return &TurnOutput{Messages: in.Messages}, errors.New("simulated failure")

	case "steer_wait":
		deadline := time.After(3 * time.Second)
		for {
			if in.Steer != nil {
				if msgs := in.Steer.DrainSteer(); len(msgs) > 0 {
					c.rt.mu.Lock()
					for _, st := range msgs {
						c.rt.steered = append(c.rt.steered, st.Content)
						in.Emit(Event{Type: EventSteerApplied, SteerID: st.ID, Text: st.Content})
					}
					c.rt.mu.Unlock()
					return reply("steered:" + msgs[0].Content), nil
				}
			}
			select {
			case <-ctx.Done():
				return &TurnOutput{Messages: in.Messages}, ctx.Err()
			case <-deadline:
				return reply("no_steer"), nil
			case <-time.After(10 * time.Millisecond):
			}
		}

	case "wrapup_gate":
		if n, ok := in.Steer.(agent.SteerWrapUpNotifier); ok {
			n.MarkWrapUp()
		}
		select {
		case <-c.rt.release:
		case <-ctx.Done():
			return &TurnOutput{Messages: in.Messages}, ctx.Err()
		}
		return reply("wrapped:" + last), nil

	case "ask":
		ans, err := in.Interaction.AskQuestion(ctx, "confirm?", []string{"yes", "no"})
		if err != nil {
			return &TurnOutput{Messages: in.Messages}, err
		}
		return reply("answered:" + ans), nil
	}

	return reply("echo:" + last), nil
}

func TestQueueFIFOAutoDequeueAndCapacity(t *testing.T) {
	rt := newGatedRuntime("gate")
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sess, err := m.CreateSession(CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// Start first turn (blocks on rt.release).
	r0 := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "first"})
	r0.Body.Close()
	if r0.StatusCode != http.StatusAccepted {
		t.Fatalf("first send status = %d", r0.StatusCode)
	}
	<-rt.started

	// Default mode ("reject") returns 409 busy.
	rBusy := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "blocked"})
	aeBusy := decodeAPIError(t, rBusy)
	rBusy.Body.Close()
	if rBusy.StatusCode != http.StatusConflict || aeBusy.Code != "busy" {
		t.Fatalf("reject while busy got (%d, %q), want (409, busy)", rBusy.StatusCode, aeBusy.Code)
	}

	// Invalid mode returns 400 bad_request.
	rBad := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "x", "mode": "bogus"})
	aeBad := decodeAPIError(t, rBad)
	rBad.Body.Close()
	if rBad.StatusCode != http.StatusBadRequest || aeBad.Code != "bad_request" {
		t.Fatalf("invalid mode got (%d, %q), want (400, bad_request)", rBad.StatusCode, aeBad.Code)
	}

	// Enqueue two follow-up messages.
	var q1, q2 SendResult
	rQ1 := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "second", "mode": "queue"})
	_ = json.NewDecoder(rQ1.Body).Decode(&q1)
	rQ1.Body.Close()
	if rQ1.StatusCode != http.StatusAccepted || !q1.Queued || q1.Position != 1 || q1.QueueID == "" {
		t.Fatalf("q1 = (%d, %+v)", rQ1.StatusCode, q1)
	}
	rQ2 := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "third", "mode": "queue"})
	_ = json.NewDecoder(rQ2.Body).Decode(&q2)
	rQ2.Body.Close()
	if rQ2.StatusCode != http.StatusAccepted || !q2.Queued || q2.Position != 2 {
		t.Fatalf("q2 = (%d, %+v)", rQ2.StatusCode, q2)
	}

	// GET /v1/sessions/{id}/queue returns both items in FIFO order.
	rList := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/queue", "secret", nil)
	var listEnv struct {
		Queue []QueuedMessage `json:"queue"`
	}
	_ = json.NewDecoder(rList.Body).Decode(&listEnv)
	rList.Body.Close()
	if len(listEnv.Queue) != 2 || listEnv.Queue[0].ID != q1.QueueID || listEnv.Queue[1].ID != q2.QueueID {
		t.Fatalf("queue list = %+v", listEnv.Queue)
	}

	// Remove the first queued item ("second"), keeping only "third".
	rRm := doReq(t, http.MethodDelete, ts.URL+"/v1/sessions/"+sess.ID+"/queue/"+q1.QueueID, "secret", nil)
	rRm.Body.Close()
	if rRm.StatusCode != http.StatusNoContent {
		t.Fatalf("remove queue item status = %d, want 204", rRm.StatusCode)
	}
	rRmMissing := doReq(t, http.MethodDelete, ts.URL+"/v1/sessions/"+sess.ID+"/queue/q_missing", "secret", nil)
	rRmMissing.Body.Close()
	if rRmMissing.StatusCode != http.StatusNotFound {
		t.Fatalf("remove missing queue item status = %d, want 404", rRmMissing.StatusCode)
	}

	// Fill up to MaxSessionQueueDepth and verify overflow returns 409 queue_full.
	for i := 1; i < MaxSessionQueueDepth; i++ {
		r := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "pad", "mode": "queue"})
		r.Body.Close()
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("pad #%d status = %d", i, r.StatusCode)
		}
	}
	rFull := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "overflow", "mode": "queue"})
	aeFull := decodeAPIError(t, rFull)
	rFull.Body.Close()
	if rFull.StatusCode != http.StatusConflict || aeFull.Code != "queue_full" {
		t.Fatalf("queue overflow got (%d, %q), want (409, queue_full)", rFull.StatusCode, aeFull.Code)
	}

	// Clear queue and re-add just "third" so release runs exactly first -> third.
	rClr := doReq(t, http.MethodDelete, ts.URL+"/v1/sessions/"+sess.ID+"/queue", "secret", nil)
	rClr.Body.Close()
	if rClr.StatusCode != http.StatusNoContent {
		t.Fatalf("clear queue status = %d", rClr.StatusCode)
	}
	rQ3 := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{"content": "third", "mode": "queue"})
	rQ3.Body.Close()

	// Release gate: first finishes, "third" auto-dequeues and runs immediately.
	close(rt.release)
	select {
	case got := <-rt.started:
		if got != "third" {
			t.Fatalf("auto-dequeued prompt = %q, want third", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued follow-up turn did not auto-start")
	}

	// Wait until session settles idle.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, _, _ := m.GetSession(sess.ID)
		if info.Status == SessionIdle && len(info.Queue) == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	info, msgs, err := m.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != SessionIdle || len(info.Queue) != 0 || len(msgs) != 4 {
		t.Fatalf("final state status=%s queue=%d msgs=%d", info.Status, len(info.Queue), len(msgs))
	}
}

func TestQueueCancelClearsAndFailurePreserves(t *testing.T) {
	t.Run("cancel clears queue", func(t *testing.T) {
		rt := newGatedRuntime("gate")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		sub, _, _, _ := m.Subscribe(sess.ID, 0)
		defer sub.Close()

		turn, err := m.Send(sess.ID, SendRequest{Content: "first"})
		if err != nil {
			t.Fatal(err)
		}
		<-rt.started

		if _, err := m.Submit(sess.ID, SendRequest{Content: "stale queued", Mode: SendModeQueue}); err != nil {
			t.Fatal(err)
		}
		if err := m.Cancel(sess.ID); err != nil {
			t.Fatal(err)
		}
		sum := waitTurn(t, turn)
		if sum.Status != TurnCancelled {
			t.Fatalf("status = %s, want cancelled", sum.Status)
		}

		// Queue must be cleared and stale message must NOT have executed.
		q, _ := m.ListQueue(sess.ID)
		if len(q) != 0 {
			t.Fatalf("queue after cancel = %+v, want empty", q)
		}
		rt.mu.Lock()
		ran := append([]string(nil), rt.ran...)
		rt.mu.Unlock()
		if len(ran) != 1 || ran[0] != "first" {
			t.Fatalf("ran = %v, want only [first]", ran)
		}

		evts := collectUntil(t, sub, EventQueueCleared)
		last := evts[len(evts)-1]
		if last.Reason != "cancelled" {
			t.Fatalf("queue_cleared reason = %q, want cancelled", last.Reason)
		}
	})

	t.Run("failure preserves queue without auto-dequeue", func(t *testing.T) {
		rt := newGatedRuntime("gate_fail")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		turn, err := m.Send(sess.ID, SendRequest{Content: "will fail"})
		if err != nil {
			t.Fatal(err)
		}
		<-rt.started

		if _, err := m.Submit(sess.ID, SendRequest{Content: "keep me", Mode: SendModeQueue}); err != nil {
			t.Fatal(err)
		}
		close(rt.release)
		sum := waitTurn(t, turn)
		if sum.Status != TurnFailed {
			t.Fatalf("status = %s, want failed", sum.Status)
		}

		q, _ := m.ListQueue(sess.ID)
		if len(q) != 1 || q[0].Content != "keep me" {
			t.Fatalf("queue after failure = %+v, want [keep me] preserved", q)
		}
		rt.mu.Lock()
		ran := append([]string(nil), rt.ran...)
		rt.mu.Unlock()
		if len(ran) != 1 {
			t.Fatalf("failed turn must not auto-dequeue; ran = %v", ran)
		}
	})
}

func TestSteerMidTurnWrapUpFallbackPromotionAndAskIsolation(t *testing.T) {
	t.Run("mid-turn steer delivered to running turn", func(t *testing.T) {
		rt := newGatedRuntime("steer_wait")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		sub, _, _, _ := m.Subscribe(sess.ID, 0)
		defer sub.Close()

		turn, err := m.Send(sess.ID, SendRequest{Content: "initial"})
		if err != nil {
			t.Fatal(err)
		}
		<-rt.started

		res, err := m.Submit(sess.ID, SendRequest{Content: "change direction", Mode: SendModeSteer})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Steered || res.SteerID == "" || res.Fallback != "" {
			t.Fatalf("steer result = %+v", res)
		}

		sum := waitTurn(t, turn)
		if sum.Status != TurnCompleted || sum.FinalOutput != "steered:change direction" {
			t.Fatalf("turn summary = %+v", sum)
		}
		evts := collectUntil(t, sub, EventTurnEnd)
		var sawQueued, sawApplied bool
		for _, e := range evts {
			if e.Type == EventSteerQueued && e.SteerID == res.SteerID {
				sawQueued = true
			}
			if e.Type == EventSteerApplied && e.SteerID == res.SteerID {
				sawApplied = true
			}
		}
		if !sawQueued || !sawApplied {
			t.Fatalf("missing steer events (queued=%v applied=%v) in %v", sawQueued, sawApplied, eventTypes(evts))
		}
	})

	t.Run("wrapUp forces steer fallback to queue", func(t *testing.T) {
		rt := newGatedRuntime("wrapup_gate")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		turn, _ := m.Send(sess.ID, SendRequest{Content: "wrapping"})
		<-rt.started
		// Wait briefly until MarkWrapUp has been called.
		time.Sleep(30 * time.Millisecond)

		res, err := m.Submit(sess.ID, SendRequest{Content: "after wrapup", Mode: SendModeSteer})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Queued || res.Steered || res.Fallback != SendModeQueue {
			t.Fatalf("expected fallback to queue, got %+v", res)
		}

		// Release gate: wrapping turn finishes, fallback queued message runs next.
		rt.mu.Lock()
		rt.mode = "gate"
		rt.mu.Unlock()
		close(rt.release)
		waitTurn(t, turn)
		select {
		case got := <-rt.started:
			if got != "after wrapup" {
				t.Fatalf("next turn = %q, want 'after wrapup'", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("fallback queued steer did not auto-start")
		}
	})

	t.Run("unconsumed steer promoted to follow-up queue on turn completion", func(t *testing.T) {
		rt := newGatedRuntime("gate")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		turn, _ := m.Send(sess.ID, SendRequest{Content: "first"})
		<-rt.started

		// "gate" runtime never drains steer; steer arrives just before completion.
		res, err := m.Submit(sess.ID, SendRequest{Content: "late steer", Mode: SendModeSteer})
		if err != nil || !res.Steered {
			t.Fatalf("steer = (%+v, %v)", res, err)
		}

		close(rt.release)
		waitTurn(t, turn)

		select {
		case got := <-rt.started:
			if got != "late steer" {
				t.Fatalf("promoted steer turn = %q, want 'late steer'", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("unconsumed steer was dropped instead of promoted to queue")
		}
	})

	t.Run("steer does not answer pending ask_clarification", func(t *testing.T) {
		rt := newGatedRuntime("ask")
		m, _ := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
		defer m.Close(context.Background())

		sess, _ := m.CreateSession(CreateSessionRequest{})
		sub, _, _, _ := m.Subscribe(sess.ID, 0)
		defer sub.Close()

		turn, _ := m.Send(sess.ID, SendRequest{Content: "ask me"})
		evts := collectUntil(t, sub, EventQuestion)
		qID := evts[len(evts)-1].Question.ID

		// Sending mode=steer must NOT resolve the pending question.
		res, err := m.Submit(sess.ID, SendRequest{Content: "unrelated steer", Mode: SendModeSteer})
		if err != nil || !res.Steered {
			t.Fatalf("steer = (%+v, %v)", res, err)
		}
		time.Sleep(40 * time.Millisecond)
		info, _, _ := m.GetSession(sess.ID)
		if info.Status != SessionRunning || len(info.Pending) != 1 || info.Pending[0].ID != qID {
			t.Fatalf("steer erroneously resolved pending question: %+v", info)
		}

		// Explicitly answering resolves the question cleanly.
		if err := m.Answer(sess.ID, qID, "yes"); err != nil {
			t.Fatal(err)
		}
		sum := waitTurn(t, turn)
		if sum.FinalOutput != "answered:yes" {
			t.Fatalf("FinalOutput = %q, want answered:yes", sum.FinalOutput)
		}
	})
}

func TestQueueSteerWSAndScheduleMidTurnSteer(t *testing.T) {
	rt := newGatedRuntime("steer_wait")
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	svc, _ := newScheduleTestService(t, m)
	srv := NewServer(ServerOptions{Manager: m, Token: "secret", Schedules: svc})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Trigger a scheduled run and steer it mid-turn over WS.
	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name:    "steerable-job",
		Prompt:  "do audit",
		Trigger: schedule.Trigger{Type: schedule.TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(context.Background(), sch.ID, "req-steer")
	if err != nil {
		t.Fatal(err)
	}
	<-rt.started

	// Wait until run.SessionID is recorded.
	var runSessID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := svc.GetRun(context.Background(), run.ID)
		if err == nil && r.SessionID != "" {
			runSessID = r.SessionID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runSessID == "" {
		t.Fatal("scheduled run never got a session_id")
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=secret"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sendWS := func(req WSRequest) WSMessage {
		t.Helper()
		if err := websocket.JSON.Send(conn, req); err != nil {
			t.Fatal(err)
		}
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "reply" && msg.ID == req.ID {
				return msg
			}
		}
	}

	// Steer the running scheduled task via WS!
	rep := sendWS(WSRequest{ID: "1", Op: "send", SessionID: runSessID, Content: "focus on errors only", Mode: SendModeSteer})
	if !rep.OK {
		t.Fatalf("WS steer failed: %+v", rep.Error)
	}
	doneRun := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if !strings.Contains(doneRun.Output, "steered:focus on errors only") {
		t.Fatalf("scheduled run output = %q, want steered output", doneRun.Output)
	}

	// 2. Test WS queue ops (list_queue, remove_queue_item, clear_queue) on a non-default bot + cross-bot 404.
	if _, err := m.CreateBot(context.Background(), CreateBotRequest{ID: "reviewer", Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.mode = "gate"
	rt.release = make(chan struct{})
	rt.mu.Unlock()

	revSess, err := m.CreateSessionForBot("reviewer", CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rep := sendWS(WSRequest{ID: "2", Op: "send", BotID: "reviewer", SessionID: revSess.ID, Content: "first"}); !rep.OK {
		t.Fatalf("send first: %+v", rep.Error)
	}
	<-rt.started

	repQ := sendWS(WSRequest{ID: "3", Op: "send", BotID: "reviewer", SessionID: revSess.ID, Content: "queued-1", Mode: SendModeQueue})
	if !repQ.OK {
		t.Fatalf("queue-1 failed: %+v", repQ.Error)
	}
	// Flat /v1/sessions/{revSess}/queue must return 404 (bot isolation).
	rFlat := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+revSess.ID+"/queue", "secret", nil)
	rFlat.Body.Close()
	if rFlat.StatusCode != http.StatusNotFound {
		t.Fatalf("flat GET reviewer queue status = %d, want 404", rFlat.StatusCode)
	}
	// Scoped WS list_queue & clear_queue succeed.
	repList := sendWS(WSRequest{ID: "4", Op: "list_queue", BotID: "reviewer", SessionID: revSess.ID})
	if !repList.OK {
		t.Fatalf("list_queue: %+v", repList.Error)
	}
	if repClr := sendWS(WSRequest{ID: "5", Op: "clear_queue", BotID: "reviewer", SessionID: revSess.ID}); !repClr.OK {
		t.Fatalf("clear_queue: %+v", repClr.Error)
	}
	close(rt.release)
}

func TestAgentRuntimeEndToEndSteer(t *testing.T) {
	var mu sync.Mutex
	var llmCalls int
	var secondReqBody string

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		llmCalls++
		call := llmCalls
		if call == 2 {
			secondReqBody = string(bodyBytes)
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			chunk := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"slow_step","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
			_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
			return
		}
		chunk := `{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ack steered"},"finish_reason":"stop"}]}`
		_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
	}))
	defer mockLLM.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	modelReg, err := llm.NewModelRegistry([]llm.ModelDef{{
		Name:     "mock",
		Provider: "openai-compat",
		Model:    "mock-model",
		BaseURL:  mockLLM.URL,
	}}, "mock")
	if err != nil {
		t.Fatal(err)
	}

	toolStarted := make(chan struct{})
	toolRelease := make(chan struct{})
	toolReg := tools.NewRegistry()
	_ = toolReg.Register(models.Tool{
		Name:        "slow_step",
		Description: "blocks until released",
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			close(toolStarted)
			<-toolRelease
			return models.ToolResult{Status: models.CallStatusCompleted, Content: "step complete"}, nil
		},
	})

	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:         modelReg,
		Tools:          toolReg,
		WorkDir:        t.TempDir(),
		SandboxBaseDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	sess, err := m.CreateSession(CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sub, _, _, _ := m.Subscribe(sess.ID, 0)
	defer sub.Close()

	turn, err := m.Send(sess.ID, SendRequest{Content: "run slow_step"})
	if err != nil {
		t.Fatal(err)
	}
	<-toolStarted

	res, err := m.Submit(sess.ID, SendRequest{Content: "skip old plan and only check v2", Mode: SendModeSteer})
	if err != nil || !res.Steered {
		t.Fatalf("Submit steer = (%+v, %v)", res, err)
	}
	close(toolRelease)

	sum := waitTurn(t, turn)
	if sum.Status != TurnCompleted || sum.FinalOutput != "ack steered" {
		t.Fatalf("turn summary = %+v", sum)
	}

	mu.Lock()
	body2 := secondReqBody
	mu.Unlock()
	if !strings.Contains(body2, "skip old plan and only check v2") {
		t.Fatalf("second LLM request missing steered user message:\n%s", body2)
	}

	evts := collectUntil(t, sub, EventTurnEnd)
	var sawApplied bool
	for _, e := range evts {
		if e.Type == EventSteerApplied && e.SteerID == res.SteerID {
			sawApplied = true
		}
	}
	if !sawApplied {
		t.Fatalf("missing EventSteerApplied in session stream: %v", eventTypes(evts))
	}
}
