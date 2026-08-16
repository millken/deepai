package agent

import (
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// Subagent progress used to reach the TUI as one free-text line ("✓ read_file"),
// so the parent could show what a subagent was doing THIS INSTANT and nothing
// else — no cumulative counts, no per-tool history, and an agent type that had
// to be scraped out of the description string. subagentProgress converts the
// subagent's own AgentEvents into structured TaskEvents instead.

func toolEvt(name, args, status, errText string, durationMS int64) *ToolCallEvent {
	return &ToolCallEvent{
		Name:          name,
		ArgumentsText: args,
		Status:        models.CallStatus(status),
		Error:         errText,
		DurationMS:    durationMS,
	}
}

func TestSubagentProgress_CarriesStructuredToolFields(t *testing.T) {
	p := &subagentProgress{agentType: "code-reviewer"}

	evt, ok := p.event(AgentEvent{
		Type:      AgentEventToolCallStart,
		ToolEvent: toolEvt("read_file", `{"path":"src/app.zig"}`, "running", "", 0),
	})
	if !ok {
		t.Fatal("tool start should produce a progress event")
	}
	if evt.ToolName != "read_file" {
		t.Fatalf("ToolName = %q, want read_file", evt.ToolName)
	}
	if evt.ToolStatus != "running" {
		t.Fatalf("ToolStatus = %q, want running", evt.ToolStatus)
	}
	if evt.AgentType != "code-reviewer" {
		t.Fatalf("AgentType = %q, want code-reviewer — it must not be scraped from the description", evt.AgentType)
	}
	// The legacy free-text field stays populated so a UI that has not been
	// updated renders exactly as before.
	if evt.Message == "" {
		t.Fatal("Message must stay populated for the legacy render path")
	}
}

func TestSubagentProgress_CountsToolCallsCumulatively(t *testing.T) {
	p := &subagentProgress{}

	// Only completions count: a start followed by its end is ONE tool call.
	p.event(AgentEvent{Type: AgentEventToolCallStart, ToolEvent: toolEvt("grep", "", "running", "", 0)})
	evt, _ := p.event(AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: toolEvt("grep", "", "ok", "", 120)})
	if evt.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", evt.ToolCalls)
	}
	if evt.DurationMS != 120 {
		t.Fatalf("DurationMS = %d, want 120", evt.DurationMS)
	}

	p.event(AgentEvent{Type: AgentEventToolCallStart, ToolEvent: toolEvt("read_file", "", "running", "", 0)})
	evt, _ = p.event(AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: toolEvt("read_file", "", "ok", "", 30)})
	if evt.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want 2 after a second completed call", evt.ToolCalls)
	}
}

func TestSubagentProgress_TracksTokensFromUsage(t *testing.T) {
	p := &subagentProgress{}

	p.event(AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: toolEvt("grep", "", "ok", "", 10)})
	evt, ok := p.event(AgentEvent{
		Type:      AgentEventToolCallEnd,
		ToolEvent: toolEvt("read_file", "", "ok", "", 10),
		Usage:     &Usage{TotalTokens: 4200},
	})
	if !ok {
		t.Fatal("expected an event")
	}
	if evt.Tokens != 4200 {
		t.Fatalf("Tokens = %d, want 4200", evt.Tokens)
	}

	// A later event without usage must not reset the running total to zero.
	evt, _ = p.event(AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: toolEvt("glob", "", "ok", "", 5)})
	if evt.Tokens != 4200 {
		t.Fatalf("Tokens = %d, want the last known 4200 — a usage-less event must not clear it", evt.Tokens)
	}
}

func TestSubagentProgress_FailedToolCarriesErrorStatus(t *testing.T) {
	p := &subagentProgress{}
	evt, ok := p.event(AgentEvent{
		Type:      AgentEventToolCallEnd,
		ToolEvent: toolEvt("edit_file", "", "failed", "old_string not found", 7),
	})
	if !ok {
		t.Fatal("expected an event")
	}
	if evt.ToolStatus != "error" {
		t.Fatalf("ToolStatus = %q, want error", evt.ToolStatus)
	}
	if evt.ToolName != "edit_file" {
		t.Fatalf("ToolName = %q", evt.ToolName)
	}
}

func TestSubagentProgress_SkipsEventsWithNothingToShow(t *testing.T) {
	p := &subagentProgress{}
	if _, ok := p.event(AgentEvent{Type: AgentEventTextChunk, Text: "thinking"}); ok {
		t.Fatal("a text chunk carries no subagent progress and must be skipped")
	}
	if _, ok := p.event(AgentEvent{Type: AgentEventToolCallStart}); ok {
		t.Fatal("a tool event with no ToolEvent payload must be skipped")
	}
}

func TestSubagentProgress_ArgsSummaryIsShortAndSingleLine(t *testing.T) {
	p := &subagentProgress{}
	long := `{"path":"src/reactive/element.zig","content":"` + string(make([]byte, 400)) + `"}`
	evt, ok := p.event(AgentEvent{
		Type:      AgentEventToolCallStart,
		ToolEvent: toolEvt("write_file", long, "running", "", 0),
	})
	if !ok {
		t.Fatal("expected an event")
	}
	if len([]rune(evt.ToolArgs)) > 60 {
		t.Fatalf("ToolArgs is %d runes, want <= 60 — it renders on one status line", len([]rune(evt.ToolArgs)))
	}
	for _, r := range evt.ToolArgs {
		if r == '\n' || r == '\r' {
			t.Fatalf("ToolArgs contains a newline: %q", evt.ToolArgs)
		}
	}
}

func TestSubagentProgress_ErrorEventStillReported(t *testing.T) {
	p := &subagentProgress{}
	evt, ok := p.event(AgentEvent{Type: AgentEventError, Err: "provider exploded"})
	if !ok {
		t.Fatal("an agent error must still surface as progress")
	}
	if evt.Message == "" {
		t.Fatal("error progress must carry a message")
	}
}
