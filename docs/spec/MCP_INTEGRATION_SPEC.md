# Spec: MCP Server 配置加载（阶段 1）

## 背景与动机

deepai 已具备接入 MCP 生态的全部底层零件，但它们尚未接入运行时：

- `pkg/mcp/client.go` 是完整的 MCP → `models.Tool` 适配器（支持 stdio / SSE / HTTP），但 `mcp.Connect*` 在整个 `pkg/`、`cmd/` 里从未被调用。
- `pkg/commands/chat.go` 的工具注册表目前只装入 builtin 工具 + skill，MCP 完全在链路之外。

目标不是引入"Claude 插件"概念，而是先落地 **MCP 配置加载**——这是让 deepai 复用整个 MCP 工具生态（filesystem / github / slack / postgres / puppeteer 等）、无需自研工具的最小、最高性价比的一步。后续阶段（解析 Claude `plugin.json` 清单、加载插件内的 skills/commands/agents/hooks）建立在本阶段之上。

## 目标 / 非目标

**目标**

- deepai 启动时从配置文件发现 MCP server，连接、列出工具、注入工具注册表。
- 复用 Claude Code / Cursor 的 `.mcp.json` 配置格式，用户可零摩擦共用已有配置。
- 单 server 失败不影响其余 server 与整体 session。

**非目标（留给后续阶段）**

- 解析 Claude `plugin.json` 清单。
- 加载插件内的 skills / commands / agents / hooks。
- marketplace 安装 / 更新机制。

## 配置格式

采用 Claude Code / Cursor 的 `.mcp.json` 同款 schema：

```json
{
  "mcpServers": {
    "github": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" }
    },
    "linear": {
      "type": "sse",
      "url": "https://mcp.linear.app/sse",
      "headers": { "Authorization": "Bearer ${LINEAR_TOKEN}" }
    },
    "remote": {
      "type": "http",
      "url": "https://example.com/mcp"
    }
  }
}
```

- `type` 省略时默认 `stdio`。支持 `stdio` / `sse` / `http`，分别对应已有的 `mcp.ConnectStdio` / `ConnectSSE` / `ConnectHTTP`。
- `env` 与 `headers` 中的 `${VAR}` 通过 `os.Getenv` 展开（兼容 Claude 行为）；未设置的变量展开为空串。

## 配置发现路径

按优先级合并，后者覆盖同名 server：

1. `~/.deepai/mcp.json` —— 全局
2. `<workdir>/.mcp.json` —— 项目级，生态兼容（与 Claude / Cursor 共用同一文件）

项目级选用 `.mcp.json`（而非 `.deepai/mcp.json`）是为了直接复用生态配置；全局位置沿用 deepai 自身的 `~/.deepai/` 约定。任一文件缺失即跳过，不报错。

阶段 1 **不**额外认 `<workdir>/.deepai/mcp.json`，避免项目级来源二义性。若将来要支持第三个路径，放后续阶段，届时需明确优先级与是否弃用。

## 生命周期

- **连接时机**：eager。在 `chat` 命令启动、工具注册表构建之后、agent 运行之前，一次性连接全部 server。
- **关闭时机**：session 结束时 `defer` 关闭所有 client。
- **理由**：MCP client 的 handler 闭包持有连接，必须在整个 session 存活；lazy on-first-call 会让首次工具调用承担不可预期的延迟与失败面，eager 更简单可控。

### 两个上下文必须分开（关键不变量）

底层 mcp-go 的 stdio 传输在 `Start(ctx)` 里做了两件绑定：把传入的 `ctx` **存为传输自身的请求处理上下文**（`c.ctx = ctx`，后续每个请求都会检查它），并用 `exec.CommandContext(ctx, ...)` **启动子进程**（`ctx` 取消即 SIGKILL 子进程）。SSE/HTTP 同理把监听生命周期绑到该 `ctx`。

因此 `Start` 接收的必须是**长期存活的 session 上下文**，绝不能是带超时的子 ctx——否则超时一到，子进程被杀、之后所有工具调用因 `ctx.Err()` 失败（"连上后立刻断开"）。

正确做法是把两类上下文显式分离：

