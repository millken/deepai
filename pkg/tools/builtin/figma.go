package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
)

const (
	figmaToolMaxBytes    = 32 * 1024
	figmaOutputMaxBytes  = 24 * 1024
	figmaQuickTimeout    = 30 * time.Second
	figmaSubmitTimeout   = 20 * time.Second
	figmaWaitAutoMaxSecs = 150
	figmaWaitSlackSecs   = 40
	figmaWorkflowTimeout = 600 * time.Second
)

var (
	figmaNodeIDRe   = regexp.MustCompile(`^[0-9Ii][0-9:;]*$`)
	figmaHexRe      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	figmaModes      = map[string]bool{"design": true, "refine": true, "design-to-code": true, "code-to-design": true, "prototype": true}
	figmaEditCmds   = map[string]bool{"text": true, "fill": true, "font": true, "layout": true, "align": true, "distribute": true, "prototype": true, "instance": true, "props": true, "bind-fill": true}
	figmaLayoutKeys = map[string]bool{
		"direction": true, "horizontal-sizing": true, "vertical-sizing": true,
		"width": true, "height": true, "gap": true, "padding": true,
		"padding-top": true, "padding-right": true, "padding-bottom": true, "padding-left": true,
		"primary-align": true, "counter-align": true,
	}
)

// FigmaHandler wraps the local `figma-local` CLI (https://github.com/denki-san/local-figma):
// agent-driven native Figma editing with on-disk evidence. It merges the CLI's
// submit → wait → result round-trips into one call, translates its exit codes,
// and enforces the runtime's no-replay recovery discipline (needs-review and
// unresolved states carry explicit instructions instead of being retried).
func FigmaHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	res := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	action, _ := args["action"].(string)
	if action == "" {
		return res, fmt.Errorf("action is required")
	}

	plan, err := figmaBuildArgs(action, args)
	if err != nil {
		return res, err
	}

	stdout, code, timedOut, err := figmaExec(ctx, plan.argv, plan.timeout)
	if err != nil {
		return res, err
	}
	if timedOut {
		res.Status = models.CallStatusFailed
		res.Error = fmt.Sprintf("figma-local %s timed out after %s; if a job was submitted its state is uncertain — read it back (figma result/wait), never resubmit", action, plan.timeout)
		return res, nil
	}
	if code == 127 {
		res.Status = models.CallStatusFailed
		res.Error = "figma-local is not installed; install the figma-local-runtime .tgz from the local-figma releases (Node.js 22+ required), then retry"
		return res, nil
	}

	if plan.waitAfter && code == 0 {
		return figmaSubmitAndWait(ctx, res, action, stdout, args)
	}

	out := figmaOutput{Action: action, ExitCode: code, CLI: models.TruncateBytes(strings.TrimSpace(stdout), figmaToolMaxBytes)}
	data, _ := json.Marshal(out)
	res.Content = string(data)
	if code == 1 {
		res.Status = models.CallStatusFailed
		res.Error = figmaCLIError(out.CLI)
	}
	return res, nil
}

type figmaExecPlan struct {
	argv      []string
	timeout   time.Duration
	waitAfter bool
}

