package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/subagent"
)

// TUI is a single, persistent Bubble Tea program that owns the chat session for
// its entire lifetime. Completed output is committed to the terminal's native
// scrollback via tea.Println; only the bottom region (streaming line, status,
// input box) is the live, redrawn area. This replaces the previous model of
// spinning up a fresh program per prompt and hand-rolling ANSI cursor moves,
// which was the root cause of flicker and garbled output.
//
// All exported methods are safe to call from any goroutine: they marshal work
// onto the Bubble Tea event loop via (*tea.Program).Send.
type TUI struct {
	p     *tea.Program
	model *tuiModel

	done       chan struct{}
	finalModel tea.Model
	closeOnce  sync.Once
}

// NewTUI constructs a persistent TUI bound to the given streams. status holds
// the initial footer info (provider/model/plan mode).
func NewTUI(in io.Reader, out io.Writer, status BannerInfo) *TUI {
	m := newTUIModel(status)
	p := tea.NewProgram(m,
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	return &TUI{p: p, model: m, done: make(chan struct{})}
}

// Start launches the program in the background. It returns immediately; the
// program runs until Close is called or the user requests exit.
func (t *TUI) Start() {
	go func() {
		fm, _ := t.p.Run()
		t.finalModel = fm
		close(t.done)
	}()
}

// Close stops the program and waits for it to exit.
func (t *TUI) Close() {
	t.closeOnce.Do(func() {
		t.p.Quit()
		<-t.done
	})
}

// InterruptCh returns the channel that receives a value when the user presses
// Ctrl+C while the agent is running. The REPL selects on this to cancel a turn.
func (t *TUI) InterruptCh() <-chan struct{} { return t.model.interruptCh }

// --- output (rendering) ---

// Banner commits the startup banner to scrollback.
func (t *TUI) Banner(info BannerInfo) {
	t.p.Send(printMsg{text: renderBannerString(info)})
}

// Info commits a system/informational message (slash command output, notices).
func (t *TUI) Info(msg string) {
	t.p.Send(printMsg{text: msg})
}

// TurnStart marks the beginning of an agent turn: commits the user message and
// switches the live region into "working" mode with an animated spinner.
func (t *TUI) TurnStart(turn int, userInput string) {
	t.p.Send(turnStartMsg{turn: turn, input: userInput})
}

// RenderEvent renders a single agent event.
func (t *TUI) RenderEvent(evt agent.AgentEvent) {
	t.p.Send(agentEventMsg{evt: evt})
}

// RenderSubagentEvent renders a subagent lifecycle event.
func (t *TUI) RenderSubagentEvent(evt subagent.TaskEvent) {
	t.p.Send(subagentEventMsg{evt: evt})
}

// TurnEnd finalizes the current turn, committing any trailing streamed text and
// a usage/stats line, then stops the spinner.
func (t *TUI) TurnEnd(usage *agent.Usage) {
	t.p.Send(turnEndMsg{usage: usage})
}

// RenderInterrupted commits an "interrupted" notice.
func (t *TUI) RenderInterrupted() {
	t.p.Send(interruptedMsg{})
}

// SetStatus updates the footer (e.g. when plan mode or the model changes).
func (t *TUI) SetStatus(model string, planMode bool) {
	t.p.Send(statusMsg{model: model, planMode: planMode})
}

// --- input ---

// ReadPrompt focuses the input box and blocks until the user submits a line,
// presses Ctrl+C at an empty prompt (errInterrupted), or Ctrl+D (io.EOF).
func (t *TUI) ReadPrompt(ctx context.Context) (string, error) {
	reply := make(chan inputResult, 1)
	t.p.Send(requestInputMsg{reply: reply})
	select {
	case r := <-reply:
		return r.value, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.done:
		return "", io.EOF
	}
}

// AskQuestion implements tools.UserInteraction by rendering a question with
// optional numbered choices inline and reading the answer through the same
// persistent input box.
func (t *TUI) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	reply := make(chan inputResult, 1)
	t.p.Send(askQuestionMsg{question: question, options: options, reply: reply})
	var r inputResult
	select {
	case r = <-reply:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.done:
		return "", io.EOF
	}
	if r.err != nil {
		return "", r.err
	}
	answer := strings.TrimSpace(r.value)
	if answer == "" {
		return "", nil
	}
	if len(options) > 0 {
		if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(options) {
			return options[idx-1], nil
		}
	}
	return answer, nil
}

