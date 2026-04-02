# proxy - API 反向代理服务

独立的 OpenAI / Anthropic API 反向代理服务，转发请求到上游 API 并以**事件流**格式记录完整的请求-响应过程，用于审计和工作流分析。

## 快速开始

```bash
# 构建
make build-proxy

# 运行（至少配置一个 API Key）
OPENAI_API_KEY=sk-xxx ./bin/proxy

# 指定日志文件持久化
PROXY_LOG_FILE=./proxy.jsonl ./bin/proxy
```

## 命令行选项

```
Usage of proxy:
  -addr string             监听地址（默认 :9090）
  -openai-url string       OpenAI 上游地址（默认 https://api.openai.com/v1）
  -openai-key string       OpenAI API Key
  -anthropic-url string    Anthropic 上游地址（默认 https://api.anthropic.com）
  -anthropic-key string    Anthropic API Key
  -log-file string         日志文件路径（JSONL 事件流格式，为空使用内存存储）
  -max-inline-body int     内联 body 大小阈值（默认 4096 字节，超过则写入 bodies/ 目录）
```

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PROXY_ADDR` | `:9090` | 监听地址（命令行 `-addr` 优先） |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI 上游地址 |
| `OPENAI_API_KEY` | - | OpenAI API Key |
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Anthropic 上游地址 |
| `ANTHROPIC_API_KEY` | - | Anthropic API Key |
| `PROXY_LOG_FILE` | - | 日志文件路径（JSONL 事件流），为空则仅存内存 |
| `PROXY_MAX_INLINE_BODY` | `4096` | 内联 body 阈值（字节） |

环境变量作为默认值，同名命令行选项优先级更高。

## 使用示例

启动代理后，将原本发往 `https://api.openai.com` 或 `https://api.anthropic.com` 的请求改为发往代理地址即可：

```bash
# OpenAI 格式 — 非流式
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "hello"}]
  }'

# OpenAI 格式 — 流式
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "stream": true,
    "messages": [{"role": "user", "content": "hello"}]
  }'

# Anthropic 格式
curl http://localhost:9090/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "messages": [{"role": "user", "content": "hello"}],
    "max_tokens": 1024
  }'
```

代理会自动检测请求格式（基于 URL 路径），注入正确的认证头后转发到对应上游。

## 配合已有 SDK 使用

将 SDK 的 `base_url` 指向代理地址即可，无需修改其他代码：

```python
# Python OpenAI SDK
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:9090/v1",
    api_key="unused"  # 代理会注入真实 key
)
```

```javascript
// Node.js Anthropic SDK
import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  baseURL: "http://localhost:9090",
  apiKey: "unused",
});
```

## 日志格式

日志以 **JSONL 事件流**格式写入，每个请求拆为多条小事件，每行通常 < 1KB：

```jsonl
{"ts":"10:00:00.000","type":"start","id":"1-abc","rid":"1-abc","method":"POST","path":"/v1/chat/completions","model":"gpt-4o","format":"openai","stream":false,"upstream":"https://api.openai.com/v1"}
{"ts":"10:00:00.001","type":"req_body","id":"1-abc","rid":"1-abc","body":{"model":"gpt-4o","messages":[...]},"size":100}
{"ts":"10:00:01.000","type":"resp_body","id":"1-abc","rid":"1-abc","body":{"id":"chatcmpl-1","choices":[...]},"size":800}
{"ts":"10:00:01.001","type":"done","id":"1-abc","rid":"1-abc","status":200,"dur":"1s"}
```

流式请求会记录 `delta`（提取的文本内容）和 `usage`（token 用量）事件：

```jsonl
{"ts":"10:00:00.200","type":"delta","id":"2-def","rid":"2-def","text":"Hello"}
{"ts":"10:00:00.500","type":"delta","id":"2-def","rid":"2-def","text":"! How can I help"}
{"ts":"10:00:01.000","type":"usage","id":"2-def","rid":"2-def","input":10,"output":8,"total":18}
{"ts":"10:00:01.001","type":"done","id":"2-def","rid":"2-def","status":200,"dur":"1s","ttfb":"200ms","chunks":3}
```

### 大 body 分离存储

超过内联阈值（默认 4KB）的 body 自动写入 `bodies/` 目录，日志中保留文件引用：

```jsonl
{"ts":"...","type":"req_body","id":"3-ghi","body_file":"bodies/3-ghi.1.req.json","size":5000000}
```

### 截断标记

当请求/响应体超过 `MaxRequestBody`（默认 10MB）时被截断，事件中会标记 `truncated:true`。

## 优雅关闭

发送 `SIGINT`（Ctrl+C）或 `SIGTERM` 信号即可优雅关闭：
1. 停止接收新的日志事件
2. 等待进行中的 HTTP 请求完成
3. 关闭日志 channel，drain 所有已入队的事件
4. 退出

已入队的事件使用 `context.Background()` 写入，确保关闭期间不丢日志。
