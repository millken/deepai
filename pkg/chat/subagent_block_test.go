package chat

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/subagent"
)

// Finished subagents used to be deleted from the live block the instant they
// finished, so a fan-out of four looked like one lonely spinner as the others
// dropped off one by one — the "只看到一个" report. Resolved tasks now stay in
// the block until the turn ends, and the whole block commits to scrollback
// once at that point rather than one line per task as it lands.

func startTask(m *tuiModel, id, desc, agentType string) {
	m.handleSubagentEvent(subagent.TaskEvent{
		Type: "task_started", TaskID: id, Description: desc, AgentType: agentType,
	})
}

func TestSubagentBlock_ResolvedTaskStaysVisibleUntilTurnEnd(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")
	startTask(m, "B", "审查 ui", "ui-perf")

	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "审查 core"})

	if len(m.subagentTasks) != 2 {
		t.Fatalf("got %d entries, want 2 — a finished task must not vanish", len(m.subagentTasks))
	}
	view := m.View().Content
	if !strings.Contains(view, "审查 core") {
		t.Fatalf("finished task disappeared from the block:\n%s", view)
	}
	if !strings.Contains(view, "审查 ui") {
		t.Fatalf("running task missing from the block:\n%s", view)
	}
	if !strings.Contains(view, "✓") {
		t.Fatalf("finished task not marked as done:\n%s", view)
	}
}

func TestSubagentBlock_TerminalEventDoesNotCommitScrollback(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")

	// Committing per task AND committing the block at turn end would print the
	// same information twice.
	if cmd := m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "审查 core"}); cmd != nil {
		t.Fatal("a terminal task event must not commit to scrollback on its own")
	}
}

func TestSubagentBlock_TurnEndCommitsOnceAndClears(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")
	startTask(m, "B", "审查 ui", "ui-perf")
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "审查 core"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_failed", TaskID: "B", Error: "boom"})

	block := m.subagentSummaryBlock()
	if block == "" {
		t.Fatal("turn end must produce a summary block for the finished tasks")
	}
	if strings.Count(block, "审查 core") != 1 {
		t.Fatalf("task A appears %d times in the block, want exactly 1:\n%s",
			strings.Count(block, "审查 core"), block)
	}
	if !strings.Contains(block, "审查 ui") {
		t.Fatalf("failed task missing from the block:\n%s", block)
	}

	m.clearSubagentBlock()
	if len(m.subagentTasks) != 0 {
		t.Fatalf("block not cleared at turn end: %+v", m.subagentTasks)
	}
	if m.subagentSummaryBlock() != "" {
		t.Fatal("a second call must produce nothing — the block commits once")
	}
}

func TestSubagentBlock_TreeCharacters(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "第一个", "t1")
	startTask(m, "B", "第二个", "t2")
	startTask(m, "C", "第三个", "t3")

	view := m.View().Content
	if !strings.Contains(view, "├─") {
		t.Fatalf("expected branch characters for non-final entries:\n%s", view)
	}
	if !strings.Contains(view, "└─") {
		t.Fatalf("expected a terminator for the final entry:\n%s", view)
	}
	if strings.Count(view, "└─") != 1 {
		t.Fatalf("exactly one entry may be the last, got %d:\n%s", strings.Count(view, "└─"), view)
	}
}

func TestSubagentBlock_ShowsStructuredProgress(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")

	m.handleSubagentEvent(subagent.TaskEvent{
		Type: "task_running", TaskID: "A", Description: "审查 core",
		AgentType: "zi-core", ToolName: "read_file", ToolArgs: `{"path":"src/app.zig"}`,
		ToolStatus: "running", ToolCalls: 12, Tokens: 47000,
	})

	view := m.View().Content
	for _, want := range []string{"zi-core", "read_file", "12"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestSubagentBlock_AgentTypeComesFromTheField(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	// A description containing brackets used to be mis-scraped for the type.
	startTask(m, "A", "审查 [核心] 模块", "zi-core")

	if got := m.subagentTasks[0].agentType; got != "zi-core" {
		t.Fatalf("agentType = %q, want zi-core — it must come from the field, not the description", got)
	}
}

func TestSubagentBlock_HistoryIsBounded(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")

	for i := 0; i < maxSubagentHistory+40; i++ {
		m.handleSubagentEvent(subagent.TaskEvent{
			Type: "task_running", TaskID: "A", AgentType: "zi-core",
			ToolName: "read_file", ToolStatus: "ok", ToolCalls: i + 1,
		})
	}

	got := len(m.subagentTasks[0].history)
	if got > maxSubagentHistory {
		t.Fatalf("history holds %d entries, want <= %d — a long-running task must not grow without bound",
			got, maxSubagentHistory)
	}
	if got != maxSubagentHistory {
		t.Fatalf("history = %d, want it filled to %d", got, maxSubagentHistory)
	}
}

func TestSubagentBlock_HistoryKeepsMostRecent(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "审查 core", "zi-core")

	for i := 0; i < maxSubagentHistory+5; i++ {
		name := "early_tool"
		if i >= maxSubagentHistory {
			name = "late_tool"
		}
		m.handleSubagentEvent(subagent.TaskEvent{
			Type: "task_running", TaskID: "A", ToolName: name, ToolStatus: "ok",
		})
	}

	h := m.subagentTasks[0].history
	if h[len(h)-1].tool != "late_tool" {
		t.Fatalf("newest entry = %q, want late_tool — the ring must drop the OLDEST", h[len(h)-1].tool)
	}
}

func TestSubagentBlock_LastEntryDetailHasNoTrailingBar(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "第一个", "t1")
	startTask(m, "B", "最后一个", "t2")

	lines := strings.Split(strings.TrimRight(m.View().Content, "\n"), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(last, "│") {
		t.Fatalf("the final entry's detail line still draws a vertical bar after └─:\n%s", last)
	}
}

func TestSubagentBlock_CompletedDetailDoesNotEchoDescription(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	startTask(m, "A", "ui 组件性能", "ui-perf")
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "ui 组件性能"})

	view := m.View().Content
	if strings.Count(view, "ui 组件性能") != 1 {
		t.Fatalf("description repeated on the detail line of a finished task:\n%s", view)
	}
}
