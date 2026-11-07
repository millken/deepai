# DeepAI Bot 定时任务设计

让模型通过内置工具 `schedule` 自己创建、管理定时任务（一次性 / 周期），bot 在后台按时执行；任何客户端都能通过 WebSocket / SSE 实时看到任务状态和执行过程，并随时干预。

典型场景："每天晚上 10 点总结今天的工作"、"两小时后检查 CI 结果并告诉我"、"每周一早上 9 点整理上周未关闭的 issue"。

本文是设计稿，供 review；实现按第 12 节分阶段进行。前置文档：[BOT_DESIGN.md](BOT_DESIGN.md)。

---

## 0. 已确定的决策

| 问题 | 决定 |
|---|---|
| cron 解析 | 自写，定制规则（第 5 节），不引入新依赖 |
| 模型创建周期任务 | 默认 `ask`：需用户确认后才生效 |
| 执行所在会话 | 默认 `dedicated`：每个任务一个固定会话 |
| 范围 | 只在 `deepai-bot` 内。`deepai` CLI 不注册该工具、不跑调度器、不加子命令；`pkg/schedule` 与 `pkg/bot` 通过接口解耦，互不依赖实现 |

## 1. 核心思路

**一次触发 = 在某个会话里自动发一条消息、跑一个普通 turn。**

调度器只负责"什么时候、往哪个会话、发什么"，以及记录每次执行（Run）的状态。执行本身完全复用 bot 已有的会话机制，因此：

- 执行过程（文字流、工具调用、子代理进度）就是该会话的事件流，客户端用现有订阅接口即可实时观看；
- 取消、回答提问、事后追问，都是现有的会话操作；
- 会话锁、压缩/摘要、transcript 持久化、`turn_end` 状态（含 `timed_out`）全部沿用。

调度器新增的只有：任务定义与运行记录的存储、触发计算、全局事件、管理接口和一个工具。

## 2. 总体架构

```
              ┌─────────────────────── deepai-bot 进程 ───────────────────────┐
 模型 ──调用──► schedule 工具 ───┐                                              │
 客户端 ─REST/WS─► /v1/schedules ┤                                              │
                                 ▼                                              │
                       schedule.Service  ── 校验 / 规范化 / 计算下次时间         │
                          │        ▲                                            │
                 SQLite:  │        │ Run 状态回写（turn_end 回调）               │
        schedules / schedule_runs  │                                            │
        / schedule_lease  │        │                                            │
                          ▼        │                                            │
                 schedule.Scheduler（最小堆 + 单定时器）                         │
                          │ Dispatcher.Fire(run)                                │
                          ▼                                                     │
                 bot.scheduleDispatcher ──► Manager.Send(会话, 任务指令)         │
                                               │ 普通 turn                      │
                          ┌────────────────────┴───────────────┐                │
                          ▼                                    ▼                │
                 会话事件流（现有）                全局事件流 + schedule_* 事件  │
                 text_delta / tool / question       （任务与 Run 状态）           │
                          │                                    │                │
                          └──────── pkg/hook 通知（webhook / 命令）◄─────────────┘
              └──────────────────────────────────────────────────────────────────┘
```

### 2.1 包划分

| 包 | 职责 | 依赖 |
|---|---|---|
| `pkg/schedule` | 数据模型、触发规则（含 cron 解析）、SQLite 存储与迁移、`Service`（增删改查 + 校验）、`Scheduler`（调度循环）、`Dispatcher` 接口 | 只依赖标准库和 `database/sql`，**不依赖** `pkg/bot` / `pkg/agent` |
| `pkg/schedule/tool.go` | `schedule` 工具（`models.Tool`），只调用 `Service` | `pkg/models`、`pkg/tools`（取会话 ID、`AskQuestion`） |
| `pkg/bot` | `scheduleDispatcher`（实现 `Dispatcher`）、REST/WS 路由、事件类型、控制台面板 | `pkg/schedule` 的接口与类型 |
| `pkg/commands/bot.go` | 打开存储、构造 `Service`/`Scheduler`、注册工具、接线 | 两者 |

