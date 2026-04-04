// Package plugin provides a modular plugin system for DeepAI.
// This file implements shared library plugin loading using purego.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/millken/deepai/pkg/models"
)

// RequiredSymbols are the symbols that must be exported by a shared library plugin.
var RequiredSymbols = []string{
	"plugin_new",
	"plugin_name",
	"plugin_version",
	"plugin_description",
}

// OptionalSymbols are symbols that may be exported by a shared library plugin.
var OptionalSymbols = []string{
	"plugin_init",
	"plugin_start",
	"plugin_stop",
	"plugin_close",
	"plugin_type",
	"plugin_tools",
	"plugin_execute",
}

// SharedLibraryPlugin loads plugins compiled as shared libraries (.so/.dll).
// This uses purego to avoid cgo and provides better version compatibility
// than Go's standard plugin package.
//
// Plugin authors must export these C-callable functions:
//
//	//export plugin_new         // Create instance: uintptr plugin_new()
//	//export plugin_name        // Get name: char* plugin_name(uintptr ptr)
//	//export plugin_version     // Get version: char* plugin_version(uintptr ptr)
//	//export plugin_description // Get description: char* plugin_description(uintptr ptr)
//	//export plugin_init        // Initialize: void plugin_init(uintptr ptr, const char* config_json)
//	//export plugin_start       // Start: void plugin_start(uintptr ptr)
//	//export plugin_stop        // Stop: void plugin_stop(uintptr ptr)
//	//export plugin_close       // Close: void plugin_close(uintptr ptr)
//	//export plugin_execute     // Execute tool: char* plugin_execute(uintptr ptr, const char* tool_name, const char* args_json)
//	//export plugin_tools       // Get tools: char* plugin_tools(uintptr ptr)
type SharedLibraryPlugin struct {
	mu     sync.RWMutex
	lib    uintptr
	ptr    unsafe.Pointer
	info   Info
	state  PluginState
	config Config
}

// LoadSharedLibrary loads a plugin from a shared library file.
// The library must export the required symbols (plugin_new, plugin_name, etc.)
func LoadSharedLibrary(libPath string) (*SharedLibraryPlugin, error) {
	lib, err := dlopen(libPath)
	if err != nil {
		return nil, fmt.Errorf("load library %s: %w", libPath, err)
	}

	// Check required symbols
	for _, sym := range RequiredSymbols {
		if _, err := dlsym(lib, sym); err != nil {
			dlclose(lib)
			return nil, fmt.Errorf("missing required symbol %s: %w", sym, err)
		}
	}

	// Create plugin instance
	pluginNew, _ := dlsym(lib, "plugin_new")
	r1, _, _ := purego.SyscallN(pluginNew)
	if r1 == 0 {
		dlclose(lib)
		return nil, fmt.Errorf("plugin_new returned null")
	}

	ptr := unsafe.Pointer(r1)
	p := &SharedLibraryPlugin{
		lib:   lib,
		ptr:   ptr,
		state: PluginStateLoaded,
	}

	// Get plugin info
	name, err := p.callStringFunc("plugin_name")
	if err != nil {
		dlclose(lib)
		return nil, fmt.Errorf("get plugin name: %w", err)
	}

	version, err := p.callStringFunc("plugin_version")
	if err != nil {
		dlclose(lib)
		return nil, fmt.Errorf("get plugin version: %w", err)
	}

	description, err := p.callStringFunc("plugin_description")
	if err != nil {
		dlclose(lib)
		return nil, fmt.Errorf("get plugin description: %w", err)
	}

	// Try to get plugin type
	ptype := PluginTypeTool
	if typeStr, err := p.callStringFunc("plugin_type"); err == nil && typeStr != "" {
		ptype = PluginType(typeStr)
	}

	p.info = Info{
		ID:          name,
		Name:        name,
		Version:     version,
		Description: description,
		Type:        ptype,
	}

	return p, nil
}

// Info returns plugin metadata.
func (p *SharedLibraryPlugin) Info() Info {
	return p.info
}

