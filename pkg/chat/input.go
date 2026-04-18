package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// SlashCommand represents a parsed /command.
type SlashCommand struct {
	Name string
	Args string
}

// InputHandler reads user input from stdin.
type InputHandler struct {
	scanner *bufio.Scanner
	styles  Styles
}

// NewInputHandler creates an input handler reading from r.
func NewInputHandler(r io.Reader) *InputHandler {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &InputHandler{
		scanner: scanner,
		styles:  DefaultStyles(),
	}
}

// ReadPrompt reads user input. Blocks until input is received or ctx is cancelled.
// Returns empty string and nil error on EOF.
func (h *InputHandler) ReadPrompt(ctx context.Context) (string, error) {
	fmt.Fprint(os.Stderr, h.styles.UserPrompt.Render("> "))

	line, err := h.scanLine(ctx)
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)

	// Support line continuation with backslash.
	for strings.HasSuffix(line, "\\") {
		line = strings.TrimSuffix(line, "\\")
		fmt.Fprint(os.Stderr, h.styles.Dim.Render("... "))
		cont, err := h.scanLine(ctx)
		if err != nil {
			return line, err
		}
		line += strings.TrimSpace(cont)
	}

	return line, nil
}

// scanLine reads one line from stdin, respecting context cancellation.
func (h *InputHandler) scanLine(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		if !h.scanner.Scan() {
			err := h.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			ch <- result{"", err}
			return
		}
		ch <- result{h.scanner.Text(), nil}
	}()

	select {
	case r := <-ch:
		return r.line, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// AskQuestion implements tools.UserInteraction. It prints a question with optional
// numbered choices, reads user input from stdin, and returns the answer.
// If options are provided and the user enters a number, the corresponding option is returned.
// Safe to call from agent goroutine: the REPL main loop is blocked on outcomes during tool execution,
// so no concurrent reads on the shared bufio.Scanner.
func (h *InputHandler) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	fmt.Fprintf(os.Stderr, "\n  ? %s\n", question)
	for i, opt := range options {
		fmt.Fprintf(os.Stderr, "    %d. %s\n", i+1, opt)
	}
	fmt.Fprint(os.Stderr, "  > ")

	line, err := h.scanLine(ctx)
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(line)
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