- **session ctx**（长期，由 `chat.go` 传入，随 REPL 生命周期取消）→ 传给 `Start`。它被存储并用于 spawn 子进程，session 结束时取消可顺带清理子进程。
- **startup timeout ctx**（短期，仅作用于握手）→ 仅传给 `Initialize`。`Initialize` 是 per-call 的（底层 `SendRequest` 检查的是传入 ctx，而非存储的 ctx），所以握手超时不会影响后续调用。

边界由 `pkg/mcp/client.go` 的 `initializeClient` 内部强制：`Start(sessionCtx)` 后，用 `context.WithTimeout(sessionCtx, startTimeout)` 派生 initCtx 仅供 `Initialize` 使用。`mcp.Load` 与 `chat.go` 永远只传 session ctx，不得自行包裹超时。

## 失败处理

单 server 失败不致命：连接失败 / binary 不存在 / initialize 超时 / **tools/list 超时** → `slog.Warn` 记录并跳过，其余 server 正常加载。绝不因 MCP 问题 abort 整个 session。

启动阶段对每个 server 的"连接 + 列工具"都是可失败的有界操作：`Initialize` 由 `defaultHandshakeTimeout`（30s）限定，`tools/list` 由 `mcpListTimeout`（30s）限定。两者都是 per-request 调用（ctx 不被传输存储），可安全独立超时；`Start` 仍只用长期 session ctx。这样即便某个 server 在握手或列工具阶段挂死，也最多阻塞 30s 后被判失败，不会卡死整个 REPL 启动。

配置源隔离：单个配置文件不可读或 JSON 损坏 → `slog.Warn`（含路径）并跳过该文件，**继续处理其余来源**。损坏的 `~/.deepai/mcp.json` 不会让合法的 `<workdir>/.mcp.json` 一起失效。损坏文件的路径还会并入启动 report（如 `MCP: 0 loaded, config error in ~/.deepai/mcp.json`），所以即便没有任何 server 成功加载，REPL 也会显示这条提示，避免"看起来和没配置一样"的静默失效。

启动汇总（loaded / failed 列表）由 `mcp.Load` 拼成 report 字符串，经 `ReplConfig.MCPReport` 传给 REPL，在 banner 之后由 `ui.Info` 打印。例如：

```
MCP: 3 loaded (github, linear, fs), 1 failed (db: connect timeout)
```

## 工具命名与注册

复用 `pkg/mcp/client.go` 已有逻辑：

- 工具名 `"<server>.<tool>"`，如 `github.create_issue`。
- Groups `["mcp", "<server>"]`。
- 直接 `registry.Register(tool)` 进入现有注册表，与 builtin / skill 工具并列。

## 安全模型

MCP server 是用户自行安装的本地或远程进程，以用户权限运行——与 Claude Code 一致。它们**不进入** deepai 的沙箱（沙箱只约束 deepai 自身的 builtin 工具）。这是预期行为，会在 `pkg/mcp/README.md` 注明。本阶段不做额外沙箱化。

## 代码改动点

1. **改** `pkg/mcp/client.go`（执行上文「两个上下文」不变量）
   - 新增 `const defaultHandshakeTimeout = 30 * time.Second`。
   - `ConnectStdio` / `ConnectSSE` / `ConnectHTTP` **签名不变**；握手超时为包内常量，阶段 1 不做成可配置项，先把行为收敛。
   - `initializeClient(ctx, name, client)`：`client.Start(ctx)` 使用 session ctx（长期）；`client.Initialize(initCtx, ...)` 使用 `context.WithTimeout(ctx, defaultHandshakeTimeout)` 派生的 initCtx（仅握手）；握手失败执行 `client.Close()` 后返回错误。
