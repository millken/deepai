# proxy 包

`proxy` 包提供一个透明的 API 反向代理服务，用于转发 OpenAI / Anthropic 请求，并将完整的请求-响应过程以**事件流**形式记录到日志存储中，便于后续审计和工作流分析。

## 功能概览

- **反向代理透传**：客户端发送标准 OpenAI/Anthropic API 请求，代理原样转发到上游，客户端无感知
- **事件流日志**：每个请求拆分为多条小事件（`start` → `req_body` → `delta`... → `usage` → `done`），每行 < 1KB，IDE 友好
- **流式文本提取**：SSE 流内容解析为可读文本（`delta` 事件），不再是原始 SSE JSON
- **分离式 Body 存储**：大 body（> 4KB）自动写入 `bodies/` 目录，主日志文件保持轻量
- **可扩展存储**：通过 `EventStore` 接口抽象，内置 `MemoryEventStore` 和 `FileEventStore`

## 文件结构

```
pkg/proxy/
  proxy.go         -- Proxy 核心结构、Config、路由、中间件、生命周期
  handler.go       -- handleProxy 请求处理、事件构造、API 格式检测、认证注入
  streaming.go     -- streamingRecorder 双写、SSE 文本提取（OpenAI/Anthropic）
  logstore.go      -- EventStore 接口、LogEvent、RequestSummary、RawBody 类型
  memory_store.go  -- 内存实现 + updateSummaryFromEvent 公共函数
  file_store.go    -- JSONL 事件流 + bodies/ 分离存储
  proxy_test.go    -- 单元测试和集成测试
```

## 事件流格式

每个代理请求产生多条 `LogEvent`，按时间顺序写入 JSONL 文件：

### 非流式请求

```
{"ts":"10:00:00.000","type":"start","id":"1-abc","rid":"1-abc","method":"POST","path":"/v1/chat/completions","model":"gpt-4o","format":"openai","stream":false,"upstream":"https://api.openai.com/v1"}
{"ts":"10:00:00.001","type":"req_body","id":"1-abc","rid":"1-abc","body":{"model":"gpt-4o","messages":[...]},"size":100}
{"ts":"10:00:01.000","type":"resp_body","id":"1-abc","rid":"1-abc","body":{"id":"chatcmpl-1","choices":[...]},"size":800}
{"ts":"10:00:01.001","type":"done","id":"1-abc","rid":"1-abc","status":200,"dur":"1s"}
```

### 流式请求

```
{"ts":"10:00:00.000","type":"start","id":"2-def","rid":"2-def","method":"POST","path":"/v1/chat/completions","model":"gpt-4o","format":"openai","stream":true,"upstream":"..."}
{"ts":"10:00:00.001","type":"req_body","id":"2-def","rid":"2-def","body":{"model":"gpt-4o","stream":true,"messages":[...]},"size":150}
{"ts":"10:00:00.200","type":"delta","id":"2-def","rid":"2-def","text":"Hello"}
{"ts":"10:00:00.500","type":"delta","id":"2-def","rid":"2-def","text":"! How can I help"}
{"ts":"10:00:00.800","type":"delta","id":"2-def","rid":"2-def","text":" you today?"}
{"ts":"10:00:01.000","type":"usage","id":"2-def","rid":"2-def","input":10,"output":8,"total":18}
{"ts":"10:00:01.001","type":"done","id":"2-def","rid":"2-def","status":200,"dur":"1s","ttfb":"200ms","chunks":3}
```

### 大 body 分离存储

当 body 超过 4KB 阈值时，自动写入 `bodies/` 目录：

```
{"ts":"...","type":"req_body","id":"3-ghi","body_file":"bodies/3-ghi.1.req.json","size":5000000}
```

## 事件类型

| 类型 | 说明 | 关键字段 |
|------|------|----------|
| `start` | 请求到达 | method, path, model, format, stream, upstream |
| `req_body` | 请求体 | body, size, body_file, truncated |
| `delta` | 流式文本片段 | text |
| `usage` | Token 用量 | input, output, total |
| `resp_body` | 非流式响应体 | body, size, body_file, truncated |
| `done` | 请求完成 | status, dur, ttfb, chunks, error |

## 主要类型

### Config

```go
type UpstreamConfig struct {
    BaseURL string  // 上游 API 地址
    APIKey  string  // 认证密钥
}

type Config struct {
    Addr            string          // 监听地址（默认 :9090）
    OpenAI          UpstreamConfig  // OpenAI 上游配置
    Anthropic       UpstreamConfig  // Anthropic 上游配置
    Logger          *log.Logger
    ShutdownTimeout time.Duration   // 默认 15s
    MaxRequestBody  int64           // 默认 10MB
}
```

