package chat

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SlashCommand represents a parsed /command.
type SlashCommand struct {
	Name string
	Args string
}

// errInterrupted is returned when the user presses Ctrl+C at the prompt.
var errInterrupted = errors.New("interrupted")

const maxHistory = 200

// historyStore persists the input line history across sessions.
type historyStore struct {
	history     []string // newest-first, capped at maxHistory
	historyPath string   // file to persist history across sessions
}

// newHistoryStore creates an empty history store.
func newHistoryStore() *historyStore { return &historyStore{} }

// LoadHistoryFile loads history from a file into the store.
// Lines are stored newest-first after loading.
// Errors are silently ignored (missing file is normal on first run).
func (h *historyStore) LoadHistoryFile(path string) {
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
func (h *historyStore) SaveHistoryFile() {
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
