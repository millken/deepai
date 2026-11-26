# DeepAI Bot 设计：常驻后台的 Agent 服务

`deepai-bot` 是和 `deepai`（REPL）并列的第二个可执行文件：把同一套 agent 运行时作为**常驻后台服务**对外提供，客户端（Web / 桌面 GUI / 手机 App / IM 机器人适配器）通过 HTTP、SSE、NDJSON 或 WebSocket 控制它。

目标是"类似 Grok bot"的使用形态：bot 常驻、会话常驻、任务不随客户端断开而中断；任何客户端都可以随时连上来看进度、回答它的提问、打断它或接着聊。

---

## 1. 总体架构

```
 ┌──────────── 客户端 ────────────┐
 │ 内置 Web 控制台 │ GUI │ App │ IM 适配器 │ curl/脚本 │
 └───────┬───────────────┬──────────────┬─────────┘
         │ WebSocket      │ REST + SSE    │ REST + NDJSON
         ▼                ▼               ▼
 ┌───────────────────────────────────────────────────┐
 │ pkg/bot.Server（传输层，薄适配）                    │
 │  鉴权(token) · Origin/Host 校验 · CORS · 内嵌控制台 │
 └───────────────────────┬───────────────────────────┘
                         ▼
 ┌───────────────────────────────────────────────────┐
 │ pkg/bot.Manager（会话与任务编排，传输无关）         │
 │  会话加载/创建/删除 · 每会话单 turn · 取消          │
 │  事件总线(带 seq + 回放缓冲) · ask_clarification 路由│
 │  会话锁(多 bot 进程互斥) · 空闲卸载                 │
 └──────────┬────────────────────────────┬───────────┘
            │ Runtime / Conversation      │ Store
            ▼                             ▼
 ┌──────────────────────────┐  ┌──────────────────────────┐
 │ pkg/bot.AgentRuntime      │  │ SQLite ~/.deepai/bot/bot.db │
 │  pkg/agent ReAct 引擎      │  │ 或 MemoryStore(--ephemeral)│
 │  工具/技能/插件/MCP/记忆   │  └──────────────────────────┘
 │  每会话沙箱 + SessionCarry │
 └──────────────────────────┘
```

| 层 | 位置 | 职责 |
|----|------|------|
| 入口 | `cmd/deepai-bot` | 参数、token、守护进程（`start/stop/status`）、优雅退出 |
| 装配 | `cmd/deepai-bot/runtime.go` | 从 `~/.deepai/config.yaml` 构建运行时，**与 REPL 同源**：模型注册表、工具、技能、Claude 插件、MCP、`DEEPAI.md`；会话与记忆在独立的 `bot.db`。`pkg/commands` 不为 bot 改动，所需的少量内部逻辑在此镜像 |
| 编排 | `pkg/bot/manager.go` | 会话生命周期、turn 执行、持久化、会话锁、提问路由 |
| 事件 | `pkg/bot/bus.go` | 每会话一条事件总线 + 一条全局生命周期总线 |
| 运行时 | `pkg/bot/agentruntime.go` | `pkg/agent` 适配：每 turn 新建 `agent.Agent`，事件翻译 |
| 传输 | `pkg/bot/server.go`、`ws.go` | REST / SSE / NDJSON / WebSocket |
| 客户端 | `pkg/bot/web/index.html` | 内嵌 Web 控制台，同时是 WS 协议的参考实现 |

`Manager` 只依赖两个接口：`Runtime`（agent 侧）和 `Store`（持久化），所以传输层和 agent 实现都可替换，测试用假 Runtime 即可覆盖全部协议。

---

## 2. 核心概念

- **Session（会话）**：一段对话，持久化在 bot 专用库 `~/.deepai/bot/bot.db`。与 CLI 的会话库分开：`deepai session list` 看不到 bot 会话，`deepai -r` 也不能续聊它们，反之亦然。同进程多 Bot（`bot_id`）见 [`BOT_MULTI_DESIGN.md`](BOT_MULTI_DESIGN.md)。
- **Turn（轮次）**：一次"用户消息 → agent 完成"。**每个会话同时只跑一个 turn**；并发发送返回 `409 busy`。turn 属于 bot 而不是请求：客户端断开不会取消它。
- **Event（事件）**：turn 期间发生的一切，统一信封，带会话内单调递增的 `seq`。
- **Question（提问）**：agent 调 `ask_clarification` 时，turn 挂起并广播 `question` 事件，任一客户端回答即恢复。未回答的问题出现在 `SessionInfo.pending`，晚连上的客户端也能看到。

