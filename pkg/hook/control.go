package hook

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CommandKind is one parsed remote-control instruction.
type CommandKind string

const (
	CommandReply      CommandKind = "reply"
	CommandInterrupt  CommandKind = "interrupt"
	CommandCancelTask CommandKind = "cancel_task"
)

// Command is one line of the control protocol. Arg is the reply text or the
// task ID; empty for interrupt.
type Command struct {
	Kind CommandKind
	Arg  string
}

// ControlState is the REPL snapshot handed to a control source on every poll,
// so a script can tell "the agent is waiting for an answer to Q" from "idle".
type ControlState struct {
	Asking   bool
	Question string
}

// Poller is the inbound side: fetch whatever commands have accumulated since
// the last poll. Deduplication is the source's own responsibility (e.g. the
// TG script's getUpdates offset) — see docs/HOOKS.md.
type Poller interface {
	Poll(ctx context.Context, state ControlState) ([]Command, error)
}

// ControlConfig mirrors the config.yaml `control:` block.
type ControlConfig struct {
	Command     []string `yaml:"command,omitempty"`
	URL         string   `yaml:"url,omitempty"`
	PollSeconds int      `yaml:"poll_seconds,omitempty"`
}

// NewControlSource builds the Poller named by cfg; command wins over URL.
// ok=false when nothing usable is configured.
func NewControlSource(cfg *ControlConfig) (p Poller, ok bool) {
	if cfg == nil {
		return nil, false
	}
	if len(cfg.Command) > 0 {
		src, err := NewCommandSource(cfg.Command)
		if err != nil {
			slog.Warn("hook control command invalid", "err", err)
			return nil, false
		}
		return src, true
	}
	if cfg.URL != "" {
		return NewURLSource(cfg.URL), true
	}
	return nil, false
}

// DefaultControlPollInterval is the cadence of the REPL's control loop.
const DefaultControlPollInterval = 3 * time.Second

const (
	controlCmdTimeout = 10 * time.Second
	controlURLTimeout = 10 * time.Second
)

// CommandSource polls a local command: each run prints zero or more protocol
// lines to stdout. The DEEPAI_* env vars carry the current REPL state.
type CommandSource struct {
	argv []string
}

func NewCommandSource(argv []string) (*CommandSource, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("control command: empty argv")
	}
	expanded := make([]string, len(argv))
	for i, a := range argv {
		expanded[i] = expandHome(a)
	}
	return &CommandSource{argv: expanded}, nil
}

func (s *CommandSource) Poll(ctx context.Context, state ControlState) ([]Command, error) {
	cctx, cancel := context.WithTimeout(ctx, controlCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.argv[0], s.argv[1:]...)
	asking := "false"
	if state.Asking {
		asking = "true"
	}
	cmd.Env = append(os.Environ(),
		"DEEPAI_STATE="+controlStateTerm(state),
		"DEEPAI_ASKING="+asking,
		"DEEPAI_QUESTION="+state.Question,
	)
	// A grandchild inheriting stdout (e.g. the docs script's curl spawning
	// weirdness) would otherwise hold Output() open until the context kills
	// the direct child AND the pipe drains — WaitDelay bounds that to a
	// second past the kill.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("hook control command failed", "argv", s.argv, "err", err)
		return nil, nil
	}
	return parseControlLines(string(out)), nil
}

func controlStateTerm(state ControlState) string {
	if state.Asking {
		return "asking"
	}
	return "idle"
}

// URLSource polls a GET endpoint: the response body is the protocol lines.
type URLSource struct {
	url    string
	client *http.Client
}

func NewURLSource(url string) *URLSource {
	return &URLSource{url: url, client: &http.Client{Timeout: controlURLTimeout}}
}

func (s *URLSource) Poll(ctx context.Context, state ControlState) ([]Command, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Warn("hook control poll failed", "url", s.url, "err", err)
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("hook control poll non-2xx", "url", s.url, "status", resp.Status)
		return nil, nil
	}
	body, err := io.ReadAll(bufio.NewReader(io.LimitReader(resp.Body, 1<<20)))
	if err != nil {
		slog.Warn("hook control poll body", "url", s.url, "err", err)
		return nil, nil
	}
	return parseControlLines(string(body)), nil
}

// parseControlLines parses the whole polled output. Unknown, empty, noop and
// slash-prefixed lines are dropped rather than treated as replies.
func parseControlLines(output string) []Command {
	var cmds []Command
	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if cmd, ok := parseControlLine(sc.Text()); ok {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

func parseControlLine(line string) (Command, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line == "noop" || strings.HasPrefix(line, "/") {
		return Command{}, false
	}
	switch {
	case line == "interrupt":
		return Command{Kind: CommandInterrupt}, true
	case strings.HasPrefix(line, "reply "):
		if arg := strings.TrimSpace(line[len("reply "):]); arg != "" {
			return Command{Kind: CommandReply, Arg: arg}, true
		}
		return Command{}, false
	case strings.HasPrefix(line, "cancel-task "):
		if arg := strings.TrimSpace(line[len("cancel-task "):]); arg != "" {
			return Command{Kind: CommandCancelTask, Arg: arg}, true
		}
		return Command{}, false
	default:
		return Command{Kind: CommandReply, Arg: line}, true
	}
}
