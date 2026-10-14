package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func figmaCall(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return FigmaHandler(context.Background(), models.ToolCall{
		ID: "c1", Name: "figma", Arguments: args,
	})
}

// 只覆盖不触发桥连接的参数校验分支；桥生命周期见 figmabridge 包测试。
func TestFigma_ArgumentValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"missing action", map[string]any{}, "action is required"},
		{"unknown action", map[string]any{"action": "inspect"}, "unknown action"},
		{"init without url", map[string]any{"action": "init"}, "url is required"},
		{"run without code", map[string]any{"action": "run"}, "code is required"},
		{"result without id", map[string]any{"action": "result"}, "job_id is required"},
		{"ack without id", map[string]any{"action": "ack", "note": "n"}, "job_id and note are required"},
		{"ack without note", map[string]any{"action": "ack", "job_id": "j"}, "job_id and note are required"},
	}
	for _, c := range cases {
		_, err := figmaCall(t, c.args)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error %q, got %v", c.name, c.wantErr, err)
		}
	}
}

func TestFigmaTool_Schema(t *testing.T) {
	tool := FigmaTool()
	if tool.Name != "figma" || tool.Handler == nil {
		t.Fatalf("tool name/handler invalid: %q", tool.Name)
	}
	schema := tool.InputSchema["properties"].(map[string]any)
	for _, key := range []string{"action", "url", "code", "node", "timeout", "job_id", "note"} {
		if _, ok := schema[key]; !ok {
			t.Errorf("schema missing property %q", key)
		}
	}
	enum := tool.InputSchema["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
	want := []string{"init", "doctor", "status", "run", "result", "history", "ack"}
	if strings.Join(enum, ",") != strings.Join(want, ",") {
		t.Errorf("action enum = %v, want %v", enum, want)
	}
}
