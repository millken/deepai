package chat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// Streamed text accumulates without committing; the whole message is rendered
// and committed at the next boundary (via flushPartial).
func TestStreamingBuffersUntilFlush(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})

	// Chunks accumulate; nothing is committed mid-stream, even across newlines.
	if cmd := m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventTextChunk, Text: "hello"}); cmd != nil {
		t.Fatalf("expected no commit mid-stream")
	}
	if cmd := m.handleAgentEvent(agent.AgentEvent{Type: agent.AgentEventTextChunk, Text: " world\nrest"}); cmd != nil {
		t.Fatalf("expected no commit mid-stream across newline")
	}
	if m.aiPartial != "hello world\nrest" {
		t.Fatalf("aiPartial = %q, want full accumulated text", m.aiPartial)
	}

	// flushPartial drains, records the raw text for re-emit, and clears.
	if got := m.flushPartial(); !strings.Contains(got, "rest") {
		t.Fatalf("flushPartial = %q, want it to contain %q", got, "rest")
	}
	if m.aiPartial != "" {
		t.Fatalf("aiPartial not cleared after flush: %q", m.aiPartial)
	}
	if m.lastAIRaw != "hello world\nrest" {
		t.Fatalf("lastAIRaw = %q, want the raw message for raw re-emit", m.lastAIRaw)
	}
	if m.flushPartial() != "" {
		t.Fatalf("flushPartial on empty buffer should return empty")
	}
}

func TestMarkdownRenderToggle(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})
	m.width = 80
	if !m.renderMD {
		t.Fatal("markdown rendering should default on")
	}
	// Rendered markdown decorates a heading (ANSI styling), so it differs from raw.
	rendered := m.renderMarkdown("# Title\n\nsome **bold** text")
	if rendered == "" {
		t.Fatal("renderMarkdown returned empty for valid markdown")
	}
	// Raw mode: flushPartial must return the text verbatim (copyable).
	m.renderMD = false
	m.aiPartial = "# Title\n\ncode: `x := 1`"
	got := m.flushPartial()
	if !strings.Contains(got, "# Title") || !strings.Contains(got, "`x := 1`") {
		t.Fatalf("raw mode should preserve markdown source, got %q", got)
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

func TestToolStartLineBashBlock(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	cmd := "zig build 2>&1 | grep error\necho done"
	evt := agent.AgentEvent{
		Type: agent.AgentEventToolCallStart,
		ToolCall: &models.ToolCall{
			Name:      "bash",
			Arguments: map[string]any{"command": cmd},
		},
	}
	line := m.toolStartLine(evt)
	if !strings.Contains(line, "Bash") {
		t.Fatalf("bash block missing header: %q", line)
	}
	// Both source lines must appear in the rendered (highlighted) block. Tokens
	// are separated by ANSI codes, so check the individual words, not substrings.
	for _, tok := range []string{"zig", "build", "grep", "error", "echo", "done"} {
		if !strings.Contains(line, tok) {
			t.Fatalf("bash block dropped token %q: %q", tok, line)
		}
	}
	// Two source lines → two bar prefixes, plus the header line.
	if got := strings.Count(line, "\n"); got != 2 {
		t.Fatalf("bash block line count = %d, want 2 (header + 2 cmd lines): %q", got, line)
	}
}

func TestBashCommandBlockCapsLongInput(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	var sb strings.Builder
	for i := 0; i < maxToolCmdLines+5; i++ {
		sb.WriteString("echo line\n")
	}
	block := m.bashCommandBlock(sb.String())
	if !strings.Contains(block, "5 more lines") {
		t.Fatalf("expected overflow footer for >cap lines, got: %q", block)
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

func TestHandleSubagentEvent_RendersRunningProgress(t *testing.T) {
	m := &tuiModel{}
	cases := []struct {
		name   string
		evt    subagent.TaskEvent
		render bool
	}{
		{"tool progress live", subagent.TaskEvent{Type: "task_running", Description: "implement", Message: "⚙ edit_file"}, false},
		{"lifecycle noise dropped", subagent.TaskEvent{Type: "task_running", Message: "task started"}, false},
		{"empty message dropped", subagent.TaskEvent{Type: "task_running", Message: ""}, false},
		{"started is live", subagent.TaskEvent{Type: "task_started", Description: "review"}, false},
		// Terminal events resolve the entry in place; the whole fan-out block
		// commits once at turn end, so none of these commit on their own.
		{"completed does not commit", subagent.TaskEvent{Type: "task_completed", Description: "implementing · round 1/4"}, false},
		{"timeout does not commit", subagent.TaskEvent{Type: "task_timed_out", Error: "deadline"}, false},
		{"cancelled does not commit", subagent.TaskEvent{Type: "task_cancelled", Error: "context canceled"}, false},
	}
	for _, c := range cases {
		got := m.handleSubagentEvent(c.evt) != nil
		if got != c.render {
			t.Errorf("%s: rendered=%v, want %v", c.name, got, c.render)
		}
	}
}

func TestHandleSubagentEvent_CancelledResolvesInPlace(t *testing.T) {
	m := &tuiModel{subagentTasks: []subagentTaskLine{{taskID: "A", line: "  ↳ [subagent] working"}}}
	if cmd := m.handleSubagentEvent(subagent.TaskEvent{Type: "task_cancelled", TaskID: "A", Error: "context canceled"}); cmd != nil {
		t.Fatal("a terminal event must not commit on its own — the block commits at turn end")
	}
	if len(m.subagentTasks) != 1 {
		t.Fatalf("subagentTasks = %+v, want the entry kept until turn end", m.subagentTasks)
	}
	if m.subagentTasks[0].status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", m.subagentTasks[0].status)
	}
}

// --- Multi-task subagent status (M2-1a) ---
//
// The subagent pool runs up to 4 concurrent tasks. Each task's live status
// line must be independent: one task starting, updating, or finishing must
// not clobber another task's line.

func TestHandleSubagentEvent_MultiTask_BothStartedLinesPresentInOrder(t *testing.T) {
	m := &tuiModel{}
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "A", Description: "task A"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "B", Description: "task B"})

	view := m.View().Content
	idxA := strings.Index(view, "task A")
	idxB := strings.Index(view, "task B")
	if idxA == -1 || idxB == -1 {
		t.Fatalf("expected both task lines present in view, got %q", view)
	}
	if idxA > idxB {
		t.Fatalf("expected insertion order (A before B) in view, got %q", view)
	}
}

