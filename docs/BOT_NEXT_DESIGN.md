# DeepAI Bot 后续能力演进设计（Queue / Steer · Attachments · Automation P3 · IM Channels · Governance）

> 状态：设计定稿（分阶段实施）  
> 前置文档：[`BOT_DESIGN.md`](BOT_DESIGN.md)（核心服务，已落地）、[`BOT_SCHEDULER_DESIGN.md`](BOT_SCHEDULER_DESIGN.md)（P1+P2，已落地）、[`BOT_MULTI_DESIGN.md`](BOT_MULTI_DESIGN.md)（多 Bot 与多 Workdir 隔离，v2 定稿）

---

## 1. 设计总览与不变量

`deepai-bot` 已具备「常驻后台 + 定时调度 + 出站通知 + Web 控制台」的核心闭环。本设计承接 [`BOT_DESIGN.md`](BOT_DESIGN.md) §8 与 [`BOT_SCHEDULER_DESIGN.md`](BOT_SCHEDULER_DESIGN.md) P3，把 `deepai-bot` 从**单人可控后台 Agent** 推进到**真正随时可聊、可插话、可传图、可事件驱动、可从手机 IM 直接对话的常驻个人 Agent 平台**。

### 1.1 四条架构不变量（所有后续模块必须遵守）

1. **Manager 是唯一编排中心**：无论是 Web、curl、定时器、Webhook、任务链还是 Telegram/飞书适配器，最终都只是调用 `Manager` 的会话接口，**绝不绕过 `Manager` 直调 `pkg/agent`**。
2. **每会话同时只跑一个 turn**：不引入同会话并发 agent 写冲突；并发消息通过显式的「排队（Queue）」或「安全检查点插话（Steer）」消化。
3. **`pkg/agent` 变更极小且向后兼容**：新增的 `TurnInjectionExtra`、`SteerSource` 全部为可选字段（`nil`/空字符串时零开销、行为与现状一字不差），绝不影响 `deepai` CLI。
4. **独立 `bot.db` + 沙箱边界**：附件、队列、Webhook、渠道绑定、多 Token 全部落在 `~/.deepai/bot/` 与 `bot.db`，不触碰 CLI 的 `deepai.db`。

### 1.2 基础性能优化：修复 Prompt 前缀缓存失效（建议最先随 M1 落地）

**现状问题**：`pkg/bot/agentruntime.go` 当前在每轮通过 `a.AppendSystemPrompt(cfg.SystemPromptSuffix())` 把 `当前时间：YYYY-MM-DD HH:MM（TZ）` 拼入 `systemPrompt`。但 `pkg/agent`（M4-2，`pkg/agent/react.go`）已将易变内容移至尾部 `turnInjection` 以保证 `systemPrompt` 字节稳定、命中 OpenAI/Anthropic/DeepSeek 自动前缀缓存。把分钟级时间戳塞进 `systemPrompt` 会让 `deepai-bot` 每分钟都全量失效前缀缓存，显著拉高 TTFT 与输入 token 费用。

**修复方案**：
- `pkg/agent.AgentConfig` 新增可选字段 `TurnInjectionExtra func() string`（在 `buildTurnInjection` 末尾追加，不进入 `systemPrompt`，不落盘到 `messages`）；
- `pkg/bot/agentruntime.go` 将 `cfg.SystemPromptSuffix` 改接到 `AgentConfig.TurnInjectionExtra`；
- 静态的 `DEEPAI.md` 与 `BotProfile.Instructions` 留在 `systemPrompt`，跨轮 100% 字节稳定。

---

## 2. 消息排队与运行中插话（Follow-up Queue & Mid-turn Steer）

**痛点**：当一个长 turn（如代码分析或定时任务）运行 5–15 分钟时，用户想到补充要求或纠偏，直接发送消息会收到 `409 busy`，被迫二选一：要么干等结束，要么 `cancel` 浪费掉已完成的前半段工作。

### 2.1 发送模式（完全向后兼容）

