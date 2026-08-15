package chat

import (
	"context"
	"encoding/base64"
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
	"github.com/charmbracelet/glamour"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/imageproc"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// Spinner characters for subagent tasks (distributed phases)
var subagentSpinners = []string{"⠋", "⠙", "⠹", "⠸"}

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
func (t *TUI) ReadPrompt(ctx context.Context) (string, []models.MessageImage, error) {
	reply := make(chan inputResult, 1)
	t.p.Send(requestInputMsg{reply: reply})
	select {
	case r := <-reply:
		return r.value, r.images, r.err
	case <-ctx.Done():
		return "", nil, ctx.Err()
	case <-t.done:
		return "", nil, io.EOF
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
	value  string
	images []models.MessageImage
	err    error
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
	aiPartial    string // accumulated streamed assistant text (rendered on flush)

	// markdown rendering
	renderMD   bool                  // render AI output as markdown (toggle with ctrl+r)
	lastAIRaw  string                // raw text of the last AI message, for raw re-emit
	mdRenderer *glamour.TermRenderer // cached, rebuilt on width change
	mdWidth    int

	// channels to the controller
	inputReply  chan inputResult
	interruptCh chan struct{}

	// turn/status
	turn          int
	turnStart     time.Time
	elapsed       time.Duration
	lastUsage     *agent.Usage
	model         string
	planMode      bool
	contextWindow int

	// input history
	history    []string
	histIdx    int
	savedInput string
	histStore  *historyStore // file load/save helpers

	// slash-command autocomplete
	suggestions []slashCmd
	suggestIdx  int

	// pendingImages holds images attached via Ctrl+V clipboard paste, to be
	// sent with the next submitted message.
	pendingImages []models.MessageImage

	// subagentTasks holds the live status line for each active subagent
	// task, rendered in the live region (not scrollback) so each task
	// updates in place independently, keyed by TaskID. Order is insertion
	// order (first-started first).
	subagentTasks []subagentTaskLine
}

// subagentTaskLine is one entry in tuiModel.subagentTasks: the live status
// line for a single in-flight subagent task.
type subagentTaskLine struct {
	taskID      string
	line        string
	description string
	agentType   string    // e.g., "coder", "tester", "bash"
	startedAt   time.Time
	spinnerIdx  int
}

// maxLiveSubagentLines caps how many subagent status lines View() renders in
// the live region at once. Beyond the cap, the remainder collapses into a
// single "+N more" summary line so a wide fan-out (e.g. 12 concurrent tasks)
// can't push the input prompt off-screen. Every task stays tracked in
// m.subagentTasks regardless — only the rendering is bounded.
const maxLiveSubagentLines = 5

func newTUIModel(status BannerInfo) *tuiModel {
	styles := DefaultStyles()

	ta := textarea.New()
	promptStr := styles.UserPrompt.Render("> ")
	ta.SetPromptFunc(2, func(textarea.PromptInfo) string { return promptStr })
	ta.Placeholder = "Type a message  ·  Alt+Enter for newline  ·  /help"
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 10
	ta.SetWidth(80)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Highlight

	return &tuiModel{
		styles:        styles,
		ta:            ta,
		sp:            sp,
		histIdx:       -1,
		model:         status.Model,
		interruptCh:   make(chan struct{}, 1),
		histStore:     newHistoryStore(),
		renderMD:      true,
		contextWindow: status.ContextWindow,
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
			// Update spinner indices for all subagent tasks to create animation
			for i := range m.subagentTasks {
				m.subagentTasks[i].spinnerIdx = (m.subagentTasks[i].spinnerIdx + 1) % 4
			}
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

	case "ctrl+v":
		// Try to read an image from the system clipboard. If the clipboard
		// has an image, attach it and consume the event. If not, fall
		// through to the default handler so the textarea performs a normal
		// text paste.
		if m.inputVisible && m.tryPasteClipboardImage() {
			return m, nil
		}

	case "ctrl+r":
		// Toggle AI output between markdown and raw, and re-emit the last reply
		// in the new mode so you can flip the same message back and forth
		// (raw = copyable source, markdown = rendered).
		m.renderMD = !m.renderMD
		mode := "raw (copyable)"
		if m.renderMD {
			mode = "markdown"
		}
		lines := []string{m.styles.Dim.Render("  ⎿ output: " + mode)}
		if last := m.renderLastMessage(); last != "" {
			lines = append(lines, last)
		}
		return m, commit(strings.Join(lines, "\n"))

	case "tab":
		if len(m.suggestions) > 0 {
			m.applySuggestion()
			return m, nil
		}

	case "esc":
		if len(m.suggestions) > 0 {
			m.suggestions = nil
			m.suggestIdx = 0
			return m, nil
		}
		// Clear pending clipboard images if any.
		if len(m.pendingImages) > 0 {
			m.pendingImages = nil
			return m, nil
		}

	case "enter":
		if !m.inputVisible {
			return m, nil
		}
		// Suggestion selected → complete instead of submitting.
		if len(m.suggestions) > 0 {
			m.applySuggestion()
			return m, nil
		}
		// Modifier+Enter inserts a newline.
		if msg.Mod != 0 {
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			m.updateSuggestions()
			return m, cmd
		}
		val := m.ta.Value()
		// Allow submit with empty text if there are pending images.
		if !m.askActive && strings.TrimSpace(val) == "" && len(m.pendingImages) == 0 {
			return m, nil
		}
		if !m.askActive {
			m.recordHistory(val)
		}
		m.submitInput(inputResult{value: val})
		return m, nil

	case "up":
		if n := len(m.suggestions); n > 0 {
			m.suggestIdx = (m.suggestIdx - 1 + n) % n
			return m, nil
		}
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
		if n := len(m.suggestions); n > 0 {
			m.suggestIdx = (m.suggestIdx + 1) % n
			return m, nil
		}
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
		m.updateSuggestions()
		return m, cmd
	}
	return m, nil
}

// tryPasteClipboardImage attempts to read an image from the system clipboard.
// If successful, it saves the optimized image to a temp file (so vision MCP
// tools can access it by path), appends it to pendingImages, and inserts an
// [image:/path] token into the textarea. Returns false if the clipboard has
// no image or processing fails, so the caller falls through to text-paste.
func (m *tuiModel) tryPasteClipboardImage() bool {
	raw, err := imageproc.ReadClipboardImage()
	if err != nil || len(raw) == 0 {
		return false
	}

	result, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
	if err != nil {
		return false
	}

	// Save to temp file so vision MCP tools can access by path.
	ext := ".jpg"
	if result.MimeType == "image/png" {
		ext = ".png"
	}
	tmpFile, err := os.CreateTemp("", "deepai-clipboard-*"+ext)
	if err != nil {
		return false
	}
	imgBytes, err := base64.StdEncoding.DecodeString(result.Base64)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return false
	}
	if _, err := tmpFile.Write(imgBytes); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return false
	}
	tmpFile.Close()

	idx := len(m.pendingImages)
	m.pendingImages = append(m.pendingImages, models.MessageImage{
		MimeType: result.MimeType,
		Base64:   result.Base64,
	})
	// Insert path placeholder so the model can call MCP vision tools.
	m.ta.InsertString(fmt.Sprintf("[image#%d:%s] ", idx, tmpFile.Name()))
	return true
}

