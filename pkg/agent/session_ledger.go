package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// Session working set. Cross-session memory (pkg/memory) stores durable
// biography and is off the hot path unless AgentConfig.InjectMemory is set.
// What a long coding session actually needs — which files are in play, and
// which commands just failed — is destroyed by compaction (tool bodies become
// a short head/tail excerpt, large arguments become a placeholder). This
// ledger is derived from tool results as they happen and rendered into the
// trailing turn injection, which compaction does not rewrite.

const (
	sessionBriefHeader = "[System note: Session working set. Kept outside the transcript so compaction can drop old tool output without losing the files in play or the latest failures.]"

	maxSessionFiles      = 8
	maxSessionErrors     = 4
	maxObjectiveRunes    = 200
	sessionBriefMaxBytes = 1600
)

type sessionTouch struct {
	path    string
	op      string
	outcome string
}

type sessionLedger struct {
	objective string
	touches   []sessionTouch
	errors    []string
}

// absorb folds canonical history into the ledger. Summarized tool results
// (compaction's own output) never overwrite a richer live note: a restarted
// process can still recover paths from surviving arguments, and a process
// that already watched the real result keeps the failure text compaction ate.
func (l *sessionLedger) absorb(msgs []models.Message) {
	if l == nil {
		return
	}
	pending := make(map[string]models.ToolCall)
	for _, m := range msgs {
		if opensUserTurn(m) {
			if text := oneLine(m.Content, maxObjectiveRunes); text != "" {
				l.objective = text
			}
			continue
		}
		if m.Role == models.RoleAI {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					pending[tc.ID] = tc
				}
			}
			continue
		}
		if m.Role != models.RoleTool || m.ToolResult == nil {
			continue
		}
		call := pending[m.ToolResult.CallID]
		if call.Name == "" {
			call.Name = m.ToolResult.ToolName
		}
		content := m.Content
		if content == "" {
			content = m.ToolResult.Content
		}
		l.note(call, *m.ToolResult, content, isSummarizedToolResult(content))
	}
}

// note records one tool result. fromSummary is true when content is already
// compaction's placeholder; those updates only fill in a path the ledger has
// never seen. Returns whether the rendered brief changed.
func (l *sessionLedger) note(call models.ToolCall, result models.ToolResult, content string, fromSummary bool) bool {
	if l == nil {
		return false
	}
	tool := result.ToolName
	if tool == "" {
		tool = call.Name
	}
	path := sessionPath(call)
	op := sessionOp(tool)
	failed, failLine := sessionFailure(tool, path, call, result, content)

	if fromSummary {
		if path == "" || op == "" || l.hasPath(path) {
			return false
		}
		return l.upsertFile(path, op, "compacted")
	}

	changed := false
	if path != "" && op != "" {
		outcome := "ok"
		if failed {
			outcome = "failed"
		}
		if l.upsertFile(path, op, outcome) {
			changed = true
		}
	}
	if failed && failLine != "" {
		if l.addError(failLine) {
			changed = true
		}
	} else if path != "" && l.clearErrorsFor(path) {
		changed = true
	}
	return changed
}

func (l *sessionLedger) render() string {
	if l == nil || (l.objective == "" && len(l.touches) == 0 && len(l.errors) == 0) {
		return ""
	}
	touches := l.touches
	errors := l.errors
	out := renderSessionParts(l.objective, touches, errors)
	for len(out) > sessionBriefMaxBytes && (len(touches) > 1 || len(errors) > 0) {
		if len(errors) > 0 && (len(touches) == 0 || len(errors) >= len(touches)) {
			errors = errors[1:]
		} else if len(touches) > 1 {
			touches = touches[1:]
		} else {
			break
		}
		out = renderSessionParts(l.objective, touches, errors)
	}
	if len(out) > sessionBriefMaxBytes {
		out = models.TruncateBytes(out, sessionBriefMaxBytes)
	}
	return out
}

