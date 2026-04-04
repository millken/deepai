package plugin

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// MockPlugin is a test plugin implementation.
type MockPlugin struct {
	BasePlugin
	tools  []models.Tool
	groups []string
	hooks  []HookPoint
}

func NewMockPlugin(id string, ptype PluginType) *MockPlugin {
	return &MockPlugin{
		BasePlugin: *NewBasePlugin(Info{
			ID:          id,
			Name:        "Mock Plugin",
			Version:     "1.0.0",
			Description: "A mock plugin for testing",
			Type:        ptype,
		}),
	}
}

func (p *MockPlugin) Tools(ctx context.Context) ([]models.Tool, error) {
	return p.tools, nil
}

func (p *MockPlugin) Groups() []string {
	return p.groups
}

func (p *MockPlugin) Hooks() []HookPoint {
	return p.hooks
}

func (p *MockPlugin) OnHook(ctx context.Context, hctx *HookContext) error {
	return nil
}

func TestPluginInfo(t *testing.T) {
	info := Info{
		ID:          "test-plugin",
		Name:        "Test Plugin",
		Version:     "1.0.0",
		Description: "A test plugin",
		Type:        PluginTypeTool,
	}

	if info.ID != "test-plugin" {
		t.Errorf("expected ID test-plugin, got %s", info.ID)
	}
	if info.Type != PluginTypeTool {
		t.Errorf("expected type tool, got %s", info.Type)
	}
}

