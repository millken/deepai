# clarification 包

`clarification` 包提供一个用于请求用户澄清（clarification）的轻量管理器与 HTTP API，以及将澄清请求作为可调用工具暴露的辅助函数。

主要功能
- 管理澄清请求生命周期（创建、查询、解析、列举）
- 支持基于线程的隔离（ThreadID）和事件回调（EventSink）
- 提供 `AskClarificationTool` 将澄清请求作为 `models.Tool` 暴露给运行时
- 提供基于 `Manager` 的简单 HTTP API 处理函数

核心类型
- `Clarification`：表示单个澄清请求（见 [pkg/clarification/types.go](pkg/clarification/types.go#L1-L200)）。
- `Manager`：内存管理器，负责保存、发布和解析澄清（见 [pkg/clarification/manager.go](pkg/clarification/manager.go#L1-L200)）。

快速开始

创建管理器并发出澄清请求：

```go
mgr := clarification.NewManager(0) // 使用默认缓冲
ctx := clarification.WithThreadID(context.Background(), "thread-1")

req := clarification.ClarificationRequest{
    Question: "Which color do you prefer?",
    Options: []clarification.ClarificationOption{{Label: "Red", Value: "red"}, {Label: "Blue", Value: "blue"}},
    Required: true,
}
item, err := mgr.Request(ctx, req)
if err != nil {
    // 处理错误
}
fmt.Println("created clarification id:", item.ID)
```

通过 `AskClarificationTool` 将澄清请求作为工具暴露（适用于工具运行时注册）：

```go
tool := clarification.AskClarificationTool(mgr)
// 将 tool 注册到你的工具集合中，运行时会在需要时调用 tool.Handler
```

使用 HTTP API

创建 API 实例并挂载到路由：

```go
api := clarification.NewAPI(mgr)
// POST /threads/{thread}/clarifications -> api.HandleCreate
// GET  /threads/{thread}/clarifications/{id} -> api.HandleGet
// POST /threads/{thread}/clarifications/{id}/resolve -> api.HandleResolve
```

常用方法说明（API 参考）
- `NewManager(buffer int) *Manager`：创建 `Manager`，`buffer` 为 Pending channel 缓冲大小。
- `(*Manager) Request(ctx context.Context, req ClarificationRequest) (*Clarification, error)`：创建并发布一个澄清请求。
- `(*Manager) Resolve(id, answer string) error`：解析指定 `id` 的澄清并记录答案。
- `(*Manager) Get(id string) (*Clarification, bool)`：按 id 获取澄清副本。
- `(*Manager) ListByThread(threadID string) []Clarification`：按线程列出澄清。
- `(*Manager) Pending() <-chan *Clarification`：返回待处理澄清的发布通道。

辅助函数
- `WithThreadID(ctx, threadID)` / `ThreadIDFromContext(ctx)`：在上下文中设置/读取线程 ID。
- `WithEventSink(ctx, sink)` / `EmitEvent(ctx, item)`：设置事件回调并触发事件。

实现细节
- `Manager` 在内存中保存澄清，使用 `pendingCh` 异步发布新创建的项，调用 `EmitEvent` 触发可能的监听器。
- `AskClarificationTool` 会将请求解析并通过 `Manager.Request` 创建澄清，然后返回包含 `id` 与元信息的 `models.ToolResult`。

测试
- 包含单元测试：参见 [pkg/clarification/manager_test.go](pkg/clarification/manager_test.go#L1-L200) 和 [pkg/clarification/tool_test.go](pkg/clarification/tool_test.go#L1-L200)。

扩展建议
- 将 `Manager` 的持久化改为数据库或外部存储以支持重启恢复。
- 将 `Pending()` 通道的消费端与 WebSocket/消息队列连接，以便实时通知前端或代理。

如需帮助我可以：添加示例 CLI、完善 API 路由示例，或为 `AskClarificationTool` 写更多集成测试。
