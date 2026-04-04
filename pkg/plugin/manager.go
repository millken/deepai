package plugin

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

// ManagerConfig holds configuration for the plugin manager.
type ManagerConfig struct {
	// PluginDirs are directories to search for plugins.
	PluginDirs []string `json:"plugin_dirs" yaml:"plugin_dirs"`
	// AutoLoad enables automatic plugin discovery and loading.
	AutoLoad bool `json:"auto_load" yaml:"auto_load"`
	// AutoStart enables automatic plugin starting after loading.
	AutoStart bool `json:"auto_start" yaml:"auto_start"`
	// LoadTimeout is the maximum time to wait for plugin loading.
	LoadTimeout time.Duration `json:"load_timeout" yaml:"load_timeout"`
	// StartTimeout is the maximum time to wait for plugin starting.
	StartTimeout time.Duration `json:"start_timeout" yaml:"start_timeout"`
	// Strict mode fails on first plugin error.
	Strict bool `json:"strict" yaml:"strict"`
	// MaxConcurrent limits parallel plugin operations.
	MaxConcurrent int `json:"max_concurrent" yaml:"max_concurrent"`
	// EnabledPlugins is a whitelist of plugin IDs to load.
	EnabledPlugins []string `json:"enabled_plugins" yaml:"enabled_plugins"`
	// DisabledPlugins is a blacklist of plugin IDs to skip.
	DisabledPlugins []string `json:"disabled_plugins" yaml:"disabled_plugins"`
}

// DefaultManagerConfig returns the default configuration.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		PluginDirs:    []string{"./plugins"},
		AutoLoad:      true,
		AutoStart:     false,
		LoadTimeout:   30 * time.Second,
		StartTimeout:  10 * time.Second,
		Strict:        false,
		MaxConcurrent: 10,
	}
}

// wrapper wraps a plugin with its state and metadata.
type wrapper struct {
	plugin    Plugin
	manifest  *Manifest
	state     PluginState
	config    Config
	loadedAt  time.Time
	startedAt time.Time
	err       error
}

// Manager handles plugin lifecycle operations.
type Manager struct {
	mu       sync.RWMutex
	wrappers map[string]*wrapper
	loader   *CompositeLoader
	resolver *DependencyResolver
	config   ManagerConfig
	registry *Registry
	logger   *log.Logger
	events   chan Event
	done     chan struct{}

	// Hooks for lifecycle notifications
	onLoad  []func(Event)
	onStart []func(Event)
	onStop  []func(Event)
	onFail  []func(Event)
}

// NewManager creates a new plugin manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 30 * time.Second
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 10 * time.Second
	}
	if len(cfg.PluginDirs) == 0 {
		cfg.PluginDirs = []string{"./plugins"}
	}

	return &Manager{
		wrappers: make(map[string]*wrapper),
		loader:   NewCompositeLoader(),
		resolver: NewDependencyResolver(),
		config:   cfg,
		registry: globalRegistry,
		logger:   log.New(os.Stderr, "[plugin] ", log.LstdFlags),
		events:   make(chan Event, 256),
		done:     make(chan struct{}),
	}
}

// SetLogger configures a custom logger.
func (m *Manager) SetLogger(logger *log.Logger) *Manager {
	if logger != nil {
		m.logger = logger
	}
	return m
}

// SetRegistry configures a custom plugin registry.
func (m *Manager) SetRegistry(registry *Registry) *Manager {
	if registry != nil {
		m.registry = registry
	}
	return m
}

// Events returns the event channel for monitoring plugin lifecycle.
func (m *Manager) Events() <-chan Event {
	return m.events
}

// OnLoad registers a callback for plugin load events.
func (m *Manager) OnLoad(fn func(Event)) {
	m.onLoad = append(m.onLoad, fn)
}

// OnStart registers a callback for plugin start events.
func (m *Manager) OnStart(fn func(Event)) {
	m.onStart = append(m.onStart, fn)
}

// OnStop registers a callback for plugin stop events.
func (m *Manager) OnStop(fn func(Event)) {
	m.onStop = append(m.onStop, fn)
}

// OnFail registers a callback for plugin failure events.
func (m *Manager) OnFail(fn func(Event)) {
	m.onFail = append(m.onFail, fn)
}

