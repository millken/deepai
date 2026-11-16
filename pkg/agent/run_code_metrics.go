package agent

// fillRunCodeMetrics reads the §7.3 measurement fields out of a run_code
// result's Data: script length, nested call count, and the nested results'
// total bytes — the direct measure of what this run kept OUT of context.
// The CALLER gates on ToolName == "run_code" (handleResult); this function
// is purely shape-driven. The journal arrives as []map[string]any on the
// live handler path and as []any after any JSON round trip — both are read.
func fillRunCodeMetrics(m *ToolResultMetric, data map[string]any) {
	if m == nil || data == nil {
		return
	}
	if s, ok := data["script"].(string); ok {
		m.ScriptBytes = len(s)
	}
	if o, ok := data["outcome"].(string); ok {
		m.Outcome = o
	}
	switch journal := data["journal"].(type) {
	case []map[string]any:
		for _, e := range journal {
			m.NestedCalls++
			m.NestedBytes += entryBytes(e)
		}
	case []any:
		for _, raw := range journal {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			m.NestedCalls++
			m.NestedBytes += entryBytes(e)
		}
	}
}

func entryBytes(e map[string]any) int {
	switch v := e["bytes"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
