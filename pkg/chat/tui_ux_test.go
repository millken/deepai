package chat

import (
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

func promptingModel() (*tuiModel, chan inputResult) {
	m := newTUIModel(BannerInfo{Model: "test"})
	reply := make(chan inputResult, 1)
	m.Update(requestInputMsg{reply: reply})
	return m, reply
}

// A single Ctrl+C at an empty prompt used to end the whole session; it now
// only arms the exit, and the second press within the window exits.
func TestCtrlC_EmptyPromptNeedsSecondPressToExit(t *testing.T) {
	m, reply := promptingModel()

	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil {
		t.Fatal("first Ctrl+C should schedule the disarm timer")
	}
	select {
	case r := <-reply:
		t.Fatalf("first Ctrl+C must not exit, got %+v", r)
	default:
	}
	if !m.exitArmed || !strings.Contains(m.idleFooter(), "Ctrl+C again") {
		t.Fatalf("first Ctrl+C should arm and show the hint; footer=%q", m.idleFooter())
	}

	m.Update(key("ctrl+c"))
	select {
	case r := <-reply:
		if !errors.Is(r.err, errInterrupted) {
			t.Fatalf("second Ctrl+C err = %v, want errInterrupted", r.err)
		}
	default:
		t.Fatal("second Ctrl+C should exit")
	}
}

func TestCtrlC_ArmExpiresAndOtherKeysDisarm(t *testing.T) {
	m, reply := promptingModel()

	m.Update(key("ctrl+c"))
	m.Update(exitDisarmMsg{seq: m.exitArmSeq})
	if m.exitArmed {
		t.Fatal("disarm timer should clear the armed exit")
	}

	m.Update(key("ctrl+c"))
	m.Update(key("a"))
	if m.exitArmed {
		t.Fatal("typing should disarm the exit")
	}

	// A stale timer from an earlier arming must not cancel a fresh one.
	m.ta.SetValue("")
	m.Update(key("ctrl+c"))
	m.Update(exitDisarmMsg{seq: m.exitArmSeq - 1})
	if !m.exitArmed {
		t.Fatal("stale disarm timer cleared a newer arming")
	}
	select {
	case r := <-reply:
		t.Fatalf("no exit expected yet, got %+v", r)
	default:
	}
}

func TestCtrlC_NonEmptyPromptClearsWithoutArming(t *testing.T) {
	m, _ := promptingModel()
	m.ta.SetValue("draft")
	m.Update(key("ctrl+c"))
	if m.ta.Value() != "" || m.exitArmed {
		t.Fatalf("Ctrl+C on a draft should clear it only; value=%q armed=%v", m.ta.Value(), m.exitArmed)
	}
}

func TestRecordHistory_SkipsConsecutiveDuplicate(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.recordHistory("go test ./...")
	m.recordHistory("go test ./...")
	m.recordHistory("ls")
	m.recordHistory("go test ./...")
	want := []string{"go test ./...", "ls", "go test ./..."}
	if strings.Join(m.history, "|") != strings.Join(want, "|") {
		t.Fatalf("history = %q, want %q", m.history, want)
	}
}

func TestAskOptions_ArrowSelectAndEnterSubmits(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	reply := make(chan inputResult, 1)
	m.Update(askQuestionMsg{question: "Pick one", options: []string{"alpha", "beta", "gamma"}, reply: reply})

	if m.askSel != -1 {
		t.Fatalf("no option should be preselected, got %d", m.askSel)
	}
	m.Update(key("down"))
	m.Update(key("down"))
	if m.askSel != 1 || !strings.Contains(m.askHeader, "▸ 2. beta") {
		t.Fatalf("askSel=%d header=%q, want beta highlighted", m.askSel, m.askHeader)
	}
	m.Update(key("up"))
	m.Update(key("up"))
	if m.askSel != 2 {
		t.Fatalf("up should wrap to the last option, got %d", m.askSel)
	}

	m.Update(key("enter"))
	select {
	case r := <-reply:
		// The picker submits the option NUMBER, resolved by AskQuestion's
		// existing mapping (see TestResolveAskAnswer).
		if r.value != "3" {
			t.Fatalf("submitted %q, want 3", r.value)
		}
	default:
		t.Fatal("Enter with a highlighted option should submit it")
	}
	if m.askOptions != nil || m.askSel != -1 {
		t.Fatal("ask state should reset after submit")
	}
}

func TestAskOptions_TypedAnswerWins(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	reply := make(chan inputResult, 1)
	m.Update(askQuestionMsg{question: "Pick", options: []string{"a", "b"}, reply: reply})
	m.Update(key("down"))
	if m.askSel != 0 {
		t.Fatalf("down should highlight the first option, got %d", m.askSel)
	}
	m.Update(key("x"))
	if m.askSel != -1 || strings.Contains(m.askHeader, "▸") {
		t.Fatalf("typing should drop the highlight; askSel=%d header=%q", m.askSel, m.askHeader)
	}
	m.Update(key("enter"))
	if r := <-reply; r.value != "x" {
		t.Fatalf("typed answer should win over the highlight, got %q", r.value)
	}
}

// An option whose text is itself an in-range number must still resolve to the
// highlighted option, not to the option at that index.
func TestResolveAskAnswer(t *testing.T) {
	opts := []string{"No", "1"}
	m := newTUIModel(BannerInfo{})
	reply := make(chan inputResult, 1)
	m.Update(askQuestionMsg{question: "Proceed?", options: opts, reply: reply})
	m.Update(key("down"))
	m.Update(key("down"))
	m.Update(key("enter"))
	if got := resolveAskAnswer((<-reply).value, opts); got != "1" {
		t.Fatalf("highlighting option \"1\" resolved to %q", got)
	}
	for in, want := range map[string]string{"1": "No", " 2 ": "1", "3": "3", "maybe": "maybe", "": ""} {
		if got := resolveAskAnswer(in, opts); got != want {
			t.Errorf("resolveAskAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}

// A clock tick queued by the previous turn must not keep renewing once a wait
// starts its own chain — that ran the whole CI wait at double rate.
func TestTicks_StaleChainDiesOnRestart(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.Update(turnStartMsg{turn: 1, input: "hi"})
	stale := elapsedTickMsg{gen: m.tickGen}
	m.Update(turnEndMsg{})
	m.Update(waitStartMsg{reason: "Waiting for CI"})

	if _, cmd := m.Update(stale); cmd != nil {
		t.Fatal("stale-generation tick should not reschedule")
	}
	if _, cmd := m.Update(elapsedTickMsg{gen: m.tickGen}); cmd == nil {
		t.Fatal("current-generation tick should reschedule")
	}
}

func TestToolEndLine_ErrorStaysOnOneRow(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.width = 50
	line := m.toolEndLine(agent.AgentEvent{
		Type:      agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{Name: "bash", Error: "exit status 1\nstderr: " + strings.Repeat("boom ", 30)},
	})
	if strings.Contains(line, "\n") || lipgloss.Width(line) > m.width {
		t.Fatalf("error line should be one row within %d cols: %q", m.width, line)
	}
}

func TestStatusLines_FitNarrowTerminal(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "some-rather-long-model-name", ContextWindow: 1000})
	m.lastUsage = &agent.Usage{InputTokens: 700}
	m.Update(turnStartMsg{turn: 1, input: "hi"})
	m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolCall: &models.ToolCall{Name: "a_very_long_tool_name_here"}})
	m.setSubagentLineWithInfo("t1", "task", "coder")
	for _, w := range []int{80, 40, 10, 1} {
		m.width = w
		for name, line := range map[string]string{"busy": m.busyStatus(), "footer": m.idleFooter()} {
			if lipgloss.Width(line) > w {
				t.Errorf("width %d: %s line is %d cols: %q", w, name, lipgloss.Width(line), line)
			}
		}
		if got := m.fitLine(strings.Repeat("x", 100), 0); lipgloss.Width(got) > w {
			t.Errorf("width %d: fitLine produced %d cols", w, lipgloss.Width(got))
		}
	}
}

func TestBusyLabel_NamesActiveTool(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.Update(turnStartMsg{turn: 1, input: "hi"})
	if got := m.busyLabel(); got != "Thinking…" {
		t.Fatalf("busyLabel = %q, want Thinking…", got)
	}
	start := func(name string) {
		m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolCall: &models.ToolCall{Name: name}})
	}
	end := func(name string) {
		m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventToolCallEnd, ToolEvent: &agent.ToolCallEvent{Name: name}})
	}
	start("read_file")
	start("grep")
	if got := m.busyLabel(); got != "Running read_file +1…" {
		t.Fatalf("busyLabel = %q", got)
	}
	end("read_file")
	if got := m.busyLabel(); got != "Running grep…" {
		t.Fatalf("busyLabel = %q", got)
	}
	end("grep")
	m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventTextChunk, Text: "ok"})
	if got := m.busyLabel(); got != "Responding…" {
		t.Fatalf("busyLabel = %q", got)
	}
	m.Update(turnEndMsg{})
	if len(m.activeTools) != 0 {
		t.Fatal("turn end should clear active tools")
	}
}

