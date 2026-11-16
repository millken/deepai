package runcode

import (
	"context"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

const (
	defaultTimeoutSec = 120
	maxTimeoutSec     = 600
	defaultMaxCalls   = 50
	maxMaxCalls       = 200
)

// RunCodeDescription is everything the model sees (§4.1): deliberately short,
// no per-tool schema mirroring. It only spells out the two things a schema
// cannot express — "command failed ≠ call failed" (F2b) and where the hashline
// hashes live (F2) — both of which silently misroute a script if gotten wrong.
const RunCodeDescription = `Run a JavaScript (ES2023) script that calls your other tools, and get back only what the script returns. Every tool you can call directly is available as ` + "`await tools.<name>(args)`" + ` with exactly the same arguments as its own schema. It resolves to ` + "`{status, text, data}`" + `: ` + "`text`" + ` is what the tool would have shown you; ` + "`data`" + ` is its structured result as plain JSON, or null (grep → ` + "`data.matches[]`" + ` with ` + "`file/line/content/hash`" + `; glob → array of paths; bash → ` + "`{stdout, stderr, exit_code}`" + `).
A command that runs but fails does not throw: check ` + "`data.exit_code`" + ` for bash. A call rejects only when the tool itself failed (bad arguments, file not found, timeout); the Error carries ` + "`.text`" + ` with the tool's output.
To edit by hash after a search, use ` + "`data.matches[i].line`" + ` + ` + "`.hash`" + ` from grep. After ` + "`read_file`" + `, hashes exist only in ` + "`text`" + ` as ` + "`N:hhhhhh`" + ` line prefixes — parse them from there.
Use top-level ` + "`await`" + ` and ` + "`return`" + `. Use ` + "`Promise.all`" + ` for independent read-only calls. ` + "`console.log`" + ` output is returned but capped at 8 KB. There is no fs, network, or require — only ` + "`tools`" + `. Not available inside scripts: run_code, task, todo_write, skill, ask_clarification, present_file, plan-mode tools.
Use this when a task needs several dependent tool calls whose intermediate results you do not need to see (search-then-filter, the same edit across many files, edit-then-test). For a single lookup, or when each step needs your judgment, call tools directly.`

// RunCodeTool returns the run_code tool definition (§4.1). ParallelSafe is
// false — a script may contain writes, so it takes the conservative lane.
func RunCodeTool() models.Tool {
	return models.Tool{
		Name:         "run_code",
		Description:  RunCodeDescription,
		Groups:       []string{"builtin"},
		ParallelSafe: false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"script":         map[string]any{"type": "string", "description": "JavaScript source; top-level await and return are available"},
				"timeout_sec":    map[string]any{"type": "number", "description": "Script wall-clock budget in seconds (default 120, max 600)"},
				"max_tool_calls": map[string]any{"type": "number", "description": "Nested tool call budget (default 50, max 200)"},
			},
			"required": []any{"script"},
		},
		Handler: RunCodeHandler,
	}
}

// RunCodeHandler runs the script against the caller's live dispatch mirror.
// Contract (§6.2): ALWAYS return (result, nil). Returning an error would make
// runOneTool rebuild the result — Status failed, Error=err, Content DISCARDED
// — and the journal/writes summary, the thing the model needs most from a
// failed script, would vanish. Failure text lives in Error=Content.
func RunCodeHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	script, _ := call.Arguments["script"].(string)
	if strings.TrimSpace(script) == "" {
		res := models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusFailed,
			Error:    "script is required",
		}
		res.Content = res.Error
		return res, nil
	}

	lim := Limits{
		Timeout:        time.Duration(clampInt(argFloat(call.Arguments, "timeout_sec"), defaultTimeoutSec, maxTimeoutSec)) * time.Second,
		MaxToolCalls:   clampInt(argFloat(call.Arguments, "max_tool_calls"), defaultMaxCalls, maxMaxCalls),
		MaxConcurrency: defaultConcurrency,
	}

	dispatch := tools.DispatchFromContext(ctx)
	if dispatch == nil || dispatch.Registry == nil {
		res := failedResult(call, OutcomeError, "run_code is not available here: no tool dispatch on this execution path")
		return res, nil
	}
	if dispatch.MaxConcurrency > 0 {
		lim.MaxConcurrency = dispatch.MaxConcurrency
	}

	host := NewRegistryHost(dispatch, call.ID)
	out := NewEngine().Run(ctx, script, host, lim)
	res := Render(out, script)
	res.CallID = call.ID
	res.ToolName = call.Name
	return res, nil
}

// defaultConcurrency mirrors the agent default when the dispatch carries none
// (tests); the live path always injects a.maxToolConcurrency (F2d).
const defaultConcurrency = 10

func failedResult(call models.ToolCall, o Outcome, msg string) models.ToolResult {
	res := Render(RunOutput{Outcome: o, Err: msg}, "")
	res.CallID = call.ID
	res.ToolName = call.Name
	return res
}

func argFloat(args map[string]any, key string) float64 {
	v, _ := args[key].(float64)
	return v
}

func clampInt(v float64, def, max int) int {
	n := int(v)
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
