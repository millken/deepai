package agent

import (
	"encoding/json"
	"testing"
)

// fillRunCodeMetrics reads §7.3 fields out of a run_code result's Data. The
// journal arrives as []map[string]any on the live handler path but as []any
// after any JSON round trip (session reload, nested transport) — both shapes
// must work.
func TestFillRunCodeMetrics(t *testing.T) {
	t.Run("live shape []map[string]any", func(t *testing.T) {
		m := ToolResultMetric{}
		fillRunCodeMetrics(&m, map[string]any{
			"script":  "await tools.grep({}); return 1;",
			"outcome": "ok",
			"journal": []map[string]any{
				{"seq": 1, "tool": "grep", "status": "completed", "bytes": 3120},
				{"seq": 2, "tool": "edit_file", "status": "completed", "bytes": 410},
			},
		})
		if m.ScriptBytes != len("await tools.grep({}); return 1;") {
			t.Errorf("ScriptBytes = %d", m.ScriptBytes)
		}
		if m.NestedCalls != 2 {
			t.Errorf("NestedCalls = %d", m.NestedCalls)
		}
		if m.NestedBytes != 3530 {
			t.Errorf("NestedBytes = %d, want 3530 (the stayed-out-of-context measure)", m.NestedBytes)
		}
		if m.Outcome != "ok" {
			t.Errorf("Outcome = %q", m.Outcome)
		}
	})

	t.Run("json roundtrip shape []any", func(t *testing.T) {
		raw := `{"script":"return 1;","outcome":"timeout","journal":[{"bytes":100},{"bytes":50}]}`
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatal(err)
		}
		m := ToolResultMetric{}
		fillRunCodeMetrics(&m, data)
		if m.NestedCalls != 2 || m.NestedBytes != 150 || m.Outcome != "timeout" {
			t.Errorf("m = %+v", m)
		}
	})

	t.Run("absent keys leave zeros", func(t *testing.T) {
		m := ToolResultMetric{}
		fillRunCodeMetrics(&m, map[string]any{})
		if m.ScriptBytes != 0 || m.NestedCalls != 0 || m.NestedBytes != 0 || m.Outcome != "" {
			t.Errorf("m = %+v, want zeros", m)
		}
	})
}
