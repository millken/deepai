package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// mockLLMProvider implements llm.LLMProvider for testing.
type mockLLMProvider struct {
	response string
	model    string // 服务端返回的模型名
	err      error
}

func (m *mockLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if m.err != nil {
		return llm.ChatResponse{}, m.err
	}
	return llm.ChatResponse{
		Model: m.model,
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
func (m *mockUI) ReadPrompt(_ context.Context) (string, []models.MessageImage, error) {
	return "", nil, nil
}
func (m *mockUI) TurnStart(_ int, _ string)                {}
func (m *mockUI) TurnEnd(_ *agent.Usage)                   {}
func (m *mockUI) RenderEvent(_ agent.AgentEvent)           {}
func (m *mockUI) RenderSubagentEvent(_ subagent.TaskEvent) {}
func (m *mockUI) RenderInterrupted()                       {}
func (m *mockUI) InterruptCh() <-chan struct{}             { return nil }
func (m *mockUI) CancelTaskCh() <-chan string              { return nil }
func (m *mockUI) LoadHistory(_ string)                     {}
func (m *mockUI) SaveHistory()                             {}
func (m *mockUI) Close()                                   {}

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

// TestDoctorModels_AllHealthy verifies that the model probe reports success
// when all configured models respond to the hello request.
func TestDoctorModels_AllHealthy(t *testing.T) {
	defs := []llm.ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnv: "DOCTOR_TEST_KEY"},
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514", APIKeyEnv: "DOCTOR_TEST_KEY"},
	}
	reg, err := llm.NewModelRegistry(defs, "fast")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	// Inject mock providers so no real network calls are made.
	// smart 返回不同的模型名，测试实际模型显示逻辑。
	reg.InjectProvider("openai", "", "", &mockLLMProvider{response: "hello", model: "gpt-4o-mini"})
	reg.InjectProvider("anthropic", "", "", &mockLLMProvider{response: "hello", model: "glm-5.2"})

	r := &ChatRepl{cfg: ReplConfig{ModelRegistry: reg}, currentModel: "fast"}

	text := r.doctorModels(context.Background())
	checks := []string{
		"Models:",
		"✓ fast",
		"gpt-4o-mini",
		"endpoint:",
		"✓ smart",
		"claude-sonnet-4-20250514",
		"[actual: glm-5.2]", // 关键断言：显示实际模型
		"All 2 model(s) healthy",
	}
	for _, c := range checks {
		if !strings.Contains(text, c) {
			t.Fatalf("doctor models text missing %q:\n%s", c, text)
		}
	}
}

// TestDoctorModels_PartialFailure verifies that the model probe reports errors
// for unreachable models while still showing healthy ones.
func TestDoctorModels_PartialFailure(t *testing.T) {
	defs := []llm.ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnv: "DOCTOR_TEST_KEY"},
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514", APIKeyEnv: "DOCTOR_TEST_KEY"},
	}
	reg, err := llm.NewModelRegistry(defs, "fast")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	reg.InjectProvider("openai", "", "", &mockLLMProvider{response: "hello"})
	reg.InjectProvider("anthropic", "", "", &mockLLMProvider{err: errors.New("401 unauthorized")})

	r := &ChatRepl{cfg: ReplConfig{ModelRegistry: reg}, currentModel: "fast"}

	text := r.doctorModels(context.Background())
	if !strings.Contains(text, "✓ fast") {
		t.Fatalf("doctor models should show fast as healthy:\n%s", text)
	}
	if !strings.Contains(text, "✗ smart") {
		t.Fatalf("doctor models should show smart as failed:\n%s", text)
	}
	if !strings.Contains(text, "1/2 model(s) healthy") {
		t.Fatalf("doctor models should show 1/2 healthy:\n%s", text)
	}
	if !strings.Contains(text, "401 unauthorized") {
		t.Fatalf("doctor models should include error message:\n%s", text)
	}
}