// --- history persistence (delegated to the embedded helper) ---

// LoadHistory seeds the input history from a file. The file read happens on the
// caller's goroutine; the resulting slice is handed to the model via a message
// so it is only ever mutated on the Bubble Tea event loop.
func (t *TUI) LoadHistory(path string) {
	t.model.histStore.LoadHistoryFile(path)
	items := append([]string(nil), t.model.histStore.history...)
	t.p.Send(historyMsg{items: items})
}

// SaveHistory writes the input history (as accumulated during the session)
// back to disk.
func (t *TUI) SaveHistory() {
	// Pull the latest history out of the final model if the program has exited;
	// otherwise use the live model (Close has not yet run).
	m := t.model
	if t.finalModel != nil {
		if fm, ok := t.finalModel.(*tuiModel); ok {
			m = fm
		}
	}
	m.histStore.history = m.history
	m.histStore.SaveHistoryFile()
}

// ---------------------------------------------------------------------------
// Internal messages
// ---------------------------------------------------------------------------

type inputResult struct {
	value string
	err   error
}

type printMsg struct{ text string }
type turnStartMsg struct {
	turn  int
	input string
}
type agentEventMsg struct{ evt agent.AgentEvent }
type subagentEventMsg struct{ evt subagent.TaskEvent }
type turnEndMsg struct{ usage *agent.Usage }
type interruptedMsg struct{}
type statusMsg struct {
	model    string
	planMode bool
}
type requestInputMsg struct{ reply chan inputResult }
type historyMsg struct{ items []string }
type askQuestionMsg struct {
	question string
	options  []string
	reply    chan inputResult
}
type elapsedTickMsg struct{}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type tuiModel struct {
	styles Styles
	width  int

	ta textarea.Model
	sp spinner.Model

	// live region state
	inputVisible bool   // input box shown (idle or asking)
	agentActive  bool   // agent running -> show spinner/elapsed
	askActive    bool   // currently asking a tool question
	askHeader    string // rendered question + options shown above the input
	aiPartial    string // trailing partial line of streamed assistant text

	// channels to the controller
	inputReply  chan inputResult
	interruptCh chan struct{}

	// turn/status
	turn      int
	turnStart time.Time
	elapsed   time.Duration
	lastUsage *agent.Usage
	model     string
	planMode  bool

	// input history
	history    []string
	histIdx    int
	savedInput string
	histStore  *historyStore // file load/save helpers
}

func newTUIModel(status BannerInfo) *tuiModel {
	styles := DefaultStyles()

	ta := textarea.New()
	promptStr := styles.UserPrompt.Render("> ")
	ta.SetPromptFunc(2, func(textarea.PromptInfo) string { return promptStr })
	ta.Placeholder = "Type a message  ·  Alt+Enter for newline  ·  /help"
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetWidth(80)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Highlight

	return &tuiModel{
		styles:      styles,
		ta:          ta,
		sp:          sp,
		histIdx:     -1,
		model:       status.Model,
		interruptCh: make(chan struct{}, 1),
		histStore:   newHistoryStore(),
	}
}

