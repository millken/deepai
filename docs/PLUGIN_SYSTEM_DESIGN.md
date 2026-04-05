# DeepAI 插件系统架构设计

> 版本：2.0
> 日期：2026-04-05

## 一、设计目标

### 1.1 核心需求

| 需求 | 说明 |
|------|------|
| **可扩展** | 无需修改核心代码即可添加新能力 |
| **进程隔离** | BinaryLoader 通过子进程提供崩溃隔离；SharedLibraryLoader 提供低调用开销但不提供崩溃隔离（dlopen 加载到宿主进程内） |
| **安全性** | 插件权限可控，防御性配置校验，ABI 版本门禁 |
| **跨语言** | 通过 C ABI 支持任意语言编写的插件 |
| **热加载** | 支持运行时加载/卸载/重载插件 |
| **精确取消** | per-call ID 取消，支持并发调用 |

### 1.2 插件类型

```
┌─────────────────────────────────────────────────────────────┐
│                      Plugin Types                            │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Tool Plugin  │  │ LLM Plugin   │  │Memory Plugin │      │
│  │ 工具定义+执行│  │ Provider+路由│  │ 存储后端     │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
│  ┌──────────────┐  ┌──────────────┐                         │
│  │ Hook Plugin  │  │ MCP Plugin   │                         │
│  │ 生命周期钩子 │  │ MCP 协议适配 │                         │
│  └──────────────┘  └──────────────┘                         │
└─────────────────────────────────────────────────────────────┘
```

目前 `Tool Plugin` 有完整实现，其他类型为接口预留。

---

## 二、架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                     Application Layer                         │
│  cmd/deepai / cmd/mcp-example / pkg/gateway                  │
├─────────────────────────────────────────────────────────────┤
│                     Plugin Manager                           │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐      │
│  │ Registry │ │  Loader  │ │ Resolver │ │ Monitor  │      │
│  │ 实例注册 │ │ 加载器链 │ │ 依赖拓扑 │ │ 事件监控 │      │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘      │
├─────────────────────────────────────────────────────────────┤
│                     Plugin Runtime                           │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          Shared Library Loader (purego)              │    │
│  │  ┌─────────┐ ┌─────────┐ ┌─────────┐               │    │
│  │  │  Go     │ │ Rust    │ │  C/C++  │   (.so/.dll)  │    │
│  │  │ c-shared│ │ cdylib  │ │ shared  │               │    │
│  │  └─────────┘ └─────────┘ └─────────┘               │    │
│  │  ⚠ 进程内加载，无崩溃隔离，仅限受信任插件          │    │
│  └─────────────────────────────────────────────────────┘    │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐              │
│  │   Binary   │ │   HTTP     │ │   Config   │              │
│  │ JSON-RPC   │ │ 远程调用   │ │ 声明式定义 │              │
│  │ 进程隔离   │ │ 网络隔离   │ │            │              │
│  └────────────┘ └────────────┘ └────────────┘              │
└─────────────────────────────────────────────────────────────┘
```

宿主侧通过 [purego](https://github.com/ebitengine/purego) 加载共享库，**无需 CGO**，避免了 Go 标准插件包的 GOPATH 限制。

---

## 三、核心接口

### 3.1 类型定义

源文件：`pkg/plugin/types.go`

```go
type PluginType string

const (
    PluginTypeTool   PluginType = "tool"
    PluginTypeLLM    PluginType = "llm"
    PluginTypeMemory PluginType = "memory"
    PluginTypeHook   PluginType = "hook"
    PluginTypeMCP    PluginType = "mcp"
)

type PluginState string

const (
    PluginStateUnloaded  PluginState = "unloaded"
    PluginStateLoaded    PluginState = "loaded"
    PluginStateStarting  PluginState = "starting"
    PluginStateRunning   PluginState = "running"
    PluginStateStopping  PluginState = "stopping"
    PluginStateFailed    PluginState = "failed"
    PluginStateDisabled  PluginState = "disabled"
)
```

### 3.2 接口层次

源文件：`pkg/plugin/plugin.go`

```go
// 基础生命周期接口 — 所有插件必须实现
type Plugin interface {
    Info() Info
    Init(ctx context.Context, cfg Config) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Close() error
}

