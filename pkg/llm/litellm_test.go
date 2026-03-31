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
