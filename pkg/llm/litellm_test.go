package llm

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/providers"
)

func TestAvalibleProvider(t *testing.T) {
	t.Log(litellm.ListRegisteredProviders())
}

func TestMapChatReqToLitellmRequest(t *testing.T) {
	temp := 0.7
	max := 123
	req := ChatRequest{
		Model:           "test-model",
		Messages:        []models.Message{},
		ReasoningEffort: "high",
		Temperature:     &temp,
		MaxTokens:       &max,
		Tools: []models.Tool{{
			Name:        "toolA",
			Description: "desc",
			InputSchema: map[string]any{"field": "value"},
			Handler:     nil,
		}},
	}

	msgs := []litellm.Message{{Role: "user", Content: "hello"}}
	r := mapChatReqToLitellmRequest(req, msgs)

	if r == nil {
		t.Fatalf("expected non-nil litellm.Request")
	}
	if r.Model != req.Model {
		t.Fatalf("model mismatch: got %q want %q", r.Model, req.Model)
	}
	if len(r.Messages) != 1 || r.Messages[0].Content != "hello" {
		t.Fatalf("messages not preserved")
	}

	if r.Thinking == nil || r.Thinking.Level != "high" {
		t.Fatalf("thinking not set: %#v", r.Thinking)
	}

	if r.Temperature == nil || *r.Temperature != *req.Temperature {
		t.Fatalf("temperature not propagated: got %v want %v", r.Temperature, req.Temperature)
	}

	if r.MaxTokens == nil || *r.MaxTokens != *req.MaxTokens {
		t.Fatalf("max_tokens not propagated: got %v want %v", r.MaxTokens, req.MaxTokens)
	}

	if len(r.Tools) != 1 {
		t.Fatalf("tools not propagated: %#v", r.Tools)
	}
	tool := r.Tools[0]
	if tool.Type != "function" {
		t.Fatalf("unexpected tool type: %q", tool.Type)
	}
	if tool.Function.Name != "toolA" || tool.Function.Description != "desc" {
		t.Fatalf("function metadata mismatch: %#v", tool.Function)
	}
	if !reflect.DeepEqual(tool.Function.Parameters, req.Tools[0].InputSchema) {
		t.Fatalf("function parameters mismatch: got %#v want %#v", tool.Function.Parameters, req.Tools[0].InputSchema)
	}
}

func TestMapChatReqToLitellmRequest_DisableThinking(t *testing.T) {
	req := ChatRequest{
		Model:           "test-model",
		Messages:        []models.Message{},
		ReasoningEffort: "disabled",
	}

	msgs := []litellm.Message{{Role: "user", Content: "hello"}}
	r := mapChatReqToLitellmRequest(req, msgs)

	if r == nil {
		t.Fatalf("expected non-nil litellm.Request")
	}
	if r.Thinking == nil {
		t.Fatalf("thinking should be set when reasoning_effort is disabled")
	}
	if r.Thinking.Type != litellm.ThinkingDisabled {
		t.Fatalf("thinking type mismatch: got %q want %q", r.Thinking.Type, litellm.ThinkingDisabled)
	}
}

func TestExtractResponseContent(t *testing.T) {
	tests := []struct {
		name string
		resp *litellm.Response
		want string
	}{
		{
			name: "prefer content field",
			resp: &litellm.Response{
				Content:  "title from content",
				Contents: []litellm.MessageContent{{Type: "text", Text: "title from contents"}},
			},
			want: "title from content",
		},
		{
			name: "fallback to contents text",
			resp: &litellm.Response{
				Content:  "",
				Contents: []litellm.MessageContent{{Type: "text", Text: "title from contents"}},
			},
			want: "title from contents",
		},
		{
			name: "empty when no text",
			resp: &litellm.Response{
				Content:  "",
				Contents: []litellm.MessageContent{{Type: "text", Text: "   "}},
			},
			want: "",
		},
		{
			name: "fallback to reasoning content",
			resp: &litellm.Response{
				Content:          "",
				Contents:         []litellm.MessageContent{{Type: "text", Text: ""}},
				ReasoningContent: "title from reasoning",
			},
			want: "title from reasoning",
		},
		{
			name: "nil response",
			resp: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractResponseContent(tt.resp)
			if got != tt.want {
				t.Fatalf("extractResponseContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsRetryableStreamErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "network error",
			err:  providers.NewNetworkError("test", "conn reset", nil),
			want: true,
		},
		{
			name: "timeout error",
			err:  providers.NewError(providers.ErrorTypeTimeout, "deadline exceeded"),
			want: true,
		},
		{
			name: "rate limit error",
			err:  providers.NewHTTPError("test", 429, "slow down"),
			want: true,
		},
		{
			name: "overloaded error (529)",
			err:  providers.NewHTTPError("test", 529, "overloaded"),
			want: true,
		},
		{
			name: "provider 5xx error",
			err:  providers.NewHTTPError("test", 502, "bad gateway"),
			want: true,
		},
		{
			name: "provider 500 error",
			err:  providers.NewHTTPError("test", 500, "internal error"),
			want: true,
		},
		{
			name: "provider 4xx error (not retryable)",
			err:  providers.NewHTTPError("test", 400, "bad request"),
			want: false,
		},
		{
			name: "auth error",
			err:  providers.NewAuthError("test", "invalid key"),
			want: false,
		},
		{
			name: "validation error",
			err:  providers.NewValidationError("test", "invalid param"),
			want: false,
		},
		{
			name: "model not found error",
			err:  providers.NewModelError("test", "test-model", "model not found"),
			want: false,
		},
		{
			name: "generic error (not retryable)",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "anthropic stream error (not retryable)",
			err:  errors.New("anthropic: stream error: [] "),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryableStreamErr(tt.err)
			if got != tt.want {
				t.Errorf("isRetryableStreamErr() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamRetryDelay(t *testing.T) {
	tests := []struct {
		attempt   int
		wantMin   time.Duration
		wantMax   time.Duration
	}{
		{attempt: 0, wantMin: 500 * time.Millisecond, wantMax: 5 * time.Second},
		{attempt: 1, wantMin: 1 * time.Second, wantMax: 5 * time.Second},
		{attempt: 2, wantMin: 2 * time.Second, wantMax: 10 * time.Second},
		{attempt: 5, wantMin: 10 * time.Second, wantMax: 40 * time.Second},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			got := streamRetryDelay(tt.attempt)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("streamRetryDelay(%d) = %v, want between %v and %v", tt.attempt, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestStreamRetryDelay_Capped(t *testing.T) {
	// Very high attempt should still be capped near 30s.
	got := streamRetryDelay(20)
	if got > 40*time.Second {
		t.Errorf("streamRetryDelay(20) = %v, should be capped near 30s", got)
	}
}
