package agent

import (
	"errors"
	"testing"
)

func TestIsContextOverflowError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection reset by peer"), false},
		{"openai context_length_exceeded", errors.New("HTTP 400: context_length_exceeded"), true},
		{"openai phrase", errors.New("This model's maximum context length is 8192 tokens"), true},
		{"openai reduce length", errors.New("Please reduce the length of the messages"), true},
		{"anthropic prompt too long", errors.New("prompt is too long: 250000 tokens > 200000 maximum"), true},
		{"qwen input too long", errors.New("input is too long for requested model"), true},
		{"generic context window", errors.New("the model context window has been exhausted"), true},
		{"anthropic stop reason as error", errors.New("model_context_window_exceeded"), true},
		// Negative: the substring "too long" alone must NOT trigger to avoid
		// false positives on unrelated complaints (e.g. tool descriptions).
		{"vague too long", errors.New("description is too long"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContextOverflowError(tc.err); got != tc.want {
				t.Fatalf("isContextOverflowError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
