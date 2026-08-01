package builtin

import (
	"strings"
	"testing"
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