`POST /v1/bots/{bot_id}/sessions/{id}/messages`（及扁平 `/v1/sessions/{id}/messages`、WS `send`）的请求体扩展：

```jsonc
{
  "content": "别改旧接口，只加一个新的 v2 handler",
  "model": "",
  "mode": "reject | queue | steer"   // 缺省 "reject"（保持现有 409 busy 行为不变）
}
```

| 会话状态 | `mode: "reject"`（默认） | `mode: "queue"` | `mode: "steer"` |
|---|---|---|---|
| `idle` | 立即启动 turn（202） | 立即启动 turn（202） | 立即启动 turn（202） |
| `running` | `409 busy` | 加入该会话 FIFO 待办队列，返回 `202 {"queued":true,"queue_id":"q_...","position":1}` | 投入运行中 turn 的 steer 通道，返回 `202 {"steered":true,"steer_id":"st_..."}`；若当前 turn 已进入 `wrapUp` 收尾态，自动降级为 `queue` 并在响应标注 `fallback: "queue"` |

> Web 控制台与 IM 适配器默认使用 `queue`（并提供「立即插话（Steer）」按钮），旧脚本/自动化客户端不传 `mode` 时行为 100% 保持不变。

### 2.2 Follow-up Queue（阶段一，零改 `pkg/agent`）

- **容量与存储**：每会话内存队列（上限 `16` 条，超限返回 `409 queue_full`），单条记录 `{id, content, images, attachment_ids, model, created_at}`。队列默认驻留内存（不污染未执行的 transcript）；进程优雅退出时广播 `queue_cleared(reason=shutdown)`。
- **自动出队**：在 `Manager.runTurn` 的 `turn_end` 落盘与广播完成后、释放 `s.turn` 之前，按状态决策：
  - 上一轮为 `completed`：自动取出队首，以同一会话立即启动下一轮 turn；
  - 上一轮为 `cancelled`（客户端主动打断）或 `timed_out` / `failed`：**不自动消费队列**，保留队列或按配置暂停（默认 `cancelled` 时清空队列并广播 `queue_cleared(reason=cancelled)`，避免用户刚打断就被队列里的旧指令继续跑下去；`failed`/`timed_out` 保留队列并由用户决定是否继续或删除）。
- **管理接口与事件**：
  - `GET /v1/sessions/{id}/queue`：查看当前排队消息列表；
  - `DELETE /v1/sessions/{id}/queue/{queue_id}`：撤回单条排队消息；
  - `DELETE /v1/sessions/{id}/queue`：清空队列；
  - 会话事件新增：`queue_enqueued`、`queue_dequeued`、`queue_removed`、`queue_cleared`。

### 2.3 Mid-turn Steer（阶段二，极简 `pkg/agent` 扩展）

#### `pkg/agent` 接口设计（~30 行，零侵入）

```go
// pkg/agent/steer.go
type SteerMessage struct {
    ID      string
    Content string
    Images  []models.MessageImage
}

// SteerSource supplies mid-turn user steering messages at safe ReAct boundaries.
// Implementations must be non-blocking and goroutine-safe.
type SteerSource interface {
    DrainSteer() []SteerMessage
}
```

- `AgentConfig` 新增可选字段 `Steer SteerSource`（`nil` 时完全关闭）。
- **唯一消费检查点**：`pkg/agent/react.go` 主循环 `for turn := 0; ; turn++` 顶部、`turn > 0 && !wrapUp` 处（即上一轮 tool calls 全部执行完毕并写入 `tool_result`，下一轮 LLM 请求构造之前）：
  1. 调用 `steers := a.steer.DrainSteer()`；
  2. 对每条 `st`，构造标准 `models.Message{ID: st.ID, SessionID: sessionID, Role: models.RoleHuman, Content: st.Content, Images: st.Images, Metadata: map[string]string{"source": "steer"}, CreatedAt: time.Now().UTC()}`，追加到 `runMessages`；
  3. 发出 `AgentEvent{Type: AgentEventSteerApplied, Text: st.Content, ...}`；
  4. **不标记 `metaAgentInjected`**：它是真实用户输入，会随 `TurnOutput.Messages` 持久化进 `bot.db` transcript，并正常参与会话记忆与上下文老化。
