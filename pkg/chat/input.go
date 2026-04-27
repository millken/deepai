package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// SlashCommand represents a parsed /command.
type SlashCommand struct {
	Name string
	Args string
}

// errInterrupted is returned when the user presses Ctrl+C at the prompt.
var errInterrupted = errors.New("interrupted")

// InputHandler reads user input from stdin using a bubbletea textarea.
type InputHandler struct {
	styles      Styles
	history     []string // newest-first, capped at maxHistory
	historyPath string   // file to persist history across sessions
}

const maxHistory = 200

// NewInputHandler creates an input handler.
// Input is always read from stdin via bubbletea.
func NewInputHandler() *InputHandler {
	return &InputHandler{
		styles: DefaultStyles(),
	}
}

// LoadHistoryFile loads history from a file into the handler.
// Lines are stored newest-first after loading.
// Errors are silently ignored (missing file is normal on first run).
func (h *InputHandler) LoadHistoryFile(path string) {
	h.historyPath = path
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	// File is oldest-first; reverse to newest-first and cap.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if len(lines) > maxHistory {
		lines = lines[:maxHistory]
	}
	h.history = lines
}

// SaveHistoryFile writes the current history to the file set by LoadHistoryFile.
// Errors are silently ignored.
func (h *InputHandler) SaveHistoryFile() {
	if h.historyPath == "" || len(h.history) == 0 {
		return
	}
	f, err := os.OpenFile(h.historyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	// Write oldest-first so LoadHistoryFile can reverse back.
	for i := len(h.history) - 1; i >= 0; i-- {
		_, _ = fmt.Fprintln(w, h.history[i])
	}
	_ = w.Flush()
}

// ReadPrompt reads user input using a bubbletea textarea.
// Enter submits, Alt+Enter inserts a newline.
// Up/Down navigate input history when content is single-line.
// Returns errInterrupted on Ctrl+C, and io.EOF on Ctrl+D.
func (h *InputHandler) ReadPrompt(ctx context.Context) (string, error) {
	ta := textarea.New()
	ta.Prompt = h.styles.UserPrompt.Render("> ")
	ta.Placeholder = "Type your message... (↑↓ history, Alt+Enter newline)"
	ta.ShowLineNumbers = false
	ta.SetHeight(5)
	ta.SetWidth(80)
	ta.Focus()

	resultCh := make(chan promptResult, 1)

	p := tea.NewProgram(&promptModel{
		textarea: ta,
		history:  h.history,
		histIdx:  -1,
	}, tea.WithOutput(os.Stderr), tea.WithInput(os.Stdin))

	go func() {
		model, err := p.Run()
		if err != nil {
			resultCh <- promptResult{err: err}
			return
		}
		pm := model.(*promptModel)
		resultCh <- promptResult{value: pm.value, err: pm.err}
	}()

	var r promptResult
	select {
	case r = <-resultCh:
	case <-ctx.Done():
		p.Quit()
		<-resultCh
		return "", ctx.Err()
	}

	if r.err == nil && strings.TrimSpace(r.value) != "" {
		// prepend to history, drop oldest if over cap
		h.history = append([]string{r.value}, h.history...)
		if len(h.history) > maxHistory {
			h.history = h.history[:maxHistory]
		}
	}
	return strings.TrimSpace(r.value), r.err
}

type promptResult struct {
	value string
	err   error
}

// promptModel is a minimal bubbletea model that wraps a textarea.
// Enter submits, Alt+Enter inserts a newline.
// Up/Down navigate history when content is single-line.
type promptModel struct {
	textarea   textarea.Model
	header     string // static text rendered above the textarea
	value      string
	err        error
	submitted  bool
	history    []string // newest-first
	histIdx    int      // -1 = current input, 0..N-1 = history items newest-first
	savedInput string   // content saved before entering history navigation
}

func (m *promptModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m *promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			// Alt+Enter or Ctrl+Enter inserts newline
			if msg.Mod != 0 {
				m.textarea, _ = m.textarea.Update(msg)
				return m, nil
			}
			// Plain Enter — submit
			val := m.textarea.Value()
			if strings.TrimSpace(val) == "" {
				return m, nil
			}
			m.value = val
			m.submitted = true
			return m, tea.Quit
		case "ctrl+c":
			m.err = errInterrupted
			return m, tea.Quit
		case "ctrl+d":
			m.err = io.EOF
			return m, tea.Quit
		case "esc":
			return m, nil
		case "up":
			// Navigate history only when content has no newlines (single-line mode).
			if !strings.Contains(m.textarea.Value(), "\n") && len(m.history) > 0 {
				if m.histIdx == -1 {
					m.savedInput = m.textarea.Value()
				}
				if m.histIdx < len(m.history)-1 {
					m.histIdx++
					m.textarea.SetValue(m.history[m.histIdx])
					m.textarea.CursorEnd()
				}
				return m, nil
			}
		case "down":
			if m.histIdx >= 0 {
				m.histIdx--
				if m.histIdx == -1 {
					m.textarea.SetValue(m.savedInput)
				} else {
					m.textarea.SetValue(m.history[m.histIdx])
				}
				m.textarea.CursorEnd()
				return m, nil
			}
		}
	case tea.WindowSizeMsg:
		m.textarea.SetWidth(msg.Width - 4)
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m *promptModel) View() tea.View {
	if m.submitted {
		return tea.NewView("")
	}
	return tea.NewView(m.header + m.textarea.View())
}

// AskQuestion implements tools.UserInteraction. It prints a question with optional
// numbered choices, reads user input from stdin, and returns the answer.
// If options are provided and the user enters a number, the corresponding option is returned.
func (h *InputHandler) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	// Render question and options above the textarea so the user can see them.
	var header strings.Builder
	header.WriteString("\n  ? ")
	header.WriteString(question)
	for i, opt := range options {
		header.WriteString(fmt.Sprintf("\n    %d. %s", i+1, opt))
	}
	header.WriteString("\n")

	ta := textarea.New()
	ta.Prompt = "  > "
	ta.Placeholder = "enter number or text"
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetWidth(60)
	ta.Focus()

	resultCh := make(chan promptResult, 1)

	p := tea.NewProgram(&promptModel{
		textarea: ta,
		header:   header.String(),
	}, tea.WithOutput(os.Stderr), tea.WithInput(os.Stdin))

	go func() {
		model, err := p.Run()
		if err != nil {
			resultCh <- promptResult{err: err}
			return
		}
		pm := model.(*promptModel)
		resultCh <- promptResult{value: pm.value, err: pm.err}
	}()

	var result promptResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		p.Quit()
		<-resultCh
		return "", ctx.Err()
	}

	if result.err != nil {
		return "", result.err
	}
	answer := strings.TrimSpace(result.value)
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

// ParseSlashCommand checks if input is a slash command.
func ParseSlashCommand(input string) (SlashCommand, bool) {
	if !strings.HasPrefix(input, "/") {
		return SlashCommand{}, false
	}
	parts := strings.SplitN(input[1:], " ", 2)
	cmd := SlashCommand{Name: parts[0]}
	if len(parts) > 1 {
		cmd.Args = strings.TrimSpace(parts[1])
	}
	return cmd, true
}