func figmaBuildArgs(action string, args map[string]any) (figmaExecPlan, error) {
	plan := figmaExecPlan{timeout: figmaQuickTimeout}
	str := func(key string) string { s, _ := args[key].(string); return strings.TrimSpace(s) }
	nodeFlag := func(argv []string) ([]string, error) {
		if n := str("node"); n != "" {
			n = strings.ReplaceAll(n, "-", ":")
			if !figmaNodeIDRe.MatchString(n) {
				return argv, fmt.Errorf("node must look like 1:2 (got %q)", str("node"))
			}
			argv = append(argv, "--node", n)
		}
		return argv, nil
	}

	switch action {
	case "init":
		url := str("url")
		if url == "" {
			return plan, fmt.Errorf("url is required for init")
		}
		plan.argv = []string{"init", url}
	case "doctor", "status", "context", "history", "recover":
		plan.argv = []string{action}
	case "guide", "guide_check":
		mode := str("mode")
		if !figmaModes[mode] {
			return plan, fmt.Errorf("mode must be one of design, refine, design-to-code, code-to-design, prototype")
		}
		guideCmd := "guide"
		if action == "guide_check" {
			guideCmd = "guide-check"
		}
		plan.argv = []string{guideCmd, mode}
	case "inspect", "preview", "design_system":
		cmd := action
		if action == "design_system" {
			cmd = "design-system"
		}
		plan.argv = []string{cmd}
		var err error
		if plan.argv, err = nodeFlag(plan.argv); err != nil {
			return plan, err
		}
		plan.timeout, plan.waitAfter = figmaSubmitTimeout, true
	case "run":
		script := str("script_path")
		if script == "" {
			return plan, fmt.Errorf("script_path is required for run")
		}
		plan.argv = []string{"run", script}
		var err error
		if plan.argv, err = nodeFlag(plan.argv); err != nil {
			return plan, err
		}
		if reason := str("high_risk"); reason != "" {
			plan.argv = append(plan.argv, "--high-risk", reason)
		}
		plan.timeout, plan.waitAfter = figmaSubmitTimeout, true
	case "edit":
		cmd := strings.ReplaceAll(str("cmd"), "_", "-")
		if !figmaEditCmds[cmd] {
			return plan, fmt.Errorf("cmd must be one of text, fill, font, layout, align, distribute, prototype, instance, props, bind-fill")
		}
		argv := []string{cmd}
		switch cmd {
		case "text":
			if v := str("value"); v != "" || hasKey(args, "value") {
				argv = append(argv, v)
			} else {
				return plan, fmt.Errorf("value is required for edit text")
			}
		case "fill":
			v := str("value")
			if !figmaHexRe.MatchString(v) {
				return plan, fmt.Errorf("fill value must be #RRGGBB")
			}
			argv = append(argv, v)
		case "font":
			if family, style := str("family"), str("style"); family != "" || style != "" {
				if family == "" || style == "" {
					return plan, fmt.Errorf("font family and style must be set together")
				}
				argv = append(argv, "--family", family, "--style", style)
			}
			if t, ok := args["size"].(float64); ok {
				if t <= 0 {
					return plan, fmt.Errorf("font size must be positive")
				}
				argv = append(argv, "--size", figmaNumber(t))
			}
			if len(argv) == 1 {
				return plan, fmt.Errorf("font needs size, or family and style")
			}
		case "layout":
			obj, ok := args["layout"].(map[string]any)
			if !ok || len(obj) == 0 {
				return plan, fmt.Errorf("layout object is required for edit layout")
			}
			for key, value := range obj {
				flag := strings.ReplaceAll(key, "_", "-")
				if !figmaLayoutKeys[flag] {
					return plan, fmt.Errorf("unknown layout key %q", key)
				}
				var text string
				switch v := value.(type) {
				case string:
					text = v
				case float64:
					if v < 0 {
						return plan, fmt.Errorf("layout %s must be non-negative", flag)
					}
					text = figmaNumber(v)
				default:
					return plan, fmt.Errorf("layout %s must be a string or number", flag)
				}
				if text == "" {
					return plan, fmt.Errorf("layout %s must not be empty", flag)
				}
				argv = append(argv, "--"+flag, text)
			}
		case "align", "distribute":
			v := str("value")
			if v == "" {
				return plan, fmt.Errorf("value is required for edit %s", cmd)
			}
			argv = append(argv, v)
		case "prototype":
			v := str("target_id")
			if v == "" {
				return plan, fmt.Errorf("target_id is required for edit prototype")
			}
			argv = append(argv, v)
		case "instance":
			v := str("component_id")
			if v == "" {
				return plan, fmt.Errorf("component_id is required for edit instance")
			}
			argv = append(argv, v)
		case "props":
			name, value := str("property"), str("prop_value")
			if name == "" || !hasKey(args, "prop_value") {
				return plan, fmt.Errorf("property and prop_value are required for edit props")
			}
			argv = append(argv, name, "--value", value)
		case "bind-fill":
			v := str("variable_id")
			if v == "" {
				return plan, fmt.Errorf("variable_id is required for edit bind-fill")
			}
			argv = append(argv, v)
		}
		plan.argv = argv
		plan.timeout, plan.waitAfter = figmaSubmitTimeout, true
	case "workflow":
		wf := str("wf_action")
		switch wf {
		case "import":
			if p := str("plan_path"); p != "" {
				plan.argv = []string{"workflow", "import", p}
			} else {
				return plan, fmt.Errorf("plan_path is required for workflow import")
			}
		case "status", "run", "retry", "reconcile":
			id := str("plan_id")
			if id == "" {
				return plan, fmt.Errorf("plan_id is required for workflow %s", wf)
			}
			plan.argv = []string{"workflow", wf, id}
			plan.argv = []string{"workflow", wf, id}
			plan.timeout = figmaWorkflowTimeout
		case "approve":
			id, step, note := str("plan_id"), str("step_id"), str("note")
			if id == "" || step == "" || note == "" {
				return plan, fmt.Errorf("plan_id, step_id and note are required for workflow approve; note must reflect the user's explicit acceptance, never an invented approval")
			}
			plan.argv = []string{"workflow", "approve", id, step, note}
		default:
			return plan, fmt.Errorf("wf_action must be one of import, status, run, retry, reconcile, approve")
		}
	case "wait":
		job := str("job_id")
		if job == "" {
			return plan, fmt.Errorf("job_id is required for wait")
		}
		plan.argv = []string{"wait", job}
		plan.argv, plan.timeout = figmaWaitFlag(plan.argv, args)
	case "result", "diff", "validate":
		job := str("job_id")
		if job == "" {
			return plan, fmt.Errorf("job_id is required for %s", action)
		}
		plan.argv = []string{action, job}
	case "resolve":
		job, inspect := str("job_id"), str("inspect_job_id")
		if job == "" || inspect == "" {
			return plan, fmt.Errorf("job_id and inspect_job_id are required for resolve; only call it after the previous plugin has actually been stopped")
		}
		plan.argv = []string{"resolve", job, "--inspect", inspect, "--previous-plugin-stopped"}
	default:
		return plan, fmt.Errorf("unknown action %q", action)
	}
	return plan, nil
}