// emit sends an event to the event channel.
func (m *Manager) emit(typ EventType, id string, state PluginState, err error) {
	evt := Event{
		Type:      typ,
		PluginID:  id,
		State:     state,
		Error:     err,
		Timestamp: time.Now().UTC(),
	}

	select {
	case m.events <- evt:
	default:
		// Channel full, skip
	}

	// Call registered callbacks
	switch typ {
	case EventLoaded:
		for _, fn := range m.onLoad {
			fn(evt)
		}
	case EventStarted:
		for _, fn := range m.onStart {
			fn(evt)
		}
	case EventStopped:
		for _, fn := range m.onStop {
			fn(evt)
		}
	case EventFailed:
		for _, fn := range m.onFail {
			fn(evt)
		}
	}
}

// Discover finds all plugin manifests in the configured directories.
func (m *Manager) Discover(ctx context.Context) (map[string]*Manifest, error) {
	manifests := make(map[string]*Manifest)

	for _, dir := range m.config.PluginDirs {
		found, err := m.discoverDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("discover %s: %w", dir, err)
		}

		for id, manifest := range found {
			// Check blacklist first
			disabled := false
			for _, did := range m.config.DisabledPlugins {
				if did == id {
					disabled = true
					break
				}
			}
			if disabled {
				continue
			}

			// Check whitelist
			if len(m.config.EnabledPlugins) > 0 {
				enabled := false
				for _, eid := range m.config.EnabledPlugins {
					if eid == id {
						enabled = true
						break
					}
				}
				if !enabled {
					continue
				}
			}

			manifests[id] = manifest
		}
	}

	// Also include registered plugins
	for _, p := range m.registry.List() {
		info := p.Info()
		if _, exists := manifests[info.ID]; !exists {
			manifests[info.ID] = &Manifest{
				ID:           info.ID,
				Name:         info.Name,
				Version:      info.Version,
				Description:  info.Description,
				Author:       info.Author,
				Type:         info.Type,
				Runtime:      "go",
				Dependencies: info.Dependencies,
				Permissions:  info.Permissions,
			}
		}
	}

	return manifests, nil
}

func (m *Manager) discoverDir(dir string) (map[string]*Manifest, error) {
	manifests := make(map[string]*Manifest)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pluginDir := filepath.Join(dir, entry.Name())
		manifestPath := filepath.Join(pluginDir, "plugin.yaml")

		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}

		var manifest Manifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			m.logger.Printf("parse manifest %s: %v", manifestPath, err)
			continue
		}

		manifest.Path = pluginDir
		manifests[manifest.ID] = &manifest
	}

	return manifests, nil
}

// Load discovers and loads all plugins.
func (m *Manager) Load(ctx context.Context) error {
	manifests, err := m.Discover(ctx)
	if err != nil {
		return err
	}

	// Resolve dependency order
	order, err := m.resolver.Resolve(manifests)
	if err != nil {
		return fmt.Errorf("resolve dependencies: %w", err)
	}

	// Load in order
	for _, id := range order {
		manifest := manifests[id]
		if err := m.LoadPlugin(ctx, manifest); err != nil {
			if m.config.Strict {
				return fmt.Errorf("load %s: %w", id, err)
			}
			m.logger.Printf("load plugin %s: %v", id, err)
			continue
		}
	}

	return nil
}

// LoadPlugin loads a single plugin from its manifest.
func (m *Manager) LoadPlugin(ctx context.Context, manifest *Manifest) error {
	m.mu.Lock()

	// Check if already loaded
	if _, exists := m.wrappers[manifest.ID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s already loaded", manifest.ID)
	}

	// Create load context with timeout
	loadCtx, cancel := context.WithTimeout(ctx, m.config.LoadTimeout)
	defer cancel()

	// Try to load from registry first
	var p Plugin
	if registered, ok := m.registry.Get(manifest.ID); ok {
		p = registered
	} else {
		// Release lock before loading (I/O operation)
		m.mu.Unlock()
		loaded, err := m.loader.Load(loadCtx, manifest)
		m.mu.Lock()

		if err != nil {
			m.mu.Unlock()
			// Emit event without holding lock to prevent deadlock
			m.emit(EventFailed, manifest.ID, PluginStateFailed, err)
			return err
		}
		p = loaded
	}

	// Create wrapper
	w := &wrapper{
		plugin:   p,
		manifest: manifest,
		state:    PluginStateLoaded,
		config:   manifest.Config,
		loadedAt: time.Now().UTC(),
	}

	m.wrappers[manifest.ID] = w
	m.mu.Unlock()

	// Emit event without holding lock to prevent deadlock
	m.emit(EventLoaded, manifest.ID, PluginStateLoaded, nil)

	return nil
}