```go
// pkg/schedule
type Dispatcher interface {
    // Fire 启动一次执行并立即返回；最终状态通过 Service.FinishRun 回写。
    Fire(ctx context.Context, sch Schedule, run Run) (sessionID, turnID string, err error)
    // Cancel 中止一次正在执行的 Run。
    Cancel(run Run) error
}

type Observer interface { // bot 用它把变化发到全局事件流
    ScheduleChanged(Schedule, ChangeKind)
    RunChanged(Run)
}
```

## 3. 数据模型

### 3.1 Schedule（任务定义）

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | `sch_` + 随机 hex |
| `name` | string | 显示名，必填，≤ 80 字 |
| `prompt` | string | 每次触发发给模型的指令，必填，≤ 8 KB |
| `trigger` | Trigger | 见 3.3 |
| `session_mode` | enum | `dedicated`（默认）/ `new` / `origin`，见第 6 节 |
| `session_id` | string | `dedicated` 模式下的固定会话（首次运行时惰性创建） |
| `model` | string | 空 = bot 默认模型 |
| `timeout` | duration | 单次运行上限，默认 30m，上限 4h；超时 → Run `timed_out` |
| `max_tool_calls` | int | 0 = 沿用 bot 配置 |
| `overlap` | enum | 上一次未结束时：`skip`（默认）/ `queue` / `replace` |
| `misfire` | enum | 停机期间错过的触发：`run_once`（默认，合并补跑一次）/ `skip` |
| `misfire_grace` | duration | 超过此宽限的错过触发一律 `missed`；默认 1h（一次性）/ 一个周期（周期性，最多 24h） |
| `answer_timeout` | duration | 无人值守时 `ask_clarification` 的等待上限，默认 30m，见第 7 节 |
| `on_answer_timeout` | enum | `assume`（默认，让模型按最合理假设继续）/ `fail` |
| `notify` | enum | `always`（默认）/ `on_failure` / `never` |
| `end_at` / `max_runs` | time / int | 周期任务终止条件，可空 |
| `state` | enum | `pending_approval` / `active` / `paused` / `completed` / `deleted` |
| `created_by` | object | `{kind: "model"|"user", session_id, turn_id, client}` |
| `next_run_at` | time | 冗余，便于列表与启动恢复；`paused` 时仍计算但不触发 |
| `last_run` | RunSummary | 最近一次的 `{id, status, ended_at}` |
| `run_count` | int | 已执行次数（不含 `skipped`/`missed`） |
| `created_at` / `updated_at` | time | |

`completed`：一次性任务执行完、或周期任务到达 `end_at`/`max_runs`。`deleted` 为软删除，保留 30 天供查看历史 Run，之后清理。

### 3.2 Run（一次执行）

| 字段 | 说明 |
|---|---|
| `id` | `run_` + 随机 hex |
| `schedule_id`、`seq` | 所属任务、第几次 |
| `scheduled_for` | 计划时间；`started_at` / `ended_at` 实际时间 |
| `trigger` | `timer` / `misfire` / `manual`（run_now） |
| `session_id`、`turn_id` | 执行所在会话与 turn，客户端据此订阅执行过程 |
| `status` | 见下 |
| `output` | 最终回复（截断至 16 KB；全文在会话 transcript） |
| `error`、`usage`、`tool_calls` | 来自 `turn_end` |

```
pending ──► running ──┬──► completed
   │                  ├──► failed
   │                  ├──► timed_out
   │                  └──► cancelled
   │        ▲   │
   │        │   ▼
   │     waiting_input      （模型调用 ask_clarification，等待回答）
   │
   ├──► skipped   （overlap=skip，或任务已暂停）
   └──► missed    （停机错过且超出宽限 / misfire=skip）
```

终态与 `turn_end` 一一对应（`completed/cancelled/timed_out/failed`），只多了调度器自己的 `pending/waiting_input/skipped/missed`。

### 3.3 Trigger