### 2.1 事件信封

```json
{
  "seq": 12,
  "session_id": "20261107_031720_d298",
  "turn_id": "turn_be6167d5ea2b",
  "type": "text_delta",
  "time": "2026-11-07T03:17:20.61Z",
  "text": "..."
}
```

| type | 载荷字段 | 说明 |
|------|---------|------|
| `session_created` / `session_deleted` | `session` | 全局总线 |
| `turn_start` | `text`（用户消息） | 会话 + 全局总线 |
| `text_delta` | `text` | 模型流式输出 |
| `tool_call_start` / `tool_call_end` | `tool`（名称、参数、状态、`result_preview`、耗时） | 完整结果不进流，见 transcript |
| `subagent` | `subagent`（`task_started/running/completed/failed/cancelled`） | 子代理进度 |
| `progress` | — | 模型在思考，尚无可渲染输出（保活） |
| `compact` | `compact` | 上下文压缩统计 |
| `question` / `question_answered` | `question`（自动回答带 `auto: true`） | ask_clarification |
| `error` | `error` | 非致命错误（取消引起的错误不重复上报） |
| `turn_end` | `turn`（status: `completed/cancelled/timed_out/failed`、`cancel_source`、最终输出、usage、工具调用数）、`usage` | 一定会到达，且是该 turn 的最后一个事件 |
| `schedule_created` / `schedule_updated` / `schedule_deleted` | `schedule` | 全局总线（定时任务生命周期，见 `BOT_SCHEDULER_DESIGN.md`） |
| `schedule_run_started` / `schedule_run_updated` / `schedule_run_finished` | `run` | 全局总线（定时任务运行状态） |
| `gap` | — | seq=0，合成事件：请求的事件已被逐出回放缓冲或客户端落后被断开 → 重新拉 transcript 后从 `last_seq` 订阅 |
| `heartbeat` | — | seq=0，NDJSON/WS 保活（SSE 用注释行） |

类型集合只增不减，客户端必须忽略未知类型。

### 2.2 断线续传

每个会话的总线保留最近 4096 条事件。客户端记住最后一个 `seq`，重连时：

- SSE：浏览器 `EventSource` 自动带 `Last-Event-ID`；或显式 `?since=<seq>`。
- WebSocket：`{"op":"subscribe","session_id":"…","since":<seq>}`。

注册订阅与取回放在同一把锁内完成，回放和实时流之间不会漏事件。发布永不阻塞：订阅者缓冲（1024）满了就被断开并收到 `gap`，bot 本身不会被慢客户端拖住。

只要回放不能精确接上客户端的游标就报 `gap`：`since` 之后的事件已被逐出，或 `since` 超过了当前总线（会话被空闲回收后重新加载、或 bot 重启过）。会话从存储重新加载时，seq 从当前时间（微秒）起编号，保证旧游标一定落在新区间之前而被识别出来；因此 seq 只保证单调递增，不保证连续，客户端应把它当作不透明的游标。`since=0` 表示「从当前总线的第一条事件开始」。

turn 的结束状态：`cancelled` 是客户端调用 cancel、bot 关闭或调度器取消（`TurnSummary.cancel_source` 区分 `client` / `shutdown` / `scheduler`）；`timed_out` 是触到截止时间（`--turn-timeout` 或定时任务 `timeout`，均使用可暂停的 `turnClock`，等待 `ask_clarification` 期间暂停计时；或 agent 自身的 `request_timeout`）；其余错误为 `failed`。取消后若工具不响应 ctx，bot 最多等 10s 便放弃该次运行并结束 turn，之后那次运行再产生的任何事件都会被丢弃，不会出现在 `turn_end` 之后。

---

## 3. API

默认监听 `127.0.0.1:8787`。除 `/healthz` 与 `/`（控制台静态页）外均需鉴权：`Authorization: Bearer <token>`，或 `?token=`（给不能设 header 的 `EventSource`/`WebSocket`）。

