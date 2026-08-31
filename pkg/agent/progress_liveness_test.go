package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// A reasoning model streams thinking_delta events (llm.StreamChunk{Progress:
// true}) for minutes before its first text or tool token. The idle watchdog
// consumed those silently, so a UI had nothing to render for the whole phase
// and every run looked hung. They must now surface as AgentEventProgress.
func TestStreamProgressChunkEmitsLivenessEvent(t *testing.T) {
	provider := &toolArgProgressProvider{fragments: 5, gap: 5 * time.Millisecond, toolName: "write_file", toolCallID: "call_1"}
	a := New(AgentConfig{LLMProvider: provider, Tools: newRegistryWithNoOpTool("write_file"), MaxToolCalls: 5})
	a.streamIdleTimeout = 2 * time.Second

	if _, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "think hard"},
	}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	var pings int
	for evt := range a.Events() {
		if evt.Type == AgentEventProgress {
			pings++
		}
	}
	if pings == 0 {
		t.Fatal("no AgentEventProgress emitted — a stream that only sends Progress chunks leaves the UI frozen")
	}
	// Throttled: 5 fragments 5ms apart span far less than progressPingInterval,
	// so exactly one ping should escape.
	if pings > 1 {
		t.Fatalf("got %d pings for a 25ms burst, want 1 — progressPingInterval (%s) is not throttling", pings, progressPingInterval)
	}
}

func TestSubagentProgress_LivenessPhasesAndThrottle(t *testing.T) {
	p := &subagentProgress{agentType: "correctness-reviewer"}

	evt, ok := p.event(AgentEvent{Type: AgentEventProgress})
	if !ok {
		t.Fatal("first thinking ping dropped — the parent needs it to start its phase clock")
	}
	if evt.Phase != subagent.PhaseThinking {
		t.Fatalf("Phase = %q, want %q", evt.Phase, subagent.PhaseThinking)
	}
	if evt.ToolName != "" {
		t.Fatalf("a liveness ping must carry no tool, got %q", evt.ToolName)
	}

	if _, ok := p.event(AgentEvent{Type: AgentEventProgress}); ok {
		t.Fatal("a second ping inside subagentPingInterval must be dropped")
	}

	p.lastPing = time.Now().Add(-2 * subagentPingInterval)
	evt, ok = p.event(AgentEvent{Type: AgentEventTextChunk, Text: "hello"})
	if !ok {
		t.Fatal("text chunk past the throttle window dropped")
	}
	if evt.Phase != subagent.PhaseGenerating {
		t.Fatalf("Phase = %q, want %q", evt.Phase, subagent.PhaseGenerating)
	}

	// Content events are never throttled: a tool call landing right after a
	// ping must still reach the parent.
	evt, ok = p.event(AgentEvent{
		Type:      AgentEventToolCallStart,
		ToolEvent: &ToolCallEvent{Name: "read_file", ArgumentsText: `{"file_path":"x.go"}`},
	})
	if !ok {
		t.Fatal("tool event dropped by the liveness throttle")
	}
	if evt.Phase != subagent.PhaseTool || evt.ToolName != "read_file" {
		t.Fatalf("tool event = %+v, want phase %q and tool read_file", evt, subagent.PhaseTool)
	}
}

// A subagent leaves its own requestTimeout unset and inherits the caller's
// deadline (pkg/chat's review does exactly this), so normalizeRunError has no
// window to report. "timed out after 0s" then describes a run that actually
// took minutes as an instant failure.
func TestTimeoutError_ZeroDurationOmitsWindow(t *testing.T) {
	err := &TimeoutError{Message: "agent request timed out"}
	if got := err.Error(); got != "agent request timed out" {
		t.Fatalf("Error() = %q, want no window mentioned", got)
	}
	withWindow := &TimeoutError{Message: "agent request timed out", Duration: 5 * time.Minute}
	if got := withWindow.Error(); !strings.Contains(got, "5m0s") {
		t.Fatalf("Error() = %q, want the real window reported", got)
	}
}
