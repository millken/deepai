package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/subagent"
)

// The "卡住" report: a reviewer's status line showed `read_file {...} · 9 工具
// · 206s` unchanged for minutes. The tool had long finished — the model was
// thinking — but a finished tool stayed pinned to the line, so nothing on
// screen described what was actually happening.
func TestSubagentDetail_ThinkingPhaseReplacesFinishedTool(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "Adversarial correctness review", "correctness-reviewer")

	m.handleSubagentEvent(subagent.TaskEvent{
		Type: "task_running", TaskID: "A", Phase: subagent.PhaseTool,
		ToolName: "read_file", ToolArgs: `{"file_path":"x.go"}`, ToolStatus: "running",
		Message: "⚙ read_file",
	})
	if view := m.View().Content; !strings.Contains(view, "read_file") {
		t.Fatalf("a running tool must show on the line:\n%s", view)
	}

	m.handleSubagentEvent(subagent.TaskEvent{
		Type: "task_running", TaskID: "A", Phase: subagent.PhaseTool,
		ToolName: "read_file", ToolArgs: `{"file_path":"x.go"}`, ToolStatus: "ok",
		DurationMS: 12, ToolCalls: 9, Message: "✓ read_file",
	})
	view := m.View().Content
	if strings.Contains(view, "read_file {") {
		t.Fatalf("a finished tool call must not stay pinned to the status line:\n%s", view)
	}
	if !strings.Contains(view, "思考中") {
		t.Fatalf("the model turn after a tool must be named:\n%s", view)
	}
	if !strings.Contains(view, "9 工具") {
		t.Fatalf("running totals lost:\n%s", view)
	}

	// A generating ping upgrades the phase: the stream is producing output.
	m.handleSubagentEvent(subagent.TaskEvent{
		Type: "task_running", TaskID: "A", Phase: subagent.PhaseGenerating, Message: "⋯ generating",
	})
	if view := m.View().Content; !strings.Contains(view, "生成中") {
		t.Fatalf("generating phase not shown:\n%s", view)
	}
}

// A resolved task keeps its slot until the turn ends. Its clock must stop
// there too — the failed reviewer above reported "796s" for a run the 5-minute
// deadline had killed 8 minutes earlier.
func TestSubagentTaskAge_FreezesOnResolve(t *testing.T) {
	start := time.Now().Add(-10 * time.Minute)
	live := subagentTaskLine{startedAt: start}
	done := subagentTaskLine{startedAt: start, finishedAt: start.Add(90 * time.Second), status: "failed"}

	now := time.Now()
	if got := subagentTaskAge(live, now); got < 9*time.Minute {
		t.Fatalf("a running task's clock must keep going, got %s", got)
	}
	if got := subagentTaskAge(done, now); got != 90*time.Second {
		t.Fatalf("age = %s, want the 90s the task actually took", got)
	}
	if line := subagentSummaryLine(done, true); !strings.Contains(line, "90s") {
		t.Fatalf("summary must report the real duration: %s", line)
	}
}

func TestSubagentResolve_StampsFinishedAt(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "review", "correctness-reviewer")
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_timed_out", TaskID: "A", Error: "agent request timed out"})

	if m.subagentTasks[0].finishedAt.IsZero() {
		t.Fatal("a resolved task must stamp finishedAt so its clock stops")
	}
	if m.subagentTasks[0].phase != "" {
		t.Fatalf("phase = %q, want cleared on resolve", m.subagentTasks[0].phase)
	}
}

// Silence is the one thing the old line could not distinguish from work.
func TestSubagentDetail_StallHint(t *testing.T) {
	now := time.Now()
	t.Run("quiet", func(t *testing.T) {
		task := subagentTaskLine{
			startedAt: now.Add(-5 * time.Minute), phase: subagent.PhaseThinking,
			phaseSince: now.Add(-2 * time.Minute), lastPing: now.Add(-2 * time.Minute),
		}
		if line := subagentDetailLine(task, now, "│"); !strings.Contains(line, "无响应") {
			t.Fatalf("a task silent for 2 minutes must say so: %s", line)
		}
	})
	t.Run("slow tool is not a stall", func(t *testing.T) {
		// A tool call emits no pings while it runs; only its own clock moves.
		task := subagentTaskLine{
			startedAt: now.Add(-5 * time.Minute), phase: subagent.PhaseTool,
			currentTool: "bash", phaseSince: now.Add(-3 * time.Minute), lastPing: now.Add(-3 * time.Minute),
		}
		if line := subagentDetailLine(task, now, "│"); strings.Contains(line, "无响应") {
			t.Fatalf("a long-running tool must not be reported as unresponsive: %s", line)
		}
	})
	t.Run("alive", func(t *testing.T) {
		task := subagentTaskLine{
			startedAt: now.Add(-5 * time.Minute), phase: subagent.PhaseThinking,
			phaseSince: now.Add(-2 * time.Minute), lastPing: now.Add(-time.Second),
		}
		if line := subagentDetailLine(task, now, "│"); strings.Contains(line, "无响应") {
			t.Fatalf("a task pinging every second is not stalled: %s", line)
		}
	})
}