func figmaSubmitAndWait(ctx context.Context, res models.ToolResult, action, submitOut string, args map[string]any) (models.ToolResult, error) {
	var submit struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Evidence string `json:"evidence"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(submitOut), &submit); err != nil || submit.ID == "" {
		res.Status = models.CallStatusFailed
		res.Error = "cannot parse job submission response: " + models.TruncateBytes(strings.TrimSpace(submitOut), 512)
		res.Content = submitOut
		return res, nil
	}
	if submit.Evidence != "" {
		abs, err := filepath.Abs(submit.Evidence)
		if err == nil {
			submit.Evidence = abs
		}
	}

	waitArgv := []string{"wait", submit.ID}
	waitArgv, waitTimeout := figmaWaitFlag(waitArgv, args)
	waitOut, code, timedOut, err := figmaExec(ctx, waitArgv, waitTimeout)
	if err != nil {
		return res, err
	}
	if timedOut {
		res.Status = models.CallStatusFailed
		res.Error = fmt.Sprintf("timed out waiting for job %s; the job itself was NOT cancelled — call figma with action=wait job_id=%s to keep waiting, or action=result to read it back. Never resubmit the same edit", submit.ID, submit.ID)
		return res, nil
	}

	var record struct {
		WaitStatus  string          `json:"waitStatus"`
		Instruction string          `json:"instruction"`
		Result      json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal([]byte(waitOut), &record)

	out := figmaOutput{Action: action, ExitCode: code, JobID: submit.ID, SubmitState: submit.State, Evidence: submit.Evidence, WaitStatus: record.WaitStatus}
	var job struct {
		OK       *bool           `json:"ok"`
		Output   json.RawMessage `json:"output"`
		Error    string          `json:"error"`
		Recovery string          `json:"recovery"`
		Preview  string          `json:"preview"`
	}
	if len(record.Result) > 0 {
		_ = json.Unmarshal(record.Result, &job)
	}
	out.OK = job.OK
	if job.Preview != "" && submit.Evidence != "" {
		out.Preview = filepath.Join(submit.Evidence, job.Preview)
	}
	if len(job.Output) > 0 {
		out.Output = models.TruncateBytes(string(job.Output), figmaOutputMaxBytes)
		out.OutputTruncated = len(job.Output) > figmaOutputMaxBytes
	}
	switch {
	case job.Error != "":
		out.Error = job.Error
	case record.WaitStatus == "":
		out.Error = "wait returned no status; read the job back before any new write"
	}
	switch record.WaitStatus {
	case "timeout":
		out.Instruction = fmt.Sprintf("wait timed out; the job may still be running — call figma action=wait job_id=%s (never resubmit the same edit)", submit.ID)
	case "needs-review":
		out.Instruction = "outcome uncertain: check figma status for unresolved jobs, run an independent inspect of the original target, then resolve — never replay the script"
	default:
		if job.Recovery != "" {
			out.Instruction = job.Recovery
		}
	}

	data, _ := json.Marshal(out)
	res.Content = string(data)
	if job.OK != nil && !*job.OK {
		res.Status = models.CallStatusFailed
		res.Error = figmaFirstNonEmpty(job.Error, "job failed; evidence at "+submit.Evidence)
	}
	return res, nil
}

type figmaOutput struct {
	Action          string `json:"action"`
	ExitCode        int    `json:"exit_code"`
	JobID           string `json:"job_id,omitempty"`
	SubmitState     string `json:"submit_state,omitempty"`
	WaitStatus      string `json:"wait_status,omitempty"`
	OK              *bool  `json:"ok,omitempty"`
	Evidence        string `json:"evidence,omitempty"`
	Preview         string `json:"preview,omitempty"`
	Output          string `json:"output,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
	Instruction     string `json:"instruction,omitempty"`
	CLI             string `json:"cli,omitempty"`
}