### EventStore 接口

```go
type EventStore interface {
    Append(ctx context.Context, events ...LogEvent) error
    ListRequests(ctx context.Context, offset, limit int) ([]RequestSummary, error)
    GetTimeline(ctx context.Context, id string) ([]LogEvent, error)
    Close() error
}
```

- `Append`：批量追加事件，由后台 logWorker 异步调用
- `ListRequests`：返回请求摘要列表（轻量，不含 body）
- `GetTimeline`：返回某个请求的完整事件链

### RequestSummary

从 `start` + `usage` + `done` 事件自动重建的轻量汇总：

```go
type RequestSummary struct {
    ID, RequestID, Method, Path, Model, APIFormat string
    Streaming                                     bool
    StatusCode                                    int
    Duration                                      string
    InputTokens, OutputTokens                     int
    Error                                         string
    CreatedAt                                     time.Time
}
```

## 路由

| 方法 | 路径 | 上游 |
|------|------|------|
| `GET` | `/health` | 健康检查 |
| `POST` | `/v1/chat/completions` | OpenAI |
| `POST` | `/v1/messages` | Anthropic |

## 快速使用

### 嵌入到其他服务

```go
p, _ := proxy.NewProxy(proxy.Config{
    OpenAI:   proxy.UpstreamConfig{BaseURL: "https://api.openai.com/v1", APIKey: "sk-xxx"},
    Anthropic: proxy.UpstreamConfig{BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-xxx"},
})

// 方式一：独立启动
p.ListenAndServe()

// 方式二：嵌入到现有 HTTP 服务
mux.Handle("/proxy/", http.StripPrefix("/proxy", p.Handler()))
```

### 使用 FileEventStore 持久化

```go
store, _ := proxy.NewFileEventStore(proxy.FileEventStoreConfig{
    Path:              "/var/log/proxy.jsonl",
    MaxInlineBodySize: 4096, // 默认 4KB，超过则写入 bodies/ 目录
})
defer store.Close()
p.WithStore(store)
```

### 自定义 EventStore

```go
type MyStore struct { /* ... */ }

func (s *MyStore) Append(ctx context.Context, events ...proxy.LogEvent) error { /* 写入数据库 */ }
func (s *MyStore) ListRequests(ctx context.Context, offset, limit int) ([]proxy.RequestSummary, error) { /* ... */ }
func (s *MyStore) GetTimeline(ctx context.Context, id string) ([]proxy.LogEvent, error) { /* ... */ }
func (s *MyStore) Close() error { /* ... */ }

p.WithStore(&MyStore{})
```

## 存储实现

### MemoryEventStore

内存存储，使用 `sync.RWMutex` + 事件切片 + ID 索引。进程退出后数据丢失，适合测试和短期使用。

### FileEventStore

JSONL 事件流存储，每个事件一行 JSON 追加写入：
- 重启后自动从文件重建内存索引（`summaries` + `byID`）
- 大 body 自动分离到 `bodies/` 目录，文件名带序号防覆盖（`{id}.{seq}.{type}.json`）
- `ListRequests` / `GetTimeline` 从内存索引查询，无需重读文件

## 异步写入与关闭安全

代理通过带缓冲的 channel 异步写入事件，不阻塞请求转发：
- `emitEvents` 非阻塞发送到 channel，满了丢弃并记录警告
- `logWorker` 使用 `context.Background()` 持久化，确保关闭期间已入队的事件必然落盘
- `Shutdown` 流程：停止接收新事件 → 关闭 HTTP → 关闭 channel → 等待 drain 完成

## SSE 解析

流式响应结束后，从缓冲数据中提取文本内容和 token 用量：
- **OpenAI**：提取 `choices[0].delta.content` 和 `usage` 字段
- **Anthropic**：提取 `content_block_delta.delta.text` 和 `usage.input_tokens/output_tokens`
- API 格式从请求路径确定（`/v1/messages` → anthropic，其余 → openai），不盲猜
- Scanner buffer 1MB，支持大型工具调用等长行场景

## 截断处理

当请求/响应体超过 `MaxRequestBody`（默认 10MB）时：
- `LimitReader` 静默截断（代理继续转发，不影响客户端）
- `req_body` / `resp_body` 事件的 `truncated` 字段标记为 `true`

## 测试

```bash
go test -v -race ./pkg/proxy/...
```
