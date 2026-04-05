// Package plugin provides a modular plugin system for extending DeepAI capabilities.
package plugin

import (
	"context"
	"fmt"
	"time"
)

// PluginType defines the category of a plugin.
type PluginType string

const (
	// PluginTypeTool provides tools that can be called by agents.
	PluginTypeTool PluginType = "tool"
	// PluginTypeLLM provides LLM provider implementations.
	PluginTypeLLM PluginType = "llm"
	// PluginTypeMemory provides memory storage backends.
	PluginTypeMemory PluginType = "memory"
	// PluginTypeHook provides lifecycle hooks.
	PluginTypeHook PluginType = "hook"
	// PluginTypeMCP provides MCP server adapters.
	PluginTypeMCP PluginType = "mcp"
)

// PluginState represents the current state of a plugin.
type PluginState string

const (
	// PluginStateUnloaded indicates the plugin is not loaded.
	PluginStateUnloaded PluginState = "unloaded"
	// PluginStateLoaded indicates the plugin is loaded but not started.
	PluginStateLoaded PluginState = "loaded"
	// PluginStateStarting indicates the plugin is starting.
	PluginStateStarting PluginState = "starting"
	// PluginStateRunning indicates the plugin is running.
	PluginStateRunning PluginState = "running"
	// PluginStateStopping indicates the plugin is stopping.
	PluginStateStopping PluginState = "stopping"
	// PluginStateFailed indicates the plugin failed to start or run.
	PluginStateFailed PluginState = "failed"
	// PluginStateDisabled indicates the plugin is disabled.
	PluginStateDisabled PluginState = "disabled"
)