// 工具插件 — 提供可被 Agent 调用的工具
type ToolPlugin interface {
    Plugin
    Tools(ctx context.Context) ([]models.Tool, error)
    Groups() []string
}

// 钩子插件 — 生命周期事件监听
type HookPlugin interface {
    Plugin
    Hooks() []HookPoint
    OnHook(ctx context.Context, hctx *HookContext) error
}

// LLM 提供者插件
type ProviderPlugin interface {
    Plugin
    ProviderType() string
    Models(ctx context.Context) ([]ModelInfo, error)
}

// 记忆存储插件
type MemoryPlugin interface {
    Plugin
    StorageType() string
    Capabilities() []string
}
```

每个接口都有对应的 `Base*` 默认实现（`BasePlugin`、`BaseToolPlugin`、`BaseHookPlugin`），简化插件开发。

### 3.3 配置传递

源文件：`pkg/plugin/types.go`

```go
type Config struct {
    ID       string            `json:"id" yaml:"id"`
    Enabled  bool              `json:"enabled" yaml:"enabled"`
    Priority int               `json:"priority,omitempty" yaml:"priority,omitempty"`
    Settings map[string]any    `json:"settings,omitempty" yaml:"settings,omitempty"`
    Secrets  map[string]string `json:"-" yaml:"-"`
}
```

`Config` 实现了自定义 `UnmarshalYAML`，将 YAML 中未识别的顶层字段自动收集到 `Settings` map 中。这允许 `plugin.yaml` 使用扁平配置格式：

```yaml
config:
  default_backend: http      # ← 自动进入 Settings
  timeout: 30                 # ← 自动进入 Settings
  max_content_length: 1000000 # ← 自动进入 Settings
```

配置传递链路：

```
plugin.yaml → Manifest.Config → Manager.wrapper.config
    → Plugin.Init(ctx, config) → config.Settings → json.Marshal → plugin_init(JSON)
```

---

## 四、共享库 ABI 契约

这是插件系统最核心的设计。宿主通过 purego 加载 .so/.dll，调用导出的 C 函数。

源文件：`pkg/plugin/so_plugin.go`、`so_plugin_unix.go`、`so_plugin_windows.go`

### 4.1 符号表

**必选符号（6 个）：**

| 符号 | 签名 | 说明 |
|------|------|------|
| `plugin_new` | `uintptr plugin_new()` | 创建插件实例，返回不透明指针 |
| `plugin_name` | `char* plugin_name(uintptr ptr)` | 返回插件名称 |
| `plugin_version` | `char* plugin_version(uintptr ptr)` | 返回版本号 |
| `plugin_description` | `char* plugin_description(uintptr ptr)` | 返回描述 |
| `plugin_abi_version` | `char* plugin_abi_version()` | 返回 ABI 版本号（必须为 `"1.0"`）。返回的字符串必须动态分配，宿主校验后通过 `plugin_free_string` 释放 |
| `plugin_free_string` | `void plugin_free_string(char* s)` | 释放插件返回的 C 字符串内存。宿主在复制完所有返回值后调用 |

**可选符号（8 个）：**

| 符号 | 签名 | 说明 |
|------|------|------|
| `plugin_init` | `void plugin_init(uintptr ptr, const char* config_json)` | 接收 JSON 配置 |
| `plugin_start` | `void plugin_start(uintptr ptr)` | 启动插件 |
| `plugin_stop` | `void plugin_stop(uintptr ptr)` | 停止插件 |
| `plugin_close` | `void plugin_close(uintptr ptr)` | 释放资源 |
| `plugin_type` | `char* plugin_type(uintptr ptr)` | 返回插件类型字符串 |
| `plugin_tools` | `char* plugin_tools(uintptr ptr)` | 返回工具定义 JSON |
| `plugin_execute` | `char* plugin_execute(uintptr ptr, const char* tool_name, const char* args_json, uint64_t call_id)` | 执行工具 |
| `plugin_cancel` | `void plugin_cancel(uintptr ptr, uint64_t call_id)` | 取消指定调用 |

### 4.2 内存管理

插件通过 `C.CString`（Go）或 `CString::new().into_raw()`（Rust）分配返回值。宿主在复制完成后调用 `plugin_free_string` 释放。

```
插件分配 → 宿主 cStringToGoString 复制 → 宿主 freeCString 释放
```

`plugin_free_string` 是可选的——未导出时宿主跳过释放（内存泄漏但不崩溃）。

**CGO 注意事项**：Go c-shared 插件中，`//export` 与 C 标准库函数（如 `free`）在 cgo preamble 中存在符号解析冲突。解决方案是将 `plugin_free_string` 放在独立的 `.c` 文件中导出。

