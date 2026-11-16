package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

func runCodeStartEvent(script string) agent.AgentEvent {
	call := models.ToolCall{
		ID:        "rc_1",
		Name:      "run_code",
		Arguments: map[string]any{"script": script},
		Status:    models.CallStatusRunning,
		StartedAt: time.Now().UTC(),
	}
	return agent.AgentEvent{
		Type:     agent.AgentEventToolCallStart,
		ToolCall: &call,
		ToolEvent: &agent.ToolCallEvent{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
			Status:    models.CallStatusRunning,
		},
	}
}

func runCodeEndEvent(content string) agent.AgentEvent {
	res := models.ToolResult{
		CallID:   "rc_1",
		ToolName: "run_code",
		Status:   models.CallStatusCompleted,
		Content:  content,
	}
	return agent.AgentEvent{
		Type: agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{
			ID:            "rc_1",
			Name:          "run_code",
			Status:        models.CallStatusCompleted,
			Result:        &res,
			ResultPreview: "run_code ok…",
		},
	}
}

// Criterion 30, start half: the start line shows a script preview block —
// the user must see WHAT the script does before its writes land (§7.2).
func TestToolStartLineRunCodeShowsScriptPreview(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	line := m.toolStartLine(runCodeStartEvent("const hits = await tools.grep({pattern: \"Close\"});\nreturn hits.data.matches.length;\n"))
	if line == "" {
		t.Fatal("toolStartLine(run_code) = \"\"")
	}
	if !strings.Contains(line, "run_code") {
		t.Fatalf("start line missing tool name:\n%s", line)
	}
	if !strings.Contains(line, "tools.grep") {
		t.Fatalf("start line missing script preview content:\n%s", line)
	}
}

// Criterion 30, end half: the end line carries the writes: summary — the one
// line that must survive every truncation (§6.3).
func TestToolEndLineRunCodeShowsWritesSummary(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	content := "run_code ok — 2 tool calls in 0.1s (grep×1, edit_file×1)\nresult:\n1\nwrites: edit_file pkg/tools/builtin/grep.go [completed]"
	line := m.toolEndLine(runCodeEndEvent(content))
	if line == "" {
		t.Fatal("toolEndLine(run_code) = \"\"")
	}
	if !strings.Contains(line, "run_code") {
		t.Fatalf("end line missing tool name:\n%s", line)
	}
	if !strings.Contains(line, "writes: edit_file pkg/tools/builtin/grep.go [completed]") {
		t.Fatalf("end line missing writes: summary:\n%s", line)
	}
}

// No writes renders "writes: none" — distinguishable from "truncated away".
func TestToolEndLineRunCodeWritesNone(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	line := m.toolEndLine(runCodeEndEvent("run_code ok — 1 tool call in 0.0s (grep×1)\nresult:\n3\nwrites: none"))
	if !strings.Contains(line, "writes: none") {
		t.Fatalf("end line missing writes: none:\n%s", line)
	}
}

// A failed run_code still shows its writes: the error content carries it.
func TestToolEndLineRunCodeFailedKeepsWrites(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	res := models.ToolResult{
		CallID:   "rc_1",
		ToolName: "run_code",
		Status:   models.CallStatusFailed,
		Content:  "run_code timeout — 2 tool calls in 1.0s (bash×2)\nerror:\nscript timed out\nwrites: bash \"go test ./...\" [interrupted]",
		Error:    "run_code timeout — 2 tool calls in 1.0s (bash×2)\nerror:\nscript timed out\nwrites: bash \"go test ./...\" [interrupted]",
	}
	evt := agent.AgentEvent{
		Type: agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{
			ID:     "rc_1",
			Name:   "run_code",
			Status: models.CallStatusFailed,
			Result: &res,
			Error:  res.Error,
		},
	}
	line := m.toolEndLine(evt)
	if !strings.Contains(line, "writes: bash") {
		t.Fatalf("failed run_code end line lost the writes summary:\n%s", line)
	}
}