func TestHandleSubagentEvent_MultiTask_RunningUpdatesOnlyThatTask(t *testing.T) {
	m := &tuiModel{}
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "A", Description: "task A"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "B", Description: "task B"})

	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_running", TaskID: "A", Message: "⚙ edit_file"})

	view := m.View().Content
	if !strings.Contains(view, "⚙ edit_file") {
		t.Fatalf("expected A's line updated to running message, got %q", view)
	}
	// The description stays on the entry's head line; activity renders on the
	// detail line beneath it, so both are visible at once.
	if !strings.Contains(view, "task A") {
		t.Fatalf("expected A's description to remain visible, got %q", view)
	}
	if !strings.Contains(view, "task B") {
		t.Fatalf("expected B's line untouched by A's update, got %q", view)
	}
}

// This is the core bug: today a terminal event for one task clears the
// single shared status string, wiping out every other in-flight task's
// line. It must only remove that task's own entry.
func TestHandleSubagentEvent_MultiTask_TerminalResolvesOnlyThatTask(t *testing.T) {
	m := &tuiModel{}
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "A", Description: "task A"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "B", Description: "task B"})

	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "task A"})

	// Both stay visible: a finished task dropping out of the block is what made
	// a fan-out of four look like one.
	view := m.View().Content
	if !strings.Contains(view, "task A") || !strings.Contains(view, "task B") {
		t.Fatalf("both entries must remain, got %q", view)
	}
	if m.subagentTasks[0].status != "done" {
		t.Fatalf("A status = %q, want done", m.subagentTasks[0].status)
	}
	if m.subagentTasks[1].status != "" {
		t.Fatalf("B status = %q, want still running", m.subagentTasks[1].status)
	}
}

func TestHandleSubagentEvent_MultiTask_TurnEndBlockRecordsEveryTask(t *testing.T) {
	m := &tuiModel{}
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "A", Description: "task A"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: "B", Description: "task B"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "A", Description: "task A"})
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "B", Description: "task B"})

	block := m.subagentSummaryBlock()
	for _, want := range []string{"task A", "task B"} {
		if strings.Count(block, want) != 1 {
			t.Fatalf("%q appears %d times in the scrollback block, want exactly 1:\n%s",
				want, strings.Count(block, want), block)
		}
	}
}

