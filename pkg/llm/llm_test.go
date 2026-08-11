package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestChatRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     ChatRequest
		wantErr bool
	}{
		{
			name: "valid request",
			req: ChatRequest{
				Model:    "test-model",
				Messages: []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hello"}},
			},
			wantErr: false,
		},
		{
			name: "empty model",
			req: ChatRequest{
				Model:    "",
				Messages: []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hello"}},
			},
			wantErr: true,
		},
		{
			name: "empty messages",
			req: ChatRequest{
				Model:    "test-model",
				Messages: []models.Message{},
			},
			wantErr: true,
		},
		{
			name: "nil messages",
			req: ChatRequest{
				Model:    "test-model",
				Messages: nil,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUnavailableProvider(t *testing.T) {
	provider := &UnavailableProvider{err: errors.New("unavailable")}

	// Test Chat
	_, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "test",
		Messages: []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"}},
	})
	if err == nil {
		t.Error("Chat should return error for UnavailableProvider")
	}

	// Test Stream - the error is sent in the channel, not returned
	ch, err := provider.Stream(context.Background(), ChatRequest{
		Model:    "test",
		Messages: []models.Message{{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "hi"}},
	})
	if err != nil {
		t.Errorf("Stream should not return error directly: %v", err)
	}

	// Check the channel receives the error
	chunk := <-ch
	if chunk.Err == nil {
		t.Error("Stream chunk should contain error")
	}
}

func TestNewProvider(t *testing.T) {
	// Test with openai
	provider := NewProvider("openai")
	if provider == nil {
		t.Error("NewProvider should return a provider")
	}

	// Test with siliconflow
	provider = NewProvider("siliconflow")
	if provider == nil {
		t.Error("NewProvider should return a provider for siliconflow")
	}

	// Test with invalid provider name (should return unavailable)
	provider = NewProvider("nonexistent")
	if provider == nil {
		t.Error("NewProvider should return unavailable provider for invalid names")
	}
}

func TestUsage(t *testing.T) {
	usage := Usage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	}

	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
	if usage.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", usage.TotalTokens)
	}
}

func TestStreamChunk(t *testing.T) {
	chunk := StreamChunk{
		Delta: "Hello",
		Done:  false,
	}

	if chunk.Delta != "Hello" {
		t.Errorf("Delta = %s, want Hello", chunk.Delta)
	}
	if chunk.Done {
		t.Error("Done should be false")
	}

	// Test done chunk
	doneChunk := StreamChunk{
		Done:  true,
		Usage: &Usage{TotalTokens: 100},
	}

	if !doneChunk.Done {
		t.Error("Done should be true")
	}
	if doneChunk.Usage.TotalTokens != 100 {
		t.Errorf("Usage.TotalTokens = %d, want 100", doneChunk.Usage.TotalTokens)
	}
}

func TestChatResponse(t *testing.T) {
	resp := ChatResponse{
		Model: "test-model",
		Message: models.Message{
			ID:      "m1",
			Role:    models.RoleAI,
			Content: "Hello, world!",
		},
		Usage: Usage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
		Stop: "stop",
	}

	if resp.Model != "test-model" {
		t.Errorf("Model = %s, want 'test-model'", resp.Model)
	}
	if resp.Message.Content != "Hello, world!" {
		t.Errorf("Content = %s, want 'Hello, world!'", resp.Message.Content)
	}
	if resp.Stop != "stop" {
		t.Errorf("Stop = %s, want 'stop'", resp.Stop)
	}
}

// TestNewToolArgsJSONError_ExplainsTruncationAndWhatToDo pins the P2c5
// brief's Fix 3 directly: the bare "invalid arguments JSON: unexpected end
// of JSON input" that reached the user gave no hint that a truncated
// stream was the likely cause, even though anthropic.go's own comment next
// to that line already names it ("e.g. stream was truncated"). The
// constructed message must say what most likely happened (the response hit
// its output limit while emitting a large tool argument) and what to do
// about it (shrink the argument, or use the tool's file-based parameter if
// it has one).
func TestNewToolArgsJSONError_ExplainsTruncationAndWhatToDo(t *testing.T) {
	cause := &json.SyntaxError{Offset: 42}
	err := newToolArgsJSONError("docx_write", cause)
	msg := err.Error()

	if !strings.Contains(msg, `"docx_write"`) {
		t.Errorf("error = %q, want it to name the tool", msg)
	}
	lower := strings.ToLower(msg)
	for _, phrase := range []string{"output token limit", "smaller", "file-based parameter"} {
		if !strings.Contains(lower, phrase) {
			t.Errorf("error = %q, want it to contain %q", msg, phrase)
		}
	}
}

// TestNewToolArgsJSONError_WrapsTheOriginalCause pins that the underlying
// json.Unmarshal error (e.g. "unexpected end of JSON input") is still
// reachable via errors.Is/Unwrap, not replaced outright: a caller or log
// line that inspects the cause (e.g. isRetryableAnthropicStreamErr-style
// classification, or just a human comparing against the raw stdlib error)
// must still find it.
func TestNewToolArgsJSONError_WrapsTheOriginalCause(t *testing.T) {
	var cause error
	unmarshalErr := json.Unmarshal([]byte(`{"a":`), &struct{}{})
	cause = unmarshalErr
	if cause == nil {
		t.Fatal("test setup: expected json.Unmarshal to fail on truncated input")
	}
	err := newToolArgsJSONError("bash", cause)
	if !errors.Is(err, cause) {
		t.Errorf("newToolArgsJSONError does not wrap the original cause: %v", err)
	}
}

// TestNewToolArgsJSONError_IsProviderAgnostic pins the brief's explicit
// constraint that pkg/llm must not know about docx: the message is built
// and used identically for every tool, so it must never name a specific
// tool's own remedy (e.g. "use markdown_path") — only the generic "this
// tool's file-based parameter" framing, which stays correct regardless of
// which tool triggered it.
func TestNewToolArgsJSONError_IsProviderAgnostic(t *testing.T) {
	// A neutral tool name: the message template itself must never add
	// docx-specific wording on top of whatever tool name the caller passed
	// in — pkg/llm has no import of, or knowledge about, pkg/docx.
	err := newToolArgsJSONError("bash", errors.New("unexpected end of JSON input"))
	lower := strings.ToLower(err.Error())
	for _, term := range []string{"docx", "markdown_path", "write_file"} {
		if strings.Contains(lower, term) {
			t.Errorf("error = %q, must stay provider/tool-agnostic and not mention %q", err.Error(), term)
		}
	}
}
