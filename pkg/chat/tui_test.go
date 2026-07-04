package chat

import (
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
		{"tool progress renders", subagent.TaskEvent{Type: "task_running", Description: "implement", Message: "⚙ edit_file"}, true},
		{"lifecycle noise dropped", subagent.TaskEvent{Type: "task_running", Message: "task started"}, false},
		{"empty message dropped", subagent.TaskEvent{Type: "task_running", Message: ""}, false},
		{"started renders", subagent.TaskEvent{Type: "task_started", Description: "review"}, true},
		{"completed renders check", subagent.TaskEvent{Type: "task_completed", Description: "implementing · round 1/4"}, true},
		{"timeout renders", subagent.TaskEvent{Type: "task_timed_out", Error: "deadline"}, true},
	}
	for _, c := range cases {
		got := m.handleSubagentEvent(c.evt) != nil
		if got != c.render {
			t.Errorf("%s: rendered=%v, want %v", c.name, got, c.render)
		}
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