// TestView_SubagentTasks_CapsRenderedLinesWithOverflowSummary is the RED test
// for the unbounded live subagent region (View(), ~tui.go:1005-1010): a
// 12-task fan-out would render 12 lines and could push the input prompt
// off-screen. With 7 concurrent tasks, View() must render only the first 5
// task lines plus a dimmed "+2 more" summary line — not all 7 — while
// m.subagentTasks itself keeps tracking every task (so a terminal event for
// a hidden task still works: it's removed from subagentTasks and still
// commits its scrollback line, exactly like the already-visible ones).
func TestView_SubagentTasks_CapsRenderedLinesWithOverflowSummary(t *testing.T) {
	m := &tuiModel{}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("T%d", i)
		desc := fmt.Sprintf("task %d", i)
		m.handleSubagentEvent(subagent.TaskEvent{Type: "task_started", TaskID: id, Description: desc})
	}

	view := m.View().Content
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("task %d", i)
		if !strings.Contains(view, want) {
			t.Fatalf("view missing visible task %q, got %q", want, view)
		}
	}
	for i := 5; i < 7; i++ {
		hidden := fmt.Sprintf("task %d", i)
		if strings.Contains(view, hidden) {
			t.Fatalf("view should not render hidden task %q beyond the cap, got %q", hidden, view)
		}
	}
	if !strings.Contains(view, "+2 more") {
		t.Fatalf("view missing overflow summary for the 2 hidden tasks, got %q", view)
	}
	if len(m.subagentTasks) != 7 {
		t.Fatalf("subagentTasks = %d, want all 7 still tracked even though only 5 render", len(m.subagentTasks))
	}

	// A terminal event for a hidden task (T6, beyond the cap) must still be
	// recorded: resolved in place like a visible one, and present in the
	// turn-end block even though it never rendered live.
	m.handleSubagentEvent(subagent.TaskEvent{Type: "task_completed", TaskID: "T6", Description: "task 6"})
	if len(m.subagentTasks) != 7 {
		t.Fatalf("subagentTasks = %d, want all 7 kept until turn end", len(m.subagentTasks))
	}
	if m.subagentTasks[6].status != "done" {
		t.Fatalf("hidden task status = %q, want done", m.subagentTasks[6].status)
	}
	if !strings.Contains(m.subagentSummaryBlock(), "task 6") {
		t.Fatalf("the turn-end block must record a task that was never rendered live:\n%s", m.subagentSummaryBlock())
	}
}

func TestMatchSlashCommands(t *testing.T) {
	if got := matchSlashCommands("und"); len(got) != 1 || got[0].Name != "undo" {
		t.Fatalf("prefix 'und' should match only undo, got %+v", got)
	}
	if got := matchSlashCommands(""); len(got) != len(slashCommands) {
		t.Fatalf("empty prefix should match all %d, got %d", len(slashCommands), len(got))
	}
	if got := matchSlashCommands("zzz"); len(got) != 0 {
		t.Fatalf("no command starts with zzz, got %+v", got)
	}
}

func TestSlashSuggestions(t *testing.T) {
	m := newTUIModel(BannerInfo{Model: "test"})

	// Typing the command token shows matching suggestions.
	m.ta.SetValue("/sa")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].Name != "save" {
		t.Fatalf("/sa should suggest save, got %+v", m.suggestions)
	}

	// Applying completes to "/save " and dismisses the popup.
	m.applySuggestion()
	if m.ta.Value() != "/save " {
		t.Fatalf("applySuggestion = %q, want %q", m.ta.Value(), "/save ")
	}
	if len(m.suggestions) != 0 {
		t.Fatalf("suggestions should clear after apply")
	}

	// A space (moved to args) hides suggestions.
	m.ta.SetValue("/save my task")
	m.updateSuggestions()
	if len(m.suggestions) != 0 {
		t.Fatalf("suggestions should hide once typing arguments, got %+v", m.suggestions)
	}

	// Non-slash input never suggests.
	m.ta.SetValue("hello")
	m.updateSuggestions()
	if len(m.suggestions) != 0 {
		t.Fatalf("plain text should not suggest, got %+v", m.suggestions)
	}
}