// TestDoctorSkills_Loaded verifies that the skill section lists loaded skills
// with their source.
func TestDoctorSkills_Loaded(t *testing.T) {
	skillReg := skill.NewRegistry()
	root := t.TempDir()
	// LoadFromDir expects <root>/<skill-name>/SKILL.md subdirectories.
	skillDir := filepath.Join(root, "golang")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "---\nname: golang\ndescription: Go best practices\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := skillReg.LoadFromDir(root); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	r := &ChatRepl{cfg: ReplConfig{SkillRegistry: skillReg}}
	text := r.doctorSkills()
	if !strings.Contains(text, "Skills (1):") {
		t.Fatalf("doctor skills should show count:\n%s", text)
	}
	if !strings.Contains(text, "✓ golang") {
		t.Fatalf("doctor skills should list golang:\n%s", text)
	}
	if !strings.Contains(text, "All 1 skill(s) loaded") {
		t.Fatalf("doctor skills should show summary:\n%s", text)
	}
}

// TestDoctorSkills_None verifies the empty state.
func TestDoctorSkills_None(t *testing.T) {
	r := &ChatRepl{cfg: ReplConfig{SkillRegistry: skill.NewRegistry()}}
	text := r.doctorSkills()
	if !strings.Contains(text, "Skills: none loaded") {
		t.Fatalf("doctor skills should report none loaded:\n%s", text)
	}
}