func figmaExec(ctx context.Context, argv []string, timeout time.Duration) (stdout string, code int, timedOut bool, err error) {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = figmaQuote(a)
	}
	result, err := sandbox.ExecDirect(ctx, "figma-local "+strings.Join(quoted, " "), timeout)
	if err != nil {
		return "", 0, false, fmt.Errorf("figma-local failed to run: %w", err)
	}
	return result.Stdout(), result.ExitCode(), result.TimedOut(), nil
}

// figmaQuote single-quotes every argument: ExecDirect runs `sh -c`, so double
// quotes would let design text containing $ or backticks become injection.
func figmaQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func figmaWaitFlag(argv []string, args map[string]any) ([]string, time.Duration) {
	if t, ok := args["timeout"].(float64); ok && t >= 1 && t <= 300 {
		return append(argv, "--timeout", strconv.Itoa(int(t))), time.Duration(t+figmaWaitSlackSecs) * time.Second
	}
	// CLI default waits 30s and auto-extends to 150s on background connections.
	return argv, time.Duration(figmaWaitAutoMaxSecs+figmaWaitSlackSecs) * time.Second
}

func figmaCLIError(raw string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &e) == nil && e.Error != "" {
		return e.Error
	}
	return "figma-local exited with code 1"
}

func figmaFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func figmaNumber(f float64) string {
	if f == float64(int(f)) {
		return strconv.Itoa(int(f))
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func hasKey(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

func FigmaTool() models.Tool {
	return models.Tool{
		Name: "figma",
		Description: "Drive local-figma (figma-local CLI + Figma Desktop plugin) to READ and NATIVELY EDIT Figma designs with on-disk evidence. " +
			"One call merges the CLI's submit→wait→result round-trip for actions: inspect, preview, design_system, edit, run. " +
			"Core discipline: jobs are single-flight and must never be replayed — on timeout/needs-review, read the job back (wait/result) and follow the recovery instructions instead of resubmitting. " +
			"Actions: init(url), doctor, status, context (current selection), guide/guide_check(mode: design|refine|design-to-code|code-to-design|prototype), inspect/preview/design_system(node), " +
			"edit(cmd: text|fill|font|layout|align|distribute|prototype|instance|props|bind-fill + params, operates on the current Figma selection — call context first), " +
			"run(script_path, node, high_risk) for trusted JS with full Plugin API, workflow(wf_action: import|status|run|retry|reconcile|approve) for assertion-checked multi-step plans, " +
			"wait/result/diff/validate(job_id), history, resolve(job_id, inspect_job_id), recover. Long-running connect/serve is NOT exposed: start it via bash.",
		Groups: []string{"builtin"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":         map[string]any{"type": "string", "description": "One of: init, doctor, status, context, history, recover, guide, guide_check, inspect, preview, design_system, edit, run, workflow, wait, result, diff, validate, resolve"},
				"url":            map[string]any{"type": "string", "description": "init: Figma frame link containing node-id"},
				"node":           map[string]any{"type": "string", "description": "Subtree scope like 1:2; narrows inspect/preview/design_system/run to stay under snapshot budgets"},
				"mode":           map[string]any{"type": "string", "description": "guide/guide_check scenario: design, refine, design-to-code, code-to-design, prototype"},
				"cmd":            map[string]any{"type": "string", "description": "edit command: text, fill, font, layout, align, distribute, prototype, instance, props, bind-fill"},
				"value":          map[string]any{"type": "string", "description": "edit: new text (text), #RRGGBB (fill), or align/distribute value"},
				"size":           map[string]any{"type": "number", "description": "edit font: font size"},
				"family":         map[string]any{"type": "string", "description": "edit font: family (must pair with style)"},
				"style":          map[string]any{"type": "string", "description": "edit font: style (must pair with family)"},
				"layout":         map[string]any{"type": "object", "description": "edit layout: {direction, horizontal_sizing, vertical_sizing, width, height, gap, padding, padding_top/right/bottom/left, primary_align, counter_align}"},
				"target_id":      map[string]any{"type": "string", "description": "edit prototype: destination frame ID on the same page (replaces all ON_CLICK reactions)"},
				"component_id":   map[string]any{"type": "string", "description": "edit instance: confirmed same-page main component ID"},
				"variable_id":    map[string]any{"type": "string", "description": "edit bind-fill: confirmed color variable ID"},
				"property":       map[string]any{"type": "string", "description": "edit props: full instance property name"},
				"prop_value":     map[string]any{"type": "string", "description": "edit props: property value"},
				"script_path":    map[string]any{"type": "string", "description": "run: path to a trusted JS file executed with figma + target in scope"},
				"high_risk":      map[string]any{"type": "string", "description": "run: risk reason; requires explicit confirmation in the plugin UI before execution"},
				"wf_action":      map[string]any{"type": "string", "description": "workflow: import, status, run, retry, reconcile, approve"},
				"plan_path":      map[string]any{"type": "string", "description": "workflow import: plan JSON path"},
				"plan_id":        map[string]any{"type": "string", "description": "workflow: plan identifier"},
				"step_id":        map[string]any{"type": "string", "description": "workflow approve: step identifier"},
				"note":           map[string]any{"type": "string", "description": "workflow approve: the user's acceptance note; never invent approvals"},
				"job_id":         map[string]any{"type": "string", "description": "wait/result/diff/validate/resolve: job UUID"},
				"inspect_job_id": map[string]any{"type": "string", "description": "resolve: independent inspect job that verified the original target"},
				"timeout":        map[string]any{"type": "number", "description": "wait timeout in seconds (1-300); default 30 with auto-extend to 150 on background plugin connections"},
			},
			"required": []string{"action"},
		},
		Handler: FigmaHandler,
	}
}