```jsonc
{"type": "once",     "at": "2026-11-07T22:00:00+08:00"}
{"type": "once",     "in": "2h"}                                  // 创建时换算成 at
{"type": "interval", "every": "6h", "start_at": "2026-11-08T09:00:00+08:00"}
{"type": "cron",     "expr": "0 22 * * *", "tz": "Asia/Taipei"}
```

- 所有时间存储为 UTC，`tz` 只用于 cron 计算与展示；`tz` 缺省取 bot 配置 `schedule.timezone`，再退到系统时区。
- `once.at` 必须带时区偏移；不带偏移的按任务 `tz` 解释。早于"现在 − 1 分钟"拒绝。
- `interval.every` 最小值 = `schedule.min_interval`（默认 5m）；cron 两次触发间隔同样受此约束（校验时检查未来 10 次）。

### 3.4 SQLite 表

建在 bot 已打开的 `~/.deepai` 库中，由 `pkg/schedule` 自己迁移（`CREATE TABLE IF NOT EXISTS` + `schema_version`），CLI 不触碰这几张表。

```sql
CREATE TABLE schedules (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prompt TEXT NOT NULL,
  trigger_json TEXT NOT NULL,
  options_json TEXT NOT NULL,          -- session_mode/timeout/overlap/misfire/notify/...
  session_id TEXT,
  state TEXT NOT NULL,
  created_by_json TEXT NOT NULL,
  next_run_at INTEGER,                 -- unix ms, NULL = 不再触发
  run_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX schedules_next ON schedules(state, next_run_at);

CREATE TABLE schedule_runs (
  id TEXT PRIMARY KEY,
  schedule_id TEXT NOT NULL REFERENCES schedules(id),
  seq INTEGER NOT NULL,
  trigger TEXT NOT NULL,
  scheduled_for INTEGER NOT NULL,
  started_at INTEGER, ended_at INTEGER,
  session_id TEXT, turn_id TEXT,
  status TEXT NOT NULL,
  output TEXT, error TEXT, usage_json TEXT,
  UNIQUE(schedule_id, scheduled_for)   -- 同一计划时间只会有一个 Run：防重复触发
);
CREATE INDEX schedule_runs_by_schedule ON schedule_runs(schedule_id, seq DESC);
CREATE INDEX schedule_runs_active ON schedule_runs(status) WHERE status IN ('pending','running','waiting_input');

CREATE TABLE schedule_lease (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  owner TEXT NOT NULL,                 -- pid@host#start
  expires_at INTEGER NOT NULL
);
```

Run 历史按任务保留最近 200 条（可配置），超出的在 janitor 中清理。

## 4. 调度器

### 4.1 调度循环

单个 goroutine：内存中按 `next_run_at` 排序的最小堆 + 一个 `time.Timer`。任务增删改、暂停/恢复时通过 channel 唤醒并重算等待时长；不轮询。为防系统休眠/时钟跳变，定时器最长等待 1 分钟后重新检查一次。

触发一次的步骤：

1. **事务内**：插入 Run（`pending`，`UNIQUE(schedule_id, scheduled_for)` 冲突即说明已触发过，直接跳过）；推进 `next_run_at`；一次性任务置为不再触发。
2. 检查 `overlap`：该任务有活跃 Run 时，`skip` → Run 记 `skipped`；`queue` → 等前一次结束（最多 `timeout`，超出记 `skipped`）；`replace` → 取消前一次后继续。
3. 调用 `Dispatcher.Fire`，成功后 Run → `running`，记录 `session_id/turn_id`。
4. bot 在该 turn 的 `turn_end` 回调里调用 `Service.FinishRun`，写入终态、输出、usage；按 `notify` 发 hook 通知；检查 `end_at/max_runs` 决定是否 `completed`。

**先持久化再触发**：进程在第 1 步与第 4 步之间崩溃，最多丢失一次执行（启动时标记失败），绝不会重复执行。

### 4.2 启动恢复

1. 获取租约（4.3），拿不到则调度器不启动（只提供只读接口）并记录警告。
2. 把 `pending/running/waiting_input` 的 Run 标为 `failed`，`error = "bot restarted"`。
3. 对每个 `active` 任务，若 `next_run_at < now`：
   - 超出 `misfire_grace` 或 `misfire=skip` → 为最近一次错过的时间记一条 `missed` Run；
   - 否则 `run_once` → 立即补跑一次（`trigger=misfire`），多次错过只合并成一次；
   - 然后从 `now` 起重算 `next_run_at`。