// TestDoctorMCP_Connected verifies that MCP tools registered in the tool
// registry are grouped by server and reported.
func TestDoctorMCP_Connected(t *testing.T) {
	reg := tools.NewRegistry()
	h := func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted}, nil
	}
	if err := reg.Register(models.Tool{
		Name: "zig-mcp.build", Description: "build", Groups: []string{"mcp", "zig-mcp"}, Handler: h,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(models.Tool{
		Name: "fs.read", Description: "read", Groups: []string{"mcp", "fs"}, Handler: h,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	r := &ChatRepl{cfg: ReplConfig{ToolRegistry: reg}}
	text := r.doctorMCP()
	if !strings.Contains(text, "✓ zig-mcp") {
		t.Fatalf("doctor mcp should list zig-mcp:\n%s", text)
	}
	if !strings.Contains(text, "✓ fs") {
		t.Fatalf("doctor mcp should list fs:\n%s", text)
	}
	if !strings.Contains(text, "2 server(s) connected") {
		t.Fatalf("doctor mcp should show 2 servers:\n%s", text)
	}
}

// TestDoctorMCP_None verifies the empty state surfaces the startup report.
func TestDoctorMCP_None(t *testing.T) {
	reg := tools.NewRegistry()
	r := &ChatRepl{cfg: ReplConfig{
		ToolRegistry: reg,
		MCPReport:    "MCP: 0 loaded, 1 failed (bad: connect refused)",
	}}
	text := r.doctorMCP()
	if !strings.Contains(text, "none connected") {
		t.Fatalf("doctor mcp should report none connected:\n%s", text)
	}
	if !strings.Contains(text, "1 failed") {
		t.Fatalf("doctor mcp should surface startup failure:\n%s", text)
	}
}

// TestDoctorText_Combined verifies that the full /doctor output contains all
// three sections.
func TestDoctorText_Combined(t *testing.T) {
	defs := []llm.ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnv: "DOCTOR_TEST_KEY"},
	}
	reg, err := llm.NewModelRegistry(defs, "fast")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	reg.InjectProvider("openai", "", "", &mockLLMProvider{response: "hello"})

	r := &ChatRepl{cfg: ReplConfig{ModelRegistry: reg}, currentModel: "fast", currentEffort: "medium"}
	text := r.doctorText(context.Background())
	for _, section := range []string{"Models:", "Skills:", "MCP servers:", "Current reasoning effort: medium"} {
		if !strings.Contains(text, section) {
			t.Fatalf("doctor text missing section %q:\n%s", section, text)
		}
	}
}

// TestHandleEffortCommand verifies that /effort shows current, validates input,
// updates the setting, persists to session, and supports default reset.
func TestHandleEffortCommand(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "default", CWD: "/tmp"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	r := &ChatRepl{
		cfg:     ReplConfig{},
		ui:      &mockUI{},
		sess:    sess,
		sessMgr: store,
	}

	// /effort (no args) — shows current
	r.handleEffortCommand(context.Background(), "")
	if len(r.ui.(*mockUI).infoMsgs) != 3 {
		t.Fatalf("expected 3 info lines, got %d", len(r.ui.(*mockUI).infoMsgs))
	}

	// /effort high — sets to high and persists
	r.ui.(*mockUI).infoMsgs = nil
	r.handleEffortCommand(context.Background(), "high")
	if r.currentEffort != "high" {
		t.Fatalf("currentEffort = %q, want high", r.currentEffort)
	}
	if len(r.ui.(*mockUI).infoMsgs) != 2 {
		t.Fatalf("expected 2 info lines, got %d", len(r.ui.(*mockUI).infoMsgs))
	}
	// Verify persistence
	updated, _ := store.Resolve(sess.ID)
	if updated.Metadata["effort"] != "high" {
		t.Fatalf("effort not persisted, got %q", updated.Metadata["effort"])
	}

	// /effort default — resets to empty and persists
	r.ui.(*mockUI).infoMsgs = nil
	r.handleEffortCommand(context.Background(), "default")
	if r.currentEffort != "" {
		t.Fatalf("currentEffort should be empty after default, got %q", r.currentEffort)
	}
	updated, _ = store.Resolve(sess.ID)
	if updated.Metadata["effort"] != "" {
		t.Fatalf("effort should be empty after default, got %q", updated.Metadata["effort"])
	}

	// /effort invalid — shows error and doesn't change
	r.currentEffort = "medium"
	r.ui.(*mockUI).infoMsgs = nil
	r.handleEffortCommand(context.Background(), "invalid")
	if r.currentEffort != "medium" {
		t.Fatalf("currentEffort should stay medium after invalid input, got %q", r.currentEffort)
	}
	if len(r.ui.(*mockUI).infoMsgs) != 1 {
		t.Fatalf("expected 1 info line for error, got %d", len(r.ui.(*mockUI).infoMsgs))
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

// TestMainAgentMaxTokens_UsesSharedConstant pins that the main (interactive
// REPL) agent's MaxTokens comes from agent.ResolveMaxOutputTokens, the same
// resolver pkg/commands/chat.go's subagentMaxTokens uses. Before this fix
// the main agent never set MaxTokens at all and silently ran at the
// provider default (8192 for Anthropic) — the exact truncation exposure the
// subagent limit was raised to avoid, left in place for the agent the user
// actually talks to. This test, together with
// TestSubagentMaxTokens_UsesSharedConstant in pkg/commands, would fail if
// either wiring point were changed to a literal instead of the shared
// resolver, catching the two silently drifting apart.
func TestMainAgentMaxTokens_UsesSharedConstant(t *testing.T) {
	t.Setenv(agent.EnvMaxOutputTokens, "")
	got := mainAgentMaxTokens()
	if got == nil {
		t.Fatal("mainAgentMaxTokens() = nil, want a pointer to agent.DefaultMaxOutputTokens")
	}
	if *got != agent.DefaultMaxOutputTokens {
		t.Errorf("mainAgentMaxTokens() = %d, want agent.DefaultMaxOutputTokens (%d)", *got, agent.DefaultMaxOutputTokens)
	}
}

// TestMainAgentMaxTokens_ExplicitSettingWins pins that a valid
// DEEPAI_MAX_OUTPUT_TOKENS setting reaches the main agent's MaxTokens,
// not just the default.
func TestMainAgentMaxTokens_ExplicitSettingWins(t *testing.T) {
	t.Setenv(agent.EnvMaxOutputTokens, "40000")
	got := mainAgentMaxTokens()
	if got == nil {
		t.Fatal("mainAgentMaxTokens() = nil, want a pointer to 40000")
	}
	if *got != 40000 {
		t.Errorf("mainAgentMaxTokens() = %d, want 40000 (explicit setting)", *got)
	}
}

// TestMainAgentMaxTokens_MatchesResolver pins mainAgentMaxTokens to
// agent.ResolveMaxOutputTokens under both a valid override and an invalid
// one, so the main-agent wiring can never be changed to bypass the resolver
// (e.g. reading agent.DefaultMaxOutputTokens directly again) without this
// test catching the divergence.
func TestMainAgentMaxTokens_MatchesResolver(t *testing.T) {
	for _, raw := range []string{"", "50000", "not-a-number", "-5"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(agent.EnvMaxOutputTokens, raw)
			want := agent.ResolveMaxOutputTokens()
			got := mainAgentMaxTokens()
			if got == nil || *got != want {
				t.Errorf("mainAgentMaxTokens() = %v, want pointer to agent.ResolveMaxOutputTokens() = %d", got, want)
			}
		})
	}
}