// Start starts a loaded plugin.
func (m *Manager) Start(ctx context.Context, id string) error {
	m.mu.RLock()
	w, exists := m.wrappers[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("plugin %s not found", id)
	}

	m.mu.Lock()
	if w.state == PluginStateRunning {
		m.mu.Unlock()
		return nil
	}
	w.state = PluginStateStarting
	m.mu.Unlock()

	// Create start context with timeout
	startCtx, cancel := context.WithTimeout(ctx, m.config.StartTimeout)
	defer cancel()

	// Initialize
	if err := w.plugin.Init(startCtx, w.config); err != nil {
		m.mu.Lock()
		w.state = PluginStateFailed
		w.err = err
		m.mu.Unlock()
		m.emit(EventFailed, id, PluginStateFailed, err)
		return err
	}

	// Start
	if err := w.plugin.Start(startCtx); err != nil {
		m.mu.Lock()
		w.state = PluginStateFailed
		w.err = err
		m.mu.Unlock()
		m.emit(EventFailed, id, PluginStateFailed, err)
		return err
	}

	m.mu.Lock()
	w.state = PluginStateRunning
	w.startedAt = time.Now().UTC()
	w.err = nil
	m.mu.Unlock()

	m.emit(EventStarted, id, PluginStateRunning, nil)
	return nil
}

// StartAll starts all loaded plugins respecting dependency order.
// Plugins are started in layers - each layer contains plugins whose dependencies
// have all been started. Plugins within a layer are started in parallel.
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.RLock()

	// Build manifests from loaded plugins for dependency resolution
	manifests := make(map[string]*Manifest)
	for id, w := range m.wrappers {
		manifests[id] = w.manifest
	}
	m.mu.RUnlock()

	// Resolve dependency order
	order, err := m.resolver.Resolve(manifests)
	if err != nil {
		m.logger.Printf("dependency resolution failed: %v, starting in arbitrary order", err)
		// Fallback: start in arbitrary order
		m.mu.RLock()
		order = make([]string, 0, len(m.wrappers))
		for id := range m.wrappers {
			order = append(order, id)
		}
		m.mu.RUnlock()
	}

	// Build dependency layers for parallel start within each layer
	layers := m.buildDependencyLayers(manifests, order)

	for i, layer := range layers {
		layerCtx := ctx

		// Start plugins in this layer in parallel
		g, gctx := errgroup.WithContext(layerCtx)
		g.SetLimit(m.config.MaxConcurrent)

		for _, id := range layer {
			id := id // capture
			g.Go(func() error {
				select {
				case <-gctx.Done():
					return nil
				default:
				}

				if err := m.Start(gctx, id); err != nil {
					if m.config.Strict {
						return err
					}
					m.logger.Printf("start %s: %v", id, err)
				}
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			return fmt.Errorf("layer %d start failed: %w", i, err)
		}
	}

	return nil
}

// buildDependencyLayers groups plugins into layers based on dependencies.
// Layer 0 has no dependencies, layer N only depends on layers < N.
func (m *Manager) buildDependencyLayers(manifests map[string]*Manifest, order []string) [][]string {
	// Calculate depth for each plugin
	depth := make(map[string]int)
	for _, id := range order {
		m := manifests[id]
		maxDepDepth := 0
		for _, dep := range m.Dependencies {
			if d, ok := depth[dep.ID]; ok && d >= maxDepDepth {
				maxDepDepth = d + 1
			}
		}
		depth[id] = maxDepDepth
	}

	// Group by depth
	maxDepth := 0
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}

	layers := make([][]string, maxDepth+1)
	for id, d := range depth {
		layers[d] = append(layers[d], id)
	}

	return layers
}