- **不中断正在执行的工具**：正在跑的 `bash` / `edit_file` 会先安全结束并返回 `tool_result`，避免半截文件损坏或工具结果缺失；紧接着下一轮推理立刻看到用户的纠偏指令。
- **与 `ask_clarification` 的隔离规则**：当 turn 处于等待提问（`len(pending) > 0`）时，steer 不会自动充当 `question` 的回答（避免用户本想改方向却误答了某个二选一弹窗）；客户端应明确区分 `/answer` 与 `/messages?mode=steer`。
- **定时任务运行中 Steer（覆盖 Scheduler P3）**：因为定时任务复用普通 turn，`activeTurn` 自带 `steerBuf`，用户在 Web 控制台或 IM 看到定时任务跑偏时，直接发一条 `mode: "steer"` 即可中途纠偏，无需整轮取消重来。

---

## 3. 多模态附件与产出物工作流（Attachments & Artifacts）

**现状**：`pkg/models.Message` 已有 `Images []MessageImage` 字段且 `pkg/imageproc` 已实现图片压缩，但 `pkg/bot.SendRequest` 仅有 `Content string`，无法发图、传文档或下载 bot 生成的报告/图表。

### 3.1 上传与引用流程

1. **上传接口**：`POST /v1/sessions/{id}/attachments`（支持 `multipart/form-data`，单文件上限 `10 MiB`，单会话沙箱上传配额 `50 MiB`）。
2. **存储位置**：落在现有会话沙箱 `~/.deepai/bot/sandbox/session-<id>/uploads/<att_id>_<sanitized_name>`，会话删除或空闲回收时随沙箱自动清理，不泄漏磁盘。
3. **返回结构**：
   ```json
   {
     "id": "att_9c21f0",
     "name": "arch.png",
     "mime_type": "image/png",
     "size": 184320,
     "kind": "image",
     "sandbox_path": "uploads/att_9c21f0_arch.png"
   }
   ```
4. **消息发送扩展**：
   ```jsonc
   // SendRequest
   {
     "content": "按这张架构图检查 pkg/bot 的边界",
     "attachment_ids": ["att_9c21f0"],
     "images": [] // 可选：小图允许直接内联 base64，仍受 4 MiB body 上限约束
   }
   ```
   - `kind == "image"`（`png`/`jpeg`/`webp`/`gif`）：服务端经 `pkg/imageproc` 缩放优化后填入 `models.Message.Images`，同时保留沙箱文件路径供工具二次读取；
   - 非图片文件（`.pdf`/`.docx`/`.csv`/`.log`/代码等）：服务端在用户消息末尾注入标准化附件块 `[Attached file: uploads/att_... (mime, size)]`，并将 `attachment_ids` 记入 `Message.Metadata`，agent 可直接用 `read_file` / `docx_read` / `bash` 读取。

### 3.2 产出物预览与下载（Artifacts）

agent 在沙箱（或工作目录中显式通过 `present_file` 工具展示的文件）生成图表、`.docx`、Markdown 报告时：
- **事件支持**：当工具调用为 `present_file` 且成功时，`EventToolCallEnd` 附带可下载的相对引用（或新事件 `artifact`），前端直接渲染「预览 / 下载」卡片；
- **下载端点**：`GET /v1/sessions/{id}/files/{path...}`
  - **严格路径边界**：只允许访问以下两类路径之一，且必须经 `filepath.EvalSymlinks` + `relUnderRoot` 防符号链接穿越：
    1. 该会话沙箱目录 `~/.deepai/bot/sandbox/session-<id>/` 下的文件；
    2. 本会话 transcript 中 `present_file` 显式授权过的 workdir 内文件（避免把下载接口变成任意文件读取漏洞）。
  - **安全响应头**：强制 `X-Content-Type-Options: nosniff`，对 `.html`/`.svg`/`.js` 强制 `Content-Disposition: attachment` 防止同源 XSS。

