# llm 包

`llm` 包定义了与后端语言模型（LLM）交互的抽象接口与适配器，项目中使用 `LitellmProvider`（基于 github.com/voocel/litellm）封装 OpenAI 兼容的后端。

功能概览
- 提供统一的 `LLMProvider` 接口用于发起对话请求或流式响应。
- 定义跨提供者的请求/响应结构：`ChatRequest`、`ChatResponse`、`StreamChunk`、`Usage`。
 - 内置 `LitellmProvider`：将业务中通用的 `models.Message` 转换为 litellm 的消息格式并调用模型。

主要类型
- `LLMProvider`：接口，方法 `Chat(ctx, req)` 与 `Stream(ctx, req)`。
- `ChatRequest`：请求载荷，包含 `Model`、`Messages`、`Tools` 等字段。
- `ChatResponse`：规范化的响应，包含单条 `models.Message`、`Usage` 与停用原因（`Stop`）。
- `StreamChunk`：流式增量数据结构，支持部分文本、工具调用、最终消息与错误信息。

内置提供者
 - `LitellmProvider`（实现位于 [pkg/llm/eino.go](pkg/llm/eino.go#L1-L400)）
    - 通过 `NewLitellmProvider(name)` 创建，`name` 支持 `openai`、`siliconflow`、`anthropic` 等（字符串小写比较）。
    - 使用 `github.com/voocel/litellm` 作为后端客户端。

配置（环境变量）
- `DEFAULT_LLM_PROVIDER`：`NewProvider("")` 时的默认提供者（例如 `openai`）。
- `DEFAULT_LLM_MODEL`：默认模型名称（例如 `gpt-4.1-mini`）。
- `OPENAI_API_KEY`：OpenAI API Key（`openai` provider 必需）。
- `OPENAI_API_BASE_URL`：可选自定义 base URL（用于 OpenAI 兼容网关）。
- `SILICONFLOW_API_KEY`：SiliconFlow 的 API key（若使用 `siliconflow`）。
- `ANTHROPIC_API_KEY`：Anthropic 的 API key（若使用 `anthropic`，且需提供兼容网关地址）。

快速示例

```go
ctx := context.Background()
provider := llm.NewProvider("openai")

req := llm.ChatRequest{
    Model: "gpt-4.1-mini",
    Messages: []models.Message{{ID: "1", SessionID: "s1", Role: models.RoleHuman, Content: "Hello"}},
}

resp, err := provider.Chat(ctx, req)
if err != nil {
    // 处理错误
}
fmt.Println("reply:", resp.Message.Content)

// 流式示例
ch, err := provider.Stream(ctx, req)
if err != nil {
    // 处理错误
}
for chunk := range ch {
    if chunk.Err != nil {
        // 处理错误
        break
    }
    if chunk.Delta != "" {
        fmt.Print(chunk.Delta)
    }
}
```

测试
- 包含的测试文件：[pkg/llm/llm_test.go](pkg/llm/llm_test.go#L1-L200)。

扩展建议
- 若需支持更多后端，可实现新的 `LLMProvider`（例如直接调用 OpenAI 或 Anthropic 官方 SDK），并在 `registry.go` 中扩展 `NewProvider` 的分发逻辑。
 - 可在 `LitellmProvider` 中加入自定义中间件、度量采集和重试逻辑以提高可靠性。

如需，我可以：
- 添加更完整的示例程序（`cmd/llm-example`）并把其集成测试加入 `go test`；
- 或将 README 中的配置项补充为更详细的环境示例。