4. 建堆，进入循环。

### 4.3 单实例租约

同一个库只允许一个调度器在跑（避免两个 bot 进程重复触发）：`schedule_lease` 单行记录，持有者每 20s 续约、过期时间 60s；过期可被接管。与现有会话锁的参数一致。`UNIQUE(schedule_id, scheduled_for)` 是第二道保险。

### 4.4 并发与资源

- 全局并发上限 `schedule.max_concurrent_runs`（默认 2）；超出的 Run 保持 `pending` 排队，不计入超时。
- 每个任务单次运行受 `timeout` 和 `max_tool_calls` 约束；超时由 `Manager` 现有的 turn 超时机制落为 `timed_out`。
- 全局活跃任务数上限 `schedule.max_schedules`（默认 50）；单任务每日运行上限 `schedule.max_runs_per_day`（默认 48）。

## 5. cron 规则（自写）

只实现确定需要的子集，规则写死、行为可预测；不支持的写法在创建时报错并给出提示，由模型改写。

**格式**：5 字段 `分 时 日 月 周`，空格分隔。

| 字段 | 取值 | 支持 |
|---|---|---|
| 分 | 0–59 | `*` `n` `a-b` `a,b,c` `*/s` `a-b/s` |
| 时 | 0–23 | 同上 |
| 日 | 1–31 | 同上，另支持 `L`（当月最后一天） |
| 月 | 1–12 | 同上，另支持 `JAN`–`DEC`（不区分大小写） |
| 周 | 0–7（0 和 7 都是周日） | 同上，另支持 `SUN`–`SAT` |

**宏**：`@hourly` `@daily` `@weekly`（周一 00:00）`@monthly` `@yearly`。

**不支持**：秒字段、年字段、`?`、`W`、`#`、`@reboot`、`@every`（用 `interval` 触发代替）。

**语义约定**：

- **日与周**：两者都不是 `*` 时为"或"（与 Vixie cron 一致，例如 `0 9 1 * MON` = 每月 1 号和每个周一）；任一为 `*` 时只看另一个。
- **时区**：按任务 `tz` 的本地时间匹配。
- **夏令时**：落在"跳过的那一小时"里的时刻，顺延到跳变后的第一分钟触发一次；落在"重复的那一小时"里的时刻，只在第一次出现时触发。（`Asia/Taipei` 无夏令时，但规则需对所有时区成立。）
- **找下一次**：从"当前分钟 + 1"开始按字段逐级跳进（月不匹配跳到下月 1 日 0 点，以此类推），最多向前搜索 5 年，找不到（如 `0 0 31 2 *`）在创建时报错。
- **校验输出**：解析成功后返回人类可读描述（"每天 22:00（Asia/Taipei）"）和未来 3 次触发时间，供模型向用户复述。

代码量估计 300 行以内，配表驱动测试（含跨月、闰年、`L`、日/周"或"语义、DST 跳变/重复，用 `America/New_York` 覆盖）。

## 6. 执行所在会话

| `session_mode` | 行为 | 适用 |
|---|---|---|
| `dedicated`（默认） | 每个任务一个固定会话，首次运行时创建，标题 `⏰ <任务名>`；历次运行都在同一会话里累积 | "每日总结"之类需要参考往次结果的任务 |
| `new` | 每次运行新建会话 | 彼此独立、不希望上下文增长的任务 |
| `origin` | 回到创建该任务的会话 | "两小时后在这里提醒我" |

- 触发时发送的用户消息：

  ```
  [定时任务] 每日工作总结 · 第 12 次 · 计划时间 2026-11-07 22:00 (Asia/Taipei)

  <prompt>
  ```

  消息 `Metadata` 带 `schedule_id`、`run_id`、`source=schedule`，客户端据此渲染成"定时触发"样式。
