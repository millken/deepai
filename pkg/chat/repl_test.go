package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
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

// newMockModelRegistry builds a single-entry ModelRegistry with a mock provider
// injected, so tests don't need real API keys. Returns the registry for
// ReplConfig.ModelRegistry.
func newMockModelRegistry(mock llm.LLMProvider) *llm.ModelRegistry {
	reg := llm.NewSingleModelRegistry("test", "test-model", "")
	reg.InjectProvider("test", "", "", mock)
	return reg
}

// newNilMockModelRegistry returns a registry with no injected provider — used
// for testing the "no provider" path. ProviderFor will fail if called.
func newNilMockModelRegistry() *llm.ModelRegistry {
	return llm.NewSingleModelRegistry("test", "test-model", "")
}

// mockUI is a minimal ReplUI for testing slash commands. It captures Info
// messages and AskQuestion responses.
type mockUI struct {
	infoMsgs    []string
	askResult   string
	askErr      error
	statusModel string
	statusPlan  bool
}

func (m *mockUI) Info(msg string) { m.infoMsgs = append(m.infoMsgs, msg) }
func (m *mockUI) SetStatus(model string, planMode bool) {
	m.statusModel = model
	m.statusPlan = planMode
}
func (m *mockUI) Banner(_ BannerInfo) {}
func (m *mockUI) AskQuestion(_ context.Context, _ string, _ []string) (string, error) {
	return m.askResult, m.askErr
}
func (m *mockUI) ReadPrompt(_ context.Context) (string, error) { return "", nil }
func (m *mockUI) TurnStart(_ int, _ string)                    {}
func (m *mockUI) TurnEnd(_ *agent.Usage)                       {}
func (m *mockUI) RenderEvent(_ agent.AgentEvent)               {}
func (m *mockUI) RenderSubagentEvent(_ subagent.TaskEvent)     {}
func (m *mockUI) RenderInterrupted()                           {}
func (m *mockUI) InterruptCh() <-chan struct{}                 { return nil }
func (m *mockUI) LoadHistory(_ string)                         {}
func (m *mockUI) SaveHistory()                                 {}
func (m *mockUI) Close()                                       {}

// lastInfo returns the most recent Info message, or "" if none.
func (m *mockUI) lastInfo() string {
	if len(m.infoMsgs) == 0 {
		return ""
	}
	return m.infoMsgs[len(m.infoMsgs)-1]
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: nil},
		currentModel: "default",
		sessMgr:      store,
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
		cfg:          ReplConfig{ModelRegistry: newMockModelRegistry(mock)},
		currentModel: "default",
		sessMgr:      store,
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
			ModelRegistry: newNilMockModelRegistry(),
			ToolRegistry:  reg,
			MCPReport:     "MCP: 1 loaded (zig-mcp)",
			Commands: map[string]Command{
				"zig-mcp:build": {Name: "zig-mcp:build", Source: "plugin"},
				"review":        {Name: "review", Source: "user"},
			},
		},
		currentModel: "default",
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

// TestModelPersistAndRestore verifies that /model <alias> persists the model
// alias to session metadata and restoreModelFromSession recovers it.
func TestModelPersistAndRestore(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "default", CWD: "/tmp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Build a multi-model registry.
	reg, err := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "test", Model: "m1"},
		{Name: "fast", Provider: "test", Model: "m2"},
	}, "default")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		sess:         sess,
		sessMgr:      store,
		currentModel: "default",
	}

	// Simulate /model fast — should persist.
	r.currentModel = "fast"
	r.persistModel()

	// Verify metadata was saved.
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Metadata["model"]; got != "fast" {
		t.Fatalf("persisted model = %q, want fast", got)
	}

	// Simulate resume: new REPL with default model, then restore from session.
	r2 := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		sess:         loaded,
		currentModel: "default",
	}
	r2.restoreModelFromSession()
	if r2.currentModel != "fast" {
		t.Fatalf("restored model = %q, want fast", r2.currentModel)
	}
}

