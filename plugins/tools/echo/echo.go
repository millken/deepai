// Package main provides an example tool plugin using the purego-compatible interface.
// This plugin can be compiled with: go build -buildmode=c-shared -o echo.so echo.go
//
// The key difference from Go's standard plugin package:
// - Uses //export directives for C-compatible exports
// - Uses JSON strings for cross-language config/args passing
// - Can be loaded by any language that supports C ABI
package main

import (
	"C"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// EchoPlugin is a simple example tool plugin.
type EchoPlugin struct {
	mu     sync.RWMutex
	prefix string
	maxLen int
	tools  []ToolDef
}

// ToolDef represents a tool definition.
type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// Global plugin instance registry (for callbacks)
var plugins = make(map[uintptr]*EchoPlugin)
var pluginsMu sync.RWMutex

// New creates a new EchoPlugin instance.
func New() *EchoPlugin {
	return &EchoPlugin{
		prefix: "[ECHO] ",
		maxLen: 1000,
		tools: []ToolDef{
			{
				Name:        "echo",
				Description: "Echoes back the input message with an optional prefix",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The message to echo back",
						},
						"uppercase": map[string]interface{}{
							"type":        "boolean",
							"description": "Convert message to uppercase",
							"default":     false,
						},
					},
					"required": []string{"message"},
				},
			},
		},
	}
}

// ============== Exported C Functions ==============
// These functions are called by the plugin loader.

//export plugin_new
func plugin_new() uintptr {
	p := New()
	pluginsMu.Lock()
	ptr := uintptr(unsafe.Pointer(p))
	plugins[ptr] = p
	pluginsMu.Unlock()
	return ptr
}

//export plugin_name
func plugin_name(ptr unsafe.Pointer) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString("unknown")
	}
	return C.CString("echo-tool")
}

//export plugin_version
func plugin_version(ptr unsafe.Pointer) *C.char {
	return C.CString("1.0.0")
}

//export plugin_description
func plugin_description(ptr unsafe.Pointer) *C.char {
	return C.CString("A simple echo tool plugin that repeats messages")
}

//export plugin_abi_version
func plugin_abi_version() *C.char {
	return C.CString("1.0")
}

//export plugin_type
func plugin_type(ptr unsafe.Pointer) *C.char {
	return C.CString("tool")
}

//export plugin_init
func plugin_init(ptr unsafe.Pointer, configJSON *C.char) {
	p := getPlugin(uintptr(ptr))
	if p == nil || configJSON == nil {
		return
	}

	// Parse JSON config
	config := make(map[string]interface{})
	if goStr := C.GoString(configJSON); goStr != "" && goStr != "{}" {
		if err := json.Unmarshal([]byte(goStr), &config); err != nil {
			return
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Apply configuration
	if prefix, ok := config["prefix"].(string); ok {
		p.prefix = prefix
	}
	if maxLen, ok := config["max_length"].(float64); ok {
		p.maxLen = int(maxLen)
	}
}

//export plugin_start
func plugin_start(ptr unsafe.Pointer) {
	// Start is optional - no-op for this plugin
}

//export plugin_stop
func plugin_stop(ptr unsafe.Pointer) {
	// Stop is optional - no-op for this plugin
}

//export plugin_close
func plugin_close(ptr unsafe.Pointer) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	delete(plugins, uintptr(ptr))
}

//export plugin_cancel
func plugin_cancel(ptr unsafe.Pointer, callID uint64) {
	// Echo is synchronous and fast; no-op but required for ABI compliance.
}

//export plugin_tools
func plugin_tools(ptr unsafe.Pointer) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString("[]")
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	data, err := json.Marshal(p.tools)
	if err != nil {
		return C.CString("[]")
	}
	return C.CString(string(data))
}

//export plugin_execute
func plugin_execute(ptr unsafe.Pointer, toolName *C.char, argsJSON *C.char, callID uint64) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString(`{"error": "plugin not found"}`)
	}

	tool := C.GoString(toolName)
	argsStr := C.GoString(argsJSON)

	args := make(map[string]interface{})
	if argsStr != "" && argsStr != "{}" {
		if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
			return C.CString(`{"error": "invalid args JSON"}`)
		}
	}

	result, err := p.executeTool(tool, args)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"error": %q}`, err.Error()))
	}

	resultJSON, _ := json.Marshal(result)
	return C.CString(string(resultJSON))
}

// ============== Internal Implementation ==============

func getPlugin(ptr uintptr) *EchoPlugin {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	return plugins[ptr]
}

func (p *EchoPlugin) executeTool(name string, args map[string]interface{}) (interface{}, error) {
	switch name {
	case "echo":
		return p.handleEcho(args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (p *EchoPlugin) handleEcho(args map[string]interface{}) (interface{}, error) {
	message, ok := args["message"].(string)
	if !ok {
		return nil, fmt.Errorf("message argument is required and must be a string")
	}

	p.mu.RLock()
	prefix := p.prefix
	maxLen := p.maxLen
	p.mu.RUnlock()

	// Truncate if needed
	if len(message) > maxLen {
		message = message[:maxLen] + "..."
	}

	// Apply uppercase if requested
	if uppercase, ok := args["uppercase"].(bool); ok && uppercase {
		message = strings.ToUpper(message)
	}

	// Add prefix
	result := prefix + message

	return map[string]interface{}{
		"content": result,
	}, nil
}

// Required for c-shared build
func main() {}
