package bot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"golang.org/x/net/websocket"
)

// fakeRuntime answers each turn according to the user's message:
//
//	"block"  — waits for cancellation
//	"orphan" — waits for cancellation, then emits once more after release
//	"ask"    — asks a question and echoes the answer
//	"fail"   — returns an error
//	anything else — streams "echo: <msg>" in two deltas
type fakeRuntime struct {
	mu     sync.Mutex
	closed int
	// release unblocks the background emitter of an "orphan" turn;
	// lateEmitted is closed once it has called Emit.
	release     chan struct{}
	lateEmitted chan struct{}
}

func (f *fakeRuntime) Info() Info {
	return Info{Models: []ModelInfo{{Name: "fake"}, {Name: "other"}}, DefaultModel: "fake"}
}
func (f *fakeRuntime) DefaultModel() string   { return "fake" }
func (f *fakeRuntime) HasModel(a string) bool { return a == "fake" || a == "other" }
func (f *fakeRuntime) NewConversation(string) (Conversation, error) {
	return &fakeConv{rt: f}, nil
}

type fakeConv struct{ rt *fakeRuntime }

func (c *fakeConv) Close() error {
	c.rt.mu.Lock()
	c.rt.closed++
	c.rt.mu.Unlock()
	return nil
}

func (c *fakeConv) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	last := in.Messages[len(in.Messages)-1].Content
	reply := func(text string) *TurnOutput {
		msgs := append(append([]models.Message(nil), in.Messages...), models.Message{
			ID: in.SessionID + "_ai_" + in.TurnID, SessionID: in.SessionID, Role: models.RoleAI, Content: text,
		})
		return &TurnOutput{Messages: msgs, FinalOutput: text, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 2}, LLMTurns: 1}
	}
	switch last {
	case "orphan":
		// Models a run abandoned after cancel: something it started keeps
		// emitting after RunTurn has returned.
		go func() {
			<-c.rt.release
			in.Emit(Event{Type: EventTextDelta, Text: "late"})
			close(c.rt.lateEmitted)
		}()
		<-ctx.Done()
		return &TurnOutput{Messages: in.Messages}, ctx.Err()
	case "block":
		<-ctx.Done()
		return &TurnOutput{Messages: in.Messages}, ctx.Err()
	case "fail":
		in.Emit(Event{Type: EventError, Error: &agent.AgentError{Message: "boom"}})
		return &TurnOutput{Messages: in.Messages}, errors.New("boom")
	case "ask":
		ans, err := in.Interaction.AskQuestion(ctx, "which?", []string{"a", "b"})
		if err != nil {
			return &TurnOutput{Messages: in.Messages}, err
		}
		return reply("answer=" + ans), nil
	}
	in.Emit(Event{Type: EventToolCallStart, Tool: &agent.ToolCallEvent{ID: "t1", Name: "bash", Status: models.CallStatusRunning}})
	in.Emit(Event{Type: EventTextDelta, Text: "echo: "})
	in.Emit(Event{Type: EventTextDelta, Text: last})
	return reply("echo: " + last), nil
}

func newTestManager(t *testing.T) (*Manager, *MemoryStore, *fakeRuntime) {
	t.Helper()
	rt := &fakeRuntime{}
	store := NewMemoryStore()
	m, err := NewManager(Options{Runtime: rt, Store: store, IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, store, rt
}

func waitTurn(t *testing.T, turn *Turn) *TurnSummary {
	t.Helper()
	select {
	case <-turn.Done():
		return turn.Summary()
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not finish")
		return nil
	}
}

func collectUntil(t *testing.T, sub *Subscription, typ EventType) []Event {
	t.Helper()
	var out []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed after %d events", len(out))
			}
			out = append(out, e)
			if e.Type == typ {
				return out
			}
		case <-timeout:
			t.Fatalf("no %s event; got %v", typ, eventTypes(out))
		}
	}
}

