# llm 包

`llm` 定义与后端语言模型交互的抽象接口，并自带两个手写的 HTTP provider——不依赖任何第三方 LLM SDK
（早前的 eino / litellm 封装已移除）。

## 结构

| 文件 | 职责 |
|---|---|
| `provider.go` | `LLMProvider` 接口与跨 provider 的请求/响应类型 |
| `anthropic.go` | Anthropic Messages API（原生请求格式与 SSE 事件流） |
| `openai_compat.go` | OpenAI 兼容格式，覆盖其余全部 provider |
| `http.go` | 共用的 HTTP 客户端、超时、重试 |
| `stream_trace.go` | `DEEPAI_STREAM_TRACE_FILE` 下把原始 SSE 落盘，用于排流式问题 |
| `registry.go` | provider 解析、`ModelRegistry`（多模型别名 → provider + model） |
| `unavailable.go` | 构造失败时返回的占位 provider：调用时才报原始错误，启动不 panic |

## 主要类型

- `LLMProvider`：`Chat(ctx, req)` 与 `Stream(ctx, req)`。
- `ChatRequest`：`Model` / `Messages` / `Tools` / `SystemPrompt` / `ReasoningEffort` /
  `Temperature` / `MaxTokens` / `ImageDetail`，以及可选的 `OnChunk` 回调。
- `ChatResponse`：规范化响应（单条 `models.Message` + `Usage` + `Stop`）。
- `StreamChunk`：流式增量。`Progress=true` 的心跳块**不携带任何负载**——它只用来证明长参数累积期间
  流还活着，避免 `pkg/agent` 的 stream idle watchdog 误杀；发送方必须把其余字段留零值。
- `ModelRegistry`：把 config.yaml 里的模型别名解析成 provider 实例，按 (provider, baseURL, key 摘要)
  缓存。

## Provider 与环境变量

`providerDefs` 是唯一的注册表；`kind` 决定走哪个实现。

| name | API key | base URL 覆盖 | kind |
|---|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` | `ANTHROPIC_BASE_URL` | anthropic |
| `openai` / `openai-compat` | `OPENAI_API_KEY` | `OPENAI_BASE_URL` | openai |
| `qwen` / `gemini` / `groq` / `ollama` / `glm` / `bedrock` / `deepseek` | `<NAME>_API_KEY` | — | openai |

API key 允许以 `pkg/secret` 的密封形式传入（来自 `.env`），`resolveConfig` 里统一 `secret.Reveal`；
对明文是 no-op。未知 provider 或缺 key 不会让启动失败，而是得到一个 `UnavailableProvider`，在真正
调用时才报错。

## 快速示例

```go
provider := llm.NewProvider("anthropic")

req := llm.ChatRequest{
    Model:        "claude-opus-5",
    SystemPrompt: "You are a helpful assistant.",
    Messages:     []models.Message{{ID: "1", SessionID: "s1", Role: models.RoleHuman, Content: "Hello"}},
}

resp, err := provider.Chat(ctx, req)
if err != nil { /* 处理错误 */ }
fmt.Println(resp.Message.Content)

ch, err := provider.Stream(ctx, req)
if err != nil { /* 处理错误 */ }
for chunk := range ch {
    switch {
    case chunk.Err != nil:
        // 处理错误并 break
    case chunk.Progress:
        // 心跳，无负载，忽略即可
    case chunk.Delta != "":
        fmt.Print(chunk.Delta)
    }
}
```
