package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

func TestFileMetricsSink_WritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.jsonl")
	s := NewFileMetricsSink(path)

	s.RecordTurn(TurnMetrics{
		Turn: 3, InputTokens: 1200, OutputTokens: 80,
		Context: ContextBytes{ToolBytes: 300, TotalBytes: 1200},
	})
	s.RecordToolResult(ToolResultMetric{Turn: 3, ToolName: "read_file", ResultBytes: 4096})

	recs := readJSONL(t, path)
	if len(recs) != 2 {
		t.Fatalf("want 2 JSONL records, got %d", len(recs))
	}

	// Turn record: type tag + flattened fields + nested context.
	if recs[0]["type"] != "turn" {
		t.Errorf("record 0 type = %v, want turn", recs[0]["type"])
	}
	if recs[0]["input_tokens"].(float64) != 1200 {
		t.Errorf("input_tokens = %v, want 1200", recs[0]["input_tokens"])
	}
	ctx, ok := recs[0]["context"].(map[string]any)
	if !ok || ctx["tool_bytes"].(float64) != 300 {
		t.Errorf("nested context.tool_bytes wrong: %v", recs[0]["context"])
	}

	// Tool record.
	if recs[1]["type"] != "tool" || recs[1]["tool_name"] != "read_file" {
		t.Errorf("tool record wrong: %v", recs[1])
	}
	if recs[1]["result_bytes"].(float64) != 4096 {
		t.Errorf("result_bytes = %v, want 4096", recs[1]["result_bytes"])
	}
}

func TestFileMetricsSink_AppendsAcrossSinks(t *testing.T) {
	// Two sinks on the same path (mimicking parent agent + subagent) must append,
	// not truncate each other.
	path := filepath.Join(t.TempDir(), "shared.jsonl")
	NewFileMetricsSink(path).RecordTurn(TurnMetrics{Turn: 0})
	NewFileMetricsSink(path).RecordTurn(TurnMetrics{Turn: 1})

	recs := readJSONL(t, path)
	if len(recs) != 2 {
		t.Fatalf("append across sinks should yield 2 records, got %d", len(recs))
	}
	if recs[0]["turn"].(float64) != 0 || recs[1]["turn"].(float64) != 1 {
		t.Errorf("records out of order or wrong: %v", recs)
	}
}

func TestFileMetricsSink_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "m.jsonl")
	s := NewFileMetricsSink(path)
	s.RecordTurn(TurnMetrics{Turn: 0})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sink should create parent dirs and write file: %v", err)
	}
}

// TestMetrics_EnvToFile_EndToEnd proves the whole wire: DEEPAI_TOKEN_METRICS=path
// -> New() attaches a FileMetricsSink -> Run() emits records -> JSONL on disk.
func TestMetrics_EnvToFile_EndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	t.Setenv(envTokenMetrics, path)

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "echo",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "hi", Status: models.CallStatusCompleted}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{LLMProvider: &metricsProvider{}, Tools: reg, Model: "m"})
	if _, err := a.Run(context.Background(), "s", []models.Message{{Role: models.RoleHuman, Content: "go"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs := readJSONL(t, path)
	var turns, toolsSeen int
	for _, r := range recs {
		switch r["type"] {
		case "turn":
			turns++
		case "tool":
			toolsSeen++
		}
	}
	if turns < 1 || toolsSeen < 1 {
		t.Fatalf("expected at least 1 turn + 1 tool record on disk, got turns=%d tools=%d (recs=%v)", turns, toolsSeen, recs)
	}
}

func TestFileMetricsSink_WriteErrorIsNonFatal(t *testing.T) {
	// Point at a path whose parent is a file, so opening for write fails. The
	// sink must swallow the error rather than panic.
	base := t.TempDir()
	fileAsDir := filepath.Join(base, "afile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &FileMetricsSink{path: filepath.Join(fileAsDir, "m.jsonl")} // parent is a file
	s.RecordTurn(TurnMetrics{Turn: 0})                               // must not panic
	s.RecordToolResult(ToolResultMetric{Turn: 0, ToolName: "x"})     // must not panic
}
