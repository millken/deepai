package agent

import (
	"regexp"
	"testing"
)

// argsHashFormat is the fixed-width hex encoding computeArgsHash must return
// (FNV-1a 64, %016x) instead of leaking the raw "k=v&" argument text.
var argsHashFormat = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestComputeArgsHash tests the args hash computation. computeArgsHash must
// not return the raw concatenated "k=v&" argument text (that leaks bash
// commands/paths/secrets into the metrics JSONL); it must return a fixed
// hash instead. Equal argument sets (regardless of map insertion order,
// since Go map iteration order is randomized) must hash identically, and
// differing argument sets must hash differently, so both the metrics sink
// and the repeat-call breaker's equality semantics keep working.
func TestComputeArgsHash(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{
			name: "empty args",
			args: map[string]any{},
		},
		{
			name: "single arg",
			args: map[string]any{"path": "/tmp/test"},
		},
		{
			name: "multiple args",
			args: map[string]any{
				"path":             "/tmp/test",
				"pattern":          "foo",
				"context":          2,
				"case_insensitive": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeArgsHash(tt.args)
			if !argsHashFormat.MatchString(got) {
				t.Errorf("computeArgsHash() = %q, want match of %s", got, argsHashFormat.String())
			}
		})
	}
}

// TestComputeArgsHashDeterministic verifies that two argument maps built by
// inserting the same key/value pairs in a different order hash identically
// (map key order is not preserved/guaranteed by Go, so this exercises the
// sorted-keys hashing path, not just map equality).
func TestComputeArgsHashDeterministic(t *testing.T) {
	a := map[string]any{}
	for _, kv := range []struct {
		k string
		v any
	}{{"path", "/tmp/test"}, {"pattern", "foo"}, {"context", 2}} {
		a[kv.k] = kv.v
	}

	b := map[string]any{}
	for _, kv := range []struct {
		k string
		v any
	}{{"context", 2}, {"pattern", "foo"}, {"path", "/tmp/test"}} {
		b[kv.k] = kv.v
	}

	hashA := computeArgsHash(a)
	hashB := computeArgsHash(b)
	if hashA != hashB {
		t.Errorf("computeArgsHash() not deterministic across insertion order: %q != %q", hashA, hashB)
	}
	if !argsHashFormat.MatchString(hashA) {
		t.Errorf("computeArgsHash() = %q, want match of %s", hashA, argsHashFormat.String())
	}
}

// TestComputeArgsHashDiffers verifies that different argument sets produce
// different hashes, which the repeat-call breaker's rKey comparison and the
// metrics ArgsHash field both depend on.
func TestComputeArgsHashDiffers(t *testing.T) {
	h1 := computeArgsHash(map[string]any{"path": "/tmp/a"})
	h2 := computeArgsHash(map[string]any{"path": "/tmp/b"})
	if h1 == h2 {
		t.Errorf("computeArgsHash() produced same hash for different args: %q", h1)
	}
	if !argsHashFormat.MatchString(h1) || !argsHashFormat.MatchString(h2) {
		t.Errorf("computeArgsHash() hashes not in expected format: %q, %q", h1, h2)
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
