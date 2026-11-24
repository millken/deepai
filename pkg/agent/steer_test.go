package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

type testSteerSource struct {
	mu       sync.Mutex
	msgs     []SteerMessage
	wrappedUp bool
}

func (s *testSteerSource) Push(m SteerMessage) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
}

func (s *testSteerSource) DrainSteer() []SteerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return nil
	}
	out := s.msgs
	s.msgs = nil
	return out
}

func (s *testSteerSource) MarkWrapUp() {
	s.mu.Lock()
	s.wrappedUp = true
	s.mu.Unlock()
}

func (s *testSteerSource) remaining() []SteerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SteerMessage(nil), s.msgs...)
}

type steerTestProvider struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (p *steerTestProvider) Name() string { return "mock" }
func (p *steerTestProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}
func (p *steerTestProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	callIdx := len(p.requests)
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 2)
	go func() {
		defer close(ch)
		if callIdx == 0 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "tc1", Name: "work", Arguments: map[string]any{}}},
				Stop:      "tool_calls",
				Done:      true,
			}
			return
		}
		ch <- llm.StreamChunk{Delta: "final steered reply", Stop: "stop", Done: true}
	}()
	return ch, nil
}

func TestSteerInjectedAtReActBoundary(t *testing.T) {
	steer := &testSteerSource{}
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name:        "work",
		Description: "does work",
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			// Simulate user sending a mid-turn steer while the tool is running.
			steer.Push(SteerMessage{ID: "st_1", Content: "pivot to v2 handler only"})
			return models.ToolResult{Status: models.CallStatusCompleted, Content: "tool done"}, nil
		},
	})

	prov := &steerTestProvider{}
	a := New(AgentConfig{
		LLMProvider: prov,
		Tools:       reg,
		Steer:       steer,
	})

	var steerEvts []AgentEvent
	doneDrain := make(chan struct{})
	go func() {
		defer close(doneDrain)
		for evt := range a.Events() {
			if evt.Type == AgentEventSteerApplied {
				steerEvts = append(steerEvts, evt)
			}
		}
	}()

	res, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "start task"},
	})
	<-doneDrain
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "final steered reply" {
		t.Fatalf("FinalOutput = %q", res.FinalOutput)
	}

	// Second LLM request must see the steered human message after the tool result.
	if len(prov.requests) != 2 {
		t.Fatalf("LLM requests = %d, want 2", len(prov.requests))
	}
	var sawSteerInReq bool
	for _, m := range prov.requests[1].Messages {
		if m.Role == models.RoleHuman && strings.Contains(m.Content, "pivot to v2 handler only") {
			sawSteerInReq = true
		}
	}
	if !sawSteerInReq {
		t.Fatalf("second LLM request did not contain steered human message: %+v", prov.requests[1].Messages)
	}

	// Persisted RunResult.Messages must include the steer message with source=steer.
	var sawPersisted bool
	for _, m := range res.Messages {
		if m.ID == "st_1" && m.Role == models.RoleHuman && m.Metadata["source"] == "steer" {
			sawPersisted = true
		}
	}
	if !sawPersisted {
		t.Fatalf("RunResult.Messages missing persisted steer message: %+v", res.Messages)
	}
	if len(steerEvts) != 1 || steerEvts[0].MessageID != "st_1" || steerEvts[0].Text != "pivot to v2 handler only" {
		t.Fatalf("steer events = %+v", steerEvts)
	}
}

func TestSteerSkippedDuringWrapUpAndNotifiesCaller(t *testing.T) {
	steer := &testSteerSource{}
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name:        "work",
		Description: "does work",
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			steer.Push(SteerMessage{ID: "st_late", Content: "too late for this turn"})
			return models.ToolResult{Status: models.CallStatusCompleted, Content: "tool done"}, nil
		},
	})

	prov := &steerTestProvider{}
	// MaxToolCalls=1 forces wrapUp immediately after the first tool call completes.
	a := New(AgentConfig{
		LLMProvider:  prov,
		Tools:        reg,
		MaxToolCalls: 1,
		Steer:        steer,
	})
	go func() {
		for range a.Events() {
		}
	}()

	res, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "start task"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.BudgetExhausted {
		t.Fatalf("expected BudgetExhausted=true")
	}
	if !steer.wrappedUp {
		t.Fatalf("expected MarkWrapUp() to be called on SteerSource")
	}
	// Steer message must NOT be consumed during wrapUp so caller can promote it to queue.
	rem := steer.remaining()
	if len(rem) != 1 || rem[0].ID != "st_late" {
		t.Fatalf("remaining unconsumed steer = %+v, want [st_late]", rem)
	}
}