---

## 4. 自动化引擎升级（Scheduler P3：Webhook · 任务链 · 条件通知）

复用 `pkg/schedule` 已验证的「非阻塞调度 + Run 生命周期 + `Dispatcher.Fire` → 普通 turn」底座，把定时任务平滑升级为**通用事件自动化引擎**。

### 4.1 单任务工具白名单（`tool_allowlist`）

- `schedule.Options` 新增 `ToolAllowlist []string`；
- 触发时由 `bot.scheduleDispatcher` 传入 `TurnInput.ToolAllowlist`；
- 与所属 Bot 的 `BotProfile.ToolAllowlist` 取**交集**（若一方为空则取另一方；两者皆非空取交集），保证巡检/汇总类定时任务可被限制为纯只读工具集（如 `["read_file", "grep", "glob", "web_search"]`），彻底消除无人值守误写风险。

### 4.2 Inbound Webhook 触发器

允许外部系统（GitHub Actions/PR、Alertmanager、CI、自定义脚本）通过 HTTP Webhook 触发一个受控 Agent turn。

- **Trigger 扩展**：
  ```jsonc
  {
    "type": "webhook",
    "secret_ref": "env:GITHUB_WEBHOOK_SECRET", // 或服务端生成的随机 secret（落库加密/掩码，仅创建时返回一次）
    "signature_header": "X-Hub-Signature-256", // 支持 github_sha256 | hmac_sha256 | bearer
    "dedup_header": "X-GitHub-Delivery",       // 映射为 Run.request_id 实现天然幂等
    "event_filter": {                          // 可选：JSON path / Header 精确匹配
      "header.X-GitHub-Event": "pull_request",
      "payload.action": "opened"
    },
    "rate_limit_per_min": 5                    // 防 webhook 风暴打爆 token 预算
  }
  ```
- **独立入站端点**：`POST /v1/hooks/{schedule_id}`（**不使用** bot 主 bearer token，改用该 hook 专属签名/secret 校验，遵循最小权限原则；允许按配置暴露在反向代理的白名单路径下）。
- **模板安全渲染**：`prompt` 支持受限占位符（`{{.Event}}`、`{{.PayloadJSON}}`、`{{.Payload.pull_request.html_url}}`），使用 `text/template` 禁用任意函数调用，并对注入的外部 payload 包裹明确的非信任数据围栏（`<untrusted_webhook_payload>...</untrusted_webhook_payload>`）加长度上限（默认 16 KiB），降低 Prompt Injection 风险。
- **与 `schedule_runs` 复用**：Webhook 触发产生一条 `trigger = "webhook"` 的普通 `Run`，天然复用 `overlap`、`max_concurrent_runs`、`max_runs_per_day`、`timeout`、Run 历史、`pkg/hook` 通知与 Web 控制台展示。

### 4.3 任务链（Task Chaining：A → B）

支持将复杂周期流程拆成职责单一的多个任务（例如「任务 A：拉取日志并生成分析报告」→ 成功后触发「任务 B：由审核 Bot 复核并发送摘要」）：

- **Trigger 扩展**：
  ```jsonc
  {
    "type": "chain",
    "after_schedule_id": "sch_upstream",
    "on_status": "completed | failed | any"    // 默认 "completed"
  }
  ```
- **触发机制**：在 `Service.FinishRun` 写完上游 Run 终态后，在同事务中查询所有 `state = active` 且 `after_schedule_id == upstream.ID` 且匹配 `on_status` 的下游任务，为其插入 `trigger = "chain"`、`request_id = "chain:" + upstreamRun.ID`（天然防重复）的 `pending` Run 并 `kick` 调度器。
- **上下文传递**：下游 prompt 支持 `{{prev.status}}`、`{{prev.output}}`（截断至 8 KiB）、`{{prev.run_id}}`、`{{prev.session_id}}`。
- **环检测与深度限制**：创建/更新 `chain` 任务时做 DFS 环检测（检测到环返回 `400 cycle_detected`）；每次链式触发传递 `chain_depth`（最大 `5` 层），防止意外级联风暴。

