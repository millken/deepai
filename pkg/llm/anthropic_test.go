package llm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/millken/deepai/pkg/models"
)

func TestMapMessagesToAnthropic_SystemSkipped(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleSystem, Content: "system prompt"},
		{Role: models.RoleHuman, Content: "hello"},
	}
	result := mapMessagesToAnthropic(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("expected user role, got %s", result[0].Role)
	}
}

func TestMapMessagesToAnthropic_ToolResultMerged(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "do it"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "read_file"}}, Content: ""},
		{Role: models.RoleTool, Content: "file content", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "read_file"}},
	}
	result := mapMessagesToAnthropic(msgs)
	// Expect: user("do it"), assistant(tool_use), user(tool_result)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg[0]: expected user, got %s", result[0].Role)
	}
	if result[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("msg[1]: expected assistant, got %s", result[1].Role)
	}
	if result[2].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg[2]: expected user (tool_result), got %s", result[2].Role)
	}
	// Check tool_result block has OfToolResult set.
	hasToolResult := false
	for _, block := range result[2].Content {
		if block.OfToolResult != nil {
			hasToolResult = true
			if block.OfToolResult.ToolUseID != "c1" {
				t.Errorf("tool_use_id: got %q, want %q", block.OfToolResult.ToolUseID, "c1")
			}
		}
	}
	if !hasToolResult {
		t.Error("expected tool_result block in user message")
	}
}

func TestMapMessagesToAnthropic_ConsecutiveToolResults(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}}, Content: ""},
		{Role: models.RoleTool, Content: "r1", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "a"}},
		{Role: models.RoleTool, Content: "r2", ToolResult: &models.ToolResult{CallID: "c2", ToolName: "b"}},
	}
	result := mapMessagesToAnthropic(msgs)
	// Two consecutive tool results should be merged into one user message.
	mergedCount := 0
	for _, m := range result {
		if m.Role == anthropic.MessageParamRoleUser {
			mergedCount++
			if len(m.Content) != 2 {
				t.Errorf("expected 2 content blocks in merged user message, got %d", len(m.Content))
			}
		}
	}
	if mergedCount != 1 {
		t.Errorf("expected 1 merged user message, got %d", mergedCount)
	}
}

// TestMapMessagesToAnthropic_TrailingHumanHintMergesIntoToolResultUser
// pins the M1-7 fix: a RoleHuman breaker-hint message immediately following
// tool results must merge into the SAME user message as the tool_result
// blocks (appended after them), not start a new user message. A trailing
// standalone user message after tool_result blocks leaves a tool_use id
// without an immediately-following tool_result, which the Anthropic API
// rejects.
func TestMapMessagesToAnthropic_TrailingHumanHintMergesIntoToolResultUser(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}}, Content: ""},
		{Role: models.RoleTool, Content: "r1", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "a"}},
		{Role: models.RoleTool, Content: "r2", ToolResult: &models.ToolResult{CallID: "c2", ToolName: "b"}},
		{Role: models.RoleHuman, Content: "hint"},
	}
	result := mapMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages ([assistant, user]), got %d", len(result))
	}
	if result[0].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("msg[0]: expected assistant, got %s", result[0].Role)
	}
	if result[1].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg[1]: expected user, got %s", result[1].Role)
	}
	blocks := result[1].Content
	if len(blocks) != 3 {
		t.Fatalf("expected 3 content blocks in merged user message, got %d", len(blocks))
	}
	if blocks[0].OfToolResult == nil {
		t.Errorf("block[0]: expected tool_result, got %+v", blocks[0])
	}
	if blocks[1].OfToolResult == nil {
		t.Errorf("block[1]: expected tool_result, got %+v", blocks[1])
	}
	if blocks[2].OfText == nil {
		t.Errorf("block[2]: expected text (the human hint), got %+v", blocks[2])
	}
}