- `dedicated` 会话被用户删除时，下次运行自动重建；会话上下文的增长由现有的压缩/摘要机制处理。
- **目标会话正忙**（用户正在该会话里聊天，或 CLI 持有会话锁）：视为 overlap，按 `overlap` 策略处理；`origin` 模式下默认改为 `queue`。

## 7. 无人值守与提问

定时运行时用户大概率不在。模型调用 `ask_clarification` 时：

1. Run → `waiting_input`；会话流照常发出 `question` 事件；全局流发出 `schedule_run_updated`；hook 推送"任务「每日工作总结」需要你回答：…"。
2. 任意客户端用现有的 `answer` 接口回答 → Run 回到 `running`，turn 继续。
3. 超过 `answer_timeout` 无人回答：
   - `assume`：自动回答"用户暂时无法回应。请按最合理的假设继续完成任务，并在结果中列出你做的假设。"
   - `fail`：取消该 turn，Run 记 `failed`，`error = "no answer within 30m"`。

实现上由 bot 在调度触发的 turn 上包一层 `UserInteraction`（与现有 `turnInteraction` 组合），不需要改 `pkg/agent`。

## 8. 内置工具 `schedule`

一个工具，`action` 区分操作（与现有 `task` 工具风格一致，避免多个工具定义占用上下文）。只在 bot 进程注册。

### 8.1 参数

```jsonc
{
  "action": "create | list | get | update | pause | resume | delete | run_now | runs",
  "id": "sch_…",                          // get/update/pause/resume/delete/run_now/runs
  "name": "每日工作总结",                   // create/update
  "prompt": "汇总今天在本仓库的提交、PR 和未完成事项，按项目分组，最后列出明天的优先事项",
  "trigger": {"type": "cron", "expr": "0 22 * * *", "tz": "Asia/Taipei"},
  "session_mode": "dedicated",
  "notify": "always",
  "timeout": "30m",
  "end_at": null, "max_runs": null,
  "limit": 20                             // list/runs
}
```

### 8.2 返回（create 示例）

```json
{
  "id": "sch_3f9a1c",
  "state": "active",
  "summary": "每天 22:00（Asia/Taipei）",
  "next_runs": ["2026-11-07T22:00:00+08:00", "2026-11-08T22:00:00+08:00", "2026-11-09T22:00:00+08:00"],
  "session_mode": "dedicated"
}
```

校验失败返回结构化错误（`invalid_cron`、`interval_too_short`、`time_in_past`、`limit_exceeded`、`not_allowed_in_scheduled_run`…）和修正提示，由模型自行改正重试。

### 8.3 确认策略（`schedule.confirm`，默认 `ask`）

| 情况 | `ask`（默认） | `auto` |
|---|---|---|
| 交互 turn 中创建**周期**任务 | 工具内调用 `AskQuestion`，原文展示名称、规则描述、未来 3 次时间、prompt，选项「确认创建 / 取消」；确认才写入为 `active` | 直接 `active` |
| 交互 turn 中创建**一次性**任务 | 直接 `active`（用户刚提出，且执行前可随时取消） | 直接 `active` |
| `update` 修改 `trigger` 或 `prompt` | 同创建周期任务 | 直接生效 |
| 定时运行中调用 `create` | 拒绝（`not_allowed_in_scheduled_run`），可通过 `schedule.allow_nested` 放开，放开后也只能进入 `pending_approval` | 同左 |

`pending_approval` 的任务由用户在客户端批准（`POST /v1/schedules/{id}/approve`）或拒绝（删除）。

### 8.4 模型需要的上下文

- 每个 turn 的系统提示注入当前时间与时区：`当前时间：2026-11-07 12:45（Asia/Taipei，UTC+8）`。否则模型无法换算"今晚 10 点"。
- 工具描述里说明：优先用 `once.in` 表达相对时间；cron 只用第 5 节的子集；创建后必须把 `summary` 和 `next_runs` 复述给用户。
- 工具从 ctx 取当前会话（`tools.ThreadIDFromContext`，即 session ID），记入 `created_by`，`origin` 模式据此回投。
- 定时运行的 turn 在 ctx 中带 `schedule_id/run_id`，工具据此识别"是否处于定时运行中"。