// Info contains metadata about a plugin.
type Info struct {
	// ID is the unique identifier for the plugin.
	ID string `json:"id" yaml:"id"`
	// Name is the human-readable name.
	Name string `json:"name" yaml:"name"`
	// Version follows semantic versioning.
	Version string `json:"version" yaml:"version"`
	// Description explains what the plugin does.
	Description string `json:"description" yaml:"description"`
	// Author is the plugin creator.
	Author string `json:"author,omitempty" yaml:"author,omitempty"`
	// Type categorizes the plugin.
	Type PluginType `json:"type" yaml:"type"`
	// Dependencies lists other plugins this one depends on.
	Dependencies []Dependency `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	// Permissions declares what resources the plugin needs access to.
	Permissions []Permission `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	// Tags for categorization and search.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Homepage URL.
	Homepage string `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	// License identifier (e.g., MIT, Apache-2.0).
	License string `json:"license,omitempty" yaml:"license,omitempty"`
}

// Dependency declares a dependency on another plugin.
type Dependency struct {
	// ID of the required plugin.
	ID string `json:"id" yaml:"id"`
	// Version constraint (semver format).
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// Permission declares access to a resource.
type Permission struct {
	// Resource identifies what is being accessed (e.g., "fs:read", "network:http").
	Resource string `json:"resource" yaml:"resource"`
	// Action specifies the access level (e.g., "allow", "deny").
	Action string `json:"action" yaml:"action"`
	// Description explains why this permission is needed.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Config holds plugin configuration.
type Config struct {
	// ID matches the plugin ID.
	ID string `json:"id" yaml:"id"`
	// Enabled controls whether the plugin is active.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Priority affects loading and execution order.
	Priority int `json:"priority,omitempty" yaml:"priority,omitempty"`
	// Settings contains plugin-specific configuration.
	Settings map[string]any `json:"settings,omitempty" yaml:"settings,omitempty"`
	// Secrets contains sensitive configuration (not serialized).
	Secrets map[string]string `json:"-" yaml:"-"`
}

// UnmarshalYAML implements custom YAML unmarshaling that collects unknown
// fields into Settings. This allows plugin.yaml to use flat config:
//
//	config:
//	  default_backend: "http"
//	  timeout: 30
//
// instead of nesting under a "settings" key.
func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}

	if v, ok := raw["id"]; ok {
		c.ID = fmt.Sprintf("%v", v)
	}
	if v, ok := raw["enabled"]; ok {
		c.Enabled, _ = v.(bool)
	}
	if v, ok := raw["priority"]; ok {
		switch val := v.(type) {
		case int:
			c.Priority = val
		case float64:
			c.Priority = int(val)
		}
	}
	if v, ok := raw["settings"]; ok {
		if m, ok := v.(map[string]any); ok {
			c.Settings = m
		}
	}

	// Collect unrecognized fields into Settings so they reach plugin_init.
	known := map[string]bool{
		"id": true, "enabled": true, "priority": true, "settings": true, "secrets": true,
	}
	if c.Settings == nil {
		c.Settings = make(map[string]any)
	}
	for k, v := range raw {
		if !known[k] {
			c.Settings[k] = v
		}
	}
	return nil
}

// Manifest describes a plugin from its plugin.yaml file.
type Manifest struct {
	// ID is the unique identifier.
	ID string `yaml:"id"`
	// Name is the human-readable name.
	Name string `yaml:"name"`
	// Version follows semantic versioning.
	Version string `yaml:"version"`
	// Description explains what the plugin does.
	Description string `yaml:"description"`
	// Author is the plugin creator.
	Author string `yaml:"author"`
	// Type categorizes the plugin.
	Type PluginType `yaml:"type"`
	// Runtime specifies how the plugin runs (go, python, node, wasm, binary).
	Runtime string `yaml:"runtime"`
	// Main is the entry point file or command.
	Main string `yaml:"main"`
	// Dependencies lists required plugins.
	Dependencies []Dependency `yaml:"dependencies"`
	// Permissions declares required access.
	Permissions []Permission `yaml:"permissions"`
	// ConfigSchema defines the configuration structure.
	ConfigSchema map[string]any `yaml:"config_schema"`
	// Config provides default configuration values.
	Config Config `yaml:"config"`
	// Path is the directory containing the plugin (set during discovery).
	Path string `yaml:"-"`
}

// ToInfo converts Manifest to Info.
func (m *Manifest) ToInfo() Info {
	return Info{
		ID:           m.ID,
		Name:         m.Name,
		Version:      m.Version,
		Description:  m.Description,
		Author:       m.Author,
		Type:         m.Type,
		Dependencies: m.Dependencies,
		Permissions:  m.Permissions,
	}
}

// StateSnapshot captures the current state of a loaded plugin.
type StateSnapshot struct {
	// ID is the plugin identifier.
	ID string `json:"id"`
	// State is the current plugin state.
	State PluginState `json:"state"`
	// LoadedAt is when the plugin was loaded.
	LoadedAt time.Time `json:"loaded_at,omitempty"`
	// StartedAt is when the plugin was started.
	StartedAt time.Time `json:"started_at,omitempty"`
	// Error contains the last error if state is failed.
	Error string `json:"error,omitempty"`
	// Version is the plugin version.
	Version string `json:"version"`
}

// EventType for plugin events.
type EventType string

const (
	// EventDiscovered is emitted when a plugin is found.
	EventDiscovered EventType = "discovered"
	// EventLoaded is emitted when a plugin is loaded.
	EventLoaded EventType = "loaded"
	// EventInitialized is emitted after plugin initialization.
	EventInitialized EventType = "initialized"
	// EventStarted is emitted when a plugin starts running.
	EventStarted EventType = "started"
	// EventStopped is emitted when a plugin stops.
	EventStopped EventType = "stopped"
	// EventUnloaded is emitted when a plugin is unloaded.
	EventUnloaded EventType = "unloaded"
	// EventFailed is emitted on plugin failure.
	EventFailed EventType = "failed"
)

// Event represents a plugin lifecycle event.
type Event struct {
	// Type is the event type.
	Type EventType `json:"type"`
	// PluginID is the affected plugin.
	PluginID string `json:"plugin_id"`
	// State is the current plugin state.
	State PluginState `json:"state,omitempty"`
	// Error if the event represents a failure.
	Error error `json:"error,omitempty"`
	// Timestamp when the event occurred.
	Timestamp time.Time `json:"timestamp"`
}

// HookPoint defines where hooks can be attached.
type HookPoint string

const (
	// Agent lifecycle hooks.
	HookBeforeAgentRun HookPoint = "before_agent_run"
	HookAfterAgentRun  HookPoint = "after_agent_run"
	HookOnAgentError   HookPoint = "on_agent_error"

	// Tool lifecycle hooks.
	HookBeforeToolCall HookPoint = "before_tool_call"
	HookAfterToolCall  HookPoint = "after_tool_call"
	HookOnToolError    HookPoint = "on_tool_error"

	// LLM lifecycle hooks.
	HookBeforeLLMCall HookPoint = "before_llm_call"
	HookAfterLLMCall  HookPoint = "after_llm_call"
	HookOnLLMStream   HookPoint = "on_llm_stream"

	// Memory lifecycle hooks.
	HookBeforeMemorySave HookPoint = "before_memory_save"
	HookAfterMemoryLoad  HookPoint = "after_memory_load"
)

// HookContext provides context for hook execution.
type HookContext struct {
	// Point is the hook point being executed.
	Point HookPoint `json:"point"`
	// SessionID identifies the current session.
	SessionID string `json:"session_id,omitempty"`
	// AgentID identifies the agent.
	AgentID string `json:"agent_id,omitempty"`
	// Input contains the hook input data.
	Input any `json:"input,omitempty"`
	// Output contains the hook output data (for after hooks).
	Output any `json:"output,omitempty"`
	// Error if the operation failed.
	Error error `json:"-"`
	// Metadata for additional context.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Aborted indicates if the operation should be cancelled.
	Aborted bool `json:"aborted,omitempty"`
	// AbortReason explains why the operation was aborted.
	AbortReason string `json:"abort_reason,omitempty"`
}

// contextKey for storing values in context.
type contextKey string

const (
	// pluginContextKey stores plugin info in context.
	pluginContextKey contextKey = "plugin"
)

// WithPlugin adds plugin info to context.
func WithPlugin(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, pluginContextKey, info)
}

// PluginFromContext retrieves plugin info from context.
func PluginFromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(pluginContextKey).(Info)
	return info, ok
}
