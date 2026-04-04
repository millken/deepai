package plugin

import (
	"fmt"
	"sort"
	"sync"
)

// Registry provides static plugin registration for built-in plugins.
type Registry struct {
	mu        sync.RWMutex
	plugins   map[string]Plugin
	factories map[string]PluginFactory
	info      map[string]Info
}

// NewRegistry creates a new plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins:   make(map[string]Plugin),
		factories: make(map[string]PluginFactory),
		info:      make(map[string]Info),
	}
}

// globalRegistry is the default registry for plugin registration.
var globalRegistry = NewRegistry()

// Register registers a plugin with the global registry.
func Register(p Plugin) error {
	return globalRegistry.Register(p)
}

// RegisterFactory registers a plugin factory with the global registry.
func RegisterFactory(id string, factory PluginFactory) error {
	return globalRegistry.RegisterFactory(id, factory)
}

// Get retrieves a plugin from the global registry.
func Get(id string) (Plugin, bool) {
	return globalRegistry.Get(id)
}

// List returns all registered plugins from the global registry.
func List() []Plugin {
	return globalRegistry.List()
}

// ListByType returns plugins filtered by type from the global registry.
func ListByType(ptype PluginType) []Plugin {
	return globalRegistry.ListByType(ptype)
}

// Register adds a plugin to the registry.
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return fmt.Errorf("plugin is nil")
	}

	info := p.Info()
	if info.ID == "" {
		return fmt.Errorf("plugin ID is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.plugins[info.ID]; exists {
		return fmt.Errorf("plugin %q already registered", info.ID)
	}

	r.plugins[info.ID] = p
	r.info[info.ID] = info
	return nil
}

// RegisterFactory adds a plugin factory to the registry.
func (r *Registry) RegisterFactory(id string, factory PluginFactory) error {
	if factory == nil {
		return fmt.Errorf("factory is nil")
	}
	if id == "" {
		return fmt.Errorf("factory ID is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories[id]; exists {
		return fmt.Errorf("factory %q already registered", id)
	}

	r.factories[id] = factory
	return nil
}

// Get retrieves a plugin by ID.
func (r *Registry) Get(id string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.plugins[id]
	return p, ok
}

// GetFactory retrieves a factory by ID.
func (r *Registry) GetFactory(id string) (PluginFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	f, ok := r.factories[id]
	return f, ok
}

// Create creates a plugin instance using a registered factory.
func (r *Registry) Create(id string) (Plugin, error) {
	factory, ok := r.GetFactory(id)
	if !ok {
		return nil, fmt.Errorf("factory %q not found", id)
	}
	return factory(), nil
}

// List returns all registered plugins.
func (r *Registry) List() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	plugins := make([]Plugin, 0, len(ids))
	for _, id := range ids {
		plugins = append(plugins, r.plugins[id])
	}
	return plugins
}

// ListByType returns plugins filtered by type.
func (r *Registry) ListByType(ptype PluginType) []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var plugins []Plugin
	for _, p := range r.plugins {
		if p.Info().Type == ptype {
			plugins = append(plugins, p)
		}
	}
	return plugins
}

// ListInfo returns info for all registered plugins.
func (r *Registry) ListInfo() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.info))
	for id := range r.info {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	infos := make([]Info, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, r.info[id])
	}
	return infos
}

// Unregister removes a plugin from the registry.
func (r *Registry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.plugins, id)
	delete(r.info, id)
	delete(r.factories, id)
	return true
}

// Clear removes all plugins from the registry.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.plugins = make(map[string]Plugin)
	r.factories = make(map[string]PluginFactory)
	r.info = make(map[string]Info)
}

// Has checks if a plugin is registered.
func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.plugins[id]
	return ok
}

// Count returns the number of registered plugins.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.plugins)
}

// GetToolPlugins returns all registered tool plugins.
func (r *Registry) GetToolPlugins() []ToolPlugin {
	plugins := r.ListByType(PluginTypeTool)
	result := make([]ToolPlugin, 0, len(plugins))
	for _, p := range plugins {
		if tp, ok := p.(ToolPlugin); ok {
			result = append(result, tp)
		}
	}
	return result
}

// GetHookPlugins returns all registered hook plugins.
func (r *Registry) GetHookPlugins() []HookPlugin {
	plugins := r.ListByType(PluginTypeHook)
	result := make([]HookPlugin, 0, len(plugins))
	for _, p := range plugins {
		if hp, ok := p.(HookPlugin); ok {
			result = append(result, hp)
		}
	}
	return result
}