func TestWaitStatus_RendersLiveLine(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	_, cmd := m.Update(waitStartMsg{reason: "Waiting for CI"})
	if cmd == nil {
		t.Fatal("wait start should start the clocks")
	}
	if !m.ticking() {
		t.Fatal("a wait should keep the spinner ticking")
	}
	if v := m.View().Content; !strings.Contains(v, "Waiting for CI…") || !strings.Contains(v, "ctrl+c to stop") {
		t.Fatalf("view missing live wait line: %q", v)
	}
	m.Update(waitEndMsg{})
	if m.ticking() {
		t.Fatal("ticking should stop after the wait")
	}
}

func TestSuggestions_WindowedPopup(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.width = 100
	m.inputVisible = true
	m.ta.SetValue("/")
	m.updateSuggestions()
	if len(m.suggestions) <= maxSuggestionLines {
		t.Skip("not enough commands to exercise the window")
	}
	out := m.renderSuggestions()
	if rows := strings.Count(out, "\n") + 1; rows != maxSuggestionLines+1 {
		t.Fatalf("popup rows = %d, want %d (+ footer)", rows, maxSuggestionLines+1)
	}
	m.suggestIdx = len(m.suggestions) - 1
	out = m.renderSuggestions()
	last := m.suggestions[len(m.suggestions)-1].Name
	if !strings.Contains(out, "/"+last) {
		t.Fatalf("selected last command %q should be visible:\n%s", last, out)
	}
}