// Init initializes the plugin with configuration.
// Config is passed as a JSON string for cross-language compatibility.
func (p *SharedLibraryPlugin) Init(ctx context.Context, cfg Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	initFunc, err := dlsym(p.lib, "plugin_init")
	if err != nil {
		// Init is optional
		p.config = cfg
		return nil
	}

	// Convert config to JSON string for cross-language compatibility
	configJSON := []byte("{}")
	if len(cfg.Settings) > 0 {
		configJSON, _ = json.Marshal(cfg.Settings)
	}

	// Add null terminator
	configJSON = append(configJSON, 0)

	purego.SyscallN(initFunc, uintptr(p.ptr), uintptr(unsafe.Pointer(&configJSON[0])))
	p.config = cfg
	return nil
}

// Start begins plugin operation.
func (p *SharedLibraryPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	startFunc, err := dlsym(p.lib, "plugin_start")
	if err != nil {
		// Start is optional
		p.state = PluginStateRunning
		return nil
	}

	purego.SyscallN(startFunc, uintptr(p.ptr))
	p.state = PluginStateRunning
	return nil
}

// Stop halts plugin operation.
func (p *SharedLibraryPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	stopFunc, err := dlsym(p.lib, "plugin_stop")
	if err != nil {
		// Stop is optional
		p.state = PluginStateLoaded
		return nil
	}

	purego.SyscallN(stopFunc, uintptr(p.ptr))
	p.state = PluginStateLoaded
	return nil
}

// Close releases all resources.
func (p *SharedLibraryPlugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if closeFunc, err := dlsym(p.lib, "plugin_close"); err == nil {
		purego.SyscallN(closeFunc, uintptr(p.ptr))
	}

	if p.lib != 0 {
		dlclose(p.lib)
		p.lib = 0
	}

	p.state = PluginStateUnloaded
	return nil
}

// State returns the current plugin state.
func (p *SharedLibraryPlugin) State() PluginState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// CallTool executes a tool provided by the plugin.
// Args are passed as JSON string for cross-language compatibility.
func (p *SharedLibraryPlugin) CallTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	executeFunc, err := dlsym(p.lib, "plugin_execute")
	if err != nil {
		return "", fmt.Errorf("plugin does not support tool execution: %w", err)
	}

	// Convert args to JSON string
	argsJSON := []byte("{}")
	if len(args) > 0 {
		argsJSON, _ = json.Marshal(args)
	}
	argsJSON = append(argsJSON, 0) // null terminator

	// Convert tool name to C string
	toolNameBytes := append([]byte(toolName), 0)

	r1, _, _ := purego.SyscallN(
		executeFunc,
		uintptr(p.ptr),
		uintptr(unsafe.Pointer(&toolNameBytes[0])),
		uintptr(unsafe.Pointer(&argsJSON[0])),
	)

	if r1 == 0 {
		return "", nil
	}

	return cStringToGoString(r1), nil
}

// GetTools returns tool definitions as JSON string if the plugin supports it.
func (p *SharedLibraryPlugin) GetTools() (string, error) {
	toolsFunc, err := dlsym(p.lib, "plugin_tools")
	if err != nil {
		return "", fmt.Errorf("plugin does not provide tools: %w", err)
	}

	r1, _, _ := purego.SyscallN(toolsFunc, uintptr(p.ptr))
	if r1 == 0 {
		return "", nil
	}

	return cStringToGoString(r1), nil
}

// Tools implements ToolPlugin interface by parsing JSON tool definitions.
func (p *SharedLibraryPlugin) Tools(ctx context.Context) ([]models.Tool, error) {
	toolsJSON, err := p.GetTools()
	if err != nil {
		return nil, err
	}
	if toolsJSON == "" {
		return nil, nil
	}

	var tools []models.Tool
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		return nil, fmt.Errorf("parse tools JSON: %w", err)
	}
	return tools, nil
}

// Groups implements ToolPlugin interface.
func (p *SharedLibraryPlugin) Groups() []string {
	return []string{"external", "shared-library"}
}

func (p *SharedLibraryPlugin) callStringFunc(name string) (string, error) {
	funcPtr, err := dlsym(p.lib, name)
	if err != nil {
		return "", err
	}

	r1, _, _ := purego.SyscallN(funcPtr, uintptr(p.ptr))
	if r1 == 0 {
		return "", nil
	}

	return cStringToGoString(r1), nil
}

func cStringToGoString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}

	var result []byte
	for i := 0; i < 65536; i++ {
		b := *(*byte)(unsafe.Pointer(ptr + uintptr(i)))
		if b == 0 {
			break
		}
		result = append(result, b)
	}
	return string(result)
}
