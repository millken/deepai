package bot

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// toolCallProvider asks for the "stubborn" tool on its first stream and
// answers with plain text afterwards.
type toolCallProvider struct{ calls atomic.Int32 }

func (p *toolCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *toolCallProvider) Stream(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	if p.calls.Add(1) == 1 {
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{{ID: "call_1", Name: "stubborn", Arguments: map[string]any{}}},
			Stop:      "tool_calls",
			Done:      true,
		}
	} else {
		ch <- llm.StreamChunk{Delta: "done", Stop: "stop", Done: true}
	}
	close(ch)
	return ch, nil
}

// A cancelled turn whose tool ignores ctx is abandoned after orphanWait; the
// tool's later events must never reach Emit once RunTurn has returned.
func TestAgentConversationAbandonedRunStopsEmitting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer func(d time.Duration) { orphanWait = d }(orphanWait)
	orphanWait = 50 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	lateEmitted := make(chan struct{})
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "stubborn",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			close(started)
			<-release
			subagent.EmitEvent(ctx, subagent.TaskEvent{Type: "task_progress", TaskID: "late"})
			close(lateEmitted)
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	modelReg.InjectProvider("test", "", "", &toolCallProvider{})
	rt, err := NewAgentRuntime(AgentRuntimeConfig{Models: modelReg, Tools: reg, SandboxBaseDir: t.TempDir(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := rt.NewConversation("s1")
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()

	var (
		mu       sync.Mutex
		returned bool
		late     []Event
	)
	emit := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if returned {
			late = append(late, e)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := conv.RunTurn(ctx, TurnInput{
			SessionID: "s1",
			TurnID:    "t1",
			Model:     rt.DefaultModel(),
			Messages:  []models.Message{{Role: models.RoleHuman, Content: "go", SessionID: "s1"}},
			Emit:      emit,
		})
		mu.Lock()
		returned = true
		mu.Unlock()
		errc <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("abandoned turn returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn did not give up after orphanWait")
	}

	close(release)
	select {
	case <-lateEmitted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never resumed")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(late) != 0 {
		t.Fatalf("events emitted after RunTurn returned: %+v", late)
	}
}