2. **新增** `pkg/mcp/loader.go`
   - `type ServerConfig struct` —— `Type`、`Command`、`Args []string`、`Env`/`Headers map[string]string`、`URL`。
   - 新增 `const mcpListTimeout = 30 * time.Second`（启动期 tools/list 的独立超时）。
   - `Discover(workdir string) (servers map[string]ServerConfig, badFiles []string)` —— 读取两个路径并合并（后者覆盖同名 server）；单文件不可读/损坏 → `slog.Warn` 并跳过，其路径收集进 `badFiles` 供 report 展示，不返回错误。
   - `${VAR}` 展开辅助函数。
   - `Load(ctx context.Context, registry *tools.Registry, workdir string) (closers []func(), report string)` —— `ctx` 必须是 session ctx（**不得**在此包裹超时）；遍历 server，按 `type` 调对应 `Connect*(ctx, name, ...)`（Start 用 session ctx，握手超时由 client 内部常量控制）→ `client.Tools(listCtx)`（`listCtx` 由 `mcpListTimeout` 限定）→ `registry.Register`；server 失败与坏配置文件（`badFiles`）一并并入 report；成功收集 closer。`servers` 与 `badFiles` 均空时返回空 report（静默）。Load 本身不返回错误。返回 closers 供调用方 `defer`。
3. **改** `pkg/commands/chat.go`：在 `registerChatTools(...)` 之后、agent 运行之前调用 `mcp.Load(sessionCtx, registry, workDir, startTimeout)`，`defer` 关闭 closers；把返回的 `report` 字符串写入 `ReplConfig.MCPReport`（见下）。此层**没有 UI**，只产出 report，不打 `ui.Info`。
4. **改** `pkg/chat/repl.go`：`ReplConfig` 增加 `MCPReport string` 字段；在 `Run` 中 `r.ui.Banner(bannerInfo)` 之后通过 `ui.Info` 输出一次 MCP 汇总；仅当本次发现到至少一个 server 配置并执行过加载尝试时输出，完全未配置时不输出（即 `MCPReport == ""` 时不打印）。
5. **文档**：`pkg/mcp/README.md` 补充 `.mcp.json` 配置示例与发现规则。

### 关于 banner 工具数（无需改动）

banner 的 `Loaded` 行已通过 `bannerInfo()` 直接 `len(r.cfg.ToolRegistry.List())` 读取（`repl.go` / `banner.go`）。MCP 工具一旦注册进 registry，banner 自动反映，**不需要新改动点**。

## 测试计划

- 新增 `pkg/mcp/loader_test.go`：
  - 两个路径合并、后者覆盖同名 server。
  - **损坏的全局配置不致命**：坏文件 warn+skip，合法项目配置仍加载。
  - `${VAR}` 展开（含未设置变量）。
  - `type` 缺省时默认 `stdio`。
  - 坏 server 不影响好 server（用 fake server 进程或 stub）。
- `client.go` 上下文不变量测试：断言传给 `Start` 的是长期 session ctx（未因握手超时被取消），`Initialize` 用的是独立的超时子 ctx。可借助一个记录所收 ctx 的 fake transport，或验证握手超时后 `Tools()`/`CallTool` 仍可成功。
- 已有 `mcp_test.go` 覆盖 client 本身，不重复。

## 决策（已定）

1. **配置文件名**：阶段 1 只认 `~/.deepai/mcp.json` 与 `<workdir>/.mcp.json` 两个路径，不额外认 `<workdir>/.deepai/mcp.json`。避免项目级来源二义性，并保持与 Claude/Cursor 零摩擦复用。
2. **握手超时**：默认 `const defaultHandshakeTimeout = 30 * time.Second`。20s 对首轮 `npx`/冷启动偏紧，45s+ 又会放大 eager 启动的坏体验，30s 折中；且已限定为仅握手超时，不污染 session 生命周期。阶段 1 不做用户配置项。
3. **汇总展示时机**：只要本次发现到任意 server 配置并尝试过加载，就打印一行 report；若两个配置文件都不存在或 `mcpServers` 为空，则静默。比"仅失败时"更可观测，比"始终打印"更不吵。

## 后续阶段（仅备忘，不在本 spec 范围）

- 阶段 2：解析 Claude `.claude-plugin/plugin.json`，把其中的 `mcpServers` 复用本阶段的 `Load`，`skills/` 喂给已有的 `skill.Registry`（格式已兼容），`commands/` 接 slash 系统，`agents/` 接 subagent 池，`hooks` 接 `HookPlugin` 体系。
- 语义耦合提醒：skills / commands / agents / hooks 常假定 Claude Code 的工具面与生命周期，deepai 能"读"这些格式，但单个组件不保证可运行。纯 MCP server 插件无此问题。