### 4.4 按结果条件通知（`notify_if`）

**痛点**：巡检类定时任务（如「每小时检查错误日志」）95% 的时间一切正常，`notify: always` 太吵，`notify: on_failure` 只能看运行崩溃、看不到业务告警。

- `schedule.Options` 新增 `NotifyIf`：
  - `mode: "all" | "contains" | "not_contains" | "regex" | "marker"`（缺省按现有 `notify` 字段行为不变）；
  - 推荐默认约定 `marker` 模式：系统在定时消息提示里告知模型「若无需打扰用户，回复以 `[NO_NOTIFY]` 开头」——模型判定无异常时静默完成（Run 仍记录为 `completed` 可审计），仅在发现真正异常时推送 `pkg/hook` 通知。

---

## 5. IM 渠道适配器网关（Channel Adapter Framework）

让用户无需打开电脑浏览器，直接在 **Telegram / 飞书** 里与常驻的 `deepai-bot` 对话、审批定时任务、回答 `ask_clarification`、随时插话纠偏。

### 5.1 架构分层（独立包 `pkg/bot/channel`，零侵入核心）

```
 Telegram (Long Polling) ──┐
 飞书 (WebSocket 长连接) ───┼──► channel.Gateway ──► pkg/bot.Manager (同进程直调)
 后续 Slack / 企微 ─────────┘         │
                                      ▼
                       bot.db: channel_bindings 表
                  (channel, chat_id, thread_id) ↔ (bot_id, session_id)
```

为什么首批选 **Telegram（Long Polling）** + **飞书（长连接 WS SDK/协议）**？
- **两者均无需公网 IP、无需域名、无需内网穿透**，与 `deepai-bot` 本地常驻（本机/内网服务器/Tailscale）的部署模型完美契合。

### 5.2 核心抽象

```go
// pkg/bot/channel/channel.go
type Adapter interface {
    Name() string // "telegram" | "feishu"
    Start(ctx context.Context, onInbound func(InboundMessage)) error
    SendText(ctx context.Context, dst Destination, text string) (msgID string, err error)
    EditText(ctx context.Context, dst Destination, msgID, text string) error
    SendQuestion(ctx context.Context, dst Destination, q bot.Question) (msgID string, err error)
    SendAttachment(ctx context.Context, dst Destination, filePath, caption string) error
}
```

### 5.3 会话映射、流式渲染与交互协议

| 能力 | 设计约定 |
|---|---|
| **线程 ↔ 会话映射** | `channel_bindings(channel, chat_id, thread_id PRIMARY KEY, bot_id, session_id, updated_at)`：私聊按 `chat_id` 维持当前会话；群聊按 `thread_id`（话题/回复串）隔离，且默认**仅响应 `@bot` 或回复 bot 的消息** |
| **流式节流编辑** | 订阅 `Manager` 会话事件流：累积 `text_delta`，每 `800ms`（可配）调用一次 `EditText` 刷新消息（防触发 IM API 限流）；运行工具时在消息末尾追加状态行（如 `🔧 运行 bash...`）；超出平台单条字数上限（如 TG 4096 字符）时自动分段发新消息 |
| **提问 ↔ 内联按钮** | 收到 `question` 事件 → 调用 `SendQuestion` 渲染带选项按钮的卡片（Telegram `InlineKeyboard` / 飞书交互卡片）；用户点按钮或直接回复该消息 → 调 `Manager.Answer` 恢复 turn |
| **并发消息处理** | 运行中收到同一线程的新消息：默认按 `mode: "queue"` 入队并回复「已加入队列（第 N 位），发送 `/steer <内容>` 可立即插话，`/cancel` 可中止」 |
| **内置斜杠指令** | `/new`（开新会话）、`/cancel`（中止当前 turn）、`/steer <text>`（运行中插话）、`/bot [id]`（切换当前线程绑定的 Bot 人格）、`/model [alias]`（切换模型）、`/status`（查看当前会话与运行中任务）、`/approve <sch_id>`（手机端直接批准待审批定时任务） |
| **双向附件** | 用户在 IM 发图片/文件 → 适配器下载后写入会话沙箱 `uploads/` 并走 §3 附件管线；agent 调 `present_file` → 适配器自动回传文件/图片到对应聊天 |

