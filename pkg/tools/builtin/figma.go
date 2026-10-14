package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/figmabridge"
	"github.com/millken/deepai/pkg/models"
)

const (
	figmaRunOutputMaxBytes = 24 * 1024
	figmaRunSideMaxBytes   = 8 * 1024
	figmaRunWaitSecs       = 150
	figmaRunWaitMaxSecs    = 600
)

// FigmaHandler drives native Figma editing through deepai's embedded local
// bridge: an imported Figma Desktop plugin polls this process, executes
// submitted JS as (figma, target) on the bound frame subtree, and persists
// output/before/after/preview as on-disk evidence. JS is the interface —
// there are no semantic subcommands; the model writes Plugin-API scripts
// (standard snippets live in the figma-ui skill).
func FigmaHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	res := models.ToolResult{CallID: call.ID, ToolName: call.Name}
	action, _ := args["action"].(string)
	str := func(key string) string { s, _ := args[key].(string); return strings.TrimSpace(s) }

	switch action {
	case "":
		return res, fmt.Errorf("action is required")
	case "init":
		return figmaInit(res, str("url"))
	case "doctor", "status":
		return figmaStatus(res, action)
	case "run":
		code, _ := args["code"].(string)
		if strings.TrimSpace(code) == "" {
			return res, fmt.Errorf("code is required: JS executed as (figma, target), e.g. 'return null' to snapshot the target subtree")
		}
		node := strings.ReplaceAll(str("node"), "-", ":")
		timeout := float64(figmaRunWaitSecs)
		if t, ok := args["timeout"].(float64); ok && t >= 1 && t <= figmaRunWaitMaxSecs {
			timeout = t
		}
		return figmaRun(ctx, res, code, node, timeout)
	case "result":
		id := str("job_id")
		if id == "" {
			return res, fmt.Errorf("job_id is required")
		}
		return figmaReadEvidence(res, id)
	case "history":
		b, err := figmabridge.Get()
		if err != nil {
			res.Status = models.CallStatusFailed
			res.Error = err.Error()
			return res, nil
		}
		return figmaJSON(res, map[string]any{"action": "history", "jobs": b.History()})
	case "ack":
		id, note := str("job_id"), str("note")
		if id == "" || note == "" {
			return res, fmt.Errorf("job_id and note are required; note states what was verified in the document (an unresolved job blocks all writes until acknowledged)")
		}
		b, err := figmabridge.Get()
		if err != nil {
			res.Status = models.CallStatusFailed
			res.Error = err.Error()
			return res, nil
		}
		if err := b.Ack(id, note); err != nil {
			res.Status = models.CallStatusFailed
			res.Error = err.Error()
			return res, nil
		}
		return figmaJSON(res, map[string]any{"action": "ack", "job_id": id, "acknowledged": true})
	default:
		return res, fmt.Errorf("unknown action %q (init, doctor, status, run, result, history, ack)", action)
	}
}

func figmaInit(res models.ToolResult, url string) (models.ToolResult, error) {
	if url == "" {
		return res, fmt.Errorf("url is required: Figma frame link with node-id (right-click the frame → Copy link to selection)")
	}
	b, err := figmabridge.Init(url)
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		return res, nil
	}
	status := b.Status()
	return figmaJSON(res, map[string]any{
		"action": "init", "manifest": status.ManifestPath, "binding": status.Binding,
		"instruction": "在 Figma Desktop 打开目标文件 → Plugins → Development → Import plugin from manifest，选择上述 manifest.json，再运行 deepai Figma Bridge；面板显示「已连接 deepai」后继续。插件一次导入长期有效，deepai 重启无需重导。",
	})
}

func figmaStatus(res models.ToolResult, action string) (models.ToolResult, error) {
	b, err := figmabridge.Get()
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		if action == "doctor" {
			res.Error = err.Error() + "；首次使用请先 figma init <画板链接>"
		}
		return res, nil
	}
	out := map[string]any{"action": action}
	statusBytes, _ := json.Marshal(b.Status())
	var status map[string]any
	_ = json.Unmarshal(statusBytes, &status)
	for k, v := range status {
		out[k] = v
	}
	if !b.Status().Connected {
		out["instruction"] = "插件未连接：在 Figma Desktop 运行 deepai Figma Bridge 插件并保持面板开启（若未导入，manifest 路径见 manifestPath 字段）"
	}
	return figmaJSON(res, out)
}