### 4.3 并发取消机制

宿主为每次 `CallTool` 调用生成单调递增的 `call_id`（`uint64`，atomic），传递给 `plugin_execute` 和 `plugin_cancel`。

```
宿主 CallTool(ctx, tool, args):
  1. callID = atomic.AddUint64(&counter, 1)
  2. 启动 goroutine 监听 ctx.Done()
  3. 若 ctx 取消 → purego.SyscallN(plugin_cancel, ptr, callID)
  4. 调用 plugin_execute(ptr, tool, args, callID)
  5. 返回结果

插件侧:
  execCalls map[uint64]context.CancelFunc
  plugin_execute: execCalls[callID] = cancel
  plugin_cancel: execCalls[callID]()  // 精确取消指定调用
```

### 4.4 宿主侧关键实现

`CallTool` 在发起 FFI 调用前释放读锁，避免长时间阻塞的 `plugin_execute` 阻塞 Init/Start/Stop/Close：

```go
func (p *SharedLibraryPlugin) CallTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
    // 持锁解析符号、快照指针
    p.mu.RLock()
    executeFunc, _ := dlsym(p.lib, "plugin_execute")
    pluginPtr := p.ptr
    cancelFunc := p.cancelFunc
    p.mu.RUnlock()  // 释放锁 — FFI 调用在无锁状态下执行

    // 生成 call_id，启动取消 goroutine，执行 FFI...
    callID := atomic.AddUint64(&p.callIDCounter, 1)
    // ...
}
```

`cStringToGoString` 使用动态长度测量 + `unsafe.Slice` 批量拷贝，带有 4MB 安全上限防止恶意指针越界扫描：

```go
const maxCStringLen = 4 * 1024 * 1024 // 4 MB safety bound

func (p *SharedLibraryPlugin) cStringToGoString(ptr uintptr) string {
    var length uintptr
    for ; length < maxCStringLen; length++ {
        if *(*byte)(unsafe.Pointer(ptr + length)) == 0 { break }
    }
    if length >= maxCStringLen {
        p.freeCString(ptr) // 释放后返回空
        return ""
    }
    result := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), length)
    goStr := string(result)
    p.freeCString(ptr)
    return goStr
}
```

---

## 五、插件管理器

源文件：`pkg/plugin/manager.go`

### 5.1 生命周期状态机

```
Unloaded → Loaded → Starting → Running → Stopping → Loaded → Unloaded
                      └──→ Failed (可重试)
```

### 5.2 并发安全设计

`LoadPlugin` 使用占位模式防止 TOCTOU 竞态——所有 map 写操作均在锁内完成：

```go
func (m *Manager) LoadPlugin(ctx context.Context, manifest *Manifest) error {
    m.mu.Lock()
    if w, exists := m.wrappers[manifest.ID]; exists {
        if w.state != PluginStateFailed {
            m.mu.Unlock()
            return fmt.Errorf("already loaded")
        }
        delete(m.wrappers, manifest.ID) // 清理失败条目（仍在锁内）
    }
    m.wrappers[manifest.ID] = &wrapper{state: PluginStateLoaded} // 占位
    m.mu.Unlock()

    // 无锁 I/O 加载...
    // 失败: m.mu.Lock(); delete(m.wrappers, manifest.ID); m.mu.Unlock()
    // 成功: m.mu.Lock(); m.wrappers[manifest.ID] = realWrapper; m.mu.Unlock()
}
```

`Start` 使用单一 `Lock` 保护整个检查-状态转换流程，避免 RLock→Lock 升级的竞态。

### 5.3 加载器链

源文件：`pkg/plugin/loader.go`

