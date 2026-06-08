package chat

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
)

// feed sends a text chunk through the agent-event handler and returns whether a
// commit command was produced.
func TestStreamingBufferCommitsCompleteLines(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})

	// Partial line stays buffered, no commit.
	if cmd := m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventTextChunk, Text: "hello"}); cmd != nil {
		t.Fatalf("expected no commit for partial line")
	}
	if m.aiPartial != "hello" {
		t.Fatalf("aiPartial = %q, want %q", m.aiPartial, "hello")
	}

	// Newline commits the complete line, keeps the remainder.
	if cmd := m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventTextChunk, Text: " world\nrest"}); cmd == nil {
		t.Fatalf("expected commit when a newline arrives")
	}
	if m.aiPartial != "rest" {
		t.Fatalf("aiPartial = %q, want %q", m.aiPartial, "rest")
	}

	// flushPartial drains and clears.
	if got := m.flushPartial(); !strings.Contains(got, "rest") {
		t.Fatalf("flushPartial = %q, want it to contain %q", got, "rest")
	}
	if m.aiPartial != "" {
		t.Fatalf("aiPartial not cleared after flush: %q", m.aiPartial)
	}
	if m.flushPartial() != "" {
		t.Fatalf("flushPartial on empty buffer should return empty")
	}
}

func TestToolEndLineFormatting(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	evt := agent.AgentEvent{
		Type: agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{
			Name:          "bash",
			DurationMS:    1500,
			ResultPreview: "ok",
		},
	}
	line := m.toolEndLine(evt)
	if !strings.Contains(line, "✓") || !strings.Contains(line, "bash") || !strings.Contains(line, "1.5s") {
		t.Fatalf("tool end line missing parts: %q", line)
	}

	errEvt := agent.AgentEvent{
		Type:      agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{Name: "bash", Error: "boom"},
	}
	if l := m.toolEndLine(errEvt); !strings.Contains(l, "✗") || !strings.Contains(l, "boom") {
		t.Fatalf("error tool end line missing parts: %q", l)
	}
}

func TestRecordHistoryDedupAndCap(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.recordHistory("first")
	m.recordHistory("second")
	if len(m.history) != 2 || m.history[0] != "second" {
		t.Fatalf("history = %v, want newest-first [second first]", m.history)
	}
	m.recordHistory("   ") // blank ignored
	if len(m.history) != 2 {
		t.Fatalf("blank input should not be recorded: %v", m.history)
	}
	if m.histIdx != -1 {
		t.Fatalf("histIdx should reset to -1, got %d", m.histIdx)
	}
}

func TestSubmitInputDeliversAndHides(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	reply := make(chan inputResult, 1)
	m.inputReply = reply
	m.inputVisible = true
	m.askActive = true

	m.submitInput(inputResult{value: "answer"})

	select {
	case r := <-reply:
		if r.value != "answer" {
			t.Fatalf("got %q, want %q", r.value, "answer")
		}
	default:
		t.Fatal("submitInput did not deliver to reply channel")
	}
	if m.inputVisible || m.askActive || m.inputReply != nil {
		t.Fatalf("submitInput should reset input state")
	}
}