func TestRenderToolDiff(t *testing.T) {
	m := newTUIModel(BannerInfo{})

	// edit_file: the shared line "a" is context; only "b"→"c" changes. The
	// header shows the path and the +1/-1 counts.
	d := m.renderToolDiff("edit_file",
		map[string]any{"path": "foo.go", "old_string": "a\nb", "new_string": "a\nc"}, nil)
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ c") {
		t.Fatalf("edit diff missing -/+ lines:\n%s", d)
	}
	if strings.Contains(d, "- a") || strings.Contains(d, "+ a") {
		t.Fatalf("shared line should be context, not -/+:\n%s", d)
	}
	if !strings.Contains(d, "foo.go") || !strings.Contains(d, "+1") || !strings.Contains(d, "-1") {
		t.Fatalf("diff header missing path/counts:\n%s", d)
	}

	// write_file: only added lines.
	d = m.renderToolDiff("write_file", map[string]any{"path": "x.txt", "content": "x\ny"}, nil)
	if !strings.Contains(d, "+ x") || strings.Contains(d, "- ") {
		t.Fatalf("write diff should be all additions:\n%s", d)
	}

	// start_line drives real file line numbers in the gutter.
	d = m.renderToolDiff("edit_file",
		map[string]any{"path": "foo.go", "old_string": "b", "new_string": "c"},
		map[string]any{"start_line": 42})
	if !strings.Contains(d, "42") {
		t.Fatalf("expected line number 42 in gutter:\n%s", d)
	}

	// Oversized edits are capped with a "more lines" marker.
	big := strings.Repeat("line\n", 50)
	d = m.renderToolDiff("write_file", map[string]any{"path": "big.txt", "content": big}, nil)
	if !strings.Contains(d, "more lines") {
		t.Fatalf("large diff should be truncated with a marker:\n%s", d)
	}

	// Non-edit tools produce no diff.
	if d := m.renderToolDiff("bash", map[string]any{"command": "ls"}, nil); d != "" {
		t.Fatalf("bash should have no diff, got %q", d)
	}
}

func TestLineDiffFallbackOnLargeInput(t *testing.T) {
	// Small identical input → LCS keeps everything as context, no -/+ ops.
	ops := lineDiff([]string{"a", "b"}, []string{"a", "b"})
	for _, op := range ops {
		if op.kind != ' ' {
			t.Fatalf("small identical diff should be all context, got %c", op.kind)
		}
	}

	// Input exceeding the LCS cell ceiling falls back to remove-all/add-all,
	// so a huge edit can't spike memory or freeze the render loop.
	n := 1001 // n*n = 1,002,001 > maxLCSCells (1,000,000)
	oldLines := make([]string, n)
	newLines := make([]string, n)
	for i := range oldLines {
		oldLines[i] = "same" // identical → LCS would mark all context
		newLines[i] = "same"
	}
	ops = lineDiff(oldLines, newLines)
	if len(ops) != 2*n {
		t.Fatalf("fallback op count = %d, want %d", len(ops), 2*n)
	}
	ctx, del, add := 0, 0, 0
	for _, op := range ops {
		switch op.kind {
		case ' ':
			ctx++
		case '-':
			del++
		case '+':
			add++
		}
	}
	if ctx != 0 || del != n || add != n {
		t.Fatalf("fallback should be all -/+, got ctx=%d del=%d add=%d", ctx, del, add)
	}
}

func TestContextGauge(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	if g := m.contextGauge(); g != "" {
		t.Fatalf("no window/usage → empty gauge, got %q", g)
	}
	m.contextWindow = 1000
	m.lastUsage = &agent.Usage{InputTokens: 300}
	if g := m.contextGauge(); !strings.Contains(g, "ctx 30%") {
		t.Fatalf("expected 'ctx 30%%', got %q", g)
	}
	m.lastUsage = &agent.Usage{InputTokens: 800}
	if g := m.contextGauge(); !strings.Contains(g, "compacting soon") {
		t.Fatalf("expected high-usage warning, got %q", g)
	}
}

func TestRenderLastMessageToggleSymmetry(t *testing.T) {
	m := newTUIModel(BannerInfo{})
	m.width = 80
	m.lastAIRaw = "# Title\n\ncode: `x := 1`"

	// raw mode → verbatim source (copyable)
	m.renderMD = false
	raw := m.renderLastMessage()
	if !strings.Contains(raw, "# Title") || !strings.Contains(raw, "`x := 1`") {
		t.Fatalf("raw toggle should return source, got %q", raw)
	}

	// markdown mode → rendered, non-empty (this is the case that used to vanish)
	m.renderMD = true
	md := m.renderLastMessage()
	if md == "" {
		t.Fatal("markdown toggle returned empty — the reply would disappear on toggle-back")
	}

	// no prior reply → empty
	m.lastAIRaw = ""
	if m.renderLastMessage() != "" {
		t.Fatal("no prior reply should yield empty")
	}
}