```
CompositeLoader
  ├── RegistryLoader      — 全局注册表（内置 Go 插件）
  ├── SharedLibraryLoader — .so/.dll (purego, 无 CGO)
  ├── BinaryLoader        — 外部进程 (JSON-RPC over stdin/stdout)
  ├── HTTPLoader          — 远程 HTTP (预留)
  └── ConfigLoader        — 声明式 YAML/JSON (预留)
```

每个加载器实现 `CanLoad(manifest)` + `Load(ctx, manifest)`。`CompositeLoader` 按优先级依次尝试。

### 5.4 依赖解析

源文件：`pkg/plugin/resolver.go`

使用 DFS 拓扑排序 + 环检测。支持 semver 版本约束校验。

`StartAll` 按依赖层级并行启动——同层插件使用 `errgroup` 并发启动，层间串行。

---

## 六、插件实现示例

### 6.1 目录结构

```
plugins/
├── tools/
│   ├── echo/                    # Go c-shared 示例
│   │   ├── echo.go
│   │   ├── echo_cgo.c           # plugin_free_string 的 C 实现
│   │   └── plugin.yaml
│   └── weather_rust/            # Rust cdylib 示例
│       ├── Cargo.toml
│       ├── src/lib.rs
│       └── plugin.yaml
└── web_fetch/                   # Go c-shared 生产插件
    ├── web_fetch.go
    ├── web_fetch_test.go
    └── plugin.yaml
```

### 6.2 Go c-shared 插件（echo）

```go
// plugins/tools/echo/echo.go
package main

import "C"
import (
    "encoding/json"
    "fmt"
    "unsafe"
)

type EchoPlugin struct { prefix string; maxLen int }

var plugins = make(map[uintptr]*EchoPlugin)

//export plugin_new
func plugin_new() uintptr { /* 创建实例，存入 map，返回指针 */ }

//export plugin_execute
func plugin_execute(ptr unsafe.Pointer, toolName *C.char, argsJSON *C.char, callID uint64) *C.char {
    // 解析参数 → 执行逻辑 → 返回 JSON 结果
    return C.CString(resultJSON)
}

//export plugin_cancel
func plugin_cancel(ptr unsafe.Pointer, callID uint64) { /* no-op */ }

func main() {}
```

```c
// plugins/tools/echo/echo_cgo.c
// plugin_free_string 必须在 C 文件中导出，避免 //export 与 C.free 的符号冲突
#include <stdlib.h>
void plugin_free_string(void *s) { free(s); }
```

构建：`go build -buildmode=c-shared -o echo.so .`

### 6.3 Rust cdylib 插件（weather）

```rust
// plugins/tools/weather_rust/src/lib.rs
use std::collections::HashMap;
use std::sync::Mutex;

struct WeatherPlugin { api_key: Option<String>, cache_ttl: u64 }
static PLUGINS: Mutex<HashMap<usize, Box<WeatherPlugin>>> = Mutex::new(HashMap::new());

#[no_mangle]
pub extern "C" fn plugin_new() -> *mut () {
    let plugin = Box::new(WeatherPlugin { api_key: None, cache_ttl: 300 });
    let raw = Box::into_raw(plugin) as *mut ();
    // 存入全局注册表...
    raw
}

#[no_mangle]
pub extern "C" fn plugin_execute(ptr: *mut (), tool_name: *const c_char,
                                  args_json: *const c_char, _call_id: u64) -> *mut c_char {
    // 通过 ptr 从 PLUGINS 中查找实例 → 执行逻辑 → 返回 JSON
}

#[no_mangle]
pub extern "C" fn plugin_free_string(s: *mut c_char) {
    if !s.is_null() { unsafe { let _ = CString::from_raw(s); } }
}

#[no_mangle]
pub extern "C" fn plugin_cancel(_ptr: *mut (), _call_id: u64) { /* no-op */ }
```

构建：`cargo build --release --crate-type cdylib`

### 6.4 plugin.yaml

```yaml
id: echo-tool
name: Echo Tool
version: 1.0.0
type: tool
runtime: go
main: echo.so

config:
  prefix: "[ECHO] "
  max_length: 1000
```

---

## 七、工具注册与执行

### 7.1 双注册表架构

