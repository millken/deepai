package models

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRole_Validate(t *testing.T) {
	tests := []struct {
		name    string
		role    Role
		wantErr bool
	}{
		{"human", RoleHuman, false},
		{"ai", RoleAI, false},
		{"system", RoleSystem, false},
		{"tool", RoleTool, false},
		{"invalid", Role("invalid"), true},
		{"empty", Role(""), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.role.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Role.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMessage_Validate(t *testing.T) {
	tests := []struct {
		name    string
		msg     Message
		wantErr bool
	}{
		{
			name: "valid user message",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      RoleHuman,
				Content:   "Hello",
			},
			wantErr: false,
		},
		{
			name: "valid ai message",
			msg: Message{
				ID:        "msg_2",
				SessionID: "sess_1",
				Role:      RoleAI,
				Content:   "Hi there",
			},
			wantErr: false,
		},
		{
			name: "empty id",
			msg: Message{
				ID:        "",
				SessionID: "sess_1",
				Role:      RoleHuman,
				Content:   "Hello",
			},
			wantErr: true,
		},
		{
			name: "empty session id",
			msg: Message{
				ID:        "msg_1",
				SessionID: "",
				Role:      RoleHuman,
				Content:   "Hello",
			},
			wantErr: true,
		},
		{
			name: "empty content",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      RoleHuman,
				Content:   "",
			},
			wantErr: true,
		},
		{
			name: "invalid role",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      Role("invalid"),
				Content:   "Hello",
			},
			wantErr: true,
		},
		{
			name: "tool message without tool result",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      RoleTool,
				Content:   "Some content",
			},
			wantErr: true,
		},
		{
			name: "tool message with tool result",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      RoleTool,
				Content:   "",
				ToolResult: &ToolResult{
					CallID:   "call_1",
					ToolName: "test",
					Status:   CallStatusCompleted,
					Content:  "result",
				},
			},
			wantErr: false,
		},
		{
			name: "ai message with tool calls",
			msg: Message{
				ID:        "msg_1",
				SessionID: "sess_1",
				Role:      RoleAI,
				Content:   "",
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "test", Status: CallStatusCompleted},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.msg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMessage_JSON(t *testing.T) {
	msg := Message{
		ID:         "msg_123",
		SessionID:  "sess_456",
		Role:       RoleHuman,
		Content:    "Test content",
		Metadata:   map[string]string{"key": "value"},
		CreatedAt:  time.Now(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded.ID != msg.ID {
		t.Errorf("ID mismatch: got %v, want %v", decoded.ID, msg.ID)
	}
	if decoded.Role != msg.Role {
		t.Errorf("Role mismatch: got %v, want %v", decoded.Role, msg.Role)
	}
	if decoded.Content != msg.Content {
		t.Errorf("Content mismatch: got %v, want %v", decoded.Content, msg.Content)
	}
}

func TestToolCall_Validate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		call    ToolCall
		wantErr bool
	}{
		{
			name: "valid tool call",
			call: ToolCall{
				ID:       "call_123",
				Name:     "test",
				Arguments: map[string]any{"key": "value"},
				Status:   CallStatusPending,
			},
			wantErr: false,
		},
		{
			name: "empty id",
			call: ToolCall{
				ID:     "",
				Name:   "test",
				Status: CallStatusPending,
			},
			wantErr: true,
		},
		{
			name: "empty name",
			call: ToolCall{
				ID:     "call_123",
				Name:   "",
				Status: CallStatusPending,
			},
			wantErr: true,
		},
		{
			name: "invalid status",
			call: ToolCall{
				ID:     "call_123",
				Name:   "test",
				Status: CallStatus("invalid"),
			},
			wantErr: true,
		},
		{
			name: "started_at before requested_at",
			call: ToolCall{
				ID:          "call_123",
				Name:        "test",
				Status:      CallStatusRunning,
				RequestedAt: now,
				StartedAt:   now.Add(-time.Hour),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestToolResult_Validate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		result  ToolResult
		wantErr bool
	}{
		{
			name: "valid completed result",
			result: ToolResult{
				CallID:      "call_123",
				ToolName:    "test",
				Status:      CallStatusCompleted,
				Content:     "success",
				CompletedAt: now,
			},
			wantErr: false,
		},
		{
			name: "valid failed result",
			result: ToolResult{
				CallID:      "call_123",
				ToolName:    "test",
				Status:      CallStatusFailed,
				Error:       "something went wrong",
				CompletedAt: now,
			},
			wantErr: false,
		},
		{
			name: "empty call id",
			result: ToolResult{
				CallID:   "",
				ToolName: "test",
				Status:   CallStatusCompleted,
				Content:  "ok",
			},
			wantErr: true,
		},
		{
			name: "empty tool name",
			result: ToolResult{
				CallID:   "call_123",
				ToolName: "",
				Status:   CallStatusCompleted,
				Content:  "ok",
			},
			wantErr: true,
		},
		{
			name: "completed with error",
			result: ToolResult{
				CallID:   "call_123",
				ToolName: "test",
				Status:   CallStatusCompleted,
				Error:    "should not have error",
			},
			wantErr: true,
		},
		{
			name: "failed without error",
			result: ToolResult{
				CallID:   "call_123",
				ToolName: "test",
				Status:   CallStatusFailed,
				Content:  "no error message",
			},
			wantErr: true,
		},
		{
			name: "negative duration",
			result: ToolResult{
				CallID:    "call_123",
				ToolName:  "test",
				Status:    CallStatusCompleted,
				Content:   "ok",
				Duration:  -1,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTool_Validate(t *testing.T) {
	tests := []struct {
		name    string
		tool    Tool
		wantErr bool
	}{
		{
			name: "valid tool",
			tool: Tool{
				Name:        "test",
				Description: "A test tool",
				InputSchema: map[string]any{"type": "object"},
				Handler:     func(ctx context.Context, call ToolCall) (ToolResult, error) { return ToolResult{}, nil },
			},
			wantErr: false,
		},
		{
			name: "empty name",
			tool: Tool{
				Name:    "",
				Handler: func(ctx context.Context, call ToolCall) (ToolResult, error) { return ToolResult{}, nil },
			},
			wantErr: true,
		},
		{
			name: "nil handler",
			tool: Tool{
				Name:    "nil_handler",
				Handler: nil,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tool.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCallStatus_Validate(t *testing.T) {
	tests := []struct {
		name    string
		status  CallStatus
		wantErr bool
	}{
		{"pending", CallStatusPending, false},
		{"running", CallStatusRunning, false},
		{"completed", CallStatusCompleted, false},
		{"failed", CallStatusFailed, false},
		{"invalid", CallStatus("invalid"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.status.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