func renderSessionParts(objective string, touches []sessionTouch, errors []string) string {
	var b strings.Builder
	b.WriteString(sessionBriefHeader)
	if objective != "" {
		b.WriteString("\nObjective: ")
		b.WriteString(objective)
	}
	if len(touches) > 0 {
		b.WriteString("\nFiles:")
		for _, t := range touches {
			fmt.Fprintf(&b, "\n- %s %s — %s", t.op, t.path, t.outcome)
		}
	}
	if len(errors) > 0 {
		b.WriteString("\nRecent failures:")
		for _, e := range errors {
			b.WriteString("\n- ")
			b.WriteString(e)
		}
	}
	return b.String()
}

func (l *sessionLedger) hasPath(path string) bool {
	for _, t := range l.touches {
		if t.path == path {
			return true
		}
	}
	return false
}

func (l *sessionLedger) upsertFile(path, op, outcome string) bool {
	for i, t := range l.touches {
		if t.path != path {
			continue
		}
		if t.op == op && t.outcome == outcome && i == len(l.touches)-1 {
			return false
		}
		// Copy: append(slice[:i], slice[i+1:]...) aliases the same array and
		// can overwrite the tail it is trying to keep.
		next := make([]sessionTouch, 0, len(l.touches))
		next = append(next, l.touches[:i]...)
		next = append(next, l.touches[i+1:]...)
		l.touches = next
		break
	}
	l.touches = append(l.touches, sessionTouch{path: path, op: op, outcome: outcome})
	if len(l.touches) > maxSessionFiles {
		l.touches = append([]sessionTouch(nil), l.touches[len(l.touches)-maxSessionFiles:]...)
	}
	return true
}

func (l *sessionLedger) addError(line string) bool {
	for _, e := range l.errors {
		if e == line {
			return false
		}
	}
	l.errors = append(l.errors, line)
	if len(l.errors) > maxSessionErrors {
		l.errors = append([]string(nil), l.errors[len(l.errors)-maxSessionErrors:]...)
	}
	return true
}

func (l *sessionLedger) clearErrorsFor(path string) bool {
	if path == "" || len(l.errors) == 0 {
		return false
	}
	kept := make([]string, 0, len(l.errors))
	changed := false
	for _, e := range l.errors {
		if strings.Contains(e, path) {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if changed {
		l.errors = kept
	}
	return changed
}

func sessionOp(tool string) string {
	switch tool {
	case "read_file":
		return "read"
	case "edit_file":
		return "edited"
	case "write_file":
		return "wrote"
	case "grep", "find":
		return "searched"
	case "list_dir", "code_map":
		return "listed"
	default:
		return ""
	}
}

func sessionPath(call models.ToolCall) string {
	for _, key := range []string{"path", "file_path"} {
		if p, ok := call.Arguments[key].(string); ok {
			if p = strings.TrimSpace(p); p != "" {
				return p
			}
		}
	}
	return ""
}

func sessionFailure(tool, path string, call models.ToolCall, result models.ToolResult, content string) (bool, string) {
	if result.Status == models.CallStatusFailed || strings.TrimSpace(result.Error) != "" {
		snippet := oneLine(result.Error, 100)
		if snippet == "" {
			snippet = oneLine(content, 100)
		}
		if snippet == "" {
			snippet = "failed"
		}
		where := tool
		if path != "" {
			where = tool + " " + path
		}
		return true, oneLine(where+": "+snippet, 160)
	}
	if tool != "bash" {
		return false, ""
	}
	var parsed struct {
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil || (parsed.ExitCode == 0 && !parsed.TimedOut) {
		return false, ""
	}
	snippet := oneLine(parsed.Stderr, 80)
	if snippet == "" {
		if cmd, ok := call.Arguments["command"].(string); ok {
			snippet = oneLine(cmd, 80)
		}
	}
	if parsed.TimedOut {
		return true, oneLine("bash timed out: "+snippet, 160)
	}
	return true, oneLine(fmt.Sprintf("bash exit %d: %s", parsed.ExitCode, snippet), 160)
}

func oneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
