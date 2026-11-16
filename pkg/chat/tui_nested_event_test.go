package chat

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

func nestedStartEvent(parentID, tool string) agent.AgentEvent {
	return agent.AgentEvent{
		Type: agent.AgentEventToolCallStart,
		ToolEvent: &agent.ToolCallEvent{
			ID:       parentID + ".1",
			Name:     tool,
			ParentID: parentID,
			Status:   models.CallStatusRunning,
		},
	}
}

func nestedEndEvent(parentID, tool string, failed bool) agent.AgentEvent {
	st := models.CallStatusCompleted
	errText := ""
	if failed {
		st = models.CallStatusFailed
		errText = "boom"
	}
	return agent.AgentEvent{
		Type: agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{
			ID:         parentID + ".1",
			Name:       tool,
			ParentID:   parentID,
			Status:     st,
			DurationMS: 41,
			Error:      errText,
		},
	}
}

// Nested (ParentID-set) events render indented under the outer run_code line
// — same transcript tree as subagent visibility (§7.2 Phase 2).
func TestNestedToolEventRendersIndented(t *testing.T) {
	m := newTUIModel(BannerInfo{})

	startLine := m.nestedToolLine(nestedStartEvent("rc_1", "grep").ToolEvent, true)
	if !strings.Contains(startLine, "grep") {
		t.Fatalf("start line missing tool name:\n%q", startLine)
	}
	if !strings.Contains(startLine, "rc_1.1") {
		t.Fatalf("start line missing nested id:\n%q", startLine)
	}
	top := m.toolStartLine(runCodeStartEvent("return 1;"))
	if indentOf(startLine) <= indentOf(top) {
		t.Fatalf("nested line not indented deeper than top level:\n%q\n%q", startLine, top)
	}

	endLine := m.nestedToolLine(nestedEndEvent("rc_1", "grep", false).ToolEvent, false)
	if !strings.Contains(endLine, "grep") || !strings.Contains(endLine, "41") {
		t.Fatalf("end line missing name/duration:\n%q", endLine)
	}
	failLine := m.nestedToolLine(nestedEndEvent("rc_1", "bash", true).ToolEvent, false)
	if !strings.Contains(failLine, "boom") {
		t.Fatalf("failed nested end lost error:\n%q", failLine)
	}
}

func indentOf(s string) int {
	// lipgloss prefixes ANSI color codes before the padding — strip them so
	// the measure is of the rendered line's visible indent.
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			break
		}
		s = s[:i] + s[i+j+1:]
	}
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
		} else {
			break
		}
	}
	return n
}

// Nested events must not touch the top-level tool bookkeeping: a nested grep
// start must not append "grep" to activeTools, and a nested grep end must not
// finishActiveTool-remove an entry that belongs to the outer run_code.
func TestNestedEventsDoNotTouchActiveTools(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.activeTools = []string{"run_code"}

	m.handleAgentEvent(nestedStartEvent("rc_1", "grep"))
	if len(m.activeTools) != 1 || m.activeTools[0] != "run_code" {
		t.Fatalf("nested start polluted activeTools: %v", m.activeTools)
	}

	m.handleAgentEvent(nestedEndEvent("rc_1", "run_code", false))
	if len(m.activeTools) != 1 || m.activeTools[0] != "run_code" {
		t.Fatalf("nested end stole the outer run_code slot: %v", m.activeTools)
	}
}