func figmaRun(ctx context.Context, res models.ToolResult, code, node string, timeoutSecs float64) (models.ToolResult, error) {
	b, err := figmabridge.Get()
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		return res, nil
	}
	job, err := b.Submit(code, node)
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		return res, nil
	}
	outcome := b.Wait(ctx, job.ID, time.Duration(timeoutSecs*float64(time.Second)))

	out := map[string]any{
		"action": "run", "job_id": job.ID, "state": outcome.Job.State,
		"wait_status": outcome.WaitStatus, "evidence": b.RunDir(job.ID),
	}
	if outcome.Result != nil {
		r := outcome.Result
		out["ok"] = r.OK
		if len(r.Output) > 0 {
			out["output"] = models.TruncateBytes(string(r.Output), figmaRunOutputMaxBytes)
			out["output_truncated"] = len(r.Output) > figmaRunOutputMaxBytes
		}
		for name, raw := range map[string]json.RawMessage{"before": r.Before, "after": r.After} {
			if len(raw) > 0 {
				out[name] = models.TruncateBytes(string(raw), figmaRunSideMaxBytes)
				out[name+"_truncated"] = len(raw) > figmaRunSideMaxBytes
			}
		}
		if r.Preview != "" {
			out["preview"] = b.PreviewPath(job.ID)
		}
		if r.Error != "" {
			out["error"] = r.Error
		}
		if !r.OK {
			res.Status = models.CallStatusFailed
			res.Error = r.Error
		}
	}
	if outcome.Instruction != "" {
		out["instruction"] = outcome.Instruction
	}
	data, _ := json.Marshal(out)
	res.Content = string(data)
	return res, nil
}

func figmaReadEvidence(res models.ToolResult, id string) (models.ToolResult, error) {
	b, err := figmabridge.Get()
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		return res, nil
	}
	r, err := b.Result(id)
	if err != nil {
		res.Status = models.CallStatusFailed
		res.Error = err.Error()
		return res, nil
	}
	out := map[string]any{"action": "result", "evidence": b.RunDir(id)}
	if p := b.PreviewPath(id); p != "" {
		out["preview"] = p
	}
	rBytes, _ := json.Marshal(r)
	out["result"] = json.RawMessage(models.TruncateBytes(string(rBytes), 48*1024))
	data, _ := json.Marshal(out)
	res.Content = string(data)
	if !r.OK {
		res.Status = models.CallStatusFailed
		res.Error = r.Error
	}
	return res, nil
}

func figmaJSON(res models.ToolResult, v any) (models.ToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return res, err
	}
	res.Content = string(data)
	return res, nil
}

func FigmaTool() models.Tool {
	return models.Tool{
		Name: "figma",
		Description: "Drive native Figma editing via deepai's embedded local bridge (Figma Desktop plugin polls this process). JS is the interface: submit Figma Plugin-API scripts executed as (figma, target) on the bound frame subtree; results (output + before/after structure snapshots + optional preview.png) persist on disk as evidence. " +
			"Standard snippets: read structure = run {code:'return null'} then read the before snapshot; screenshot = run {code:'return await target.exportAsync({format:\\\"PNG\\\"})'} — the result carries an absolute preview.png path. " +
			"Actions: init(url) bind a frame link (with node-id) and emit the plugin manifest; doctor/status bridge health and connection; run(code, node?, timeout?) submit and wait (single-flight); result(job_id)/history read evidence; ack(job_id, note) release an unresolved job after verifying the document. " +
			"Discipline: jobs are never replayed — on timeout read the job back (result/wait again); a delivered job with no result blocks new writes until run-inspect + ack.",
		Groups: []string{"builtin"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []string{"init", "doctor", "status", "run", "result", "history", "ack"}},
				"url":     map[string]any{"type": "string", "description": "init: Figma frame link containing node-id"},
				"code":    map[string]any{"type": "string", "description": "run: JS body executed as (figma, target); return a JSON-serializable value, or a Uint8Array from exportAsync for a preview image"},
				"node":    map[string]any{"type": "string", "description": "run: target node ID like 1:2 within the bound subtree (default: bound frame itself)"},
				"timeout": map[string]any{"type": "number", "description": "run: wait seconds 1-600 (default 150; plugin polls ~750ms when Figma is visible)"},
				"job_id":  map[string]any{"type": "string", "description": "result/ack: job UUID"},
				"note":    map[string]any{"type": "string", "description": "ack: what was verified in the document (e.g. inspect confirmed only the intended text changed)"},
			},
			"required": []string{"action"},
		},
		Handler: FigmaHandler,
	}
}
