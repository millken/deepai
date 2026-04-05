// Package plugin provides a modular plugin system for DeepAI.
// This file implements shared library plugin loading using purego.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/millken/deepai/pkg/models"
)

// CurrentABI is the ABI version that the host expects from shared library plugins.
// Any plugin that reports a different major version will be rejected at load time.
const CurrentABI = "1.0"

// RequiredSymbols are the symbols that must be exported by a shared library plugin.
var RequiredSymbols = []string{
	"plugin_new",
	"plugin_name",
	"plugin_version",
	"plugin_description",
	"plugin_abi_version",
	"plugin_free_string",
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
	"plugin_cancel",
}

// SharedLibraryPlugin loads plugins compiled as shared libraries (.so/.dll).
// This uses purego to avoid cgo and provides better version compatibility
// than Go's standard plugin package.
//
// Plugin authors must export these C-callable functions:
//
//	//export plugin_new            // Create instance: uintptr plugin_new()
//	//export plugin_name           // Get name: char* plugin_name(uintptr ptr)
//	//export plugin_version        // Get version: char* plugin_version(uintptr ptr)
//	//export plugin_description    // Get description: char* plugin_description(uintptr ptr)
//	//export plugin_abi_version    // ABI version: char* plugin_abi_version() — must return "1.0"
//	//export plugin_init           // Initialize: void plugin_init(uintptr ptr, const char* config_json)
//	//export plugin_start          // Start: void plugin_start(uintptr ptr)
//	//export plugin_stop           // Stop: void plugin_stop(uintptr ptr)
//	//export plugin_close          // Close: void plugin_close(uintptr ptr)
//	//export plugin_execute        // Execute tool: char* plugin_execute(uintptr ptr, const char* tool_name, const char* args_json, uint64_t call_id)
//	//export plugin_tools          // Get tools: char* plugin_tools(uintptr ptr)
//	//export plugin_free_string    // Free C string: void plugin_free_string(char* s)
//	//export plugin_cancel         // Cancel specific call: void plugin_cancel(uintptr ptr, uint64_t call_id)
type SharedLibraryPlugin struct {
	mu            sync.RWMutex
	lib           uintptr
	ptr           unsafe.Pointer
	info          Info
	state         PluginState
	config        Config
	freeFunc      uintptr
	cancelFunc    uintptr
	callIDCounter uint64 // monotonically increasing, generates per-call IDs
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

	// Verify ABI version before doing anything else.
	abiVersionFunc, _ := dlsym(lib, "plugin_abi_version")
	abiR1, _, _ := purego.SyscallN(abiVersionFunc)
	if abiR1 == 0 {
		dlclose(lib)
		return nil, fmt.Errorf("plugin_abi_version returned null")
	}
	abiLen := uintptr(0)
	for ; abiLen < 64; abiLen++ {
		if *(*byte)(unsafe.Pointer(abiR1 + abiLen)) == 0 {
			break
		}
	}
	abiStr := string(unsafe.Slice((*byte)(unsafe.Pointer(abiR1)), abiLen))
	// Free the ABI version string if the plugin exports plugin_free_string.
	if freeFunc, err := dlsym(lib, "plugin_free_string"); err == nil {
		purego.SyscallN(freeFunc, abiR1)
	}
	if abiStr != CurrentABI {
		dlclose(lib)
		return nil, fmt.Errorf("ABI version mismatch: plugin reports %q, host requires %q", abiStr, CurrentABI)
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
		lib:           lib,
		ptr:           ptr,
		state:         PluginStateLoaded,
		callIDCounter: 0,
	}

	// Try to resolve optional lifecycle symbols (backward compatible)
	if freeFunc, err := dlsym(lib, "plugin_free_string"); err == nil {
		p.freeFunc = freeFunc
	}
	if cancelFunc, err := dlsym(lib, "plugin_cancel"); err == nil {
		p.cancelFunc = cancelFunc
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
// If the plugin exports plugin_cancel, the caller's context cancellation is
// propagated with a per-call ID so the plugin can cancel the exact operation.
func (p *SharedLibraryPlugin) CallTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	// Resolve the symbol and snapshot the pointers we need while holding the lock,
	// then release it so long-running FFI calls don't block Init/Start/Stop/Close.
	p.mu.RLock()
	executeFunc, err := dlsym(p.lib, "plugin_execute")
	if err != nil {
		p.mu.RUnlock()
		return "", fmt.Errorf("plugin does not support tool execution: %w", err)
	}
	pluginPtr := p.ptr
	cancelFunc := p.cancelFunc
	p.mu.RUnlock()

	// Convert args to JSON string
	argsJSON := []byte("{}")
	if len(args) > 0 {
		argsJSON, _ = json.Marshal(args)
	}
	argsJSON = append(argsJSON, 0) // null terminator

	// Convert tool name to C string
	toolNameBytes := append([]byte(toolName), 0)

	// Generate a unique call ID so the plugin can identify which call to cancel.
	callID := atomic.AddUint64(&p.callIDCounter, 1)

	// Propagate context cancellation to the plugin if it supports it.
	var cancelOnce sync.Once
	done := make(chan struct{})
	if cancelFunc != 0 && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cancelOnce.Do(func() {
					purego.SyscallN(cancelFunc, uintptr(pluginPtr), uintptr(callID))
				})
			case <-done:
			}
		}()
	}

	r1, _, _ := purego.SyscallN(
		executeFunc,
		uintptr(pluginPtr),
		uintptr(unsafe.Pointer(&toolNameBytes[0])),
		uintptr(unsafe.Pointer(&argsJSON[0])),
		uintptr(callID),
	)

	close(done)

	if r1 == 0 {
		return "", fmt.Errorf("plugin_execute returned null")
	}

	// Check if the call was cancelled by the caller's context.
	if ctx.Err() != nil {
		p.cStringToGoString(r1) // free the returned C string
		return "", ctx.Err()
	}

	return p.cStringToGoString(r1), nil
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

	return p.cStringToGoString(r1), nil
}