func (m *tuiModel) Init() tea.Cmd {
	// The spinner is only ticked while the agent is active (started in
	// turnStartMsg), so it does not redraw the idle prompt.
	return textarea.Blink
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		w := msg.Width - 4
		if w < 10 {
			w = 10
		}
		m.ta.SetWidth(w)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		// Drop ticks (and stop rescheduling) when idle so the spinner loop ends
		// between turns instead of redrawing the prompt forever.
		if !m.agentActive {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case historyMsg:
		m.history = msg.items
		return m, nil

	case elapsedTickMsg:
		if m.agentActive {
			m.elapsed = time.Since(m.turnStart)
			return m, m.elapsedTick()
		}
		return m, nil

	case printMsg:
		return m, commit(msg.text)

	case turnStartMsg:
		m.turn = msg.turn
		m.turnStart = time.Now()
		m.elapsed = 0
		m.agentActive = true
		m.inputVisible = false
		m.aiPartial = ""
		userLine := fmt.Sprintf("%s %s", m.styles.UserPrompt.Render("›"), msg.input)
		return m, tea.Batch(commit(userLine), m.sp.Tick, m.elapsedTick())

	case agentEventMsg:
		return m, m.handleAgentEvent(msg.evt)

	case subagentEventMsg:
		return m, m.handleSubagentEvent(msg.evt)

	case turnEndMsg:
		m.lastUsage = msg.usage
		m.agentActive = false
		var lines []string
		if f := m.flushPartial(); f != "" {
			lines = append(lines, f)
		}
		lines = append(lines, m.statsLine())
		return m, commit(strings.Join(lines, "\n"))

	case interruptedMsg:
		m.agentActive = false
		var lines []string
		if f := m.flushPartial(); f != "" {
			lines = append(lines, f)
		}
		lines = append(lines, m.styles.Dim.Render("  ⎿ Interrupted."))
		return m, commit(strings.Join(lines, "\n"))

	case statusMsg:
		m.model = msg.model
		m.planMode = msg.planMode
		return m, nil

	case requestInputMsg:
		m.inputReply = msg.reply
		m.inputVisible = true
		m.askActive = false
		m.askHeader = ""
		m.histIdx = -1
		m.ta.SetValue("")
		m.ta.Focus()
		return m, textarea.Blink

	case askQuestionMsg:
		m.inputReply = msg.reply
		m.inputVisible = true
		m.askActive = true
		m.askHeader = m.renderAskHeader(msg.question, msg.options)
		m.ta.SetValue("")
		m.ta.Focus()
		return m, textarea.Blink
	}

	// Forward anything else to the textarea while it is active.
	if m.inputVisible {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		switch {
		case m.askActive:
			m.submitInput(inputResult{err: errInterrupted})
			return m, nil
		case m.agentActive:
			select {
			case m.interruptCh <- struct{}{}:
			default:
			}
			return m, nil
		case m.inputVisible:
			if strings.TrimSpace(m.ta.Value()) != "" {
				m.ta.SetValue("")
				return m, nil
			}
			m.submitInput(inputResult{err: errInterrupted})
			return m, nil
		}
		return m, nil

	case "ctrl+d":
		if m.inputVisible && strings.TrimSpace(m.ta.Value()) == "" {
			m.submitInput(inputResult{err: io.EOF})
		}
		return m, nil

	case "enter":
		if !m.inputVisible {
			return m, nil
		}
		// Modifier+Enter inserts a newline.
		if msg.Mod != 0 {
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			m.syncInputHeight()
			return m, cmd
		}
		val := m.ta.Value()
		if !m.askActive && strings.TrimSpace(val) == "" {
			return m, nil
		}
		if !m.askActive {
			m.recordHistory(val)
		}
		m.submitInput(inputResult{value: val})
		return m, nil

	case "up":
		if m.inputVisible && !m.askActive && !strings.Contains(m.ta.Value(), "\n") && len(m.history) > 0 {
			if m.histIdx == -1 {
				m.savedInput = m.ta.Value()
			}
			if m.histIdx < len(m.history)-1 {
				m.histIdx++
				m.ta.SetValue(m.history[m.histIdx])
				m.ta.CursorEnd()
			}
			return m, nil
		}

	case "down":
		if m.inputVisible && !m.askActive && m.histIdx >= 0 {
			m.histIdx--
			if m.histIdx == -1 {
				m.ta.SetValue(m.savedInput)
			} else {
				m.ta.SetValue(m.history[m.histIdx])
			}
			m.ta.CursorEnd()
			return m, nil
		}
	}

	if m.inputVisible {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		m.syncInputHeight()
		return m, cmd
	}
	return m, nil
}

// submitInput delivers a result to the waiting ReadPrompt/AskQuestion call and
// hides the input box.
func (m *tuiModel) submitInput(r inputResult) {
	if m.inputReply != nil {
		m.inputReply <- r
		m.inputReply = nil
	}
	m.inputVisible = false
	m.askActive = false
	m.askHeader = ""
	m.ta.SetValue("")
}

func (m *tuiModel) recordHistory(val string) {
	if strings.TrimSpace(val) == "" {
		return
	}
	m.history = append([]string{val}, m.history...)
	if len(m.history) > maxHistory {
		m.history = m.history[:maxHistory]
	}
	m.histIdx = -1
}

