# clarification 包

`clarification` 包提供 `ask_clarification` 工具的实现，用于在 Agent 运行时向用户请求澄清。

主要功能
- 提供 `AskClarificationToolWithMode` 将澄清请求作为 `models.Tool` 暴露给运行时
- 自主模式（autonomous）：短路返回“按最佳判断处理”的提示，不阻塞
- CLI 模式：当上下文附带 `tools.UserInteraction` 时，同步通过 stdin 向用户提问
- 非交互模式（既非自主、也无 UI）：返回“无可用用户交互，按最佳判断处理”的回退提示

快速开始

```go
tool := clarification.AskClarificationToolWithMode(autonomous)
// 将 tool 注册到工具集合中，运行时会在需要时调用 tool.Handler
```

测试
- 参见 [pkg/clarification/tool_test.go](pkg/clarification/tool_test.go)。

历史说明
- 早期版本包含一个基于内存 `Manager` 的异步澄清生命周期管理器和配套 HTTP API（`NewManager`/`Manager`/`NewAPI`/`API`），供尚不存在的 HTTP 网关使用。由于零调用方（CLI 始终以 `nil` manager 调用工具），该分支已作为死代码删除；工具本身只保留 autonomous / UserInteraction / 非交互回退三条实际使用的路径。