```
pkg/plugin/registry.go  — 管理 Plugin 实例（加载/卸载/生命周期）
pkg/tools/registry.go   — 管理 Tool 定义和执行（注册/调用/验证）
```

`SharedLibraryPlugin.Tools()` 从 JSON 解析工具定义并注入 handler 闭包，使工具可以通过标准 `tools.Registry` 执行：

```go
// handler 闭包桥接：tools.Registry.Call → plugin_execute
tools[i].Handler = func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    resultJSON, err := p.CallTool(ctx, tool.Name, call.Arguments)
    // 检查 error envelope → 返回 ToolResult
}
```

`tools.Registry.Call` 检查 `ToolResult.Status == CallStatusFailed`，将失败状态转换为 error。

### 7.2 集成方式

应用启动时需要显式桥接两个注册表：

```go
pluginMgr := plugin.NewManager(cfg)
pluginMgr.Discover(ctx)
pluginMgr.Load(ctx)
pluginMgr.StartAll(ctx)

// 将插件工具注册到 tools.Registry
registry := tools.NewRegistry()
pluginTools, _ := pluginMgr.GetTools(ctx)
for _, tool := range pluginTools {
    registry.Register(tool)
}
```

---

## 八、兼容性前置条件与已知限制

### 8.1 兼容性前置条件

**ABI 版本门禁（上线阻断项）：** 宿主在 `LoadSharedLibrary` 中强制要求 `plugin_abi_version` 必选符号，且返回值必须与 `CurrentABI`（当前为 `"1.0"`）完全匹配。版本不一致的共享库会被拒绝加载，返回明确的错误信息。**没有版本协商就无法承诺平滑升级。** 当 ABI 签名发生变更时（如新增参数、改变返回格式），必须同时递增 `CurrentABI` 并重新编译所有插件。

### 8.2 威胁模型

| 运行时 | 崩溃隔离 | 适用场景 |
|--------|---------|---------|
| **SharedLibraryLoader** | 无。dlopen 加载到宿主进程，段错误/堆破坏会杀死主进程 | 受信任的第一方插件（团队自研、审计过源码） |
| **BinaryLoader** | 有。子进程崩溃不影响宿主 | 第三方插件、不可信代码 |

**结论：** SharedLibraryLoader 定位为"受信任插件通道"。需要崩溃隔离的第三方插件应通过 BinaryLoader（JSON-RPC over stdin/stdout）运行。

### 8.3 已知限制

| 项目 | 说明 |
|------|------|
| **集成层缺失** | 主应用入口（cmd/deepai、cmd/mcp-example）尚未接入 plugin.Manager，工具需要手动注册 |
| **Secrets 传递** | 共享库插件的 `plugin_init` 只接收 Settings，Secrets 无传递路径 |
| **HTTP/Config Loader** | HTTPLoader 和 ConfigLoader 的工具执行逻辑尚未实现 |

### 8.2 已修复问题记录

| 修复 | 说明 |
|------|------|
| 配置传递断裂 | `Config.UnmarshalYAML` 收集未知字段到 Settings |
| Handler 缺失 | `Tools()` 为无 Handler 的工具注入闭包 |
| C 字符串泄漏 | 新增 `plugin_free_string` ABI 符号 |
| 取消语义错误 | `plugin_cancel` 引入 `call_id` 实现精确取消 |
| 64KB→4MB 安全上限 | `cStringToGoString` 加 4MB 边界防止恶意指针越界 |
| FFI 持锁阻塞 | `CallTool` 释放锁后执行 FFI |
| NULL 返回语义 | `plugin_execute` 返回 NULL 视为错误 |
| TOCTOU 竞态 | `LoadPlugin` 占位模式 + failed 条件分支在锁内 delete |
| 锁升级竞态 | `Start` 使用单一 Lock |
| ABI 版本门禁 | `plugin_abi_version` 必选符号 + `CurrentABI` 硬性校验 |
| Rust 数据竞争 | `static mut` → `Mutex<HashMap>` 实例注册表 |
| 资源泄漏 | `plugin_close` 清理 execCalls |
| 错误页面透传 | HTTP 4xx/5xx 返回错误而非内容 |
| 无界内存读取 | `io.LimitReader` 限制响应体大小 |

---

*文档版本: 2.0*
*最后更新: 2026-04-05*
