package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestRegistry_NewRegistry(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry should not return nil")
	}
	if r.tools == nil {
		t.Error("tools map should be initialized")
	}
}

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry()

	err := r.Register(models.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: "result"}, nil
		},
	})

	if err != nil {
		t.Errorf("Register failed: %v", err)
	}

	if len(r.tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(r.tools))
	}

	// Test duplicate registration
	err = r.Register(models.Tool{
		Name:        "test_tool",
		Description: "Duplicate",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})

	if err == nil {
		t.Error("Expected error for duplicate tool registration")
	}
}

func TestRegistry_Get(t *testing.T) {
	r := NewRegistry()

	r.Register(models.Tool{
		Name:        "get_test",
		Description: "Test get",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})

	tool := r.Get("get_test")
	if tool == nil {
		t.Fatal("Expected to find tool 'get_test'")
	}
	if tool.Name != "get_test" {
		t.Errorf("Tool name = %s, want 'get_test'", tool.Name)
	}

	// Test not found
	tool = r.Get("nonexistent")
	if tool != nil {
		t.Error("Should not find nonexistent tool")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(models.Tool{
		Name: "remove_me",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	if removed := r.Unregister("remove_me"); !removed {
		t.Fatal("expected tool to be removed")
	}
	if tool := r.Get("remove_me"); tool != nil {
		t.Fatal("expected tool to be absent after unregister")
	}
	if removed := r.Unregister("remove_me"); removed {
		t.Fatal("expected second unregister to report false")
	}
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()

	r.Register(models.Tool{Name: "tool1", Description: "First", Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})
	r.Register(models.Tool{Name: "tool2", Description: "Second", Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})

	tools := r.List()
	if len(tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(tools))
	}
}

func TestRegistry_Call(t *testing.T) {
	r := NewRegistry()

	var called bool
	r.Register(models.Tool{
		Name:        "call_test",
		Description: "Test call",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"name"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			called = true
			name, ok := call.Arguments["name"].(string)
			if !ok {
				return models.ToolResult{}, errors.New("name is required")
			}
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusCompleted,
				Content:  "Hello, " + name,
			}, nil
		},
	})

	result, err := r.Call(context.Background(), "call_test", map[string]any{"name": "World"}, nil)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !called {
		t.Error("Handler was not called")
	}
	if result != "Hello, World" {
		t.Errorf("Result = %v, want 'Hello, World'", result)
	}

	// Test tool not found
	_, err = r.Call(context.Background(), "nonexistent", nil, nil)
	if err == nil {
		t.Error("Expected error for nonexistent tool")
	}
}

func TestRegistry_Restrict(t *testing.T) {
	r := NewRegistry()

	r.Register(models.Tool{Name: "allowed", Description: "Allowed", Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})
	r.Register(models.Tool{Name: "denied", Description: "Denied", Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})

	restricted := r.Restrict([]string{"allowed"})

	// Allowed tool should be present
	tool := restricted.Get("allowed")
	if tool == nil {
		t.Error("Allowed tool should be present")
	}

	// Denied tool should not be present
	tool = restricted.Get("denied")
	if tool != nil {
		t.Error("Denied tool should not be present")
	}
}

func TestRegistry_Execute(t *testing.T) {
	r := NewRegistry()

	r.Register(models.Tool{
		Name:        "execute_test",
		Description: "Test execute",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusCompleted,
				Content:  "executed",
			}, nil
		},
	})

	result, err := r.Execute(context.Background(), models.ToolCall{
		ID:     "call_1",
		Name:   "execute_test",
		Status: models.CallStatusPending,
	})

	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Content != "executed" {
		t.Errorf("Content = %s, want 'executed'", result.Content)
	}
}

func TestWithSandbox(t *testing.T) {
	ctx := context.Background()

	// Should not panic
	ctx = WithSandbox(ctx, nil)

	// Should be able to retrieve
	sb := SandboxFromContext(ctx)
	if sb != nil {
		t.Error("Should return nil for nil sandbox")
	}
}

func TestValidateArgs(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required": []any{"name"},
	}

	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{"valid args", map[string]any{"name": "test", "age": 25}, false},
		{"missing required", map[string]any{"age": 25}, true},
		{"wrong type", map[string]any{"name": 123}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArgs(schema, tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewToolCallID(t *testing.T) {
	id1 := newToolCallID("test")
	id2 := newToolCallID("test")

	if id1 == id2 {
		t.Error("Tool call IDs should be unique")
	}

	if id1 == "" {
		t.Error("Tool call ID should not be empty")
	}
}
