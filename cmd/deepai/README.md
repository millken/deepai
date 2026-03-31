# cmd/deepai

`cmd/deepai` 是一个端到端示例入口，用来演示现有能力如何组合起来运行：

- `pkg/agent` 负责 ReAct 风格的主循环
- `pkg/tools` 负责注册并执行工具
- `pkg/subagent` 负责把长任务拆成子代理任务并回传事件
- `pkg/sandbox` 为工具执行提供隔离目录

这个示例的目标是展示“工具调用 + 子代理调用 + 事件输出”的完整链路，而不是接入真实模型服务。

## 演示内容

示例会做三件事：

1. 先直接调用 `bash` 工具，打印一段命令输出。
2. 再通过 `task` 工具启动一个子代理任务，并显示任务事件。
3. 最后由 `agent.Agent` 完成一轮工具调用并输出最终结果。

## 运行方式

在仓库根目录执行：

```bash
go run ./cmd/deepai
```

## 输出说明

运行后你会看到几类输出：

- `[tool:bash]`：直接执行 `bash` 工具的结果
- `[subagent]`：子代理任务的生命周期事件
- `[event] tool call`：主 Agent 触发了工具调用
- `[event] tool result`：工具执行后的结果
- `[event] agent end`：Agent 结束时的最终输出

## 示例结构

`cmd/deepai/main.go` 中包含一个脚本化的 `LLMProvider`，用于模拟模型的 tool call 返回。

它的作用是让这个示例不依赖外部 API key，也能完整演示：

- Agent 如何接收模型输出
- Agent 如何调度 `task` 子代理工具
- 子代理如何通过 `pkg/subagent` 产生事件和结果

## 如何替换成真实模型

如果你要把它改成真实接入，可以把 `scriptedProvider` 替换成 `pkg/llm` 里的真实 provider：

- 配置 `DEFAULT_LLM_PROVIDER`
- 提供对应的 API key，例如 `OPENAI_API_KEY`
- 维持 `pkg/tools` 和 `pkg/subagent` 的注册方式不变

## 相关文件

- [cmd/deepai/main.go](cmd/deepai/main.go)
- [pkg/agent/react.go](pkg/agent/react.go)
- [pkg/tools/subagent.go](pkg/tools/subagent.go)
- [pkg/tools/builtin/bash.go](pkg/tools/builtin/bash.go)
- [pkg/subagent/README.md](../../pkg/subagent/README.md)