### 5.4 安全边界（IM 通道强制默认收紧）

IM 机器人背后连接着能执行 `bash` 的 agent，必须实施**严格白名单准入**：
- `channels.telegram.allow_users` / `allow_chats`（空列表 = **拒绝一切外部消息**并打告警日志，绝不默认向全网开放）；
- 支持为每个渠道或群聊绑定**受限 Bot 人格**（例如群聊绑定只有只读 `tool_allowlist` 的 `assistant` Bot，私聊管理员才绑定全权限 `default` Bot）；
- `config.yaml` 配置示例：
  ```yaml
  channels:
    telegram:
      enabled: true
      bot_token_env: TELEGRAM_BOT_TOKEN
      default_bot_id: default
      allow_users: [123456789]
      allow_chats: []
      stream_edit_interval: 800ms
    feishu:
      enabled: false
      app_id_env: FEISHU_APP_ID
      app_secret_env: FEISHU_APP_SECRET
      default_bot_id: default
      allow_users: ["ou_xxxx"]
  ```

---

## 6. 生产治理与生态兼容

### 6.1 多 Token 与细粒度权限（Scoped Tokens）

保留现有 `~/.deepai/bot/token` 作为零配置 `admin` 主令牌，同时在 `bot.db` 新增 `bot_tokens` 表支持签发受限令牌：

```sql
CREATE TABLE IF NOT EXISTS bot_tokens (
    id          TEXT PRIMARY KEY,          -- tok_...
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,      -- SHA-256 hex，明文仅创建时返回一次
    role        TEXT NOT NULL,             -- admin | user | readonly
    bot_ids     TEXT NOT NULL DEFAULT '[]',-- 空 = 全部 bot；非空 = 仅限指定 bot_id
    expires_at  INTEGER,                   -- 可空
    revoked_at  INTEGER,
    last_used_at INTEGER,
    created_at  INTEGER NOT NULL
);
```

- `readonly`：仅允许 `GET` 与只读 WS ops（`info`/`list_*`/`get_*`/`subscribe`），禁止发送消息、创建/修改 Bot 与调度；
- `user`：仅允许在授权 `bot_ids` 内操作会话与任务，禁止修改 Bot `tool_allowlist`、禁止管理 Token；
- `admin`：全量权限（含 `POST/DELETE /v1/tokens`）。
- 命令行辅助：`deepai-bot token create --name ipad --role user --bots default,reviewer` / `token list` / `token revoke <id>`。

### 6.2 OpenAI 兼容端点（`/v1/chat/completions`）

让 Open WebUI、Cherry Studio、Raycast、Cursor 等现成 OpenAI 生态客户端直接把 `deepai-bot` 当作带工具、带记忆、带人格的后端：

- `model` 字段解析规则：
  - `bot:<bot_id>` → 使用该 Bot 的 `default_model` 与 `instructions`；
  - `bot:<bot_id>/<model_alias>` → 指定 Bot + 覆盖模型；
  - 普通 `<model_alias>` → 路由到 `default` Bot + 指定模型。
- 会话复用：
  - 若请求带 `X-DeepAI-Session: <sid>` header，复用对应会话并追加最新一条 `user` 消息；
  - 否则创建临时会话（或按 `user` 字段映射固定会话），跑完一次 `Manager.Send`；
- `stream: true` 时将 `EventTextDelta` / `EventTurnEnd` 翻译为标准 `chat.completion.chunk` SSE 流（工具调用状态可按配置作为 `reasoning_content` 或状态注释输出，不影响主回答正文）。

### 6.3 可观测性与审计日志