func TestSuggestionWindow(t *testing.T) {
	cases := []struct{ n, sel, limit, start, end int }{
		{3, 0, 8, 0, 3},
		{20, 0, 8, 0, 8},
		{20, 10, 8, 6, 14},
		{20, 19, 8, 12, 20},
	}
	for _, c := range cases {
		s, e := suggestionWindow(c.n, c.sel, c.limit)
		if s != c.start || e != c.end {
			t.Errorf("suggestionWindow(%d,%d,%d) = %d,%d want %d,%d", c.n, c.sel, c.limit, s, e, c.start, c.end)
		}
	}
}

func TestToolEndLine_FitsTerminalWidth(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.width = 60
	line := m.toolEndLine(agent.AgentEvent{
		Type:      agent.AgentEventToolCallEnd,
		ToolEvent: &agent.ToolCallEvent{Name: "read_file", ResultPreview: strings.Repeat("word ", 40) + "\nsecond line"},
	})
	if strings.Contains(line, "\n") {
		t.Fatalf("preview should stay on one row: %q", line)
	}
	if w := lipgloss.Width(line); w > m.width {
		t.Fatalf("line width %d exceeds terminal %d: %q", w, m.width, line)
	}
}

func TestLiveTail_BoundsWrappedRows(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.width = 40
	long := strings.Repeat("abcdefghij ", 200)
	if rows := strings.Count(m.liveTail(long, 16), "\n") + 1; rows > 16 {
		t.Fatalf("live tail rows = %d, want <= 16", rows)
	}
}

func TestFormatDurationAndTokens(t *testing.T) {
	for d, want := range map[time.Duration]string{
		8400 * time.Millisecond: "8.4s",
		187 * time.Second:       "3m07s",
		3720 * time.Second:      "1h02m",
	} {
		if got := formatDuration(d, true); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := formatDuration(42*time.Second, false); got != "42s" {
		t.Errorf("formatDuration(42s,false) = %q", got)
	}
	for n, want := range map[int]string{999: "999", 1940: "1.9k", 47_000: "47.0k", 250_000: "250k"} {
		if got := formatTokensPrecise(n); got != want {
			t.Errorf("formatTokensPrecise(%d) = %q, want %q", n, got, want)
		}
	}
}