// syncInputHeight grows the input box up to a cap as the user adds lines.
func (m *tuiModel) syncInputHeight() {
	lines := strings.Count(m.ta.Value(), "\n") + 1
	if lines < 1 {
		lines = 1
	}
	if lines > 6 {
		lines = 6
	}
	m.ta.SetHeight(lines)
}

func (m *tuiModel) elapsedTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return elapsedTickMsg{} })
}

// flushPartial returns the styled trailing assistant line (if any) for commit
// and clears it.
func (m *tuiModel) flushPartial() string {
	if m.aiPartial == "" {
		return ""
	}
	s := m.styles.Assistant.Render(m.aiPartial)
	m.aiPartial = ""
	return s
}

func (m *tuiModel) handleAgentEvent(evt agent.AgentEvent) tea.Cmd {
	switch evt.Type {
	case agent.AgentEventTextChunk:
		if evt.Text == "" {
			return nil
		}
		m.aiPartial += evt.Text
		// Commit all complete lines, keep the trailing partial in the live region.
		if idx := strings.LastIndex(m.aiPartial, "\n"); idx >= 0 {
			complete := m.aiPartial[:idx]
			m.aiPartial = m.aiPartial[idx+1:]
			return commit(m.styles.Assistant.Render(complete))
		}
		return nil

	case agent.AgentEventToolCallStart:
		line := m.toolStartLine(evt)
		if line == "" {
			return nil
		}
		return m.commitWithFlush(line)

	case agent.AgentEventToolCallEnd:
		line := m.toolEndLine(evt)
		if line == "" {
			return nil
		}
		return m.commitWithFlush(line)

	case agent.AgentEventError:
		msg := evt.Err
		if evt.Error != nil {
			msg = evt.Error.Message
		}
		return m.commitWithFlush(m.styles.Error.Render("  Error: " + msg))

	case agent.AgentEventCompact:
		if evt.CompactStats == nil {
			return nil
		}
		return m.commitWithFlush(m.styles.Compaction.Render(compactLine(evt.CompactStats)))
	}
	return nil
}

func (m *tuiModel) handleSubagentEvent(evt subagent.TaskEvent) tea.Cmd {
	switch evt.Type {
	case "task_started":
		desc := evt.Description
		if len([]rune(desc)) > 80 {
			desc = string([]rune(desc)[:77]) + "..."
		}
		return m.commitWithFlush(m.styles.Dim.Render("  ↳ [subagent] " + desc))
	case "task_failed":
		errMsg := evt.Error
		if errMsg == "" {
			errMsg = evt.Message
		}
		return m.commitWithFlush(m.styles.Error.Render("  ↳ [subagent] failed: " + errMsg))
	}
	return nil
}

// commitWithFlush commits the trailing assistant partial (if any) before the
// given line, preserving output order.
func (m *tuiModel) commitWithFlush(line string) tea.Cmd {
	if f := m.flushPartial(); f != "" {
		return commit(f + "\n" + line)
	}
	return commit(line)
}

func (m *tuiModel) toolStartLine(evt agent.AgentEvent) string {
	name, preview := toolEventNamePreview(evt)
	if name == "" {
		return ""
	}
	if preview != "" {
		return m.styles.ToolCall.Render(fmt.Sprintf("  ⚙ %s(%s)…", name, preview))
	}
	return m.styles.ToolCall.Render(fmt.Sprintf("  ⚙ %s…", name))
}

func (m *tuiModel) toolEndLine(evt agent.AgentEvent) string {
	if evt.ToolEvent == nil {
		return ""
	}
	te := evt.ToolEvent
	icon := "✓"
	useErr := te.Error != ""
	if useErr {
		icon = "✗"
	}
	var detail string
	if te.DurationMS > 0 {
		detail = fmt.Sprintf(" (%.1fs)", float64(te.DurationMS)/1000)
	}
	if te.Error != "" {
		detail += " " + te.Error
	} else if te.ResultPreview != "" {
		preview := te.ResultPreview
		if lipgloss.Width(preview) > 120 {
			preview = truncateWidth(preview, 117) + "..."
		}
		detail += " → " + preview
	}
	line := fmt.Sprintf("  ⎿ %s %s%s", icon, te.Name, detail)
	if useErr {
		return m.styles.Error.Render(line)
	}
	return m.styles.ToolResult.Render(line)
}

