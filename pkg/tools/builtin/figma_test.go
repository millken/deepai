package builtin

import (
	"strings"
	"testing"
	"time"
)

func TestFigmaBuildArgs_Argv(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    []string
		wantErr string
	}{
		{"init", map[string]any{"action": "init", "url": "https://www.figma.com/design/X/Y?node-id=1-2"}, []string{"init", "https://www.figma.com/design/X/Y?node-id=1-2"}, ""},
		{"init missing url", map[string]any{"action": "init"}, nil, "url is required"},
		{"doctor", map[string]any{"action": "doctor"}, []string{"doctor"}, ""},
		{"guide", map[string]any{"action": "guide", "mode": "refine"}, []string{"guide", "refine"}, ""},
		{"guide_check dash", map[string]any{"action": "guide_check", "mode": "prototype"}, []string{"guide-check", "prototype"}, ""},
		{"guide bad mode", map[string]any{"action": "guide", "mode": "nope"}, nil, "mode must be one of"},
		{"design_system dash", map[string]any{"action": "design_system"}, []string{"design-system"}, ""},
		{"inspect node colon normalized", map[string]any{"action": "inspect", "node": "1-2"}, []string{"inspect", "--node", "1:2"}, ""},
		{"inspect bad node", map[string]any{"action": "inspect", "node": "abc"}, nil, "node must look like"},
		{"run", map[string]any{"action": "run", "script_path": "./s.js", "high_risk": "shared styles"}, []string{"run", "./s.js", "--high-risk", "shared styles"}, ""},
		{"run missing script", map[string]any{"action": "run"}, nil, "script_path is required"},
		{"edit text keeps spaces", map[string]any{"action": "edit", "cmd": "text", "value": "马上 报名"}, []string{"text", "马上 报名"}, ""},
		{"edit text empty allowed", map[string]any{"action": "edit", "cmd": "text", "value": ""}, []string{"text", ""}, ""},
		{"edit text missing", map[string]any{"action": "edit", "cmd": "text"}, nil, "value is required"},
		{"edit fill", map[string]any{"action": "edit", "cmd": "fill", "value": "#00FF00"}, []string{"fill", "#00FF00"}, ""},
		{"edit fill bad hex", map[string]any{"action": "edit", "cmd": "fill", "value": "red"}, nil, "fill value must be #RRGGBB"},
		{"edit font size only", map[string]any{"action": "edit", "cmd": "font", "size": 24.0}, []string{"font", "--size", "24"}, ""},
		{"edit font pair", map[string]any{"action": "edit", "cmd": "font", "family": "Inter", "style": "Semi Bold"}, []string{"font", "--family", "Inter", "--style", "Semi Bold"}, ""},
		{"edit font family alone", map[string]any{"action": "edit", "cmd": "font", "family": "Inter"}, nil, "must be set together"},
		{"edit font empty", map[string]any{"action": "edit", "cmd": "font"}, nil, "font needs"},
		{"edit layout string", map[string]any{"action": "edit", "cmd": "layout", "layout": map[string]any{"direction": "vertical"}}, []string{"layout", "--direction", "vertical"}, ""},
		{"edit layout numeric", map[string]any{"action": "edit", "cmd": "layout", "layout": map[string]any{"padding_top": 20.0}}, []string{"layout", "--padding-top", "20"}, ""},
		{"edit layout unknown key", map[string]any{"action": "edit", "cmd": "layout", "layout": map[string]any{"nope": 1}}, nil, "unknown layout key"},
		{"edit layout negative", map[string]any{"action": "edit", "cmd": "layout", "layout": map[string]any{"gap": -1.0}}, nil, "must be non-negative"},
		{"edit align", map[string]any{"action": "edit", "cmd": "align", "value": "left"}, []string{"align", "left"}, ""},
		{"edit prototype", map[string]any{"action": "edit", "cmd": "prototype", "target_id": "5:6"}, []string{"prototype", "5:6"}, ""},
		{"edit instance", map[string]any{"action": "edit", "cmd": "instance", "component_id": "7:8"}, []string{"instance", "7:8"}, ""},
		{"edit props", map[string]any{"action": "edit", "cmd": "props", "property": "Variant", "prop_value": "true"}, []string{"props", "Variant", "--value", "true"}, ""},
		{"edit bind_fill", map[string]any{"action": "edit", "cmd": "bind_fill", "variable_id": "v1"}, []string{"bind-fill", "v1"}, ""},
		{"edit bad cmd", map[string]any{"action": "edit", "cmd": "explode"}, nil, "cmd must be one of"},
		{"workflow import", map[string]any{"action": "workflow", "wf_action": "import", "plan_path": "./plan.json"}, []string{"workflow", "import", "./plan.json"}, ""},
		{"workflow run", map[string]any{"action": "workflow", "wf_action": "run", "plan_id": "card-demo"}, []string{"workflow", "run", "card-demo"}, ""},
		{"workflow run missing id", map[string]any{"action": "workflow", "wf_action": "run"}, nil, "plan_id is required"},
		{"workflow approve", map[string]any{"action": "workflow", "wf_action": "approve", "plan_id": "p", "step_id": "s", "note": "user accepted"}, []string{"workflow", "approve", "p", "s", "user accepted"}, ""},
		{"workflow bad action", map[string]any{"action": "workflow", "wf_action": "zap"}, nil, "wf_action must be one of"},
		{"wait", map[string]any{"action": "wait", "job_id": "j1"}, []string{"wait", "j1"}, ""},
		{"wait timeout flag", map[string]any{"action": "wait", "job_id": "j1", "timeout": 10.0}, []string{"wait", "j1", "--timeout", "10"}, ""},
		{"result", map[string]any{"action": "diff", "job_id": "j1"}, []string{"diff", "j1"}, ""},
		{"resolve", map[string]any{"action": "resolve", "job_id": "j1", "inspect_job_id": "j2"}, []string{"resolve", "j1", "--inspect", "j2", "--previous-plugin-stopped"}, ""},
		{"resolve missing inspect", map[string]any{"action": "resolve", "job_id": "j1"}, nil, "job_id and inspect_job_id are required"},
		{"unknown action", map[string]any{"action": "zap"}, nil, "unknown action"},
		{"missing action", map[string]any{}, nil, "unknown action"},
	}
	for _, c := range cases {
		plan, err := figmaBuildArgs(strOrEmpty(c.args["action"]), c.args)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: want error %q, got %v", c.name, c.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if strings.Join(plan.argv, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%s: argv = %q, want %q", c.name, plan.argv, c.want)
		}
	}
}