// TestRestoreModel_AliasNotInRegistry verifies graceful fallback when the
// persisted model alias is no longer available.
func TestRestoreModel_AliasNotInRegistry(t *testing.T) {
	reg, err := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "test", Model: "m1"},
	}, "default")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	sess := &models.Session{
		Metadata: map[string]string{"model": "deleted-alias"},
	}
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		sess:         sess,
		currentModel: "default",
	}
	r.restoreModelFromSession()
	// Should stay at default, not crash.
	if r.currentModel != "default" {
		t.Fatalf("currentModel = %q, want default (fallback)", r.currentModel)
	}
}

// TestRestoreModel_NoMetadata verifies no-op when session has no model metadata.
func TestRestoreModel_NoMetadata(t *testing.T) {
	reg, err := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "test", Model: "m1"},
	}, "default")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		sess:         &models.Session{},
		currentModel: "default",
	}
	r.restoreModelFromSession()
	if r.currentModel != "default" {
		t.Fatalf("currentModel = %q, want default", r.currentModel)
	}
}

// --- handleModelCommand tests ---

func TestHandleModelCommand_ShowCurrent(t *testing.T) {
	reg, _ := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "openai", Model: "gpt-4o"},
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}, "default")
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		ui:           ui,
		sess:         &models.Session{},
		currentModel: "default",
	}
	r.handleModelCommand(context.Background(), "")
	if len(ui.infoMsgs) == 0 {
		t.Fatal("expected Info output")
	}
	if !strings.Contains(ui.lastInfo(), "default") || !strings.Contains(ui.lastInfo(), "gpt-4o") {
		t.Fatalf("Info should show current model, got: %s", ui.lastInfo())
	}
	if !strings.Contains(ui.lastInfo(), "fast") {
		t.Fatalf("Info should list available models, got: %s", ui.lastInfo())
	}
}

func TestHandleModelCommand_SwitchByAlias(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	sess, _ := store.Create(models.CreateOpts{Model: "default"})

	reg, _ := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "openai", Model: "gpt-4o"},
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}, "default")
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		ui:           ui,
		sess:         sess,
		sessMgr:      store,
		currentModel: "default",
	}
	r.handleModelCommand(context.Background(), "fast")
	if r.currentModel != "fast" {
		t.Fatalf("currentModel = %q, want fast", r.currentModel)
	}
	if ui.statusModel != "fast" {
		t.Fatalf("statusModel = %q, want fast", ui.statusModel)
	}
	// Verify persisted to session.
	loaded, _ := store.Load(sess.ID)
	if got := loaded.Metadata["model"]; got != "fast" {
		t.Fatalf("persisted model = %q, want fast", got)
	}
}

func TestHandleModelCommand_UnknownAlias(t *testing.T) {
	reg, _ := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "openai", Model: "gpt-4o"},
	}, "default")
	ui := &mockUI{}
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		ui:           ui,
		sess:         &models.Session{},
		currentModel: "default",
	}
	r.handleModelCommand(context.Background(), "nonexistent")
	if r.currentModel != "default" {
		t.Fatalf("currentModel should not change for unknown alias")
	}
	if !strings.Contains(ui.lastInfo(), "Unknown model") {
		t.Fatalf("should show error for unknown alias, got: %s", ui.lastInfo())
	}
}

func TestHandleModelCommand_Picker(t *testing.T) {
	reg, _ := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "openai", Model: "gpt-4o"},
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}, "default")
	ui := &mockUI{askResult: "fast"}
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		ui:           ui,
		sess:         &models.Session{},
		currentModel: "default",
	}
	r.handleModelCommand(context.Background(), "?")
	if r.currentModel != "fast" {
		t.Fatalf("currentModel = %q, want fast after picker", r.currentModel)
	}
}

func TestHandleModelCommand_PickerCancel(t *testing.T) {
	reg, _ := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "openai", Model: "gpt-4o"},
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}, "default")
	ui := &mockUI{askResult: ""} // empty = cancel
	r := &ChatRepl{
		cfg:          ReplConfig{ModelRegistry: reg},
		ui:           ui,
		sess:         &models.Session{},
		currentModel: "default",
	}
	r.handleModelCommand(context.Background(), "?")
	if r.currentModel != "default" {
		t.Fatalf("currentModel should not change on cancel")
	}
}
