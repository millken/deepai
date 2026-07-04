package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// mockLLMProvider implements llm.LLMProvider for testing.
type mockLLMProvider struct {
	response string
	err      error
}

func (m *mockLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if m.err != nil {
		return llm.ChatResponse{}, m.err
	}
	return llm.ChatResponse{
		Message: models.Message{
			Role:    models.RoleAI,
			Content: m.response,
		},
	}, nil
}

func (m *mockLLMProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	// Not used in generateTitle.
	return nil, nil
}

func TestGenerateTitle_Success(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: "Test Title"}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	if loaded.Title != "Test Title" {
		t.Fatalf("expected title %q, got %q", "Test Title", loaded.Title)
	}
}

func TestGenerateTitle_LongTitle(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// LLM returns a title longer than 30 characters.
	longTitle := "This is a very long title that exceeds thirty characters"
	mock := &mockLLMProvider{response: longTitle}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should be truncated to 30 characters.
	expected := longTitle[:30]
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_EmptyResponse(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: ""}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: first 20 chars + "..."
	expected := "Hello, this is a tes..."
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_LLMError(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{err: errors.New("unavailable")}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: first 20 chars + "..."
	expected := "Hello, this is a tes..."
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_ShortFallback(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{err: errors.New("unavailable")}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Short")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: "Short" (no truncation).
	if loaded.Title != "Short" {
		t.Fatalf("expected title %q, got %q", "Short", loaded.Title)
	}
}

func TestGenerateTitle_NoProvider(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: nil,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	// Should not panic, should skip.
	r.generateTitle(sess.ID, "Hello")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Title should remain empty.
	if loaded.Title != "" {
		t.Fatalf("expected empty title, got %q", loaded.Title)
	}
}

func TestGenerateTitle_EmptyMessage(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: "Test Title"}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	// Should skip when firstUserMsg is empty.
	r.generateTitle(sess.ID, "")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Title should remain empty.
	if loaded.Title != "" {
		t.Fatalf("expected empty title, got %q", loaded.Title)
	}
}

// TestFilterUnresolvedToolUses_PartialBatch covers the interrupted-mid-batch
// case: an assistant message issues two tool calls but only the first received
// a result before the user pressed ctrl+c. Resuming such history verbatim would
// be malformed (a tool_use with no tool_result), so the unresolved call must be
// stripped while the resolved one and its result are preserved.
func TestFilterUnresolvedToolUses_PartialBatch(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "do two things"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{
			{ID: "call_a", Name: "bash"},
			{ID: "call_b", Name: "bash"},
		}},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{CallID: "call_a"}},
	}

	got := filterUnresolvedToolUses(msgs)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (human, ai, tool)", len(got))
	}
	ai := got[1]
	if ai.Role != models.RoleAI {
		t.Fatalf("got[1].Role = %q, want ai", ai.Role)
	}
	if len(ai.ToolCalls) != 1 || ai.ToolCalls[0].ID != "call_a" {
		t.Fatalf("ai.ToolCalls = %+v, want only the resolved call_a", ai.ToolCalls)
	}
}

// TestFilterUnresolvedToolUses_AllUnresolved drops an assistant message whose
// every tool call lacks a result (e.g. cancelled before any tool ran), so the
// resumed history doesn't dangle.
func TestFilterUnresolvedToolUses_AllUnresolved(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "go"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "x", Name: "bash"}}},
	}

	got := filterUnresolvedToolUses(msgs)

	if len(got) != 1 || got[0].Role != models.RoleHuman {
		t.Fatalf("got = %+v, want only the human message", got)
	}
}

// TestIsSessionInterrupted_TrailingToolResult treats a transcript ending in a
// tool result (agent never got to respond) as interrupted, which is what drives
// auto-continue on resume.
func TestIsSessionInterrupted_TrailingToolResult(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "go"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "x", Name: "bash"}}},
		{Role: models.RoleTool, ToolResult: &models.ToolResult{CallID: "x"}},
	}
	if !isSessionInterrupted(msgs) {
		t.Fatal("isSessionInterrupted = false, want true for trailing tool result")
	}

	done := []models.Message{
		{Role: models.RoleHuman, Content: "go"},
		{Role: models.RoleAI, Content: "all done"},
	}
	if isSessionInterrupted(done) {
		t.Fatal("isSessionInterrupted = true, want false for completed turn")
	}
}

func TestStatusText_ShowsLoadedAndUsage(t *testing.T) {
	reg := tools.NewRegistry()
	h := func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted}, nil
	}
	if err := reg.Register(models.Tool{Name: "bash", Description: "run shell", Handler: h}); err != nil {
		t.Fatalf("register bash: %v", err)
	}
	if err := reg.Register(models.Tool{
		Name:        "zig-mcp.build",
		Description: "build zig",
		Groups:      []string{"mcp", "zig-mcp"},
		Handler:     h,
	}); err != nil {
		t.Fatalf("register mcp tool: %v", err)
	}

	r := &ChatRepl{
		cfg: ReplConfig{
			Model:        "test-model",
			ToolRegistry: reg,
			MCPReport:    "MCP: 1 loaded (zig-mcp)",
			Commands: map[string]Command{
				"zig-mcp:build": {Name: "zig-mcp:build", Source: "plugin"},
				"review":        {Name: "review", Source: "user"},
			},
		},
		sess: &models.Session{
			ID: "s1",
			Messages: []models.Message{
				{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "zig-mcp.build", Status: models.CallStatusCompleted}},
				{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "zig-mcp.build", Status: models.CallStatusFailed, Error: "boom"}},
				{Role: models.RoleTool, ToolResult: &models.ToolResult{ToolName: "bash", Status: models.CallStatusCompleted}},
			},
		},
	}

	text := r.statusText()
	checks := []string{
		"Loaded tools: 2",
		"MCP servers: zig-mcp(1)",
		"Plugin commands: 1 (zig-mcp:build)",
		"Tool calls this session: 3 total, 1 failed",
		"zig-mcp.build: 2 (failed 1)",
	}
	for _, c := range checks {
		if !strings.Contains(text, c) {
			t.Fatalf("status text missing %q:\n%s", c, text)
		}
	}
}
