# mcp 包

本包封装了对 mark3labs 的 MCP (Model Call Protocol) 客户端的简单适配，便于将远程工具以 `models.Tool` 的形式暴露给应用。

主要功能
- 使用 `ConnectStdio` 启动与外部 MCP 进程/可执行程序的 stdio 通信。
- 将远端工具列表转换为 `models.Tool` 列表，包含名称、描述、输入 schema 和处理器。
- 提供工具调用的标准化返回类型 `models.ToolResult`。

依赖
- github.com/mark3labs/mcp-go

快速开始

示例：通过 stdio 连接并列出远端工具

```go
ctx := context.Background()
client, err := mcp.ConnectStdio(ctx, "mytool", "/path/to/executable", nil, "--serve")
if err != nil {
    // 处理错误
}
defer client.Close()

tools, err := client.Tools(ctx)
if err != nil {
    // 处理错误
}

// 注册或使用 tools（每个 Tool 包含 Handler）
for _, t := range tools {
    fmt.Println("tool:", t.Name, t.Description)
}

// 调用第一个工具示例
call := models.ToolCall{
    ID:        "call-1",
    Name:      tools[0].Name,
    Arguments: map[string]any{"example": "value"},
    Status:    models.CallStatusPending,
}
res, err := tools[0].Handler(ctx, call)
if err != nil {
    // 处理错误，res 包含标准化的错误信息
}
fmt.Println("result:", res.Status, res.Content)
```

设计说明 / API 参考
- `ConnectStdio(ctx, name, command string, env []string, args ...string) (*Client, error)`
  - 通过给定命令启动一个 stdio transport 并初始化 MCP 客户端，返回 `Client`。
  - `name` 会作为工具名前缀（如果不为空），方便区分来自不同远端的工具。

- `(*Client) Close() error`
  - 关闭底层 MCP 客户端连接。

- `(*Client) Tools(ctx) ([]models.Tool, error)`
  - 向远端请求工具列表，并将每个远端工具包装为 `models.Tool`。
  - 每个 `models.Tool` 的 `Handler` 会在内部通过 MCP 的 `CallTool` 发起远端调用，返回 `models.ToolResult`。

实现细节
- 输入 schema：若远端返回原始的 `RawInputSchema`（JSON bytes），会被解析为 `map[string]any` 并放入 `Tool.InputSchema`，否则会尝试将 `InputSchema` 字段序列化后再解析。
- 错误处理：远端返回的错误会被序列化到 `models.ToolResult.Error`，并将 `Status` 置为 `failed`。

参考源码
- 客户端实现位于：[pkg/mcp/client.go](pkg/mcp/client.go#L1-L200)

如需扩展
- 可根据需要实现其它 transport（非 stdio）或在包装 `Tool` 时添加更多 `Groups`/元数据。

配置驱动的批量加载（loader.go）

`Discover(workdir)` 与 `Load(ctx, registry, workdir)` 从配置文件发现并连接一组 MCP server，把工具直接注册进 `*tools.Registry`。配置采用与 Claude Code / Cursor 相同的 `.mcp.json` 格式，可零摩擦复用已有配置：

```json
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" }
    },
    "linear": {
      "type": "sse",
      "url": "https://mcp.linear.app/sse",
      "headers": { "Authorization": "Bearer ${LINEAR_TOKEN}" }
    },
    "remote": { "type": "http", "url": "https://example.com/mcp" }
  }
}
```

发现路径（按优先级合并，后者覆盖同名 server）：
1. `~/.deepai/mcp.json` —— 全局
2. `<workdir>/.mcp.json` —— 项目级（与 Claude / Cursor 共用同一文件）

说明：
- `type` 省略时默认 `stdio`，支持 `stdio` / `sse` / `http`。
- `env` 与 `headers` 中的 `${VAR}` 会用 `os.Getenv` 展开；未设置的环境变量展开为空串。
- `Load` 接收的 ctx 必须是长期存活的 session ctx（会被绑定到各 server 子进程/监听器生命周期）；握手超时（`defaultHandshakeTimeout`，30s）由 client 内部独立控制，仅作用于 `Initialize`，不影响连接生命周期。
- MCP server 是用户自行安装的本地/远程进程，以用户权限运行，**不进入** deepai 沙箱。
- 单个 server 连接/握手/tools-list 失败只会记录 `slog.Warn` 并跳过，不会中断其余 server 或整个会话。
- 配置文件不可读或 JSON 损坏：记录 `slog.Warn` 并跳过该文件，其余来源照常加载；损坏文件路径还会并入启动 report（如 `MCP: 0 loaded, config error in ~/.deepai/mcp.json`），避免"静默失效"。

联系方式
- 有问题请在仓库中打开 issue 或联系维护者。
