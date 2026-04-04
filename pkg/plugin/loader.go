package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// Loader interface for loading plugins from different sources.
type Loader interface {
	// Load creates a plugin instance from a manifest.
	Load(ctx context.Context, manifest *Manifest) (Plugin, error)
	// CanLoad returns true if this loader can handle the manifest.
	CanLoad(manifest *Manifest) bool
}

// CompositeLoader tries multiple loaders in order.
type CompositeLoader struct {
	loaders []Loader
}

// NewCompositeLoader creates a loader that tries multiple loaders.
func NewCompositeLoader() *CompositeLoader {
	return &CompositeLoader{
		loaders: []Loader{
			NewRegistryLoader(),      // 优先使用注册表（内置插件）
			NewSharedLibraryLoader(), // 共享库插件
			NewBinaryLoader(),        // 独立进程插件
			NewHTTPLoader(),          // 远程 HTTP 插件
			NewConfigLoader(),        // 配置驱动插件
		},
	}
}

// Add appends a loader to the chain.
func (l *CompositeLoader) Add(loader Loader) {
	l.loaders = append(l.loaders, loader)
}

// Load tries each loader until one succeeds.
func (l *CompositeLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	for _, loader := range l.loaders {
		if loader.CanLoad(manifest) {
			p, err := loader.Load(ctx, manifest)
			if err == nil {
				return p, nil
			}
			continue
		}
	}
	return nil, fmt.Errorf("no loader available for runtime: %s", manifest.Runtime)
}

// CanLoad always returns true for composite loader.
func (l *CompositeLoader) CanLoad(manifest *Manifest) bool {
	return true
}

// RegistryLoader loads plugins from the global registry (built-in plugins).
type RegistryLoader struct{}

// NewRegistryLoader creates a new registry loader.
func NewRegistryLoader() *RegistryLoader {
	return &RegistryLoader{}
}

// CanLoad returns true for registered plugins.
func (l *RegistryLoader) CanLoad(manifest *Manifest) bool {
	_, ok := globalRegistry.Get(manifest.ID)
	return ok
}

// Load returns the registered plugin.
func (l *RegistryLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	p, ok := globalRegistry.Get(manifest.ID)
	if !ok {
		return nil, fmt.Errorf("plugin %s not in registry", manifest.ID)
	}
	return p, nil
}

// BinaryLoader loads external binary plugins via JSON-RPC.
// This is the RECOMMENDED approach for external plugins.
// Each plugin runs as an independent process, communicating via stdin/stdout.
type BinaryLoader struct{}

// NewBinaryLoader creates a new binary plugin loader.
func NewBinaryLoader() *BinaryLoader {
	return &BinaryLoader{}
}

// CanLoad returns true for binary runtime.
func (l *BinaryLoader) CanLoad(manifest *Manifest) bool {
	return manifest.Runtime == "binary"
}

// Load creates a wrapper for an external binary.
func (l *BinaryLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	cmdPath := filepath.Join(manifest.Path, manifest.Main)

	// Check if executable exists
	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("binary not found: %s", cmdPath)
	}

	return NewBinaryPluginWrapper(cmdPath, manifest), nil
}

// BinaryPluginWrapper wraps an external binary as a plugin using JSON-RPC over stdin/stdout.
type BinaryPluginWrapper struct {
	info      Info
	manifest  *Manifest
	cmdPath   string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.Reader
	stderr    io.Reader
	state     PluginState
	requestID int64
	mu        sync.Mutex
}

// NewBinaryPluginWrapper creates a new binary plugin wrapper.
func NewBinaryPluginWrapper(cmdPath string, manifest *Manifest) *BinaryPluginWrapper {
	return &BinaryPluginWrapper{
		cmdPath:  cmdPath,
		manifest: manifest,
		info:     manifest.ToInfo(),
		state:    PluginStateUnloaded,
	}
}

// Info returns plugin metadata.
func (p *BinaryPluginWrapper) Info() Info {
	return p.info
}

// Init initializes the binary plugin.
func (p *BinaryPluginWrapper) Init(ctx context.Context, cfg Config) error {
	p.cmd = exec.CommandContext(ctx, p.cmdPath)
	p.cmd.Env = os.Environ()

	// Add config as environment variables
	for k, v := range cfg.Settings {
		p.cmd.Env = append(p.cmd.Env, fmt.Sprintf("PLUGIN_%s=%v", envKey(k), v))
	}
	for k, v := range cfg.Secrets {
		p.cmd.Env = append(p.cmd.Env, fmt.Sprintf("PLUGIN_SECRET_%s=%s", envKey(k), v))
	}

	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	p.stdout, err = p.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	p.stderr, err = p.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	return nil
}

// Start starts the binary process.
func (p *BinaryPluginWrapper) Start(ctx context.Context) error {
	if p.cmd == nil {
		return fmt.Errorf("plugin not initialized")
	}

	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("start binary: %w", err)
	}

	// Wait for ready signal
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := p.waitForReady(readyCtx); err != nil {
		p.cmd.Process.Kill()
		return fmt.Errorf("plugin not ready: %w", err)
	}

	p.state = PluginStateRunning
	return nil
}