// submitInput delivers a result to the waiting ReadPrompt/AskQuestion call and
// hides the input box.
func (m *tuiModel) submitInput(r inputResult) {
	// Attach any pending clipboard images to the result (only for normal
	// prompt submissions, not for tool-question answers).
	if r.images == nil && len(m.pendingImages) > 0 && !m.askActive {
		r.images = m.pendingImages
	}
	if m.inputReply != nil {
		m.inputReply <- r
		m.inputReply = nil
	}
	m.inputVisible = false
	m.askActive = false
	m.askHeader = ""
	m.ta.SetValue("")
	m.suggestions = nil
	m.suggestIdx = 0
	m.pendingImages = nil
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

// updateSuggestions recomputes the slash-command popup from the current input.
// Suggestions show only while the command token is being typed (leading "/",
// no space/newline yet); once the user moves on to arguments they disappear.
func (m *tuiModel) updateSuggestions() {
	prev := len(m.suggestions)
	m.suggestions = nil
	if m.askActive {
		m.suggestIdx = 0
		return
	}
	val := m.ta.Value()
	if !strings.HasPrefix(val, "/") || strings.ContainsAny(val, " \n") {
		m.suggestIdx = 0
		return
	}
	m.suggestions = matchSlashCommands(val[len("/"):])
	if len(m.suggestions) != prev || m.suggestIdx >= len(m.suggestions) {
		m.suggestIdx = 0
	}
}

// applySuggestion completes the input to the selected command and dismisses the
// popup, leaving a trailing space so the user can type arguments.
func (m *tuiModel) applySuggestion() {
	if m.suggestIdx < 0 || m.suggestIdx >= len(m.suggestions) {
		return
	}
	m.ta.SetValue("/" + m.suggestions[m.suggestIdx].Name + " ")
	m.ta.CursorEnd()
	m.suggestions = nil
	m.suggestIdx = 0
}

// renderSuggestions draws the slash-command popup, highlighting the selection.
func (m *tuiModel) renderSuggestions() string {
	var b strings.Builder
	for i, c := range m.suggestions {
		line := fmt.Sprintf("  /%s  %s", padRight(c.Name, 10), c.Desc)
		if i == m.suggestIdx {
			b.WriteString(m.styles.Highlight.Render("▸" + line))
		} else {
			b.WriteString(m.styles.Dim.Render(" " + line))
		}
		if i < len(m.suggestions)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
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
	raw := m.aiPartial
	m.aiPartial = ""
	m.lastAIRaw = raw
	if m.renderMD {
		if md := m.renderMarkdown(raw); md != "" {
			return md
		}
	}
	return m.styles.Assistant.Render(raw)
}

// renderLastMessage formats the last AI reply in the current output mode
// (rendered markdown or raw source). Used by the ctrl+r toggle to re-show the
// same message after flipping modes. Empty when there is no prior reply.
func (m *tuiModel) renderLastMessage() string {
	if strings.TrimSpace(m.lastAIRaw) == "" {
		return ""
	}
	if m.renderMD {
		if md := m.renderMarkdown(m.lastAIRaw); md != "" {
			return md
		}
	}
	return m.lastAIRaw
}

func (m *tuiModel) handleAgentEvent(evt agent.AgentEvent) tea.Cmd {
	switch evt.Type {
	case agent.AgentEventTextChunk:
		if evt.Text == "" {
			return nil
		}
		// Accumulate the whole message; it is markdown-rendered and committed as
		// one block at the next message boundary (tool call, turn end). The live
		// region shows the raw stream until then.
		m.aiPartial += evt.Text
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
		// Extract agent type from description if available
		agentType := ""
		if strings.Contains(desc, "[") && strings.Contains(desc, "]") {
			start := strings.Index(desc, "[") + 1
			end := strings.Index(desc[start:], "]")
			if end > 0 {
				agentType = desc[start : start+end]
			}
		}
		m.setSubagentLineWithInfo(evt.TaskID, "  ↳ [subagent] "+desc, desc, agentType)
		return nil
	case "task_running":
		msg := strings.TrimSpace(evt.Message)
		if msg == "" || msg == "task started" {
			return nil
		}
		if d := strings.TrimSpace(evt.Description); d != "" {
			msg = "[" + d + "] " + msg
		}
		m.updateSubagentLine(evt.TaskID, "  ↳ "+msg)
		return nil
	case "task_completed":
		m.clearSubagentLine(evt.TaskID)
		desc := strings.TrimSpace(evt.Description)
		if desc == "" {
			desc = "done"
		}
		return m.commitWithFlush(m.styles.ToolResult.Render("  ↳ ✓ " + desc))
	case "task_timed_out":
		m.clearSubagentLine(evt.TaskID)
		return m.commitWithFlush(m.styles.Error.Render("  ↳ [subagent] timed out: " + evt.Error))
	case "task_failed":
		m.clearSubagentLine(evt.TaskID)
		errMsg := evt.Error
		if errMsg == "" {
			errMsg = evt.Message
		}
		return m.commitWithFlush(m.styles.Error.Render("  ↳ [subagent] failed: " + errMsg))
	case "task_cancelled":
		m.clearSubagentLine(evt.TaskID)
		errMsg := evt.Error
		if errMsg == "" {
			errMsg = evt.Message
		}
		return m.commitWithFlush(m.styles.Error.Render("  ↳ ⊘ [subagent] cancelled: " + errMsg))
	}
	return nil
}

// setSubagentLineWithInfo creates a new task entry with full metadata.
// Called only when a task first starts (task_started event).
func (m *tuiModel) setSubagentLineWithInfo(taskID, line, description, agentType string) {
	m.subagentTasks = append(m.subagentTasks, subagentTaskLine{
		taskID:      taskID,
		line:        line,
		description: description,
		agentType:   agentType,
		startedAt:   time.Now(),
		spinnerIdx:  len(m.subagentTasks) % 4, // Distribute spinner phases
	})
}

// updateSubagentLine updates an existing task's line text.
// Called for task_running events to update progress.
func (m *tuiModel) updateSubagentLine(taskID, line string) {
	for i := range m.subagentTasks {
		if m.subagentTasks[i].taskID == taskID {
			m.subagentTasks[i].line = line
			return
		}
	}
}

// setSubagentLine appends a new live status line for taskID, or updates it
// in place if taskID already has an entry (order-preserving).
func (m *tuiModel) setSubagentLine(taskID, line string) {
	for i := range m.subagentTasks {
		if m.subagentTasks[i].taskID == taskID {
			m.subagentTasks[i].line = line
			return
		}
	}
	// New task: record start time and extract description from line
	desc := line
	if idx := strings.Index(line, "[subagent]"); idx >= 0 {
		// Extract description after "[subagent] " prefix
		desc = strings.TrimSpace(line[idx+len("[subagent]"):])
	}
	m.subagentTasks = append(m.subagentTasks, subagentTaskLine{
		taskID:      taskID,
		line:        line,
		description: desc,
		startedAt:   time.Now(),
		spinnerIdx:  len(m.subagentTasks) % 4, // Distribute spinner phases
	})
}

// clearSubagentLine removes the live status line for taskID, if present,
// leaving every other task's line untouched.
func (m *tuiModel) clearSubagentLine(taskID string) {
	for i := range m.subagentTasks {
		if m.subagentTasks[i].taskID == taskID {
			m.subagentTasks = append(m.subagentTasks[:i], m.subagentTasks[i+1:]...)
			return
		}
	}
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
	// Bash commands get a full, shell-highlighted block instead of a truncated
	// inline preview so the user can see exactly what is being executed.
	if name == "bash" {
		if cmd := toolEventCommand(evt); strings.TrimSpace(cmd) != "" {
			return m.bashCommandBlock(cmd)
		}
	}
	if preview != "" {
		return m.styles.ToolCall.Render(fmt.Sprintf("  ⚙ %s(%s)…", name, preview))
	}
	return m.styles.ToolCall.Render(fmt.Sprintf("  ⚙ %s…", name))
}

// maxToolCmdLines caps how many command lines a bash block prints before
// collapsing the remainder into a "… (N more lines)" footer. Matches the diff
// block cap so multi-line tool output stays consistent in the transcript.
const maxToolCmdLines = 16

// bashCommandBlock renders a bash command as a "⚙ Bash" header followed by the
// full command, shell-syntax-highlighted, one bar-prefixed line per source line.
func (m *tuiModel) bashCommandBlock(cmd string) string {
	lines := strings.Split(strings.TrimRight(cmd, "\n"), "\n")
	var b strings.Builder
	b.WriteString(m.styles.ToolCall.Render("  ⚙ Bash"))
	bar := m.styles.Dim.Render("  │ ")
	shown := 0
	for _, ln := range lines {
		if shown >= maxToolCmdLines {
			break
		}
		// Truncate raw text (no ANSI yet) so width math and highlighting stay correct.
		if lipgloss.Width(ln) > 120 {
			ln = truncateWidth(ln, 117) + "..."
		}
		b.WriteString("\n")
		b.WriteString(bar)
		b.WriteString(highlightShellLine(ln))
		shown++
	}
	if more := len(lines) - shown; more > 0 {
		b.WriteString("\n")
		b.WriteString(m.styles.Dim.Render(fmt.Sprintf("  … (%d more lines)", more)))
	}
	return b.String()
}

// toolEventCommand extracts the "command" argument from a bash tool-call event,
// handling both the ToolEvent and ToolCall carriers.
func toolEventCommand(evt agent.AgentEvent) string {
	var args map[string]any
	switch {
	case evt.ToolEvent != nil:
		args = evt.ToolEvent.Arguments
	case evt.ToolCall != nil:
		args = evt.ToolCall.Arguments
	}
	s, _ := args["command"].(string)
	return s
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
	// For file edits, show a colored +/- diff instead of an opaque preview.
	diff := ""
	if !useErr {
		var data map[string]any
		if te.Result != nil {
			data = te.Result.Data
		}
		diff = m.renderToolDiff(te.Name, te.Arguments, data)
	}
	if te.Error != "" {
		detail += " " + te.Error
	} else if diff == "" && te.ResultPreview != "" {
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
	rendered := m.styles.ToolResult.Render(line)
	if diff != "" {
		rendered += "\n" + diff
	}
	return rendered
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

	// Live streamed assistant text (raw, not yet committed). Bounded to a tail
	// so a long message doesn't blow up the live region; the full message is
	// markdown-rendered into scrollback on completion.
	if m.aiPartial != "" {
		b.WriteString(m.styles.Assistant.Render(tailLines(m.aiPartial, 16)))
		b.WriteString("\n")
	}

	// Status line.
	if m.agentActive {
		b.WriteString(m.busyStatus())
		b.WriteString("\n")
	}

		// Subagent live status (updates in place, not scrollback), one line per
		// in-flight task, in insertion (start) order. Capped at
		// maxLiveSubagentLines so a wide fan-out can't push the input prompt
		// off-screen; every task beyond the cap is still tracked in
		// m.subagentTasks (terminal events for hidden tasks still fire and
		// commit to scrollback normally) — only the live rendering is bounded.
		visible := m.subagentTasks
		overflow := 0
		if len(visible) > maxLiveSubagentLines {
			overflow = len(visible) - maxLiveSubagentLines
			visible = visible[:maxLiveSubagentLines]
		}
		for _, task := range visible {
			// Calculate elapsed time for this task
			elapsed := time.Since(task.startedAt)
			elapsedSec := int(elapsed.Seconds())
			
			// Get spinner character for this task
			spinChar := subagentSpinners[task.spinnerIdx]
			
			// Build enhanced line with spinner, type, and duration
			var line string
			if task.agentType != "" {
				line = fmt.Sprintf("  ↳ %s [%s] %s (%ds)", spinChar, task.agentType, task.description, elapsedSec)
			} else if task.line != "" {
				line = fmt.Sprintf("%s (%ds)", task.line, elapsedSec)
			} else {
				line = fmt.Sprintf("  ↳ %s %s (%ds)", spinChar, task.description, elapsedSec)
			}
			b.WriteString(m.styles.Dim.Render(line))
			b.WriteString("\n")
		}
		if overflow > 0 {
			// Show summary with total count
			totalTasks := len(m.subagentTasks)
			b.WriteString(m.styles.Dim.Render(fmt.Sprintf("  ↳ … +%d more [total: %d tasks]", overflow, totalTasks)))
			b.WriteString("\n")
		}

	// Input region.
	if m.inputVisible {
		if m.askHeader != "" {
			b.WriteString(m.askHeader)
			b.WriteString("\n")
		}
		b.WriteString(m.ta.View())
		if len(m.pendingImages) > 0 {
			b.WriteString("\n")
			b.WriteString(m.renderPendingImages())
		}
		if len(m.suggestions) > 0 {
			b.WriteString("\n")
			b.WriteString(m.renderSuggestions())
		}
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
	if ctx := m.contextGauge(); ctx != "" {
		parts = append(parts, ctx)
	}
	parts = append(parts, m.styles.Dim.Render("· ctrl+c to interrupt"))
	return "  " + strings.Join(parts, " ")
}

// contextGauge renders the context-window fill as a colored "· ctx N%", warning
// as it approaches the 0.75 auto-compaction threshold. Empty when unknown.
func (m *tuiModel) contextGauge() string {
	if m.contextWindow <= 0 || m.lastUsage == nil || m.lastUsage.InputTokens <= 0 {
		return ""
	}
	pct := m.lastUsage.InputTokens * 100 / m.contextWindow
	label := fmt.Sprintf("· ctx %d%%", pct)
	switch {
	case pct >= 75:
		return m.styles.Error.Render(label + " (compacting soon)")
	case pct >= 60:
		return m.styles.SeverityWarn.Render(label)
	default:
		return m.styles.Dim.Render(label)
	}
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
	out := "markdown"
	if !m.renderMD {
		out = "raw"
	}
	footer := m.styles.Dim.Render(fmt.Sprintf("  %s · %s · ctrl+r:%s · ctrl+v:image · /help", model, mode, out))
	if ctx := m.contextGauge(); ctx != "" {
		footer += " " + ctx
	}
	return footer
}

// renderPendingImages shows a summary line for clipboard-attached images.
func (m *tuiModel) renderPendingImages() string {
	parts := make([]string, 0, len(m.pendingImages))
	for i, img := range m.pendingImages {
		// Approximate decoded size from base64 length.
		sizeKB := len(img.Base64) * 3 / 4 / 1024
		label := fmt.Sprintf("📎 image#%d %s ~%dKB", i+1, img.MimeType, sizeKB)
		parts = append(parts, m.styles.Highlight.Render(label))
	}
	hint := m.styles.Dim.Render("Enter to send · Ctrl+V for more · Esc to clear")
	return "  " + strings.Join(parts, "  ") + "\n  " + hint
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
