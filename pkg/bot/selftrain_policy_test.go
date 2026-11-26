package bot

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/tools"
)

type inspectToolsProvider struct {
	mu        sync.Mutex
	seenTools [][]string
}

func (p *inspectToolsProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *inspectToolsProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	p.mu.Lock()
	p.seenTools = append(p.seenTools, names)
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Delta: "ok", Stop: "stop", Done: true}
	close(ch)
	return ch, nil
}

func TestTrainingGuardsAndTurnDisableSelfTrain(t *testing.T) {
	ctx := context.Background()
	for _, cp := range []rune{rune(7), rune(8203)} {
		if err := validateTrainingTextSafety("a" + string(cp) + "b"); err == nil {
			t.Fatalf("expected codepoint %d to be rejected", cp)
		}
	}

	prov := &inspectToolsProvider{}
	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	modelReg.InjectProvider("test", "", "", prov)
	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:         modelReg,
		Tools:          tools.NewRegistry(),
		SandboxBaseDir: t.TempDir(),
		BotDataDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	sess, err := mgr.CreateSession(CreateSessionRequest{Title: "chan"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := mgr.SendForBot("default", sess.ID, SendRequest{Content: "hi"}, SendOptions{DisableSelfTrain: true})
	if err != nil {
		t.Fatal(err)
	}
	<-turn.Done()

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.seenTools) != 1 {
		t.Fatalf("seenTools turns = %d, want 1", len(prov.seenTools))
	}
	if slices.Contains(prov.seenTools[0], "save_bot_skill") || slices.Contains(prov.seenTools[0], "update_bot_persona") {
		t.Fatalf("DisableSelfTrain turn still exposed self-training tools: %v", prov.seenTools[0])
	}
}