func (p *BinaryPluginWrapper) waitForReady(ctx context.Context) error {
	// Send handshake request
	resp, err := p.call(ctx, "handshake", map[string]any{
		"version": "1.0.0",
	})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("handshake failed: %s", resp.Error.Message)
	}
	return nil
}

func (p *BinaryPluginWrapper) call(ctx context.Context, method string, params any) (*jsonRPCResponse, error) {
	p.mu.Lock()
	p.requestID++
	id := p.requestID
	p.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	data = append(data, '\n')
	if _, err := p.stdin.Write(data); err != nil {
		return nil, err
	}

	// Read response (simplified - should use buffered reader)
	decoder := json.NewDecoder(p.stdout)
	var resp jsonRPCResponse
	if err := decoder.Decode(&resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Stop stops the binary process.
func (p *BinaryPluginWrapper) Stop(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	// Send shutdown request
	_, _ = p.call(ctx, "shutdown", nil)

	// Wait for graceful exit
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
	}

	p.state = PluginStateLoaded
	return nil
}

// Close releases resources.
func (p *BinaryPluginWrapper) Close() error {
	if p.stdin != nil {
		p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.cmd = nil
	p.state = PluginStateUnloaded
	return nil
}

// GetTools returns tools from the binary plugin.
func (p *BinaryPluginWrapper) GetTools(ctx context.Context) ([]ToolDefinition, error) {
	resp, err := p.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}

	var result struct {
		Tools []ToolDefinition `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// Tools implements ToolPlugin interface by converting ToolDefinition to models.Tool.
func (p *BinaryPluginWrapper) Tools(ctx context.Context) ([]models.Tool, error) {
	toolDefs, err := p.GetTools(ctx)
	if err != nil {
		return nil, err
	}

	tools := make([]models.Tool, 0, len(toolDefs))
	for _, td := range toolDefs {
		tool := models.Tool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: td.InputSchema,
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// Groups implements ToolPlugin interface.
func (p *BinaryPluginWrapper) Groups() []string {
	return []string{"binary", "external"}
}

// ExecuteTool executes a tool on the binary plugin.
func (p *BinaryPluginWrapper) ExecuteTool(ctx context.Context, name string, args map[string]any) (any, error) {
	resp, err := p.call(ctx, "tools/execute", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}

	var result any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ToolDefinition represents a tool definition from a plugin.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func envKey(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
}

// HTTPLoader loads plugins from HTTP endpoints (remote plugins).
type HTTPLoader struct {
	client *http.Client
}

// NewHTTPLoader creates a new HTTP plugin loader.
func NewHTTPLoader() *HTTPLoader {
	return &HTTPLoader{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// CanLoad returns true for http runtime.
func (l *HTTPLoader) CanLoad(manifest *Manifest) bool {
	return manifest.Runtime == "http" || manifest.Runtime == "remote"
}

// Load creates a remote plugin wrapper.
func (l *HTTPLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	if manifest.Main == "" {
		return nil, fmt.Errorf("http plugin requires main URL")
	}

	return &HTTPPlugin{
		info:     manifest.ToInfo(),
		endpoint: manifest.Main,
		client:   l.client,
	}, nil
}

// HTTPPlugin wraps a remote HTTP plugin.
type HTTPPlugin struct {
	info     Info
	endpoint string
	client   *http.Client
	state    PluginState
}

// Info returns plugin metadata.
func (p *HTTPPlugin) Info() Info {
	return p.info
}

// Init initializes the remote plugin.
func (p *HTTPPlugin) Init(ctx context.Context, cfg Config) error {
	return nil
}

// Start starts the remote plugin connection.
func (p *HTTPPlugin) Start(ctx context.Context) error {
	p.state = PluginStateRunning
	return nil
}

// Stop stops the remote plugin connection.
func (p *HTTPPlugin) Stop(ctx context.Context) error {
	p.state = PluginStateLoaded
	return nil
}

// Close releases resources.
func (p *HTTPPlugin) Close() error {
	return nil
}

// ConfigLoader loads plugins from configuration files.
// This is the RECOMMENDED approach for most use cases.
// Tools are defined declaratively in YAML/JSON without compilation.
type ConfigLoader struct{}

// NewConfigLoader creates a new config loader.
func NewConfigLoader() *ConfigLoader {
	return &ConfigLoader{}
}

// CanLoad returns true for config runtime.
func (l *ConfigLoader) CanLoad(manifest *Manifest) bool {
	return manifest.Runtime == "config" || manifest.Runtime == "yaml" || manifest.Runtime == "json"
}

// Load creates a plugin from configuration.
func (l *ConfigLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	return NewConfigPlugin(manifest), nil
}

// ConfigPlugin is a plugin defined entirely by configuration.
type ConfigPlugin struct {
	info   Info
	config Config
	tools  []ConfigToolDefinition
	state  PluginState
}

// ConfigToolDefinition defines a tool via configuration.
type ConfigToolDefinition struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	InputSchema map[string]any `yaml:"input_schema" json:"input_schema"`
	Executor    ConfigExecutor `yaml:"executor" json:"executor"`
}

// ConfigExecutor defines how a tool is executed.
type ConfigExecutor struct {
	Type     string            `yaml:"type" json:"type"` // http, command, template
	Command  string            `yaml:"command,omitempty" json:"command,omitempty"`
	Endpoint string            `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Method   string            `yaml:"method,omitempty" json:"method,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Template string            `yaml:"template,omitempty" json:"template,omitempty"`
	Timeout  time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// NewConfigPlugin creates a new config-based plugin.
func NewConfigPlugin(manifest *Manifest) *ConfigPlugin {
	return &ConfigPlugin{
		info:  manifest.ToInfo(),
		state: PluginStateUnloaded,
	}
}

// Info returns plugin metadata.
func (p *ConfigPlugin) Info() Info {
	return p.info
}

// Init initializes the config plugin with safe type assertions.
func (p *ConfigPlugin) Init(ctx context.Context, cfg Config) error {
	p.config = cfg

	// Load tool definitions from config with safe type assertions
	toolsRaw, ok := cfg.Settings["tools"]
	if !ok {
		return nil
	}

	toolsSlice, ok := toolsRaw.([]any)
	if !ok {
		return fmt.Errorf("config: tools must be an array")
	}

	for i, t := range toolsSlice {
		toolMap, ok := t.(map[string]any)
		if !ok {
			return fmt.Errorf("config: tools[%d] must be an object", i)
		}

		// Safely extract required fields
		name, ok := toolMap["name"].(string)
		if !ok {
			return fmt.Errorf("config: tools[%d].name is required and must be string", i)
		}

		description, _ := toolMap["description"].(string) // optional, default empty

		tool := ConfigToolDefinition{
			Name:        name,
			Description: description,
		}

		if schema, ok := toolMap["input_schema"].(map[string]any); ok {
			tool.InputSchema = schema
		}

		if exec, ok := toolMap["executor"].(map[string]any); ok {
			tool.Executor = ConfigExecutor{}

			if execType, ok := exec["type"].(string); ok {
				tool.Executor.Type = execType
			}
			if cmd, ok := exec["command"].(string); ok {
				tool.Executor.Command = cmd
			}
			if ep, ok := exec["endpoint"].(string); ok {
				tool.Executor.Endpoint = ep
			}
			if method, ok := exec["method"].(string); ok {
				tool.Executor.Method = method
			}
			if headers, ok := exec["headers"].(map[string]any); ok {
				tool.Executor.Headers = make(map[string]string)
				for k, v := range headers {
					tool.Executor.Headers[k] = fmt.Sprint(v)
				}
			}
		}

		p.tools = append(p.tools, tool)
	}

	return nil
}

// Start starts the config plugin.
func (p *ConfigPlugin) Start(ctx context.Context) error {
	p.state = PluginStateRunning
	return nil
}

// Stop stops the config plugin.
func (p *ConfigPlugin) Stop(ctx context.Context) error {
	p.state = PluginStateLoaded
	return nil
}

// Close releases resources.
func (p *ConfigPlugin) Close() error {
	p.state = PluginStateUnloaded
	return nil
}

// GetTools returns tool definitions.
func (p *ConfigPlugin) GetTools() []ConfigToolDefinition {
	return p.tools
}

// Tools implements ToolPlugin interface by converting ConfigToolDefinition to models.Tool.
func (p *ConfigPlugin) Tools(ctx context.Context) ([]models.Tool, error) {
	tools := make([]models.Tool, 0, len(p.tools))
	for _, td := range p.tools {
		tool := models.Tool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: td.InputSchema,
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// Groups implements ToolPlugin interface.
func (p *ConfigPlugin) Groups() []string {
	return []string{"config", "declarative"}
}

// SharedLibraryLoader loads plugins from shared libraries (.so/.dylib/.dll).
type SharedLibraryLoader struct{}

// NewSharedLibraryLoader creates a new shared library loader.
func NewSharedLibraryLoader() *SharedLibraryLoader {
	return &SharedLibraryLoader{}
}

// CanLoad returns true for shared library runtime.
func (l *SharedLibraryLoader) CanLoad(manifest *Manifest) bool {
	return manifest.Runtime == "so" || manifest.Runtime == "shared" ||
		strings.HasSuffix(manifest.Main, ".so") ||
		strings.HasSuffix(manifest.Main, ".dylib") ||
		strings.HasSuffix(manifest.Main, ".dll")
}

// Load loads a shared library plugin.
func (l *SharedLibraryLoader) Load(ctx context.Context, manifest *Manifest) (Plugin, error) {
	libPath := filepath.Join(manifest.Path, manifest.Main)

	// Check if file exists
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("shared library not found: %s", libPath)
	}

	return LoadSharedLibrary(libPath)
}
