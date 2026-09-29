package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

type fakeEmitter struct {
	events []hook.Event
}

func (f *fakeEmitter) Fire(_ context.Context, e hook.Event) {
	f.events = append(f.events, e)
}

func (f *fakeEmitter) count(kind hook.Kind) int {
	n := 0
	for _, e := range f.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

type fakeRemote struct {
	submits   []string
	interrupt int
}

func (f *fakeRemote) SubmitRemoteInput(text string) { f.submits = append(f.submits, text) }
func (f *fakeRemote) RemoteInterrupt()              { f.interrupt++ }

type fakeControlSource struct {
	cmds   []hook.Command
	states []hook.ControlState
	polls  int
}

func (f *fakeControlSource) Poll(_ context.Context, state hook.ControlState) ([]hook.Command, error) {
	f.polls++
	f.states = append(f.states, state)
	return f.cmds, nil
}

type fakeTaskCanceller struct {
	cancelled []string
}

func (f *fakeTaskCanceller) CancelTask(id string) bool {
	f.cancelled = append(f.cancelled, id)
	return true
}

func newHookRepl() *ChatRepl {
	return &ChatRepl{
		cfg:  ReplConfig{},
		sess: &models.Session{ID: "sess-hook"},
	}
}

// Acceptance #3: `reply hello` reaches SubmitRemoteInput exactly once.
func TestPollControlOnce_ReplyDispatch(t *testing.T) {
	r := newHookRepl()
	fr := &fakeRemote{}
	r.remote = fr
	r.cfg.Control = &fakeControlSource{cmds: []hook.Command{{Kind: hook.CommandReply, Arg: "hello"}}}

	r.pollControlOnce(context.Background())

	if len(fr.submits) != 1 || fr.submits[0] != "hello" {
		t.Fatalf("submits = %v, want exactly [hello]", fr.submits)
	}
	if fr.interrupt != 0 {
		t.Fatalf("interrupt fired %d times, want 0", fr.interrupt)
	}
}

// Acceptance #5: cancel-task lands on cfg.TaskCanceller exactly once.
func TestPollControlOnce_CancelTaskDispatch(t *testing.T) {
	r := newHookRepl()
	fr := &fakeRemote{}
	r.remote = fr
	fc := &fakeTaskCanceller{}
	r.cfg.TaskCanceller = fc
	r.cfg.Control = &fakeControlSource{cmds: []hook.Command{{Kind: hook.CommandCancelTask, Arg: "abc123"}}}

	r.pollControlOnce(context.Background())

	if len(fc.cancelled) != 1 || fc.cancelled[0] != "abc123" {
		t.Fatalf("cancelled = %v, want exactly [abc123]", fc.cancelled)
	}
	if len(fr.submits) != 0 || fr.interrupt != 0 {
		t.Fatalf("cancel-task must not touch the remote target: %+v", fr)
	}
}

func TestPollControlOnce_InterruptDispatch(t *testing.T) {
	r := newHookRepl()
	fr := &fakeRemote{}
	r.remote = fr
	r.cfg.Control = &fakeControlSource{cmds: []hook.Command{{Kind: hook.CommandInterrupt}}}

	r.pollControlOnce(context.Background())

	if fr.interrupt != 1 {
		t.Fatalf("interrupt = %d, want 1", fr.interrupt)
	}
}

// Acceptance #9: nil Control and nil Hooks — no poll, no delivery.
func TestPollControlOnce_NilConfigIsInert(t *testing.T) {
	r := newHookRepl()
	fr := &fakeRemote{}
	r.remote = fr
	unused := &fakeControlSource{}

	r.pollControlOnce(context.Background())

	if unused.polls != 0 {
		t.Fatalf("source polled %d times, want 0", unused.polls)
	}
	if len(fr.submits) != 0 || fr.interrupt != 0 {
		t.Fatalf("remote target touched with no control config: %+v", fr)
	}
}

// The pending ask must be visible to the control source while AskQuestion
// blocks, and cleared after — that is what tells a TG user what they are
// answering.
func TestPollControlOnce_PassesAskState(t *testing.T) {
	r := newHookRepl()
	fc := &fakeControlSource{}
	r.cfg.Control = fc

	r.pollControlOnce(context.Background())
	if fc.states[0].Asking {
		t.Fatalf("state = %+v, want Asking false with no pending ask", fc.states[0])
	}

	q := "Pick one: A or B?"
	r.remoteAsk.Store(&q)
	r.pollControlOnce(context.Background())
	if !fc.states[1].Asking || fc.states[1].Question != q {
		t.Fatalf("state = %+v, want Asking true with the question verbatim", fc.states[1])
	}
}

// Acceptance #1 (REPL side): entering AskQuestion emits the ask event before
// blocking on the user, with the question verbatim and the session id.
func TestAskHookUI_EmitsAskBeforeAnswer(t *testing.T) {
	r := newHookRepl()
	ui := &mockUI{askResult: "42"}
	r.ui = ui
	em := &fakeEmitter{}
	r.cfg.Hooks = em
	r.cfg.Control = &fakeControlSource{}

	probe := &askProbeUI{r: r, mockUI: ui}
	r.ui = probe

	ans, err := r.askUI().AskQuestion(context.Background(), "Pick one: A or B?", nil)
	if err != nil || ans != "42" {
		t.Fatalf("AskQuestion = %q, %v; want 42, nil", ans, err)
	}
	if em.count(hook.EventAsk) != 1 {
		t.Fatalf("ask events = %d, want exactly 1", em.count(hook.EventAsk))
	}
	if got := em.events[0].Message; got != "Pick one: A or B?" {
		t.Fatalf("ask message = %q, want the question verbatim", got)
	}
	if got := em.events[0].SessionID; got != "sess-hook" {
		t.Fatalf("ask session = %q, want sess-hook", got)
	}
	if !probe.sawAsk {
		t.Fatal("remoteAsk was not set while AskQuestion was blocking")
	}
	if probe.sawQuestion != "Pick one: A or B?" {
		t.Fatalf("remoteAsk question = %q, want the question verbatim", probe.sawQuestion)
	}
	if p := r.remoteAsk.Load(); p != nil {
		t.Fatalf("remoteAsk not cleared after AskQuestion returned: %q", *p)
	}
}

type askProbeUI struct {
	*mockUI
	r           *ChatRepl
	sawAsk      bool
	sawQuestion string
}

func (a *askProbeUI) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	if p := a.r.remoteAsk.Load(); p != nil {
		a.sawAsk = true
		a.sawQuestion = *p
	}
	return a.mockUI.AskQuestion(ctx, question, options)
}

