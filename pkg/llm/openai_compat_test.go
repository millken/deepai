package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/openai/openai-go/v3"
)

func TestMapMessagesToOpenAI_SystemPrompt(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "hello"},
	}
	result := mapMessagesToOpenAI("you are helpful", msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(result))
	}
	if result[0].OfSystem == nil {
		t.Error("first message should be system")
	}
	if result[1].OfUser == nil {
		t.Error("second message should be user")
	}
}

func TestMapMessagesToOpenAI_ToolResult(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "test"}}, Content: ""},
		{Role: models.RoleTool, Content: "result", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "test"}},
	}
	result := mapMessagesToOpenAI("", msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].OfAssistant == nil {
		t.Error("first message should be assistant")
	}
	if result[1].OfTool == nil {
		t.Error("second message should be tool")
	}
	if result[1].OfTool.ToolCallID != "c1" {
		t.Errorf("tool_call_id: got %q, want %q", result[1].OfTool.ToolCallID, "c1")
	}
}

func TestMapMessagesToOpenAI_AssistantWithToolCalls(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "/test.go"}},
		}, Content: "thinking..."},
	}
	result := mapMessagesToOpenAI("", msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].OfAssistant == nil {
		t.Fatal("expected assistant message")
	}
	if len(result[0].OfAssistant.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(result[0].OfAssistant.ToolCalls))
	}
	if result[0].OfAssistant.ToolCalls[0].OfFunction == nil {
		t.Fatal("expected function tool call")
	}
	if result[0].OfAssistant.ToolCalls[0].OfFunction.ID != "c1" {
		t.Errorf("tool call id: got %q", result[0].OfAssistant.ToolCalls[0].OfFunction.ID)
	}
}

func TestMapMessagesToOpenAI_EmptyContent(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: ""},
		{Role: models.RoleAI, Content: ""},
		{Role: models.RoleTool, Content: "", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "t"}},
	}
	result := mapMessagesToOpenAI("", msgs)
	for _, m := range result {
		if m.OfTool != nil {
			if m.OfTool.Content.OfString.Value == "" {
				t.Error("tool content should not be empty")
			}
		}
	}
}

func TestIsRetryableOpenAIStreamErr(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{400, false},
		{401, false},
		{403, false},
	}
	for _, tt := range tests {
		err := &openai.Error{StatusCode: tt.statusCode}
		got := isRetryableOpenAIStreamErr(err)
		if got != tt.want {
			t.Errorf("statusCode %d: got %v, want %v", tt.statusCode, got, tt.want)
		}
	}
	if isRetryableOpenAIStreamErr(errors.New("generic")) {
		t.Error("generic error should not be retryable")
	}
}

func TestMapToolsToOpenAI(t *testing.T) {
	tools := []models.Tool{
		{Name: "read_file", Description: "Read a file", InputSchema: map[string]any{"type": "object"}},
	}
	result := mapToolsToOpenAI(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].OfFunction == nil {
		t.Fatal("expected function tool")
	}
	if result[0].OfFunction.Function.Name != "read_file" {
		t.Errorf("tool name: got %q", result[0].OfFunction.Function.Name)
	}
}

func TestNewOpenAICompatProvider_NoAPIKey(t *testing.T) {
	_, err := NewOpenAICompatProvider("test", "", "", &http.Client{})
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestAssembleToolCalls_SparseAndOutOfOrderIndices(t *testing.T) {
	// Non-contiguous, non-zero-based indices arriving out of order. The old
	// 0..len-1 positional loop dropped these; assembly must keep all of them,
	// ordered by index.
	builders := map[int64]*toolCallBuilder{
		2: {id: "c2", name: "grep", args: `{"q":"b"}`},
		5: {id: "c5", name: "bash", args: `{"cmd":"ls"}`},
	}
	calls, bad, err := assembleToolCalls(builders)
	if err != nil {
		t.Fatalf("unexpected error (%s): %v", bad, err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (sparse indices dropped)", len(calls))
	}
	if calls[0].Name != "grep" || calls[1].Name != "bash" {
		t.Fatalf("calls not ordered by index: %s, %s", calls[0].Name, calls[1].Name)
	}
}

func TestAssembleToolCalls_SingleNonZeroIndex(t *testing.T) {
	builders := map[int64]*toolCallBuilder{
		1: {id: "c1", name: "read_file", args: `{"path":"x"}`},
	}
	calls, _, err := assembleToolCalls(builders)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("single non-zero-index tool call lost: %+v", calls)
	}
}

func TestAssembleToolCalls_MalformedArgs(t *testing.T) {
	builders := map[int64]*toolCallBuilder{
		0: {id: "c0", name: "bash", args: `{"cmd":`},
	}
	_, bad, err := assembleToolCalls(builders)
	if err == nil {
		t.Fatal("expected error for malformed JSON args")
	}
	if bad != "bash" {
		t.Fatalf("bad tool name = %q, want bash", bad)
	}
}

func TestIsTransientStreamError(t *testing.T) {
	// The exact in-band SSE error event the user hit: HTTP 200, transient
	// "网络错误，请稍后重试" delivered as an error event. Must be retryable.
	userErr := errors.New(`received error while streaming: {"type":"error","error":{"type":"api_error","code":"1234","message":"[1234][网络错误，错误id：202606091444139caa2f31f60240f3，请稍后重试][202606091444139caa2f31f60240f3]"},"request_id":"202606091444139caa2f31f60240f3"}`)
	if !isTransientStreamError(userErr) {
		t.Fatal("user's in-band network error must be classified retryable")
	}

	retryable := []error{
		errors.New("unexpected EOF"),
		errors.New("connection reset by peer"),
		errors.New(`{"type":"error","error":{"type":"overloaded_error"}}`),
		errors.New("502 Bad Gateway"),
		errors.New("服务繁忙，请稍后重试"),
	}
	for _, e := range retryable {
		if !isTransientStreamError(e) {
			t.Errorf("expected retryable: %v", e)
		}
	}

	notRetryable := []error{
		errors.New("invalid_request_error: messages.0 missing"),
		errors.New("authentication_error: invalid x-api-key"),
		errors.New("model not found"),
		context.Canceled,
		context.DeadlineExceeded,
	}
	for _, e := range notRetryable {
		if isTransientStreamError(e) {
			t.Errorf("expected NOT retryable: %v", e)
		}
	}
}