func (m *tuiModel) statsLine() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("Turn %d", m.turn))
	if m.lastUsage != nil {
		parts = append(parts, fmt.Sprintf("%d in / %d out", m.lastUsage.InputTokens, m.lastUsage.OutputTokens))
	}
	parts = append(parts, fmt.Sprintf("%.1fs", time.Since(m.turnStart).Seconds()))
	return m.styles.Stats.Render("  " + strings.Join(parts, " · "))
}

func (m *tuiModel) renderAskHeader(question string, options []string) string {
	var b strings.Builder
	b.WriteString(m.styles.Highlight.Render("  ? " + question))
	for i, opt := range options {
		b.WriteString(fmt.Sprintf("\n    %d. %s", i+1, opt))
	}
	return b.String()
}

func (m *tuiModel) View() tea.View {
	var b strings.Builder

	// Live trailing assistant text (streamed, not yet committed).
	if m.aiPartial != "" {
		b.WriteString(m.styles.Assistant.Render(m.aiPartial))
		b.WriteString("\n")
	}

	// Status line.
	if m.agentActive {
		b.WriteString(m.busyStatus())
		b.WriteString("\n")
	}

	// Input region.
	if m.inputVisible {
		if m.askHeader != "" {
			b.WriteString(m.askHeader)
			b.WriteString("\n")
		}
		b.WriteString(m.ta.View())
		if !m.askActive {
			b.WriteString("\n")
			b.WriteString(m.idleFooter())
		}
	}

	return tea.NewView(b.String())
}

func (m *tuiModel) busyStatus() string {
	elapsed := m.elapsed
	if elapsed == 0 {
		elapsed = time.Since(m.turnStart)
	}
	parts := []string{
		m.sp.View(),
		m.styles.Dim.Render(fmt.Sprintf("Working… %.0fs", elapsed.Seconds())),
	}
	if m.lastUsage != nil {
		parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("· %d tok", m.lastUsage.OutputTokens)))
	}
	parts = append(parts, m.styles.Dim.Render("· ctrl+c to interrupt"))
	return "  " + strings.Join(parts, " ")
}

func (m *tuiModel) idleFooter() string {
	mode := "code"
	if m.planMode {
		mode = "plan"
	}
	model := m.model
	if model == "" {
		model = "model"
	}
	return m.styles.Dim.Render(fmt.Sprintf("  %s · %s · /help", model, mode))
}

// commit emits a permanent scrollback line above the live region.
func commit(text string) tea.Cmd {
	return tea.Printf("%s", text)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// toolEventNamePreview extracts the tool name and an argument preview from a
// tool-call event, handling both the ToolEvent and ToolCall carriers.
func toolEventNamePreview(evt agent.AgentEvent) (string, string) {
	if evt.ToolEvent != nil {
		return evt.ToolEvent.Name, toolArgsPreview(evt.ToolEvent.Arguments)
	}
	if evt.ToolCall != nil {
		return evt.ToolCall.Name, toolArgsPreview(evt.ToolCall.Arguments)
	}
	return "", ""
}

func compactLine(cs *agent.CompactStats) string {
	if cs.AfterTokens > 0 {
		return fmt.Sprintf("  Context compacted: %d messages, %.0f%% -> %.0f%% of %d",
			cs.MessagesAfter, cs.Ratio*100, cs.AfterRatio*100, cs.ContextWindow)
	}
	return fmt.Sprintf("  Context compacted: %d messages (%.0f%% of %d)",
		cs.MessagesAfter, cs.Ratio*100, cs.ContextWindow)
}

// renderBannerString renders the startup banner into a string for committing to
// the TUI scrollback.
func renderBannerString(info BannerInfo) string {
	var sb strings.Builder
	RenderBanner(&sb, info)
	return strings.TrimRight(sb.String(), "\n")
}

// isInteractiveTTY reports whether both stdin and stderr are terminals, which is
// required for the persistent Bubble Tea TUI (raw mode + redraw).
func isInteractiveTTY() bool {
	return isTTY() && IsTerminal(os.Stderr)
}