// Stop stops a running plugin.
func (m *Manager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	w, exists := m.wrappers[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s not found", id)
	}

	if w.state != PluginStateRunning {
		m.mu.Unlock()
		return nil
	}

	w.state = PluginStateStopping
	m.mu.Unlock()

	if err := w.plugin.Stop(ctx); err != nil {
		m.mu.Lock()
		w.state = PluginStateFailed
		w.err = err
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	w.state = PluginStateLoaded
	m.mu.Unlock()

	m.emit(EventStopped, id, PluginStateLoaded, nil)
	return nil
}

// StopAll stops all running plugins in parallel.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.wrappers))
	for id, w := range m.wrappers {
		if w.state == PluginStateRunning {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.config.MaxConcurrent)

	for _, id := range ids {
		id := id // capture
		g.Go(func() error {
			if err := m.Stop(gctx, id); err != nil {
				m.logger.Printf("stop %s: %v", id, err)
			}
			return nil // Don't fail on stop errors
		})
	}

	return g.Wait()
}

// Unload removes a plugin completely.
func (m *Manager) Unload(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	w, exists := m.wrappers[id]
	if !exists {
		return nil
	}

	// Stop if running
	if w.state == PluginStateRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.plugin.Stop(ctx)
	}

	// Close
	_ = w.plugin.Close()

	delete(m.wrappers, id)
	m.emit(EventUnloaded, id, PluginStateUnloaded, nil)
	return nil
}

// Get retrieves a plugin by ID.
func (m *Manager) Get(id string) (Plugin, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	w, exists := m.wrappers[id]
	if !exists || w.state != PluginStateRunning {
		return nil, false
	}
	return w.plugin, true
}

// GetState returns the current state of a plugin.
func (m *Manager) GetState(id string) (StateSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	w, exists := m.wrappers[id]
	if !exists {
		return StateSnapshot{}, false
	}

	snapshot := StateSnapshot{
		ID:        id,
		State:     w.state,
		LoadedAt:  w.loadedAt,
		StartedAt: w.startedAt,
		Version:   w.plugin.Info().Version,
	}
	if w.err != nil {
		snapshot.Error = w.err.Error()
	}
	return snapshot, true
}

// List returns all loaded plugins.
func (m *Manager) List() []Plugin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	plugins := make([]Plugin, 0, len(m.wrappers))
	for _, w := range m.wrappers {
		plugins = append(plugins, w.plugin)
	}
	return plugins
}

// ListByType returns plugins filtered by type.
func (m *Manager) ListByType(ptype PluginType) []Plugin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var plugins []Plugin
	for _, w := range m.wrappers {
		if w.plugin.Info().Type == ptype && w.state == PluginStateRunning {
			plugins = append(plugins, w.plugin)
		}
	}
	return plugins
}

// GetTools returns all tools from tool plugins.
func (m *Manager) GetTools(ctx context.Context) ([]models.Tool, error) {
	plugins := m.ListByType(PluginTypeTool)
	var tools []models.Tool

	for _, p := range plugins {
		if tp, ok := p.(ToolPlugin); ok {
			pluginTools, err := tp.Tools(ctx)
			if err != nil {
				return nil, fmt.Errorf("get tools from %s: %w", p.Info().ID, err)
			}
			tools = append(tools, pluginTools...)
		}
	}
	return tools, nil
}

// ExecuteHook runs all hook plugins for a given hook point with timeout protection.
func (m *Manager) ExecuteHook(ctx context.Context, point HookPoint, hctx *HookContext) error {
	plugins := m.ListByType(PluginTypeHook)

	for _, p := range plugins {
		hp, ok := p.(HookPlugin)
		if !ok {
			continue
		}

		// Check if plugin subscribes to this hook
		subscribed := false
		for _, h := range hp.Hooks() {
			if h == point {
				subscribed = true
				break
			}
		}
		if !subscribed {
			continue
		}

		// Execute hook with timeout protection
		hookTimeout := m.config.StartTimeout
		if hookTimeout == 0 {
			hookTimeout = 5 * time.Second
		}
		hookCtx, cancel := context.WithTimeout(ctx, hookTimeout)

		err := hp.OnHook(hookCtx, hctx)
		cancel()

		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("hook %s timeout after %v", point, hookTimeout)
			}
			m.logger.Printf("hook %s plugin %s: %v", point, p.Info().ID, err)
			if m.config.Strict {
				return err
			}
		}

		// Check if aborted
		if hctx.Aborted {
			return fmt.Errorf("aborted by %s: %s", p.Info().ID, hctx.AbortReason)
		}
	}

	return nil
}

// Close shuts down the manager and all plugins.
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = m.StopAll(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, w := range m.wrappers {
		_ = w.plugin.Close()
	}

	m.wrappers = make(map[string]*wrapper)
	close(m.done)

	return nil
}
