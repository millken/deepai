# DeepAI Plugin System

DeepAI 的插件系统提供了一个模块化、可扩展的框架，用于向 Agent 添加新功能。

## 功能特性

- **多种插件类型**：工具插件、LLM 提供者、记忆存储、生命周期钩子
- **跨语言支持**：通过 purego 加载共享库，支持 Go、Rust、C 等语言
- **并行加载**：插件并行启动，避免单个插件阻塞所有插件
- **超时保护**：Hook 执行和插件启动都有超时保护
- **依赖管理**：自动解析依赖顺序，检测循环依赖
- **版本控制**：支持语义版本约束
- **生命周期管理**：加载、启动、停止、卸载
- **事件系统**：监控插件状态变化
- **权限控制**：声明式权限管理

## 快速开始

### 创建一个工具插件

```go
package main

import (
    "context"

    "github.com/millken/deepai/pkg/models"
    "github.com/millken/deepai/pkg/plugin"
)

// MyPlugin 实现工具插件接口
type MyPlugin struct {
    plugin.BasePlugin
}

func New() plugin.Plugin {
    return &MyPlugin{
        BasePlugin: *plugin.NewBasePlugin(plugin.Info{
            ID:          "my-tool",
            Name:        "My Tool",
            Version:     "1.0.0",
            Description: "A custom tool plugin",
            Type:        plugin.PluginTypeTool,
        }),
    }
}

// Tools 返回工具定义
func (p *MyPlugin) Tools(ctx context.Context) ([]models.Tool, error) {
    return []models.Tool{
        {
            Name:        "my_function",
            Description: "执行自定义功能",
            InputSchema: map[string]any{
                "type": "object",
                "properties": map[string]any{
                    "input": map[string]any{
                        "type":        "string",
                        "description": "输入参数",
                    },
                },
                "required": []string{"input"},
            },
            Handler: p.handleMyFunction,
        },
    }, nil
}

func (p *MyPlugin) handleMyFunction(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    input := call.Arguments["input"].(string)
    // 处理逻辑...
    return models.ToolResult{
        CallID:   call.ID,
        ToolName: call.Name,
        Status:   models.CallStatusCompleted,
        Content:  "处理结果: " + input,
    }, nil
}

// 注册插件
func init() {
    plugin.Register(New())
}
```

### 插件清单 (plugin.yaml)

```yaml
id: my-tool
name: My Tool
version: 1.0.0
description: A custom tool plugin
author: Your Name
type: tool
runtime: go
main: my-tool.so

permissions:
  - resource: fs:read
    action: allow
    description: 读取文件

config_schema:
  type: object
  properties:
    max_items:
      type: integer
      default: 100

config:
  max_items: 100
```

## 使用插件管理器

```go
package main

import (
    "context"
    "log"

    "github.com/millken/deepai/pkg/plugin"
)

func main() {
    ctx := context.Background()

    // 创建管理器
    mgr := plugin.NewManager(plugin.ManagerConfig{
        PluginDirs:    []string{"./plugins/tools"},
        AutoLoad:      true,
        AutoStart:     true,
        LoadTimeout:   30 * time.Second,
        StartTimeout:  10 * time.Second,
        MaxConcurrent: 10,  // 并行加载/启动的最大并发数
    })

    // 监听事件
    mgr.OnLoad(func(evt plugin.Event) {
        log.Printf("Plugin loaded: %s", evt.PluginID)
    })

    // 加载插件
    if err := mgr.Load(ctx); err != nil {
        log.Fatal(err)
    }
    defer mgr.Close()

    // 获取工具
    tools, err := mgr.GetTools(ctx)
    if err != nil {
        log.Fatal(err)
    }

    // 使用工具...
}
```

## 插件类型

### ToolPlugin（工具插件）

提供 Agent 可调用的工具：

```go
type ToolPlugin interface {
    Plugin
    Tools(ctx context.Context) ([]models.Tool, error)
    Groups() []string
}
```

### HookPlugin（钩子插件）

订阅 Agent 生命周期事件：

```go
type HookPlugin interface {
    Plugin
    Hooks() []HookPoint
    OnHook(ctx context.Context, hctx *HookContext) error
}
```

可用的钩子点：

- `before_agent_run` / `after_agent_run`
- `before_tool_call` / `after_tool_call`
- `before_llm_call` / `after_llm_call`
- `before_memory_save` / `after_memory_load`

## 跨语言插件（共享库）

通过 purego 加载共享库，实现跨语言兼容。插件需要导出 C 兼容的函数接口：

```c
// 必需导出
void*     plugin_new()                              // 创建实例
char*     plugin_name(void* ptr)                    // 获取名称
char*     plugin_version(void* ptr)                 // 获取版本
char*     plugin_description(void* ptr)             // 获取描述

// 可选导出
void      plugin_init(void* ptr, const char* config_json)  // 初始化
void      plugin_start(void* ptr)                          // 启动
void      plugin_stop(void* ptr)                           // 停止
void      plugin_close(void* ptr)                          // 关闭
char*     plugin_tools(void* ptr)                          // 获取工具定义 (JSON)
char*     plugin_execute(void* ptr, const char* tool_name, const char* args_json)  // 执行工具
```