### 3.1 REST

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/healthz` | 存活探针 |
| GET | `/v1/info` | 版本、工作目录、模型列表、默认模型 |
| GET | `/v1/sessions?limit=50` | 本工作目录的会话列表（合并内存中的运行状态），见 §5 |
| POST | `/v1/sessions` | 创建 `{"title"?, "model"?}` |
| GET | `/v1/sessions/{id}` | `{session, messages}`：状态 + 完整 transcript |
| PATCH | `/v1/sessions/{id}` | 改名 `{"title"}` |
| DELETE | `/v1/sessions/{id}` | 删除（运行中返回 409） |
| POST | `/v1/sessions/{id}/messages` | 发消息 `{"content", "model"?}`，见下 |
| POST | `/v1/sessions/{id}/cancel` | 打断当前 turn（仍会收到 `turn_end: cancelled`，已产生的消息会保存） |
| POST | `/v1/sessions/{id}/answer` | 回答提问 `{"question_id", "answer"}` |
| GET | `/v1/sessions/{id}/events?since=N` | 会话事件流（SSE；`?stream=ndjson` 改 NDJSON） |
| GET | `/v1/events?since=N` | 全局生命周期流（会话增删、turn 开始/结束） |
| GET | `/v1/ws` | WebSocket |

`POST …/messages` 的三种响应方式由客户端选：

| 选择方式 | 响应 |
|---------|------|
| `Accept: text/event-stream` 或 `?stream=sse` | 本 turn 的 SSE 事件直到 `turn_end` |
| `Accept: application/x-ndjson` 或 `?stream=ndjson` | 同样的事件，每行一个 JSON（非浏览器 App 最省事） |
| `?wait=1` | 阻塞，返回 `TurnSummary` |
| 默认 | `202 {"session_id","turn_id"}`，进度从 `/events` 或 WS 取 |

错误统一为 `{"error":{"code","message"}}`：`not_found`(404)、`busy`/`session_locked`/`not_running`(409)、`unknown_model`/`empty_message`/`bad_request`(400)、`unauthorized`(401)、`forbidden_origin`/`forbidden_host`(403)、`unsupported_media_type`(415)、`shutting_down`(503)。

### 3.2 WebSocket

一条连接同时承载控制与多路事件。客户端帧：

```json
{"id":"7","op":"send","session_id":"…","content":"帮我看看这个仓库"}
```

ops：`ping`、`info`、`list_sessions`、`create_session`、`get_session`、`rename_session`、`delete_session`、`subscribe`（`session_id:"*"` 为全局流，`since` 可选）、`unsubscribe`、`send`、`cancel`、`answer`。

服务端帧：

```json
{"kind":"reply","id":"7","ok":true,"data":{"turn_id":"turn_…"}}
{"kind":"event","sub":"20261107_…","event":{"seq":3,"type":"tool_call_start", …}}
```

`sub` 标明事件来自哪个订阅（`"*"` 或会话 id）。**每条总线的 seq 独立计数**，客户端须按 `sub` 分别记录续传游标。

### 3.3 最小客户端示例

```bash
TOKEN=$(deepai-bot token)
SID=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{}' localhost:8787/v1/sessions | jq -r .id)
curl -N -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' -d '{"content":"列出当前目录的 Go 包"}' \
  localhost:8787/v1/sessions/$SID/messages
```

```js
const ws = new WebSocket(`ws://127.0.0.1:8787/v1/ws?token=${token}`);
ws.onopen = () => {
  ws.send(JSON.stringify({ id: "1", op: "subscribe", session_id: sid, since: 0 }));
  ws.send(JSON.stringify({ id: "2", op: "send", session_id: sid, content: "hi" }));
};
ws.onmessage = (m) => console.log(JSON.parse(m.data));
```

---

## 4. 安全模型

bot 能执行 bash、改文件，等同于把一个 shell 暴露出去，所以默认收紧：

1. **默认只监听回环地址**（`127.0.0.1:8787`）。
2. **默认强制 token**：未指定时首次启动随机生成 48 位十六进制 token 写入 `~/.deepai/bot/token`（0600）；`deepai-bot token [--rotate]` 查看/轮换。启动时打印 `http://…/#token=…`，token 放在 URL fragment 里，不会进入服务端或代理日志。
3. **`--no-auth` 只允许回环地址**，且此时校验 `Host` 头必须是 `localhost`/回环 IP，防 DNS rebinding。
4. **Origin 校验**：带 `Origin` 的请求必须同源或在 `--cors-origin` 白名单内，否则 403；WebSocket 握手同样受此约束。
5. **POST/PATCH 必须 `Content-Type: application/json`**：浏览器跨域无法发"简单请求"绕过预检。
6. token 比较用常量时间；请求体上限 4 MiB。
7. 需要远程访问时，推荐放在 TLS 反向代理（Caddy/nginx）或 SSH 隧道 / Tailscale 后面，而不是直接 `--addr 0.0.0.0`。