func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}

func TestFigmaBuildArgs_WaitAfterAndTimeouts(t *testing.T) {
	plan, err := figmaBuildArgs("inspect", map[string]any{"action": "inspect"})
	if err != nil || !plan.waitAfter {
		t.Errorf("inspect must wait after submit (err=%v)", err)
	}
	if plan.timeout != figmaSubmitTimeout {
		t.Errorf("inspect timeout = %v, want %v", plan.timeout, figmaSubmitTimeout)
	}
	plan, _ = figmaBuildArgs("status", map[string]any{"action": "status"})
	if plan.waitAfter || plan.timeout != figmaQuickTimeout {
		t.Errorf("status must be quick and not wait (waitAfter=%v timeout=%v)", plan.waitAfter, plan.timeout)
	}
	plan, _ = figmaBuildArgs("workflow", map[string]any{"action": "workflow", "wf_action": "run", "plan_id": "p"})
	if plan.waitAfter || plan.timeout != figmaWorkflowTimeout {
		t.Errorf("workflow run: waitAfter=%v timeout=%v", plan.waitAfter, plan.timeout)
	}
}

func TestFigmaQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      `'plain'`,
		"with space": `'with space'`,
		"it's":       `'it'\''s'`,
		"$HOME":      `'$HOME'`,
		"a`b":        "'a`b'",
		"中文\n": `'中文
'`,
	}
	for in, want := range cases {
		if got := figmaQuote(in); got != want {
			t.Errorf("figmaQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFigmaWaitFlag(t *testing.T) {
	_, timeout := figmaWaitFlag(nil, nil)
	if want := time.Duration(figmaWaitAutoMaxSecs+figmaWaitSlackSecs) * time.Second; timeout != want {
		t.Errorf("default wait timeout = %v, want %v", timeout, want)
	}
	_, timeout = figmaWaitFlag(nil, map[string]any{"timeout": 5.0})
	if want := time.Duration(5+figmaWaitSlackSecs) * time.Second; timeout != want {
		t.Errorf("explicit timeout slack = %v, want %v", timeout, want)
	}
	argv, _ := figmaWaitFlag([]string{"wait", "j"}, map[string]any{"timeout": 7.0})
	if strings.Join(argv, " ") != "wait j --timeout 7" {
		t.Errorf("timeout flag argv = %v", argv)
	}
}

func TestFigmaCLIError(t *testing.T) {
	if got := figmaCLIError(`{"error":"认证失败"}`); got != "认证失败" {
		t.Errorf("figmaCLIError json = %q", got)
	}
	if got := figmaCLIError("not json"); got != "figma-local exited with code 1" {
		t.Errorf("figmaCLIError fallback = %q", got)
	}
}
