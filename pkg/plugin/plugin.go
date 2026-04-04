package plugin

import (
	"context"

	"github.com/millken/deepai/pkg/models"
)

// Plugin is the base interface that all plugins must implement.
type Plugin interface {
	// Info returns plugin metadata.
	Info() Info

	// Init initializes the plugin with configuration.
	Init(ctx context.Context, cfg Config) error

	// Start begins plugin operation.
	Start(ctx context.Context) error

	// Stop halts plugin operation.
	Stop(ctx context.Context) error

	// Close releases all resources.
	Close() error
}

// ToolPlugin provides tools for agent use.
type ToolPlugin interface {
	Plugin

	// Tools returns the tool definitions provided by this plugin.
	Tools(ctx context.Context) ([]models.Tool, error)

	// Groups returns tool group names for organization.
	Groups() []string
}

// HookPlugin provides lifecycle hooks.
type HookPlugin interface {
	Plugin

	// Hooks returns the hook points this plugin subscribes to.
	Hooks() []HookPoint

	// OnHook is called when a subscribed hook point is reached.
	OnHook(ctx context.Context, hctx *HookContext) error
}

// ProviderPlugin provides LLM capabilities.
type ProviderPlugin interface {
	Plugin

	// ProviderType returns the provider type identifier.
	ProviderType() string

	// Models returns available models.
	Models(ctx context.Context) ([]ModelInfo, error)
}

// ModelInfo describes an LLM model.
type ModelInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	ContextLimit int      `json:"context_limit"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// MemoryPlugin provides memory storage backends.
type MemoryPlugin interface {
	Plugin

	// StorageType returns the storage type identifier.
	StorageType() string

	// Capabilities returns storage capabilities.
	Capabilities() []string // e.g., "vector", "full-text", "persistent"
}

// BasePlugin provides a default implementation for common plugin operations.
// Embed this struct to simplify plugin development.
type BasePlugin struct {
	info    Info
	config  Config
	started bool
}

// NewBasePlugin creates a new BasePlugin with the given info.
func NewBasePlugin(info Info) *BasePlugin {
	return &BasePlugin{
		info: info,
	}
}

// Info returns plugin metadata.
func (p *BasePlugin) Info() Info {
	return p.info
}

// Init initializes the plugin with configuration.
func (p *BasePlugin) Init(ctx context.Context, cfg Config) error {
	p.config = cfg
	return nil
}

// Start begins plugin operation.
func (p *BasePlugin) Start(ctx context.Context) error {
	p.started = true
	return nil
}

// Stop halts plugin operation.
func (p *BasePlugin) Stop(ctx context.Context) error {
	p.started = false
	return nil
}

// Close releases all resources.
func (p *BasePlugin) Close() error {
	return nil
}

// Config returns the current configuration.
func (p *BasePlugin) Config() Config {
	return p.config
}

// IsStarted returns whether the plugin is started.
func (p *BasePlugin) IsStarted() bool {
	return p.started
}

// BaseToolPlugin provides a base implementation for tool plugins.
type BaseToolPlugin struct {
	BasePlugin
	tools  []models.Tool
	groups []string
}

// NewBaseToolPlugin creates a new BaseToolPlugin.
func NewBaseToolPlugin(info Info, tools []models.Tool, groups []string) *BaseToolPlugin {
	return &BaseToolPlugin{
		BasePlugin: *NewBasePlugin(info),
		tools:      tools,
		groups:     groups,
	}
}

// Tools returns the tool definitions.
func (p *BaseToolPlugin) Tools(ctx context.Context) ([]models.Tool, error) {
	return p.tools, nil
}

// Groups returns tool group names.
func (p *BaseToolPlugin) Groups() []string {
	return p.groups
}

// BaseHookPlugin provides a base implementation for hook plugins.
type BaseHookPlugin struct {
	BasePlugin
	hooks []HookPoint
}

// NewBaseHookPlugin creates a new BaseHookPlugin.
func NewBaseHookPlugin(info Info, hooks []HookPoint) *BaseHookPlugin {
	return &BaseHookPlugin{
		BasePlugin: *NewBasePlugin(info),
		hooks:      hooks,
	}
}

// Hooks returns the subscribed hook points.
func (p *BaseHookPlugin) Hooks() []HookPoint {
	return p.hooks
}

// OnHook is called for each hook event. Override this method in subclasses.
func (p *BaseHookPlugin) OnHook(ctx context.Context, hctx *HookContext) error {
	// Default implementation does nothing
	return nil
}

// PluginFactory creates plugin instances.
type PluginFactory func() Plugin

// ToolPluginFactory creates tool plugin instances.
type ToolPluginFactory func() ToolPlugin

// HookPluginFactory creates hook plugin instances.
type HookPluginFactory func() HookPlugin