// Tools implements ToolPlugin interface by parsing JSON tool definitions
// and injecting a handler that bridges to plugin_execute.
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

	// Inject a handler for each tool so it can be registered and executed
	// through the standard Tool model (which requires Handler != nil).
	for i := range tools {
		if tools[i].Handler != nil {
			continue
		}
		tool := tools[i] // capture
		tools[i].Handler = func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			resultJSON, err := p.CallTool(ctx, tool.Name, call.Arguments)
			if err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: tool.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, nil
			}

			// Check whether the plugin returned an error envelope.
			var envelope struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(resultJSON), &envelope) == nil && envelope.Error != "" {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: tool.Name,
					Status:   models.CallStatusFailed,
					Error:    envelope.Error,
					Content:  resultJSON,
				}, nil
			}

			return models.ToolResult{
				CallID:   call.ID,
				ToolName: tool.Name,
				Status:   models.CallStatusCompleted,
				Content:  resultJSON,
			}, nil
		}
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

	return p.cStringToGoString(r1), nil
}

const maxCStringLen = 4 * 1024 * 1024 // 4 MB — safety bound for untrusted plugin pointers

func (p *SharedLibraryPlugin) cStringToGoString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}

	// Measure length up to the safety bound.
	var length uintptr
	for ; length < maxCStringLen; length++ {
		b := *(*byte)(unsafe.Pointer(ptr + length))
		if b == 0 {
			break
		}
	}
	if length >= maxCStringLen {
		// No NUL terminator found within the safety bound — treat as a bad pointer.
		p.freeCString(ptr)
		return ""
	}

	// unsafe.Slice requires Go 1.17+; build a byte slice pointing at the C memory.
	result := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), length)

	// Copy before freeing — the C memory is invalidated by freeCString.
	goStr := string(result)

	// Free the C-allocated string if the plugin supports it.
	p.freeCString(ptr)

	return goStr
}

func (p *SharedLibraryPlugin) freeCString(ptr uintptr) {
	if p.freeFunc == 0 || ptr == 0 {
		return
	}
	purego.SyscallN(p.freeFunc, ptr)
}
