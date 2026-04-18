package llm

import (
	"reflect"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/voocel/litellm"
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
