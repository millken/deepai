package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestBashOutputLimit(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		maxBytes    int
		shouldTrunc bool
		minLength   int
	}{
		{
			name:        "small output under limit",
			output:      "Hello, World!",
			maxBytes:    1024,
			shouldTrunc: false,
			minLength:   13, // "Hello, World!" length
		},
		{
			name:        "output exactly at limit",
			output:      strings.Repeat("A", 100),
			maxBytes:    100,
			shouldTrunc: false,
			minLength:   100,
		},
		{
			name:        "output over limit",
			output:      strings.Repeat("B", 200),
			maxBytes:    100,
			shouldTrunc: true,
			minLength:   100, // Should be truncated to maxBytes
		},
		{
			name:        "large output over 50KB limit",
			output:      strings.Repeat("C", 60000),
			maxBytes:    BashMaxOutputBytes,
			shouldTrunc: true,
			minLength:   BashMaxOutputBytes - 20, // Account for truncation message
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateOutput(tt.output, tt.maxBytes)

			if tt.shouldTrunc && len(result) > tt.maxBytes {
				t.Errorf("Expected truncation, but output length %d exceeds max %d", len(result), tt.maxBytes)
			}
			if !tt.shouldTrunc && len(result) != len(tt.output) {
				t.Errorf("Unexpected truncation: got %d, want %d", len(result), len(tt.output))
			}
			if len(result) < tt.minLength {
				t.Errorf("Result too short: got %d, want at least %d", len(result), tt.minLength)
			}

			// Verify truncation message is present when truncated
			if tt.shouldTrunc {
				if !strings.Contains(result, "... (output truncated)") {
					t.Error("Expected truncation message in output")
				}
			} else {
				if strings.Contains(result, "... (output truncated)") {
					t.Error("Unexpected truncation message in output")
				}
			}
		})
	}
}

func TestBashOutputLimitHeadTail(t *testing.T) {
	// Test that head 70% + tail 30% preservation works
	largeOutput := strings.Repeat("X", 10000)
	maxBytes := 1000

	result := truncateOutput(largeOutput, maxBytes)

	// Should contain truncation message
	if !strings.Contains(result, "... (output truncated)") {
		t.Error("Expected truncation message")
	}

	// Should be within limit
	if len(result) > maxBytes {
		t.Errorf("Result length %d exceeds max %d", len(result), maxBytes)
	}

	// Verify head and tail preservation
	expectedHeadSize := int(float64(maxBytes-20) * 0.7) // 20 for truncation message
	expectedTailSize := (maxBytes - 20) - expectedHeadSize

	if len(result) < expectedHeadSize+expectedTailSize+20 {
		t.Errorf("Result too short for head+tail preservation: got %d, expected ~%d", len(result), expectedHeadSize+expectedTailSize+20)
	}
}

func TestBashOutputLimitEmptyOutput(t *testing.T) {
	result := truncateOutput("", 100)
	if result != "" {
		t.Errorf("Empty input should return empty output, got %q", result)
	}
}

func TestBashOutputLimitSmallLimit(t *testing.T) {
	// Test when maxBytes is smaller than truncation message
	output := "Hello, World!"
	maxBytes := 5

	result := truncateOutput(output, maxBytes)
	if len(result) > maxBytes {
		t.Errorf("Result length %d exceeds max %d", len(result), maxBytes)
	}
}

// TestBashHandler_TimeoutIsExplicit pins the contract that a killed command
// explains itself. A silently hanging command produces no output at all, so
// `{"stdout":"","stderr":"","exit_code":-1}` gave the model nothing to reason
// about and it just re-ran the command — eight times in session
// 20260812_093415_fc6e.
func TestBashHandler_TimeoutIsExplicit(t *testing.T) {
	res, err := BashHandler(context.Background(), models.ToolCall{
		ID:        "c1",
		Name:      "bash",
		Arguments: map[string]any{"command": "sleep 5", "timeout": float64(1)},
	})
	if err != nil {
		t.Fatalf("BashHandler: %v", err)
	}

	if res.Status != models.CallStatusFailed {
		t.Errorf("status = %q, want failed (the breaker, the provider and the UI all read this)", res.Status)
	}
	if !strings.Contains(res.Error, "TIMED OUT") {
		t.Errorf("Error does not say it timed out: %q", res.Error)
	}
	if !strings.Contains(res.Error, "NO output") {
		t.Errorf("Error should state that nothing was captured: %q", res.Error)
	}
	// The model is told what to do instead of re-running it verbatim.
	for _, want := range []string{"raise the timeout", "narrower subset"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("Error is missing actionable guidance %q: %s", want, res.Error)
		}
	}

	var out BashOutput
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("content is not BashOutput JSON: %v", err)
	}
	if !out.TimedOut {
		t.Error("BashOutput.timed_out must be set so the payload is self-describing too")
	}
	if out.DurationSeconds < 1 {
		t.Errorf("duration_seconds = %v, want >= 1", out.DurationSeconds)
	}
}

// TestBashHandler_TimeoutKeepsPartialOutput: when the command DID print before
// being killed, that output must survive into the message the model sees —
// which is res.Error, because toolMessageContent prefers Error over Content.
func TestBashHandler_TimeoutKeepsPartialOutput(t *testing.T) {
	res, err := BashHandler(context.Background(), models.ToolCall{
		ID:        "c2",
		Name:      "bash",
		Arguments: map[string]any{"command": "echo starting-work; sleep 5", "timeout": float64(1)},
	})
	if err != nil {
		t.Fatalf("BashHandler: %v", err)
	}
	if !strings.Contains(res.Error, "TIMED OUT") {
		t.Errorf("Error does not say it timed out: %q", res.Error)
	}
	if !strings.Contains(res.Error, "starting-work") {
		t.Errorf("partial output was dropped from the message the model sees: %q", res.Error)
	}
}

// TestBashHandler_NormalExitIsUnchanged guards the non-timeout path: no
// timed_out field, no synthetic failure status.
func TestBashHandler_NormalExitIsUnchanged(t *testing.T) {
	res, err := BashHandler(context.Background(), models.ToolCall{
		ID:        "c3",
		Name:      "bash",
		Arguments: map[string]any{"command": "echo hi; exit 3"},
	})
	if err != nil {
		t.Fatalf("BashHandler: %v", err)
	}
	if res.Status != "" || res.Error != "" {
		t.Errorf("a command that exited on its own must not be marked failed by the handler: status=%q err=%q",
			res.Status, res.Error)
	}
	var out BashOutput
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("content is not BashOutput JSON: %v", err)
	}
	if out.TimedOut {
		t.Error("timed_out set on a command that exited on its own")
	}
	if out.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", out.ExitCode)
	}
}
