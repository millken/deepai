package llm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/openai/openai-go"
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
	if result[0].OfAssistant.ToolCalls[0].ID != "c1" {
		t.Errorf("tool call id: got %q", result[0].OfAssistant.ToolCalls[0].ID)
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
	if result[0].Function.Name != "read_file" {
		t.Errorf("tool name: got %q", result[0].Function.Name)
	}
}

func TestNewOpenAICompatProvider_NoAPIKey(t *testing.T) {
	_, err := NewOpenAICompatProvider("test", "", "", &http.Client{})
	if err == nil {
		t.Error("expected error for empty API key")
	}
}