// With no hook config at all, askUI returns the plain UI — the wrapper must
// not exist in the default path.
func TestAskHookUI_NoHooksReturnsPlainUI(t *testing.T) {
	r := newHookRepl()
	ui := &mockUI{}
	r.ui = ui
	if got := r.askUI(); got != tools.UserInteraction(ui) {
		t.Fatal("askUI must return the plain UI when no hooks/control are configured")
	}
}

// hookStreamProvider completes a turn with one immediate done chunk —
// mockLLMProvider's Stream returns nil and blocks the real agent loop.
type hookStreamProvider struct{}

func (p *hookStreamProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *hookStreamProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "ok"}, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

// Acceptance #2 (REPL side): one clean runTurn fires exactly one turn_end
// into the command sink, with session_id on stdin and DEEPAI_EVENT in env.
func TestRunTurn_TurnEndCommandSink(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sink.env")
	script := "#!/bin/sh\nprintf '%s' \"$DEEPAI_EVENT\" > " + out + "\ncat > " + out + ".json\n"
	scriptPath := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r, _ := newSessionCarryTestRepl(t, &hookStreamProvider{})
	r.cfg.Hooks = hook.NewDispatcher([]hook.NotificationConfig{
		{Events: []string{string(hook.EventTurnEnd)}, Command: []string{scriptPath}},
	})

	if err := r.runTurn(context.Background(), "hello", nil, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	var env string
	var stdin map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(out + ".json"); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &stdin)
			if data, err := os.ReadFile(out); err == nil {
				env = string(data)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if env != "turn_end" {
		t.Fatalf("DEEPAI_EVENT capture = %q, want turn_end", env)
	}
	if stdin == nil || stdin["session_id"] != r.sess.ID {
		t.Fatalf("stdin session_id = %v, want %q", stdin["session_id"], r.sess.ID)
	}
}

// Acceptance #13: a stale interrupt token buffered while idle must not cancel
// the next turn.
func TestRunTurnWithSignal_DrainsStaleInterrupt(t *testing.T) {
	dir := t.TempDir()
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	r, ui := newMissionRepl(t, dir)
	ui.interruptCh = ch

	ran := false
	te := r.runTurnWithSignal(context.Background(), func(ctx context.Context) error {
		ran = true
		return nil
	})
	if !ran {
		t.Fatal("fn never ran")
	}
	if te != nil {
		t.Fatalf("turnError = %+v, want nil (stale interrupt must be drained)", te)
	}
}

// Acceptance #14: the idle/session_end emission seam (fireHooks — the exact
// call Run's loop makes) is directly drivable and carries the session id.
func TestFireHooks_IdleAndSessionEnd(t *testing.T) {
	r := newHookRepl()
	em := &fakeEmitter{}
	r.cfg.Hooks = em

	r.fireHooks(hook.EventIdle, "waiting for input")
	r.fireHooks(hook.EventSessionEnd, "session ended")

	if em.count(hook.EventIdle) != 1 || em.count(hook.EventSessionEnd) != 1 {
		t.Fatalf("events = %+v, want exactly one idle and one session_end", em.events)
	}
	for _, e := range em.events {
		if e.SessionID != "sess-hook" {
			t.Fatalf("event %s session = %q, want sess-hook", e.Kind, e.SessionID)
		}
	}
}

// control.poll_seconds must actually reach the poller: test seam wins, then
// the config value, then the package default.
func TestControlPollIntervalOrDefault(t *testing.T) {
	r := newHookRepl()
	if got := r.controlPollIntervalOrDefault(); got != hook.DefaultControlPollInterval {
		t.Fatalf("default = %v, want %v", got, hook.DefaultControlPollInterval)
	}
	r.cfg.ControlPollInterval = 42 * time.Second
	if got := r.controlPollIntervalOrDefault(); got != 42*time.Second {
		t.Fatalf("config value = %v, want 42s", got)
	}
	r.controlPollInterval = 7 * time.Second
	if got := r.controlPollIntervalOrDefault(); got != 7*time.Second {
		t.Fatalf("test seam = %v, want 7s", got)
	}
}