### Go 共享库插件示例

```go
package main

import (
    "C"
    "encoding/json"
    "unsafe"
)

//export plugin_new
func plugin_new() uintptr {
    return uintptr(unsafe.Pointer(NewPlugin()))
}

//export plugin_tools
func plugin_tools(ptr unsafe.Pointer) *C.char {
    tools := []ToolDef{...}
    data, _ := json.Marshal(tools)
    return C.CString(string(data))
}

//export plugin_execute
func plugin_execute(ptr unsafe.Pointer, toolName, argsJSON *C.char) *C.char {
    result := executeTool(C.GoString(toolName), C.GoString(argsJSON))
    return C.CString(result)
}

func main() {}
```

编译：`go build -buildmode=c-shared -o myplugin.so myplugin.go`

### Rust 共享库插件示例

```rust
use std::ffi::{CStr, CString};
use std::os::raw::c_char;

#[no_mangle]
pub extern "C" fn plugin_new() -> *mut () {
    // 创建实例
}

#[no_mangle]
pub extern "C" fn plugin_tools(_ptr: *mut ()) -> *mut c_char {
    let tools = serde_json::to_string(&tools).unwrap();
    CString::new(tools).unwrap().into_raw()
}

#[no_mangle]
pub extern "C" fn plugin_execute(_ptr: *mut (), tool: *const c_char, args: *const c_char) -> *mut c_char {
    // 执行工具
}
```

编译：`cargo build --release --crate-type cdylib`

## 依赖管理

在 plugin.yaml 中声明依赖：

```yaml
dependencies:
  - id: core-utils
    version: ">=1.0.0"
  - id: logger
    version: "^2.0.0"
```

依赖解析器会自动计算加载顺序，确保依赖先于依赖者加载。

## 目录结构

```
plugins/
├── tools/
│   ├── echo/                    # Go 示例插件
│   │   ├── plugin.yaml
│   │   └── echo.go
│   └── weather_rust/            # Rust 示例插件
│       ├── plugin.yaml
│       ├── Cargo.toml
│       └── src/
│           └── lib.rs
├── llm/
│   └── openai/
│       ├── plugin.yaml
│       └── openai.go
└── hooks/
    └── audit/
        ├── plugin.yaml
        └── audit.go
```

## 运行时支持

| 运行时 | 说明 | 隔离级别 | 跨语言 |
|--------|------|----------|--------|
| `go` | Go 原生插件 / 共享库 | 进程内 | ✅ |
| `binary` | 外部可执行文件 (JSON-RPC) | 进程隔离 | ✅ |
| `http` | 远程 HTTP 插件 | 网络隔离 | ✅ |
| `config` | 配置驱动插件 (YAML/JSON) | 进程内 | - |
| `wasm` | WebAssembly (规划中) | 沙箱隔离 | ✅ |

## API 参考

### Plugin 接口

```go
type Plugin interface {
    Info() Info
    Init(ctx context.Context, cfg Config) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Close() error
}
```

### ManagerConfig 配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `PluginDirs` | `[]string` | `["./plugins"]` | 插件目录 |
| `AutoLoad` | `bool` | `true` | 自动加载 |
| `AutoStart` | `bool` | `false` | 自动启动 |
| `LoadTimeout` | `Duration` | `30s` | 加载超时 |
| `StartTimeout` | `Duration` | `10s` | 启动超时 |
| `MaxConcurrent` | `int` | `10` | 并行操作限制 |
| `Strict` | `bool` | `false` | 严格模式 |

### Manager 方法

| 方法 | 说明 |
|------|------|
| `Load(ctx)` | 发现并加载所有插件 |
| `LoadPlugin(ctx, manifest)` | 加载单个插件 |
| `Start(ctx, id)` | 启动插件 |
| `StartAll(ctx)` | **并行**启动所有插件 |
| `StopAll(ctx)` | **并行**停止所有插件 |
| `Get(id)` | 获取插件实例 |
| `GetTools(ctx)` | 获取所有工具 (`[]models.Tool`) |
| `ExecuteHook(ctx, point, hctx)` | 执行钩子 (**带超时保护**) |

## 最佳实践

1. **单一职责**：每个插件专注于一个功能领域
2. **显式依赖**：声明所有依赖，避免隐式耦合
3. **版本约束**：使用语义版本约束依赖
4. **超时处理**：插件操作应尊重 context 超时
5. **错误处理**：优雅处理错误，提供有用的错误信息
6. **资源清理**：在 `Close()` 中释放所有资源
7. **JSON 接口**：跨语言插件使用 JSON 字符串传递配置和参数
8. **文档完善**：提供清晰的 plugin.yaml 和 README

## 示例

- Go 共享库插件：`plugins/tools/echo/echo.go`
- Rust 共享库插件：`plugins/tools/weather_rust/src/lib.rs`