---

## 5. 存储与 CLI 的关系

| 与 CLI 共享 | 与 CLI 分开 |
|---|---|
| `~/.deepai/config.yaml`、`~/.deepai/.env`（模型与凭证） | 会话与 transcript：CLI 在 `~/.deepai/deepai.db`，bot 在 `~/.deepai/bot/bot.db` |
| 工作目录上的 `DEEPAI.md`、技能、插件、`.mcp.json` 发现 | 长期记忆：bot 存在 `bot.db`，scope 为 `bot:default`，与 CLI 按工作目录划分的记忆没有共享行 |
| | 会话锁：只在 bot 进程之间生效，与 CLI 无关 |

- **独立 SQLite**：`~/.deepai/bot/bot.db`，DSN 习惯与 REPL 相同（WAL、`foreign_keys`、`busy_timeout`、`_txlock=immediate`），表结构与 CLI 会话库同形，长期记忆表也建在这里。bot 进程从不打开 `deepai.db`。
- **工作目录**：一个 bot 进程服务一个工作目录（`--workdir`，默认启动目录），工具在该目录执行。多项目就起多个 bot（不同端口），它们共用同一个 `bot.db`。
- **会话按工作目录隔离**：bot 只列出、只打开 `cwd` 等于自己 `--workdir` 的会话。其它目录的 bot 进程创建的会话 id 一律按不存在处理（`404 not_found`），以免在错误的目录里执行工具。
- **会话锁**：turn 运行期间 bot 持有该会话的单写者锁（每 20s 心跳，远小于 60s 失效阈值），结束即释放，防止两个 bot 进程同时写同一会话。
- **每会话沙箱**：`~/.deepai/bot/sandbox/session-*`，会话卸载或删除时清理。

多 Bot（同进程多个人格，`bots` 表与 `sessions.bot_id` 同样在 `bot.db` 中）见 [BOT_MULTI_DESIGN.md](BOT_MULTI_DESIGN.md)。

---

## 6. 运行与部署

```bash
make build-bot                      # ./bin/deepai-bot
deepai-bot                          # 前台运行（= serve）
deepai-bot start -w ~/code/myproj   # 后台守护：日志 ~/.deepai/bot/bot.log，实例状态 ~/.deepai/bot/instances/<hash>.json
deepai-bot status [-w ~/code/myproj] [--json]
deepai-bot stop [-w ~/code/myproj]  # SIGTERM → 取消运行中的 turn、落盘、再退出
deepai-bot token [create|list|revoke]
deepai-bot secret [set|list|delete] --bot <id>
```

- `start` 以独立进程组方式重新拉起自身的 `serve`（并发回收子进程避免启动异常时残留僵尸进程），等待 `/healthz` 通过才返回。
- 每个工作目录实例的状态写入 `~/.deepai/bot/instances/<hash>.json`（无活跃主实例时同步镜像至 `bot.json`），支持同机启动多个不同 `-w` 的守护实例并按 `-w` 精准 `status` / `stop`。
- 生产环境建议交给 systemd / launchd / Docker 管理 `deepai-bot serve`（前台模式），见 `cmd/deepai-bot/README.md` 中的 unit 示例。
- 优雅退出顺序：停止 IM 网关与调度器接收新任务 → `Manager.Close` 取消所有 turn 并等待落盘（30s）→ 等待调度回调收尾 → 连接上的客户端收到 `turn_end` → 关闭流 → 关闭 MCP 与数据库。

---

## 7. 关键设计取舍