- **Prometheus `/metrics`**（受 admin token 或显式 `--metrics-addr 127.0.0.1:9090` 保护）：
  - `deepai_bot_active_turns{bot_id}`（Gauge）
  - `deepai_bot_turn_duration_seconds{bot_id,status}`（Histogram）
  - `deepai_bot_tokens_total{bot_id,model,kind="input|output|cached"}`（Counter）
  - `deepai_bot_tool_calls_total{bot_id,tool,status}`（Counter）
  - `deepai_bot_queue_depth{bot_id}`（Gauge）
  - `deepai_bot_schedule_runs_total{bot_id,trigger,status}`（Counter）
  - `deepai_bot_schedule_lag_seconds`（Histogram，实际启动时刻 − 计划槽位）
- **结构化审计日志**：写往 `~/.deepai/bot/audit.jsonl`（按大小轮转），记录鉴权失败、Token 签发/吊销、Bot 创建/修改/删除、Webhook 触发、定时任务批准与执行终态、IM 渠道被拒消息。

---

## 7. 分阶段实施路线图（M1 → M5）

每个 Milestone 均可拆为 1–2 个独立 PR，做到**每合一个 PR 即立即可用、零破坏升级**：

| Milestone | 核心内容 | 涉及包 | 核心验收标准 |
|---|---|---|---|
| **M1（基础与多 Bot）** | 1. 修复 `SystemPromptSuffix` → `TurnInjectionExtra`（保前缀缓存）<br>2. `tools.Registry.Subset` + `schedule` 按 `work_dir` 分桶租约<br>3. [`BOT_MULTI_DESIGN.md`](BOT_MULTI_DESIGN.md) 完整实现（`BotRegistry`、`/v1/bots`、扁平别名 404、`DELETE ?force=1`、Web Bot 切换器） | `pkg/agent`、`pkg/tools`、`pkg/schedule`、`pkg/bot`、`cmd/deepai-bot` | 同机双 `work_dir` 调度互不干扰；双 Bot 记忆/prompt/工具完全隔离；PATCH 后下轮热生效；`systemPrompt` 跨轮字节稳定单测通过 |
| **M2（交互流畅度）** | 1. 会话 Follow-up Queue（`mode=queue` + 队列 API/事件 + Web 排队条）<br>2. Mid-turn Steer（`pkg/agent.SteerSource` + `mode=steer` + 定时任务运行中 steer） | `pkg/agent`、`pkg/bot`、`pkg/bot/web` | 长 turn 期间连续发消息不再吃 409；steer 在下一轮 ReAct 边界准确注入且不误答 `ask_clarification`；取消 turn 不误消费旧队列 |
| **M3（多模态与产物）** | 1. 附件上传 `/attachments` + 图片压缩接入 `models.Message.Images`<br>2. 沙箱/授权产出物安全下载 `/files/{path}` + Web 拖放上传与预览卡片 | `pkg/bot`、`pkg/bot/web` | 拖入截图给 bot 分析全链路通畅；路径穿越/符号链接逃逸单测 100% 拦截；删除会话自动清理上传沙箱 |
| **M4（自动化 P3）** | 1. 单任务 `tool_allowlist`（与 Bot 取交集）<br>2. Inbound Webhook 触发器（HMAC + 模板 + `delivery_id` 去重 + 限流）<br>3. 任务链（A → B + 环检测 + `{{prev.output}}`）与条件通知 `notify_if` | `pkg/schedule`、`pkg/bot`、`pkg/hook` | GitHub Webhook 签名校验 + 重复投递幂等单测通过；A→B 链式触发与环检测通过；`[NO_NOTIFY]` 静默不发通知 |
| **M5（IM 与治理）** | 1. `pkg/bot/channel` 网关 + Telegram（Long Polling）& 飞书（WS 长连接）适配器<br>2. Scoped Tokens（`bot_tokens`）、`/v1/chat/completions` 兼容层、`/metrics` 与 `audit.jsonl` | `pkg/bot/channel`、`pkg/bot`、`cmd/deepai-bot` | 手机 Telegram/飞书完整对话 + 按钮回答提问 + 审批定时任务；未在白名单的外部用户 100% 拒绝；Open WebUI 直连流式输出正常 |