### 8.5 权限

定时运行使用与 bot 相同的工具集和自治级别（bot 本身就是无人值守运行）。单任务工具白名单留到 P3。`prompt` 的全文在确认卡片中原样展示，用户批准的是确切会执行的内容。

## 9. 观察与干预（ws / sse / REST）

### 9.1 事件

**全局流**（`GET /v1/events`，WS `subscribe "*"`）新增类型，沿用现有事件信封：

| type | 载荷 | 时机 |
|---|---|---|
| `schedule_created` | `schedule` | 创建（含 `pending_approval`） |
| `schedule_updated` | `schedule` | 修改、暂停/恢复、批准、`next_run_at` 变化、`completed` |
| `schedule_deleted` | `schedule`（只含 id） | 删除 |
| `schedule_run_started` | `run` | Run 进入 `running`（带 `session_id/turn_id`） |
| `schedule_run_updated` | `run` | `waiting_input` ↔ `running` 等中间变化 |
| `schedule_run_finished` | `run` | 任一终态（含 `skipped/missed`） |

```jsonc
{"seq": 812, "type": "schedule_run_started", "time": "2026-11-07T14:00:00Z",
 "run": {"id": "run_a1b2", "schedule_id": "sch_3f9a1c", "seq": 12, "status": "running",
         "session_id": "…", "turn_id": "t_…", "scheduled_for": "2026-11-07T14:00:00Z"}}
```

**执行过程**：客户端拿到 `run.session_id` 后订阅该会话的事件流（`/v1/sessions/{id}/events` 或 WS `subscribe`），与人工聊天看到的完全相同。调度器不复制会话事件。

### 9.2 REST

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/schedules?state=active` | 列表 |
| POST | `/v1/schedules` | 创建（用户直接创建，不走确认） |
| GET | `/v1/schedules/{id}` | 详情（含最近 Run） |
| PATCH | `/v1/schedules/{id}` | 修改 |
| DELETE | `/v1/schedules/{id}` | 删除；有活跃 Run 时 `?cancel=true` 才允许 |
| POST | `/v1/schedules/{id}/pause` · `/resume` | 暂停 / 恢复 |
| POST | `/v1/schedules/{id}/approve` | 批准 `pending_approval` |
| POST | `/v1/schedules/{id}/run` | 立即执行一次（`trigger=manual`，受 overlap 约束） |
| POST | `/v1/schedules/{id}/skip` | 跳过下一次 |
| GET | `/v1/schedules/{id}/runs?limit=20` | 历史 Run |
| GET | `/v1/runs/{id}` | Run 详情 |
| POST | `/v1/runs/{id}/cancel` | 中止 Run（内部即会话 cancel → `cancelled`） |
| POST | `/v1/schedule/validate` | 只校验触发规则，返回描述与未来 N 次时间（客户端编辑器用） |

鉴权、Origin/Host 校验、JSON content-type 要求与现有接口一致。

### 9.3 WebSocket ops

`list_schedules`、`get_schedule`、`create_schedule`、`update_schedule`、`delete_schedule`、`pause_schedule`、`resume_schedule`、`approve_schedule`、`run_schedule`、`skip_schedule`、`list_runs`、`get_run`、`cancel_run`、`validate_trigger`，请求/响应形状与 REST 相同。

### 9.4 干预一览

| 想做的事 | 方式 |
|---|---|
| 看某次执行的实时过程 | 订阅 `run.session_id` 的会话流 |
| 中止正在跑的一次 | `cancel_run`（或直接对该会话 cancel） |
| 回答它的提问 | 现有 `answer` |
| 执行完后追问/纠正 | 直接往该会话发消息 |
| 停掉 / 恢复 / 改时间 / 改指令 | `pause` / `resume` / `update` |
| 马上跑一次 / 跳过下一次 | `run` / `skip` |
| 批准模型建的任务 | `approve` |

运行中途插话纠偏（steer）目前不支持（现有 turn 不能中途注入消息），第一版用"取消后重发"代替；见 BOT_DESIGN.md 后续路线第 1 条。

### 9.5 Web 控制台

侧栏新增"任务"页签：任务列表（名称、规则描述、下次时间、最近一次状态徽标）、待批准任务置顶并带「批准 / 拒绝」按钮；点开任务看 Run 历史，点某次 Run 跳到对应会话并定位到该 turn。

## 10. 通知（pkg/hook）

复用 `~/.deepai/config.yaml` 中已有的 `notifications` 配置，新增事件种类：

| kind | 时机 | Message 示例 |
|---|---|---|
| `schedule_run_end` | Run 终态且符合 `notify` | `「每日工作总结」完成：<output 前 500 字>` |
| `schedule_ask` | Run 进入 `waiting_input` | `「每日工作总结」需要你回答：<问题>` |
| `schedule_approval` | 出现 `pending_approval` 任务 | `模型请求创建任务「…」：每天 22:00` |

`Event` 增加可选字段 `schedule_id`、`run_id`，便于 webhook 端做深链接。bot 当前尚未接入 `pkg/hook`，P2 一并接入（同时覆盖 BOT_DESIGN.md 后续路线第 8 条）。

## 11. 配置

```yaml
# ~/.deepai/config.yaml
schedule:
  enabled: true              # false = 不注册工具、不启动调度器（接口返回 404）
  timezone: Asia/Taipei      # 缺省 = 系统时区
  confirm: ask               # ask | auto
  allow_nested: false        # 定时运行中是否允许创建任务（仍需批准）
  min_interval: 5m
  max_schedules: 50
  max_concurrent_runs: 2
  max_runs_per_day: 48
  run_history: 200
  default_timeout: 30m
  default_answer_timeout: 30m