// TestMapMessagesToAnthropic_TrailingHumanHintWithImagesMergesIntoToolResultUser
// covers the images variant of the same M1-7 fix: a RoleHuman message that
// carries Images (e.g. a vision-tool hint) must ALSO merge into the same
// user message as preceding tool_result blocks — image and text blocks
// appended after the tool_result blocks, tool_result-first ordering
// preserved.
func TestMapMessagesToAnthropic_TrailingHumanHintWithImagesMergesIntoToolResultUser(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}}, Content: ""},
		{Role: models.RoleTool, Content: "r1", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "a"}},
		{Role: models.RoleTool, Content: "r2", ToolResult: &models.ToolResult{CallID: "c2", ToolName: "b"}},
		{
			Role:    models.RoleHuman,
			Content: "hint",
			Images:  []models.MessageImage{{MimeType: "image/png", Base64: "AAAA"}},
		},
	}
	result := mapMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages ([assistant, user]), got %d", len(result))
	}
	if result[0].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("msg[0]: expected assistant, got %s", result[0].Role)
	}
	if result[1].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg[1]: expected user, got %s", result[1].Role)
	}
	blocks := result[1].Content
	if len(blocks) != 4 {
		t.Fatalf("expected 4 content blocks in merged user message ([tool_result, tool_result, image, text]), got %d", len(blocks))
	}
	if blocks[0].OfToolResult == nil {
		t.Errorf("block[0]: expected tool_result, got %+v", blocks[0])
	}
	if blocks[1].OfToolResult == nil {
		t.Errorf("block[1]: expected tool_result, got %+v", blocks[1])
	}
	if blocks[2].OfImage == nil {
		t.Errorf("block[2]: expected image, got %+v", blocks[2])
	}
	if blocks[3].OfText == nil {
		t.Errorf("block[3]: expected text (the human hint), got %+v", blocks[3])
	}
}

func TestMapMessagesToAnthropic_AssistantWithToolCalls(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "/test.go"}},
		}, Content: ""},
	}
	result := mapMessagesToAnthropic(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	m := result[0]
	if m.Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("expected assistant, got %s", m.Role)
	}
	hasToolUse := false
	for _, block := range m.Content {
		if block.OfToolUse != nil {
			hasToolUse = true
			if block.OfToolUse.Name != "read_file" {
				t.Errorf("tool name: got %q, want %q", block.OfToolUse.Name, "read_file")
			}
			if block.OfToolUse.ID != "c1" {
				t.Errorf("tool id: got %q, want %q", block.OfToolUse.ID, "c1")
			}
		}
	}
	if !hasToolUse {
		t.Error("expected tool_use block in assistant message")
	}
}

func TestMapMessagesToAnthropic_EmptyContent(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: ""},
		{Role: models.RoleTool, Content: "", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "t"}},
	}
	result := mapMessagesToAnthropic(msgs)
	for _, m := range result {
		for _, block := range m.Content {
			if block.OfText != nil && block.OfText.Text == "" {
				t.Error("text block should not be empty")
			}
			if block.OfToolResult != nil {
				for _, c := range block.OfToolResult.Content {
					if c.OfText != nil && c.OfText.Text == "" {
						t.Error("tool result text should not be empty")
					}
				}
			}
		}
	}
}

func TestIsRetryableAnthropicStreamErr(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{429, true},
		{529, true},
		{500, true},
		{502, true},
		{503, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
	}
	for _, tt := range tests {
		err := &anthropic.Error{StatusCode: tt.statusCode}
		got := isRetryableAnthropicStreamErr(err)
		if got != tt.want {
			t.Errorf("statusCode %d: got %v, want %v", tt.statusCode, got, tt.want)
		}
	}
	// Non-API error should not be retryable.
	if isRetryableAnthropicStreamErr(errors.New("generic")) {
		t.Error("generic error should not be retryable")
	}
}

func TestMapToolsToAnthropic(t *testing.T) {
	tools := []models.Tool{
		{Name: "read_file", Description: "Read a file", InputSchema: map[string]any{"type": "object"}},
	}
	result := mapToolsToAnthropic(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].OfTool == nil {
		t.Fatal("expected OfTool to be set")
	}
	if result[0].OfTool.Name != "read_file" {
		t.Errorf("tool name: got %q", result[0].OfTool.Name)
	}
}

func TestNewAnthropicProvider_NoAPIKey(t *testing.T) {
	_, err := NewAnthropicProvider("", "", &http.Client{})
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestBuildParams_TemperatureSuppressedForNoSamplingModels(t *testing.T) {
	p := &AnthropicProvider{}
	temp := 0.2

	for _, model := range []string{
		"claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-8-20260101",
		"claude-opus-5", "claude-opus-6", "claude-sonnet-5",
		"claude-fable-5", "claude-mythos-5",
	} {
		params := p.buildParams(ChatRequest{Model: model, Temperature: &temp})
		if params.Temperature.Valid() {
			t.Errorf("%s: temperature should be suppressed, got sent", model)
		}
	}

	for _, model := range []string{
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"glm-5.3", "claude-opus-4-1-20250805",
	} {
		params := p.buildParams(ChatRequest{Model: model, Temperature: &temp})
		if !params.Temperature.Valid() {
			t.Errorf("%s: temperature should be sent", model)
		}
	}
}
