package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/millken/deepai/pkg/subagent"
)

// Task mode is a separate mode reached with Ctrl+T rather than a hijack of the
// arrow keys: up/down already drive completion candidates and input history,
// and stealing them whenever a subagent happens to be running would break
// ordinary typing for the whole duration of a fan-out.

func key(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	switch s {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+t":
		return tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}
	case "ctrl+x":
		return tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
	}
	panic("unmapped key " + s)
}

func modelWithTasks(n int) *tuiModel {
	m := newTUIModel(BannerInfo{Model: "test"})
	names := []string{"第一个", "第二个", "第三个"}
	for i := 0; i < n; i++ {
		m.handleSubagentEvent(subagent.TaskEvent{
			Type: "task_started", TaskID: string(rune('A' + i)), Description: names[i%len(names)],
			AgentType: "t",
		})
	}
	return m
}

func TestTaskMode_CtrlTNoopWithoutTasks(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	m.handleKey(key("ctrl+t"))
	if m.taskMode {
		t.Fatal("task mode must not engage when there are no tasks to inspect")
	}
}

func TestTaskMode_CtrlTTogglesAndEscExits(t *testing.T) {
	m := modelWithTasks(2)

	m.handleKey(key("ctrl+t"))
	if !m.taskMode {
		t.Fatal("ctrl+t should enter task mode")
	}
	m.handleKey(key("ctrl+t"))
	if m.taskMode {
		t.Fatal("ctrl+t should toggle back out")
	}

	m.handleKey(key("ctrl+t"))
	m.handleKey(key("esc"))
	if m.taskMode {
		t.Fatal("esc should leave task mode")
	}
}

func TestTaskMode_ArrowsMoveSelectionAndClamp(t *testing.T) {
	m := modelWithTasks(3)
	m.handleKey(key("ctrl+t"))

	if m.taskSel != 0 {
		t.Fatalf("taskSel = %d, want the first entry selected on entry", m.taskSel)
	}
	m.handleKey(key("down"))
	m.handleKey(key("down"))
	if m.taskSel != 2 {
		t.Fatalf("taskSel = %d, want 2", m.taskSel)
	}
	// Clamps rather than wrapping or running off the end.
	m.handleKey(key("down"))
	if m.taskSel != 2 {
		t.Fatalf("taskSel = %d, want it clamped at the last entry", m.taskSel)
	}
	m.handleKey(key("up"))
	m.handleKey(key("up"))
	m.handleKey(key("up"))
	if m.taskSel != 0 {
		t.Fatalf("taskSel = %d, want it clamped at 0", m.taskSel)
	}
}

func TestTaskMode_EnterTogglesExpansionOfSelected(t *testing.T) {
	m := modelWithTasks(2)
	m.handleKey(key("ctrl+t"))
	m.handleKey(key("down"))
	m.handleKey(key("enter"))

	if !m.subagentTasks[1].expanded {
		t.Fatal("enter should expand the selected task")
	}
	if m.subagentTasks[0].expanded {
		t.Fatal("enter must only affect the selected task")
	}
	m.handleKey(key("enter"))
	if m.subagentTasks[1].expanded {
		t.Fatal("enter should collapse an expanded task")
	}
}

// The regression this mode exists to avoid.
func TestTaskMode_ArrowsUntouchedOutsideTaskMode(t *testing.T) {
	m := modelWithTasks(2)
	m.inputVisible = true
	m.history = []string{"第二条", "第一条"}
	m.histIdx = -1

	// Not in task mode: up must still walk input history even though two
	// subagents are running.
	m.handleKey(key("up"))
	if m.taskMode {
		t.Fatal("up must not engage task mode")
	}
	if m.ta.Value() != "第二条" {
		t.Fatalf("input = %q, want the previous history entry — arrow keys must keep working during a fan-out", m.ta.Value())
	}
}

func TestTaskMode_SelectionAndHintRendered(t *testing.T) {
	m := modelWithTasks(2)
	m.handleKey(key("ctrl+t"))

	view := m.View().Content
	if !strings.Contains(view, "Ctrl+X") {
		t.Fatalf("task mode hint missing from the view:\n%s", view)
	}
	if !strings.Contains(view, taskSelectionMarker) {
		t.Fatalf("selected entry not marked:\n%s", view)
	}
	if strings.Count(view, taskSelectionMarker) != 1 {
		t.Fatalf("exactly one entry may be marked, got %d:\n%s",
			strings.Count(view, taskSelectionMarker), view)
	}
}

func TestTaskMode_NoHintWhenNotInMode(t *testing.T) {
	m := modelWithTasks(2)
	if strings.Contains(m.View().Content, "Ctrl+X") {
		t.Fatal("the task-mode hint must not show outside the mode")
	}
}

func TestTaskMode_SelectionSurvivesTaskCountShrinking(t *testing.T) {
	m := modelWithTasks(3)
	m.handleKey(key("ctrl+t"))
	m.handleKey(key("down"))
	m.handleKey(key("down"))

	// The turn ends and the block is cleared while the mode is still engaged.
	m.clearSubagentBlock()
	view := m.View().Content // must not panic on a stale index

	if m.taskMode {
		t.Fatal("task mode should disengage once there is nothing to inspect")
	}
	if strings.Contains(view, taskSelectionMarker) {
		t.Fatalf("stale selection marker rendered:\n%s", view)
	}
}

func TestTaskMode_CancelRequestsSelectedTaskOnly(t *testing.T) {
	m := modelWithTasks(3)
	m.handleKey(key("ctrl+t"))
	m.handleKey(key("down"))
	m.handleKey(key("ctrl+x"))

	select {
	case id := <-m.cancelTaskCh:
		if id != "B" {
			t.Fatalf("cancel requested for %q, want B (the selected task)", id)
		}
	default:
		t.Fatal("ctrl+x should request cancellation of the selected task")
	}
}

func TestTaskMode_CancelIgnoredForFinishedTask(t *testing.T) {
	m := modelWithTasks(2)
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "第一个"})
	m.handleKey(key("ctrl+t"))
	m.handleKey(key("ctrl+x")) // A is selected and already done

	select {
	case id := <-m.cancelTaskCh:
		t.Fatalf("cancel sent for an already-finished task %q", id)
	default:
	}
}