```

`deepai-bot serve` 另加 `--no-schedule` 临时关闭。

## 12. 分阶段实现

| 阶段 | 内容 |
|---|---|
| **P1** | `pkg/schedule`：模型、存储与迁移、`once`/`interval` 触发、`Service`、`Scheduler`（含租约、先持久化再触发、overlap、并发上限）；`schedule` 工具（含 `ask` 确认、嵌套限制）；系统提示注入当前时间；`bot.scheduleDispatcher`；REST + WS + 全局事件；单元测试 + 用假 Dispatcher/假时钟的调度测试 |
| **P2** | 自写 cron（第 5 节）及表驱动测试；启动恢复与 misfire；`waiting_input` 与 `answer_timeout`；`pkg/hook` 接入 bot 与三个新事件；Web 控制台任务页签；文档与 E2E（mock LLM） |
| **P3** | 单任务工具白名单；运行中 steer（需 `pkg/agent` 配合）；任务链（A 完成后触发 B）；按结果条件通知 |

**测试策略**：`Scheduler` 依赖可注入的 `Clock` 接口（`Now()`、`After(d)`），测试中用假时钟推进时间，不 sleep；Dispatcher 用假实现记录调用；bot 层集成测试复用现有 `fakeRuntime`。

## 13. 关键取舍

| 取舍 | 理由 |
|---|---|
| 触发 = 普通 turn | 观察、干预、持久化、锁、压缩全部复用，调度器保持很小；客户端无需学新的执行协议 |
| 单个 `schedule` 工具 + `action` | 少占上下文；与 `task` 工具一致 |
| 服务端规范化并回显未来时间 | 模型算时间容易错，让服务端算、模型复述、用户确认 |
| 自写 cron 子集 | 零新依赖；规则写死、可完整测试；不支持的写法明确报错而不是猜 |
| 先持久化再触发 + 唯一约束 | 宁可漏一次（可见、可补跑），不重复执行（可能有副作用） |
| `dedicated` 默认 | 周期任务大多需要参考往次结果；上下文增长由现有压缩处理 |
| 周期任务默认需确认 | 周期任务会无人值守地反复执行，代价高；一次性任务风险低且执行前可取消 |
| 只在 bot 内 | CLI 是前台交互进程，无法保证到点时还在运行；两者独立，避免共享调度状态 |
