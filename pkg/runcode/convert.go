package runcode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// ExcludedTools are never callable inside a script (§4.3): tools whose effect
// lands in toolBatchState rather than their own handler (skill/todo_write →
// apply*Result, task/invoke_acp_agent → usage accounting), plan-mode flow
// control, human-in-the-loop, and run_code itself (no recursion). Exported for
// the regression table in pkg/agent (§10).
var ExcludedTools = []string{
	"run_code",
	"task",
	"invoke_acp_agent",
	"todo_write",
	"skill",
	"enter_plan_mode",
	"exit_plan_mode",
	"write_plan",
	"ask_clarification",
	"present_file",
}

func excludedSet() map[string]bool {
	m := make(map[string]bool, len(ExcludedTools))
	for _, n := range ExcludedTools {
		m[n] = true
	}
	return m
}

// registryHost adapts the live Dispatch mirror to the engine's Host: ToolInfo
// reads the CURRENT registry (plan mode / allowlist restrictions apply), Call
// goes through Registry.Execute — schema validation, read gate, sandbox and
// hashline verification all unchanged (F5/F6). Nested calls deliberately do
// NOT pass through toolBatchState.handleResult (§4.5): its offload, history
// append and UI events are exactly what run_code exists to avoid.
type registryHost struct {
	dispatch *tools.Dispatch
	parentID string
	excluded map[string]bool
	seq      atomic.Uint64
}

// NewRegistryHost builds the Host over the caller's dispatch mirror.
func NewRegistryHost(d *tools.Dispatch, parentCallID string) Host {
	return &registryHost{dispatch: d, parentID: parentCallID, excluded: excludedSet()}
}

func (h *registryHost) ToolInfo(name string) (visible, parallelSafe bool) {
	if h.dispatch == nil || h.dispatch.Registry == nil {
		return false, false
	}
	if h.excluded[name] {
		return false, false
	}
	t := h.dispatch.Registry.Get(name)
	return t != nil, t != nil && t.ParallelSafe
}

func (h *registryHost) Call(ctx context.Context, name string, args map[string]any) HostResult {
	if visible, _ := h.ToolInfo(name); !visible {
		return HostResult{Err: fmt.Sprintf("tool not available in run_code: %s", name), Tool: name}
	}
	seq := h.seq.Add(1)
	call := models.ToolCall{
		ID:        fmt.Sprintf("%s.%d", h.parentID, seq),
		Name:      name,
		Arguments: args,
		Status:    models.CallStatusPending,
	}
	res, err := h.dispatch.Registry.Execute(ctx, call)
	// Bash nonzero exits stay completed (F2b) — "command failed" must not
	// read as "call failed" (§4.4); reject is only for tool-layer failures.
	if err != nil || res.Status == models.CallStatusFailed {
		msg := res.Error
		if msg == "" {
			msg = err.Error()
		}
		return HostResult{Err: msg, Tool: name, Status: string(res.Status), Text: res.Content, Data: toJSONValue(res.Data)}
	}
	return HostResult{Status: string(res.Status), Text: res.Content, Data: dataOrContentJSON(res)}
}

// toJSONValue runs a Go-typed Data map through a JSON round trip so the script
// sees plain JSON values ([]grepMatch → [{file,line,content,hash}] objects),
// dropping internal marker keys the agent would have consumed (§4.4).
func toJSONValue(data map[string]any) any {
	if len(data) == 0 {
		return nil
	}
	clean := make(map[string]any, len(data))
	for k, v := range data {
		if k == models.ToolDataOffloaded || k == models.ToolDataNoOffload {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return nil
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// dataOrContentJSON implements the Data-empty fallback (F2): bash's JSON
// object and glob's JSON ARRAY live in Content — parse any legal JSON value.
// Content that is not JSON (read_file's hashlined text) yields nil: the script
// reads it from .text (§4.1).
func dataOrContentJSON(res models.ToolResult) any {
	if v := toJSONValue(res.Data); v != nil {
		return v
	}
	s := res.Content
	trimmed := trimJSONSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil
	}
	var out any
	if json.Unmarshal([]byte(trimmed), &out) != nil {
		return nil
	}
	return out
}

func trimJSONSpace(s string) string {
	// json.Unmarshal tolerates surrounding whitespace itself; this only does
	// a cheap first-byte check guard.
	for len(s) > 0 {
		switch s[0] {
		case ' ', '\t', '\n', '\r':
			s = s[1:]
		default:
			return s
		}
	}
	return ""
}