| 决策 | 理由 |
|------|------|
| 每 turn 新建 `agent.Agent`，跨 turn 只带 `SessionCarry` | 与 REPL 完全一致的语义（技能、熔断器、压缩锚点跨轮保留），不引入第二套 agent 生命周期 |
| 默认每会话单 turn，支持显式 `mode=queue\|steer` | 默认 `reject`(409) 保持确定性；显式 `queue` 排队或 `steer` 在 ReAct 安全检查点无缝纠偏 |
| turn 与请求解耦 | "常驻 bot"的核心：手机锁屏、网页刷新都不影响任务；任何客户端都能接管 |
| 自带 seq + 回放缓冲，而不是只靠 transcript | transcript 只有完整消息；流式增量、工具进度、提问需要可续传的事件流 |
| 慢订阅者直接断开 + `gap` | 发布端永不阻塞，一个卡住的客户端不会拖慢 agent |
| 工具完整结果不进事件流 | 大文件读取会淹没所有客户端；`result_preview` 足够渲染，完整内容在 transcript |
| WebSocket 用 `golang.org/x/net/websocket` | 已是直接依赖，不新增模块；传输层隔离在 `ws.go`，需要时可换 `coder/websocket` |
| 独立 `bot.db`，复用 CLI 会话库的表结构与实现 | bot 与 CLI 的历史、记忆互不干扰；表结构、迁移和会话锁直接沿用 `pkg/chat`；多 `bot_id` 见 BOT_MULTI_DESIGN |
| 内嵌单文件控制台 | 开箱可用，也是协议的活文档；正式 GUI/App 可独立开发 |

### 7.1 运行时关键保障（已落地）

1. **Prompt 前缀缓存保护**：`AgentRuntime` 已将 `SystemPromptSuffix`（动态当前时间）接入 `AgentConfig.TurnInjectionExtra`（尾部注入），使静态 `SystemPrompt`、Bot `Instructions` 与连接器能力摘要跨轮字节稳定，充分命中 LLM 前缀缓存（详见 [`BOT_NEXT_DESIGN.md`](BOT_NEXT_DESIGN.md) §1.2）。
2. **多 Workdir 共用 `bot.db` 调度与守护隔离**：`schedule_leases` 与 `schedule_metas` 已按 `work_dir` 分桶，守护进程状态文件按 `instances/<hash>.json` 隔离，同机多项目并行互不抢租约（详见 [`BOT_MULTI_DESIGN.md`](BOT_MULTI_DESIGN.md) §4.2）。

---

## 8. 演进路线与设计文档索引

### 8.1 已落地能力全景

- **核心常驻服务（PR #40）**：本文件 §1–§7（REST / SSE / NDJSON / WebSocket、事件总线与断线续传、`ask_clarification`、独立 `bot.db` 与 `cwd` 隔离、会话锁、多实例守护进程、内置 Web 控制台）。
- **定时任务引擎 P1–P3（PR #43 / #49 / M4）**：`once` / `interval` / 5 字段 `cron` / Inbound `webhook`（HMAC 签名校验 + 模板 + 幂等） / 任务链 `chain`（A → B + 环检测）、可暂停 `turnClock`、单任务 `tool_allowlist`、条件通知 `notify_if` 与 `pkg/hook` 出站通知；详见 [`BOT_SCHEDULER_DESIGN.md`](BOT_SCHEDULER_DESIGN.md) 与 [`BOT_NEXT_DESIGN.md`](BOT_NEXT_DESIGN.md) §4。
- **多 Bot 人格、对话自训练与受控第三方业务 API 连接器（M1 / M6）**：「出生一致、后天对话训练分化」架构——共享 `ModelRegistry`/`AgentRuntime`，按 `bot_id` 隔离会话、调度、长期记忆（`bot:<id>`）、硬件绑定密钥仓（`bot_secrets`）与专属技能目录（`~/.deepai/bot/bots/<id>/skills/`），内置 `http_request`、`save_bot_skill` 与 `update_bot_persona`；详见 [`BOT_MULTI_DESIGN.md`](BOT_MULTI_DESIGN.md)。
- **消息排队、运行中插话与多模态附件（M2 / M3）**：会话 FIFO 队列（`mode=queue`）、ReAct 检查点插话（`mode=steer`）、沙箱附件上传 `/attachments` 与产出物安全下载 `/files/{path}`；详见 [`BOT_NEXT_DESIGN.md`](BOT_NEXT_DESIGN.md) §2–§3。
- **IM 渠道网关与生产治理（M5）**：`pkg/bot/channel` 统一网关（Telegram Long Polling + 飞书长连接/交互卡片、默认白名单拒绝、私聊 `/secret` 脱敏管理）、多角色 Scoped Tokens（`admin`/`user`/`readonly`）、OpenAI `/v1/chat/completions` 兼容端点、Prometheus `/metrics` 与 `audit.jsonl` 审计日志；详见 [`BOT_NEXT_DESIGN.md`](BOT_NEXT_DESIGN.md) §5–§6。

### 8.2 远期备选

- 会话级多工作目录绑定、内置 Web 控制台可视化编辑连接器与密钥面板、REPL `/plan` 与 `/mission` API 化。