func TestBasePlugin(t *testing.T) {
	info := Info{
		ID:      "base-test",
		Name:    "Base Test",
		Version: "1.0.0",
		Type:    PluginTypeTool,
	}

	p := NewBasePlugin(info)

	if p.Info().ID != "base-test" {
		t.Errorf("expected ID base-test, got %s", p.Info().ID)
	}

	ctx := context.Background()
	cfg := Config{ID: "base-test", Enabled: true}

	if err := p.Init(ctx, cfg); err != nil {
		t.Errorf("Init failed: %v", err)
	}

	if err := p.Start(ctx); err != nil {
		t.Errorf("Start failed: %v", err)
	}

	if !p.IsStarted() {
		t.Error("expected plugin to be started")
	}

	if err := p.Stop(ctx); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	if p.IsStarted() {
		t.Error("expected plugin to be stopped")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	p1 := NewMockPlugin("plugin-1", PluginTypeTool)
	p2 := NewMockPlugin("plugin-2", PluginTypeHook)

	// Test Register
	if err := r.Register(p1); err != nil {
		t.Errorf("Register p1 failed: %v", err)
	}
	if err := r.Register(p2); err != nil {
		t.Errorf("Register p2 failed: %v", err)
	}

	// Test duplicate registration
	if err := r.Register(p1); err == nil {
		t.Error("expected error for duplicate registration")
	}

	// Test Has
	if !r.Has("plugin-1") {
		t.Error("expected Has(plugin-1) to be true")
	}
	if r.Has("nonexistent") {
		t.Error("expected Has(nonexistent) to be false")
	}

	// Test Get
	if p, ok := r.Get("plugin-1"); !ok || p.Info().ID != "plugin-1" {
		t.Error("Get failed")
	}

	// Test Count
	if r.Count() != 2 {
		t.Errorf("expected count 2, got %d", r.Count())
	}

	// Test ListByType
	toolPlugins := r.ListByType(PluginTypeTool)
	if len(toolPlugins) != 1 {
		t.Errorf("expected 1 tool plugin, got %d", len(toolPlugins))
	}

	// Test Unregister
	if !r.Unregister("plugin-1") {
		t.Error("expected Unregister to succeed")
	}
	if r.Has("plugin-1") {
		t.Error("expected plugin-1 to be unregistered")
	}
}

func TestGlobalRegistry(t *testing.T) {
	// Save current state
	original := globalRegistry
	defer func() { globalRegistry = original }()

	// Reset for test
	globalRegistry = NewRegistry()

	p := NewMockPlugin("global-test", PluginTypeTool)
	if err := Register(p); err != nil {
		t.Errorf("Register failed: %v", err)
	}

	if _, ok := Get("global-test"); !ok {
		t.Error("Get failed for registered plugin")
	}
}

func TestManager(t *testing.T) {
	// Create a test registry
	r := NewRegistry()
	p1 := NewMockPlugin("managed-1", PluginTypeTool)
	p1.tools = []models.Tool{
		{Name: "test_tool", Description: "A test tool"},
	}
	r.Register(p1)

	// Create manager
	cfg := DefaultManagerConfig()
	cfg.AutoLoad = false
	m := NewManager(cfg)
	m.SetRegistry(r)

	ctx := context.Background()

	// Load plugin
	manifest := &Manifest{
		ID:          "managed-1",
		Name:        "Managed Plugin",
		Version:     "1.0.0",
		Description: "A managed plugin",
		Type:        PluginTypeTool,
		Runtime:     "go",
		Config:      Config{ID: "managed-1", Enabled: true},
	}

	if err := m.LoadPlugin(ctx, manifest); err != nil {
		t.Fatalf("LoadPlugin failed: %v", err)
	}

	// Check state
	state, ok := m.GetState("managed-1")
	if !ok {
		t.Fatal("GetState failed")
	}
	if state.State != PluginStateLoaded {
		t.Errorf("expected state loaded, got %s", state.State)
	}

	// Start plugin
	if err := m.Start(ctx, "managed-1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	state, _ = m.GetState("managed-1")
	if state.State != PluginStateRunning {
		t.Errorf("expected state running, got %s", state.State)
	}

	// Get plugin
	loaded, ok := m.Get("managed-1")
	if !ok {
		t.Fatal("Get failed")
	}
	if loaded.Info().ID != "managed-1" {
		t.Errorf("expected ID managed-1, got %s", loaded.Info().ID)
	}

	// Stop plugin
	if err := m.Stop(ctx, "managed-1"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	state, _ = m.GetState("managed-1")
	if state.State != PluginStateLoaded {
		t.Errorf("expected state loaded, got %s", state.State)
	}
}

func TestManagerHooks(t *testing.T) {
	r := NewRegistry()
	hp := NewMockPlugin("hook-plugin", PluginTypeHook)
	hp.hooks = []HookPoint{HookBeforeToolCall, HookAfterToolCall}
	r.Register(hp)

	cfg := DefaultManagerConfig()
	m := NewManager(cfg)
	m.SetRegistry(r)

	ctx := context.Background()

	manifest := &Manifest{
		ID:      "hook-plugin",
		Name:    "Hook Plugin",
		Version: "1.0.0",
		Type:    PluginTypeHook,
		Runtime: "go",
		Config:  Config{ID: "hook-plugin", Enabled: true},
	}

	if err := m.LoadPlugin(ctx, manifest); err != nil {
		t.Fatalf("LoadPlugin failed: %v", err)
	}

	if err := m.Start(ctx, "hook-plugin"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Execute hook
	hctx := &HookContext{
		Point:     HookBeforeToolCall,
		SessionID: "test-session",
	}

	if err := m.ExecuteHook(ctx, HookBeforeToolCall, hctx); err != nil {
		t.Errorf("ExecuteHook failed: %v", err)
	}
}

func TestDependencyResolver(t *testing.T) {
	r := NewDependencyResolver()

	// Test simple dependency order
	manifests := map[string]*Manifest{
		"plugin-a": {
			ID: "plugin-a",
			Dependencies: []Dependency{
				{ID: "plugin-b", Version: ">=1.0.0"},
			},
		},
		"plugin-b": {
			ID:      "plugin-b",
			Version: "1.0.0",
		},
		"plugin-c": {
			ID: "plugin-c",
			Dependencies: []Dependency{
				{ID: "plugin-a"},
			},
		},
	}

	order, err := r.Resolve(manifests)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	// plugin-b should come before plugin-a
	// plugin-a should come before plugin-c
	bIdx := indexOf(order, "plugin-b")
	aIdx := indexOf(order, "plugin-a")
	cIdx := indexOf(order, "plugin-c")

	if bIdx > aIdx {
		t.Error("plugin-b should be loaded before plugin-a")
	}
	if aIdx > cIdx {
		t.Error("plugin-a should be loaded before plugin-c")
	}
}

func TestDependencyResolverCircular(t *testing.T) {
	r := NewDependencyResolver()

	// Test circular dependency detection
	manifests := map[string]*Manifest{
		"plugin-a": {
			ID: "plugin-a",
			Dependencies: []Dependency{
				{ID: "plugin-b"},
			},
		},
		"plugin-b": {
			ID: "plugin-b",
			Dependencies: []Dependency{
				{ID: "plugin-a"},
			},
		},
	}

	_, err := r.Resolve(manifests)
	if err == nil {
		t.Error("expected error for circular dependency")
	}
}

func TestPluginState(t *testing.T) {
	states := []PluginState{
		PluginStateUnloaded,
		PluginStateLoaded,
		PluginStateStarting,
		PluginStateRunning,
		PluginStateStopping,
		PluginStateFailed,
		PluginStateDisabled,
	}

	for _, state := range states {
		if state == "" {
			t.Errorf("empty state")
		}
	}
}

func TestHookContext(t *testing.T) {
	hctx := &HookContext{
		Point:     HookBeforeToolCall,
		SessionID: "test-session",
		AgentID:   "test-agent",
		Input:     map[string]any{"arg": "value"},
		Metadata:  map[string]any{"key": "value"},
	}

	if hctx.Point != HookBeforeToolCall {
		t.Errorf("unexpected point: %s", hctx.Point)
	}
	if hctx.Aborted {
		t.Error("should not be aborted by default")
	}

	hctx.Aborted = true
	hctx.AbortReason = "test abort"

	if !hctx.Aborted {
		t.Error("should be aborted")
	}
}

// Helper function
func indexOf(slice []string, item string) int {
	for i, v := range slice {
		if v == item {
			return i
		}
	}
	return -1
}
