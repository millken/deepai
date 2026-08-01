package agent

import (
	"testing"
)

// TestComputeArgsHash tests the args hash computation
func TestComputeArgsHash(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		wantHash string
	}{
		{
			name:     "empty args",
			args:     map[string]any{},
			wantHash: "",
		},
		{
			name:     "single arg",
			args:     map[string]any{"path": "/tmp/test"},
			wantHash: "path=/tmp/test&",
		},
		{
			name: "multiple args",
			args: map[string]any{
				"path":      "/tmp/test",
				"pattern":   "foo",
				"context":   2,
				"case_insensitive": true,
			},
			wantHash: "case_insensitive=true&context=2&path=/tmp/test&pattern=foo&",
		},
		{
			name:     "same args produce same hash",
			args:     map[string]any{"path": "/tmp/test", "pattern": "foo"},
			wantHash: "path=/tmp/test&pattern=foo&",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeArgsHash(tt.args)
			if got != tt.wantHash {
				t.Errorf("computeArgsHash() = %q, want %q", got, tt.wantHash)
			}
		})
	}
}

// TestExtractPathFromArgs tests path extraction from tool arguments
func TestExtractPathFromArgs(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		args     map[string]any
		wantPath string
	}{
		{
			name:     "read_file with path",
			toolName: "read_file",
			args:     map[string]any{"path": "/tmp/test.txt"},
			wantPath: "/tmp/test.txt",
		},
		{
			name:     "edit_file with path",
			toolName: "edit_file",
			args:     map[string]any{"path": "/tmp/test.txt", "content": "new content"},
			wantPath: "/tmp/test.txt",
		},
		{
			name:     "code_map with directory",
			toolName: "code_map",
			args:     map[string]any{"directory": "/tmp/project"},
			wantPath: "/tmp/project",
		},
		{
			name:     "non-file tool",
			toolName: "bash",
			args:     map[string]any{"command": "ls -la"},
			wantPath: "",
		},
		{
			name:     "file tool without path",
			toolName: "read_file",
			args:     map[string]any{"pattern": "test"},
			wantPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPathFromArgs(tt.toolName, tt.args)
			if got != tt.wantPath {
				t.Errorf("extractPathFromArgs() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

// TestEnhancedToolResultMetrics tests the enhanced metrics structure
func TestEnhancedToolResultMetrics(t *testing.T) {
	metric := ToolResultMetric{
		Turn:        1,
		ToolName:    "read_file",
		ResultBytes: 1024,
		ArgsHash:    "path=/tmp/test&",
		Path:        "/tmp/test",
		Offloaded:   false,
		DurationMs:  150,
	}

	// Verify all fields are populated
	if metric.Turn != 1 {
		t.Errorf("Turn = %d, want 1", metric.Turn)
	}
	if metric.ToolName != "read_file" {
		t.Errorf("ToolName = %s, want read_file", metric.ToolName)
	}
	if metric.ResultBytes != 1024 {
		t.Errorf("ResultBytes = %d, want 1024", metric.ResultBytes)
	}
	if metric.ArgsHash == "" {
		t.Error("ArgsHash should not be empty")
	}
	if metric.Path == "" {
		t.Error("Path should not be empty for file tools")
	}
	if metric.DurationMs != 150 {
		t.Errorf("DurationMs = %d, want 150", metric.DurationMs)
	}
}