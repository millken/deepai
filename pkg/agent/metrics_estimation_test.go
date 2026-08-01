package agent

import (
	"testing"
)

func TestEstimateInputTokens(t *testing.T) {
	tests := []struct {
		name     string
		ctx      ContextBytes
		expected int
	}{
		{
			name:     "empty context",
			ctx:      ContextBytes{TotalBytes: 0},
			expected: 0,
		},
		{
			name:     "small context (300 bytes)",
			ctx:      ContextBytes{TotalBytes: 300},
			expected: 90, // 300 / 3.3 = 90.9
		},
		{
			name:     "medium context (30KB)",
			ctx:      ContextBytes{TotalBytes: 30000},
			expected: 9090, // 30000 / 3.3 = 9090.9
		},
		{
			name:     "large context (300KB)",
			ctx:      ContextBytes{TotalBytes: 300000},
			expected: 90909, // 300000 / 3.3 = 90909.09
		},
		{
			name: "realistic session context",
			ctx: ContextBytes{
				SystemBytes:    13676,
				SchemaBytes:    21637,
				HumanBytes:     107,
				AIContentBytes: 86,
				AIArgsBytes:    1949,
				ToolBytes:      20430,
				TotalBytes:     57885,
			},
			expected: 17540, // 57885 / 3.3 = 17540.9
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateInputTokens(tt.ctx)
			if got != tt.expected {
				t.Errorf("estimateInputTokens() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestEstimateInputTokens_Rounding(t *testing.T) {
	// Test that we use integer division (floor)
	tests := []struct {
		bytes    int
		expected int
	}{
		{1, 0},    // 1 / 3.3 = 0
		{2, 0},    // 2 / 3.3 = 0
		{3, 0},    // 3 / 3.3 = 0
		{4, 1},    // 4 / 3.3 = 1
		{5, 1},    // 5 / 3.3 = 1
		{6, 1},    // 6 / 3.3 = 1
		{299, 90}, // 299 / 3.3 = 90.6
		{300, 90}, // 300 / 3.3 = 90.9
		{301, 91}, // 301 / 3.3 = 91.2
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.bytes)), func(t *testing.T) {
			ctx := ContextBytes{TotalBytes: tt.bytes}
			got := estimateInputTokens(ctx)
			if got != tt.expected {
				t.Errorf("estimateInputTokens(%d bytes) = %d, want %d", tt.bytes, got, tt.expected)
			}
		})
	}
}