func eventTypes(evts []Event) []EventType {
	out := make([]EventType, len(evts))
	for i, e := range evts {
		out[i] = e.Type
	}
	return out
}

func TestManagerTurnStreamsAndPersists(t *testing.T) {
	m, store, _ := newTestManager(t)
	info, err := m.CreateSession(CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "fake" {
		t.Fatalf("model = %q, want default", info.Model)
	}
	if _, msgs, _ := m.GetSession(info.ID); msgs == nil {
		t.Fatal("new session transcript must be an empty array, not null")
	}
	sub, _, _, err := m.Subscribe(info.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	turn, err := m.Send(info.ID, SendRequest{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	evts := collectUntil(t, sub, EventTurnEnd)
	got := eventTypes(evts)
	want := []EventType{EventTurnStart, EventToolCallStart, EventTextDelta, EventTextDelta, EventTurnEnd}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if evts[i].Seq != int64(i+1) || evts[i].TurnID != turn.ID || evts[i].SessionID != info.ID {
			t.Fatalf("event %d envelope = %+v", i, evts[i])
		}
	}
	sum := waitTurn(t, turn)
	if sum.Status != TurnCompleted || sum.FinalOutput != "echo: hello" || sum.Usage == nil {
		t.Fatalf("summary = %+v", sum)
	}

	msgs, _ := store.LoadMessages(info.ID)
	if len(msgs) != 2 || msgs[0].Role != models.RoleHuman || msgs[1].Content != "echo: hello" {
		t.Fatalf("persisted = %+v", msgs)
	}
	got2, _, _ := m.GetSession(info.ID)
	if got2.Title != "hello" || got2.Status != SessionIdle || got2.MsgCount != 2 {
		t.Fatalf("session = %+v", got2)
	}
}

func TestManagerBusyCancelAndReplay(t *testing.T) {
	m, _, _ := newTestManager(t)
	info, _ := m.CreateSession(CreateSessionRequest{})
	turn, err := m.Send(info.ID, SendRequest{Content: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(info.ID, SendRequest{Content: "again"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second send err = %v, want ErrBusy", err)
	}
	if err := m.DeleteSession(info.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete running err = %v, want ErrBusy", err)
	}
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
	if sum := waitTurn(t, turn); sum.Status != TurnCancelled {
		t.Fatalf("status = %s, want cancelled", sum.Status)
	}
	if err := m.Cancel(info.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel idle err = %v", err)
	}

	sub, backlog, gap, _ := m.Subscribe(info.ID, 0)
	defer sub.Close()
	if gap || len(backlog) != 2 || backlog[0].Type != EventTurnStart || backlog[1].Type != EventTurnEnd {
		t.Fatalf("replay = %v gap=%v", eventTypes(backlog), gap)
	}
	_, backlog, _, _ = m.Subscribe(info.ID, 1)
	if len(backlog) != 1 || backlog[0].Seq != 2 {
		t.Fatalf("replay since 1 = %v", backlog)
	}
}

func TestManagerQuestionAnswer(t *testing.T) {
	m, _, _ := newTestManager(t)
	info, _ := m.CreateSession(CreateSessionRequest{})
	sub, _, _, _ := m.Subscribe(info.ID, -1)
	defer sub.Close()
	turn, _ := m.Send(info.ID, SendRequest{Content: "ask"})

	evts := collectUntil(t, sub, EventQuestion)
	q := evts[len(evts)-1].Question
	if q == nil || q.Text != "which?" || len(q.Options) != 2 {
		t.Fatalf("question = %+v", q)
	}
	s, _, _ := m.GetSession(info.ID)
	if len(s.Pending) != 1 || s.Pending[0].ID != q.ID {
		t.Fatalf("pending = %+v", s.Pending)
	}
	if err := m.Answer(info.ID, "nope", "a"); !errors.Is(err, ErrUnknownQuestion) {
		t.Fatalf("bad answer err = %v", err)
	}
	if err := m.Answer(info.ID, q.ID, "b"); err != nil {
		t.Fatal(err)
	}
	if sum := waitTurn(t, turn); sum.FinalOutput != "answer=b" {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestManagerFailedTurnAndUnknownModel(t *testing.T) {
	m, _, _ := newTestManager(t)
	if _, err := m.CreateSession(CreateSessionRequest{Model: "nope"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v", err)
	}
	info, _ := m.CreateSession(CreateSessionRequest{Model: "other"})
	if _, err := m.Send(info.ID, SendRequest{Content: "  "}); !errors.Is(err, ErrEmptyMessage) {
		t.Fatalf("empty err = %v", err)
	}
	turn, _ := m.Send(info.ID, SendRequest{Content: "fail"})
	if sum := waitTurn(t, turn); sum.Status != TurnFailed || sum.Error != "boom" || sum.Model != "other" {
		t.Fatalf("summary = %+v", sum)
	}
	if _, err := m.Send("missing", SendRequest{Content: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestManagerCloseCancelsTurnsAndEvicts(t *testing.T) {
	rt := &fakeRuntime{}
	m, _ := NewManager(Options{Runtime: rt, IdleTimeout: -1})
	info, _ := m.CreateSession(CreateSessionRequest{})
	turn, _ := m.Send(info.ID, SendRequest{Content: "block"})
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sum := waitTurn(t, turn); sum.Status != TurnCancelled {
		t.Fatalf("status = %s", sum.Status)
	}
	if rt.closed != 1 {
		t.Fatalf("conversations closed = %d", rt.closed)
	}
	if _, err := m.CreateSession(CreateSessionRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close err = %v", err)
	}

	m2, _, rt2 := newTestManager(t)
	info2, _ := m2.CreateSession(CreateSessionRequest{})
	waitTurn(t, must(m2.Send(info2.ID, SendRequest{Content: "hi"})))
	m2.evictIdle(time.Now().Add(time.Hour))
	if rt2.closed != 1 {
		t.Fatalf("evicted conversations closed = %d", rt2.closed)
	}
	if _, msgs, err := m2.GetSession(info2.ID); err != nil || len(msgs) != 2 {
		t.Fatalf("reload after eviction: %v, %d msgs", err, len(msgs))
	}
}

func must(turn *Turn, err error) *Turn {
	if err != nil {
		panic(err)
	}
	return turn
}

func TestBusLagAndGap(t *testing.T) {
	b := newBus(4)
	sub, _, _ := b.subscribe(-1)
	for i := 0; i < subscriberBuffer+1; i++ {
		b.publish(Event{Type: EventTextDelta})
	}
	n := 0
	for range sub.C {
		n++
	}
	if !sub.Lagged() || n != subscriberBuffer {
		t.Fatalf("lagged=%v drained=%d", sub.Lagged(), n)
	}
	_, backlog, gap := b.subscribe(0)
	if !gap || len(backlog) != 4 || backlog[0].Seq != int64(subscriberBuffer-2) {
		t.Fatalf("gap=%v backlog=%d first=%d", gap, len(backlog), backlog[0].Seq)
	}
}

func TestManagerScopedToWorkDir(t *testing.T) {
	store := NewMemoryStore()
	newM := func(dir string) *Manager {
		m, err := NewManager(Options{Runtime: &fakeRuntime{}, Store: store, WorkDir: dir, IdleTimeout: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		return m
	}
	a, b := newM("/proj/a"), newM("/proj/b")

	mine, _ := a.CreateSession(CreateSessionRequest{Title: "a"})
	theirs, _ := b.CreateSession(CreateSessionRequest{Title: "b"})
	cli, err := store.Create(models.CreateOpts{CWD: "/proj/a", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}

	list, err := a.ListSessions(0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	if len(list) != 2 || !ids[mine.ID] || !ids[cli.ID] {
		t.Fatalf("list = %+v, want only this workdir's sessions (%s, %s)", list, mine.ID, cli.ID)
	}

	if _, _, err := a.GetSession(cli.ID); err != nil {
		t.Fatalf("CLI session in the same workdir: %v", err)
	}
	if _, _, err := a.GetSession(theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other workdir's session: err = %v, want ErrNotFound", err)
	}
	if _, err := a.Send(theirs.ID, SendRequest{Content: "hi"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("send to other workdir's session: err = %v, want ErrNotFound", err)
	}
	if err := a.DeleteSession(theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete other workdir's session: err = %v, want ErrNotFound", err)
	}
	if _, _, err := b.GetSession(theirs.ID); err != nil {
		t.Fatalf("owner lost its session: %v", err)
	}
}

func TestManagerDropsEventsAfterTurnEnd(t *testing.T) {
	m, _, rt := newTestManager(t)
	rt.release = make(chan struct{})
	rt.lateEmitted = make(chan struct{})
	info, _ := m.CreateSession(CreateSessionRequest{})
	sub, _, _, _ := m.Subscribe(info.ID, -1)
	defer sub.Close()

	turn, _ := m.Send(info.ID, SendRequest{Content: "orphan"})
	_ = m.Cancel(info.ID)
	collectUntil(t, sub, EventTurnEnd)
	waitTurn(t, turn)
	before := m.mustBusSeq(t, info.ID)

	close(rt.release)
	<-rt.lateEmitted
	select {
	case e := <-sub.C:
		t.Fatalf("event after turn_end: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	if after := m.mustBusSeq(t, info.ID); after != before {
		t.Fatalf("bus seq moved %d -> %d after turn_end", before, after)
	}
}

func (m *Manager) mustBusSeq(t *testing.T, id string) int64 {
	t.Helper()
	s, err := m.load(id)
	if err != nil {
		t.Fatal(err)
	}
	return s.bus.lastSeq()
}

func TestManagerTimeoutStatus(t *testing.T) {
	m, err := NewManager(Options{Runtime: &fakeRuntime{}, IdleTimeout: -1, TurnTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	info, _ := m.CreateSession(CreateSessionRequest{})
	turn, _ := m.Send(info.ID, SendRequest{Content: "block"})
	if sum := waitTurn(t, turn); sum.Status != TurnTimedOut || sum.Error == "" {
		t.Fatalf("summary = %+v, want timed_out", sum)
	}
}

func TestManagerGapAfterReload(t *testing.T) {
	m, _, _ := newTestManager(t)
	info, _ := m.CreateSession(CreateSessionRequest{})
	waitTurn(t, must(m.Send(info.ID, SendRequest{Content: "hi"})))
	cursor := m.mustBusSeq(t, info.ID)

	sub, backlog, gap, _ := m.Subscribe(info.ID, cursor)
	sub.Close()
	if gap || len(backlog) != 0 {
		t.Fatalf("caught-up cursor: gap=%v backlog=%d", gap, len(backlog))
	}

	m.evictIdle(time.Now().Add(time.Hour))
	sub, backlog, gap, err := m.Subscribe(info.ID, cursor)
	if err != nil {
		t.Fatal(err)
	}
	sub.Close()
	if m.mustBusSeq(t, info.ID) <= cursor {
		t.Fatal("reloaded bus must number past every cursor of the old one")
	}
	if !gap || len(backlog) != 0 {
		t.Fatalf("cursor %d on reloaded session: gap=%v backlog=%d, want gap", cursor, gap, len(backlog))
	}
	sub, backlog, gap, _ = m.Subscribe(info.ID, 0)
	sub.Close()
	if gap || len(backlog) != 0 {
		t.Fatalf("since=0 on a fresh bus: gap=%v backlog=%d", gap, len(backlog))
	}

	// A fresh bus that has since published must still flag the stale cursor.
	waitTurn(t, must(m.Send(info.ID, SendRequest{Content: "again"})))
	sub, _, gap, _ = m.Subscribe(info.ID, cursor)
	sub.Close()
	if !gap {
		t.Fatalf("stale cursor %d ahead of reloaded bus (seq %d) must report a gap", cursor, m.mustBusSeq(t, info.ID))
	}
}

func TestBusGapRulesWithBase(t *testing.T) {
	b := newBusAt(8, 100)
	b.publish(Event{Type: EventTextDelta})
	b.publish(Event{Type: EventTextDelta})
	for _, c := range []struct {
		since int64
		gap   bool
		n     int
	}{{0, false, 2}, {100, false, 2}, {101, false, 1}, {102, false, 0}, {5, true, 2}, {99, true, 2}, {103, true, 0}} {
		_, backlog, gap := b.subscribe(c.since)
		if gap != c.gap || len(backlog) != c.n {
			t.Errorf("since=%d: gap=%v n=%d, want gap=%v n=%d", c.since, gap, len(backlog), c.gap, c.n)
		}
	}
}

func TestBusGapRules(t *testing.T) {
	b := newBus(4)
	if _, _, gap := b.subscribe(3); !gap {
		t.Fatal("empty bus, since>0: want gap")
	}
	for i := 0; i < 6; i++ {
		b.publish(Event{Type: EventTextDelta})
	}
	cases := []struct {
		since int64
		gap   bool
		n     int
	}{
		{-1, false, 0}, {0, true, 4}, {1, true, 4}, {2, false, 4}, {5, false, 1}, {6, false, 0}, {7, true, 0},
	}
	for _, c := range cases {
		_, backlog, gap := b.subscribe(c.since)
		if gap != c.gap || len(backlog) != c.n {
			t.Errorf("since=%d: gap=%v n=%d, want gap=%v n=%d", c.since, gap, len(backlog), c.gap, c.n)
		}
	}
}

// --- HTTP / WebSocket ---

func newTestServer(t *testing.T, token string) (*httptest.Server, *Manager) {
	t.Helper()
	m, _, _ := newTestManager(t)
	ts := httptest.NewServer(NewServer(ServerOptions{Manager: m, Token: token}))
	t.Cleanup(ts.Close)
	return ts, m
}

func doJSON(t *testing.T, method, url, token, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestServerAuthAndOrigin(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	if r := doJSON(t, "GET", ts.URL+"/v1/info", "", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/v1/info", "secret", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("token: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/v1/info?token=secret", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("query token: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/healthz", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/v1/info", "secret", "", map[string]string{"Origin": "https://evil.example"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/v1/info", "secret", "", map[string]string{"Origin": ts.URL}); r.StatusCode != http.StatusOK {
		t.Fatalf("same origin: %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", ts.URL+"/v1/sessions/x/messages", "secret", "", map[string]string{"Content-Type": "text/plain"}); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain post: %d", r.StatusCode)
	}
}

func TestServerNoAuthRejectsForeignHost(t *testing.T) {
	ts, _ := newTestServer(t, "")
	if r := doJSON(t, "GET", ts.URL+"/v1/info", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("loopback: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/info", nil)
	req.Host = "rebind.example:80"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding host: %d", resp.StatusCode)
	}
}

func createSessionHTTP(t *testing.T, ts *httptest.Server, token string) SessionInfo {
	t.Helper()
	r := doJSON(t, "POST", ts.URL+"/v1/sessions", token, `{"title":"t"}`, nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", r.StatusCode)
	}
	var info SessionInfo
	_ = json.NewDecoder(r.Body).Decode(&info)
	return info
}

func TestServerSendModes(t *testing.T) {
	ts, _ := newTestServer(t, "k")
	info := createSessionHTTP(t, ts, "k")
	base := ts.URL + "/v1/sessions/" + info.ID

	// SSE stream of exactly this turn.
	r := doJSON(t, "POST", base+"/messages", "k", `{"content":"one"}`, map[string]string{"Accept": "text/event-stream"})
	if ct := r.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var sseTypes []string
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			sseTypes = append(sseTypes, v)
		}
	}
	if strings.Join(sseTypes, ",") != "turn_start,tool_call_start,text_delta,text_delta,turn_end" {
		t.Fatalf("sse = %v", sseTypes)
	}

	// NDJSON.
	r = doJSON(t, "POST", base+"/messages?stream=ndjson", "k", `{"content":"two"}`, nil)
	var last Event
	dec := json.NewDecoder(r.Body)
	n := 0
	for dec.More() {
		if err := dec.Decode(&last); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 5 || last.Type != EventTurnEnd || last.Turn.FinalOutput != "echo: two" {
		t.Fatalf("ndjson n=%d last=%+v", n, last)
	}

	// Blocking.
	r = doJSON(t, "POST", base+"/messages?wait=1", "k", `{"content":"three"}`, nil)
	var sum TurnSummary
	_ = json.NewDecoder(r.Body).Decode(&sum)
	if r.StatusCode != http.StatusOK || sum.FinalOutput != "echo: three" {
		t.Fatalf("wait: %d %+v", r.StatusCode, sum)
	}

	// Async + conflict + cancel.
	r = doJSON(t, "POST", base+"/messages", "k", `{"content":"block"}`, nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("async: %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/messages", "k", `{"content":"x"}`, nil); r.StatusCode != http.StatusConflict {
		t.Fatalf("busy: %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/cancel", "k", "", nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel: %d", r.StatusCode)
	}

	// Transcript.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = doJSON(t, "GET", base, "k", "", nil)
		var got struct {
			Session  SessionInfo      `json:"session"`
			Messages []models.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Session.Status == SessionIdle {
			if len(got.Messages) != 7 {
				t.Fatalf("messages = %d", len(got.Messages))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if r := doJSON(t, "POST", base+"/messages", "k", `not json`, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", ts.URL+"/v1/sessions/nope", "k", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing: %d", r.StatusCode)
	}
	if r := doJSON(t, "DELETE", base, "k", "", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
}

func TestServerEventsReplaySSE(t *testing.T) {
	ts, m := newTestServer(t, "")
	info := createSessionHTTP(t, ts, "")
	waitTurn(t, must(m.Send(info.ID, SendRequest{Content: "hi"})))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+info.ID+"/events", nil)
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var ids []string
	for sc.Scan() && len(ids) < 2 {
		if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			ids = append(ids, v)
		}
	}
	if strings.Join(ids, ",") != "4,5" {
		t.Fatalf("replayed ids = %v", ids)
	}
}

func TestServerWebSocket(t *testing.T) {
	ts, _ := newTestServer(t, "k")
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=k"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var events []Event
	var info SessionInfo
	call := func(req WSRequest) WSMessage {
		t.Helper()
		if err := websocket.JSON.Send(conn, req); err != nil {
			t.Fatal(err)
		}
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "event" {
				if msg.Sub != info.ID {
					t.Fatalf("event %s tagged sub=%q, want %q", msg.Event.Type, msg.Sub, info.ID)
				}
				events = append(events, *msg.Event)
				continue
			}
			if msg.ID == req.ID {
				return msg
			}
		}
	}

	reply := call(WSRequest{ID: "1", Op: "create_session"})
	if !reply.OK {
		t.Fatalf("create: %+v", reply.Error)
	}
	raw, _ := json.Marshal(reply.Data)
	_ = json.Unmarshal(raw, &info)

	if r := call(WSRequest{ID: "2", Op: "subscribe", SessionID: info.ID}); !r.OK {
		t.Fatalf("subscribe: %+v", r.Error)
	}
	if r := call(WSRequest{ID: "3", Op: "send", SessionID: info.ID, Content: "ask"}); !r.OK {
		t.Fatalf("send: %+v", r.Error)
	}
	if r := call(WSRequest{ID: "x", Op: "bogus"}); r.OK || r.Error == nil {
		t.Fatalf("bogus op: %+v", r)
	}
	if r := call(WSRequest{ID: "y", Op: "send", SessionID: "missing", Content: "a"}); r.Error == nil || r.Error.Code != "not_found" {
		t.Fatalf("missing session: %+v", r)
	}

	waitEvent := func(typ EventType) Event {
		t.Helper()
		for i, e := range events {
			if e.Type == typ {
				events = events[i+1:]
				return e
			}
		}
		events = nil
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "event" && msg.Event.Type == typ {
				return *msg.Event
			}
		}
	}
	qid := waitEvent(EventQuestion).Question.ID
	if r := call(WSRequest{ID: "4", Op: "answer", SessionID: info.ID, QuestionID: qid, Answer: "a"}); !r.OK {
		t.Fatalf("answer: %+v", r.Error)
	}
	if end := waitEvent(EventTurnEnd); end.Turn.FinalOutput != "answer=a" {
		t.Fatalf("turn_end = %+v", end.Turn)
	}
}

func TestTranslateAgentEvent(t *testing.T) {
	ctx := context.Background()
	if _, ok := translateAgentEvent(ctx, agent.AgentEvent{Type: agent.AgentEventEnd}); ok {
		t.Fatal("end should be dropped")
	}
	full := &models.ToolResult{}
	e, ok := translateAgentEvent(ctx, agent.AgentEvent{Type: agent.AgentEventToolCallEnd, ToolEvent: &agent.ToolCallEvent{Name: "bash", Result: full, ResultPreview: "p"}})
	if !ok || e.Type != EventToolCallEnd || e.Tool.Result != nil || e.Tool.ResultPreview != "p" {
		t.Fatalf("tool end = %+v", e)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := translateAgentEvent(cctx, agent.AgentEvent{Type: agent.AgentEventError, Err: "context canceled"}); ok {
		t.Fatal("cancellation error should be dropped")
	}
	e, ok = translateAgentEvent(ctx, agent.AgentEvent{Type: agent.AgentEventError, Err: "x"})
	if !ok || e.Error == nil || e.Error.Message != "x" {
		t.Fatalf("error = %+v", e)
	}
}

// turn_end must be the last event of its turn even when the next Send
// lands the instant Turn.Done() fires.
func TestManagerTurnEndPrecedesNextTurn(t *testing.T) {
	// Registered before newTestManager so it runs after m.Close has
	// waited for every turn goroutine.
	t.Cleanup(func() { afterTurnDone = func() {} })
	m, _, _ := newTestManager(t)
	info, _ := m.CreateSession(CreateSessionRequest{})
	sub, _, _, _ := m.Subscribe(info.ID, -1)
	defer sub.Close()
	gsub, _, _ := m.SubscribeAll(-1)
	defer gsub.Close()

	next := make(chan *Turn, 1)
	var once sync.Once
	afterTurnDone = func() {
		once.Do(func() {
			turn, err := m.Send(info.ID, SendRequest{Content: "second"})
			if err != nil {
				t.Errorf("send from Done: %v", err)
				close(next)
				return
			}
			next <- turn
			<-turn.Done()
		})
	}

	first, err := m.Send(info.ID, SendRequest{Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, first)
	second, ok := <-next
	if !ok {
		t.FailNow()
	}
	waitTurn(t, second)

	check := func(name string, c <-chan Event) {
		var order []string
		for len(order) < 4 {
			select {
			case e := <-c:
				if e.Type == EventTurnStart || e.Type == EventTurnEnd {
					order = append(order, string(e.Type)+":"+e.TurnID)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: got %v", name, order)
			}
		}
		want := []string{"turn_start:" + first.ID, "turn_end:" + first.ID, "turn_start:" + second.ID, "turn_end:" + second.ID}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("%s order = %v, want %v", name, order, want)
			}
		}
	}
	check("session", sub.C)
	check("global", gsub.C)
}
