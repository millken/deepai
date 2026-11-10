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
    // Fire 启动一次执行并立即返回，不得等待执行或任何锁；最终状态通过
    // Service.FinishRun 回写。目标会话忙时返回 ErrBusy，Run 留在 pending。
    Fire(ctx context.Context, sch Schedule, run Run) (sessionID, turnID string, err error)
    // Cancel 只发出取消，立即返回；结束仍经由 FinishRun。
    Cancel(run Run, reason CancelReason) error
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
| `timezone` | string | IANA 时区名，`once`/`interval`/`cron` 共用，见 3.3；缺省 `schedule.timezone` → 系统时区，创建时固化写入 |
| `session_mode` | enum | `dedicated`（默认）/ `new` / `origin`，见第 6 节 |
| `session_id` | string | `dedicated` 模式下的固定会话（首次运行时惰性创建） |
| `model` | string | 空 = bot 默认模型 |
| `timeout` | duration | 单次运行的**执行时长**上限，默认 30m，上限 4h；不含排队时间和等待回答的时间（7.2）；超时 → Run `timed_out` |
| `max_tool_calls` | int | 0 = 沿用 bot 配置 |
| `overlap` | enum | 上一次未结束时：`skip`（默认）/ `queue` / `replace`，见 4.2 |
| `misfire` | enum | 停机期间错过的触发：`run_once`（默认，合并补跑一次）/ `skip` |
| `misfire_grace` | duration | 超过此宽限的错过触发一律 `missed`；默认 1h（一次性）/ 一个周期（周期性，最多 24h） |
| `answer_timeout` | duration | 无人值守时每个提问的等待上限，默认 30m，见第 7 节 |
| `on_answer_timeout` | enum | `assume`（默认，让模型按最合理假设继续）/ `fail` |
| `max_auto_answers` | int | 单次 Run 内 `assume` 自动回答的次数上限，默认 3；用尽后再超时按 `fail` 处理 |
| `notify` | enum | `always`（默认）/ `on_failure` / `never`，见第 10 节 |
| `end_at` / `max_runs` | time / int | 周期任务终止条件，可空 |
| `state` | enum | `active` / `paused` / `pending_approval` / `completed` / `deleted` |
| `created_by` | object | `{kind: "model"|"user", session_id, turn_id, client}` |
| `next_run_at` | time | 下一个计划槽位（3.2.1）；`paused`/`pending_approval` 时为空 |
| `last_run` | RunSummary | 最近一次的 `{id, status, ended_at}` |
| `run_count` | int | 已执行次数（不含 `skipped`/`missed`） |
| `created_at` / `updated_at` | time | |

**状态迁移**：

- `completed`：一次性任务执行完、或周期任务到达 `end_at`/`max_runs`。
- `deleted`：软删除，保留 30 天供查看历史 Run，之后清理。
- `pause`：清空 `next_run_at`；已在执行的 Run 不受影响；排队中（`pending`）的 Run 记为 `skipped`（`reason=paused`）。暂停期间的槽位不产生任何 Run。
- `resume`：从"现在"起重算 `next_run_at`（第一个 `> now` 的槽位），**不走 misfire 补跑**——暂停期间不执行是用户的明确意图。一次性任务若 `at` 已过，resume 返回错误 `time_in_past`，需 `update` 新时间。

### 3.2 Run（一次执行）

| 字段 | 说明 |
|---|---|
| `id` | `run_` + 随机 hex |
| `schedule_id`、`seq` | 所属任务、第几次 |
| `trigger` | `timer` / `misfire` / `manual`（run_now）/ `skip` |
| `slot` | 计划槽位（3.2.1）；`manual` 为空 |
| `scheduled_for` | 意图执行时间：有槽位时等于 `slot`，`manual` 为请求时刻 |
| `request_id` | `manual` 的幂等键（客户端或工具生成），可空 |
| `coalesced` | `misfire` 补跑合并掉的槽位数 |
| `queued_reason` | `pending` 时为何未开始：`overlap` / `concurrency` / `session_busy` |
| `queue_deadline` | 排队截止时间，过了仍未开始 → `skipped` |
| `session_id`、`turn_id` | 执行所在会话与 turn，客户端据此订阅执行过程 |
| `status` | 见下 |
| `auto_answers` | 本次 Run 已自动回答的提问数 |
| `cancel_requested_at` | 取消请求（来自本进程或备用进程，4.4） |
| `started_at` / `ended_at` | 实际开始 / 结束时间 |
| `output` | 最终回复（截断至 16 KB；全文在会话 transcript） |
| `error`、`error_code`、`usage`、`tool_calls` | 来自 `turn_end` 或调度器 |

```
pending ──► running ──┬──► completed
   │          ▲  │    ├──► failed
   │          │  ▼    ├──► timed_out
   │     waiting_input└──► cancelled
   │     （等待回答）
   ├──► skipped   （overlap=skip / 排队超时 / 暂停 / 用户 skip）
   └──► missed    （停机错过且超出宽限 / misfire=skip）
```

终态与 `turn_end` 一一对应（`completed/cancelled/timed_out/failed`），只多了调度器自己的 `pending/waiting_input/skipped/missed`。

`cancelled` 只表示**有意**的取消：客户端取消（`cancel_run` 或对会话 cancel），或 `overlap=replace` 被新一次顶替（`error_code=replaced`）。其余中断都算失败：bot 关闭 → `failed`（`error_code=bot_shutdown`）；提问无人回答 → `failed`（`error_code=answer_timeout`，7.1）。为此 `TurnSummary` 增加 `cancel_source: "client" | "shutdown" | "scheduler"`，Manager 在 Cancel / Close / 调度器取消时分别设置，调度器再结合自己记录的原因映射到 Run 状态。

#### 3.2.1 槽位与唯一约束

**槽位（slot）**是触发规则算出的计划时刻，精确到秒：`once` 为 `at`；`interval` 为 `start_at + k·every`；`cron` 为匹配的整分钟。唯一约束只加在槽位上：

```sql
UNIQUE(schedule_id, slot)      -- slot 为 NULL（manual）时不参与，SQLite 允许多个 NULL
UNIQUE(schedule_id, request_id)
```

| trigger | `slot` | `scheduled_for` | 唯一性 |
|---|---|---|---|
| `timer` | 计划槽位 | = slot | 槽位唯一：同一槽位只会触发一次 |
| `misfire` | 被补跑的**最近一个**错过槽位 | = slot | 同上；合并掉的较早槽位数记入 `coalesced`，不单独建 Run |
| `skip` | 被跳过的**下一个**未来槽位 | = slot | 插入一条 `skipped` Run 占住该槽位，再推进 `next_run_at`；启动恢复看到槽位已占用，不会再补跑 |
| `manual` | 空 | 请求时刻 | 不与任何槽位冲突，与 timer 同一分钟也允许；重复请求靠 `request_id` 去重（返回已有 Run） |

`next_run_at` = 第一个 `> now` 且**未被占用**的槽位。连续 `skip` 两次即占住接下来两个槽位。

`missed` 同样占住它所代表的槽位（停机错过、超出宽限时，为最近一个错过槽位写一条 `missed`，较早的记入 `coalesced`）。

### 3.3 Trigger 与时区

```jsonc
{"type": "once",     "at": "2026-11-07T22:00:00+08:00"}
{"type": "once",     "at": "2026-11-07T22:00"}          // 无偏移：按任务 timezone 解释
{"type": "once",     "in": "2h"}                         // 创建时换算成 at
{"type": "interval", "every": "6h", "start_at": "2026-11-08T09:00"}
{"type": "cron",     "expr": "0 22 * * *"}               // 按任务 timezone 匹配
```

- 时区只有一处：Schedule 的 `timezone` 字段。触发规则本身不带时区。
- 所有时刻存储为 UTC；`timezone` 用于：解释无偏移的 `at`/`start_at`、cron 匹配、展示。
- 带偏移的 `at`/`start_at` 按偏移解释，与 `timezone` 无关。
- 修改 `timezone` 视同修改 `trigger`（重算 `next_run_at`，`ask` 模式下需确认）。
- `once.at` 早于"现在 − 1 分钟"拒绝（`time_in_past`）。
- `interval.every` 最小值 = `schedule.min_interval`（默认 5m）；cron 两次触发间隔同样受此约束（校验时检查未来 10 次）。`start_at` 缺省 = 创建时刻 + `every`。
- **P1 不支持 `cron`**：创建时返回 `trigger_not_supported`，提示"cron 将在后续版本提供，请改用 interval 或 once"。

### 3.4 SQLite 表

建在 bot 已打开的 `~/.deepai` 库中，由 `pkg/schedule` 自己迁移（`CREATE TABLE IF NOT EXISTS` + `schema_version`），CLI 不触碰这几张表。

```sql
CREATE TABLE schedules (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prompt TEXT NOT NULL,
  trigger_json TEXT NOT NULL,
  timezone TEXT NOT NULL,
  options_json TEXT NOT NULL,          -- session_mode/timeout/overlap/misfire/notify/...
  session_id TEXT,
  state TEXT NOT NULL,
  created_by_json TEXT NOT NULL,
  next_run_at INTEGER,                 -- unix ms, NULL = 当前不会触发
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
  slot INTEGER,                        -- NULL = manual
  scheduled_for INTEGER NOT NULL,
  request_id TEXT,
  coalesced INTEGER NOT NULL DEFAULT 0,
  queued_reason TEXT, queue_deadline INTEGER,
  started_at INTEGER, ended_at INTEGER,
  session_id TEXT, turn_id TEXT,
  status TEXT NOT NULL,
  auto_answers INTEGER NOT NULL DEFAULT 0,
  cancel_requested_at INTEGER,
  output TEXT, error TEXT, error_code TEXT, usage_json TEXT,
  UNIQUE(schedule_id, slot),
  UNIQUE(schedule_id, request_id)
);
CREATE INDEX schedule_runs_by_schedule ON schedule_runs(schedule_id, seq DESC);
CREATE INDEX schedule_runs_active ON schedule_runs(status) WHERE status IN ('pending','running','waiting_input');

CREATE TABLE schedule_lease (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  owner TEXT NOT NULL,                 -- pid@host#start
  expires_at INTEGER NOT NULL
);

CREATE TABLE schedule_meta (           -- 单行；每次写 schedules/schedule_runs 时 version+1
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
```

Run 历史按任务保留最近 200 条（可配置），超出的在 janitor 中清理。

## 4. 调度器

### 4.1 调度循环：永不阻塞

单个 goroutine，内存中一个按时刻排序的最小堆 + 一个 `time.Timer`。堆里只有两类条目：

- **槽位到期**：某任务的 `next_run_at`；
- **排队截止**：某个 `pending` Run 的 `queue_deadline`。

循环体只做"取出到期条目 → 一个短事务 → 调一次非阻塞的 `tryStart`"，**不 sleep、不 Wait、不等任何业务事件**。等待前一次结束、等并发槽、等会话空闲，全部表示为 Run 停在 `pending`，由事件驱动重试。

唤醒来源（统一投递到循环的 `kick` channel，循环内串行处理，无需额外加锁）：

| 来源 | 动作 |
|---|---|
| 定时器到期 | 处理到期的槽位 / 排队截止 |
| 任务增删改、pause/resume/approve/skip | 重建该任务的堆条目 |
| `Service.FinishRun`（任一 Run 结束） | 对该任务、以及所有 `queued_reason=concurrency` 的 Run 调 `tryStart` |
| bot 全局流上任一会话的 `turn_end` | 对 `queued_reason=session_busy` 且目标为该会话的 Run 调 `tryStart` |
| 租约同步 tick（4.4，20s） | `schedule_meta.version` 变了就全量重载堆；处理 `cancel_requested_at` |
| 兜底 | 定时器最长 1 分钟必醒一次，防系统休眠 / 时钟跳变 |

**槽位到期**：

1. 事务内插入 Run（`trigger=timer`，`status=pending`，`slot=scheduled_for=槽位`）；`UNIQUE(schedule_id, slot)` 冲突说明已触发过，跳过。推进 `next_run_at` 到下一个未占用槽位；一次性任务置空。
2. 按 `overlap` 决定（该任务已有 `running`/`waiting_input` 的 Run 时）：
   - `skip`：新 Run 直接记 `skipped`（`reason=overlap`）。
   - `queue`：新 Run 保持 `pending`（`queued_reason=overlap`，`queue_deadline = slot + timeout`）。每个任务**最多一个**排队 Run；已有排队 Run 时，新的记 `skipped`（`reason=coalesced_into=<排队 run_id>`）。
   - `replace`：对前一次调 `Dispatcher.Cancel`（非阻塞，只发出取消），新 Run 保持 `pending`（`queued_reason=overlap`）；前一次的 `FinishRun` 到来时自然被 `tryStart` 启动。
3. 否则直接 `tryStart(run)`。

**`tryStart(run)`**（非阻塞，可被任意唤醒源重复调用，幂等）：

1. 该任务仍有活跃 Run → 保持 `pending`（`overlap`），返回。
2. 全局活跃数 ≥ `max_concurrent_runs` → 保持 `pending`（`concurrency`），返回。多个排队 Run 按 `scheduled_for` 先后出队。
3. 调 `Dispatcher.Fire`。它内部只做 `Manager.Send`，该调用本身立即返回 Turn，不等执行。
   - 返回 `ErrBusy`（目标会话正有 turn，或 CLI 持有会话锁）→ 保持 `pending`（`session_busy`），返回。
   - 其它错误 → Run 记 `failed`（`error_code=dispatch_failed`）。
   - 成功 → Run → `running`，记录 `session_id/turn_id/started_at`。
4. **排队截止**到期仍为 `pending` → `skipped`（`reason=queue_timeout`）。

**结束**：bot 在该 turn 的 `turn_end` 回调里调 `Service.FinishRun`：写入终态、输出、usage；按 `notify` 发通知（第 10 节）；检查 `end_at/max_runs` 决定是否 `completed`；然后 kick 循环。

**先持久化再触发**：进程在插入 Run 与 `FinishRun` 之间崩溃，最多丢失一次执行（启动时标记失败），绝不会重复执行。

### 4.2 overlap 小结

| 策略 | 新槽位到期而上一次仍在跑 | 何时真正开始 |
|---|---|---|
| `skip`（默认） | 新 Run 记 `skipped` | — |
| `queue` | 新 Run `pending`；最多排一个，多余的 `skipped` | 上一次 `FinishRun` 后由 `tryStart` 启动；超过 `queue_deadline` 未开始 → `skipped` |
| `replace` | 取消上一次，新 Run `pending` | 上一次结束（`cancelled`）后启动 |

`run_now`（manual）同样受 overlap 约束，但不记 `skipped`：`skip` 策略下直接返回 `409 run_in_progress`；`queue`/`replace` 下返回 `pending` 的 Run。

### 4.3 启动恢复

1. 尝试获取租约（4.4）。拿不到 → 进入**备用**模式，不执行以下步骤。
2. `running`/`waiting_input` 的 Run → `failed`（`error_code=bot_restarted`）。
3. `pending` 的 Run：未过 `queue_deadline` 的保留并重新排队；已过的 → `skipped`（`queue_timeout`）。
4. 对每个 `active` 任务，若 `next_run_at < now`（错过的槽位均未被占用）：
   - 最近一个错过槽位在 `misfire_grace` 内且 `misfire=run_once` → 为它建 `trigger=misfire` 的 Run 并 `tryStart`，其余记入 `coalesced`；
   - 否则为最近一个错过槽位写 `missed`，其余记入 `coalesced`；
   - 然后从 `now` 起重算 `next_run_at`。
5. 建堆，进入循环。

### 4.4 单实例租约与备用模式

同一个库只允许一个进程**执行**任务：`schedule_lease` 单行记录，持有者每 20s 续约、60s 过期；过期可被接管（与会话锁参数一致）。`UNIQUE(schedule_id, slot)` 是第二道保险。

**没拿到租约的进程（备用）照常读写库，只是不 Fire**：

| 操作 | 备用进程中的行为 |
|---|---|
| 列表 / 详情 / 历史 | 正常 |
| 创建 / 修改 / 暂停 / 恢复 / 删除 / 批准 / skip | 正常写库并 `version+1`；持有者在下一次同步 tick（≤ 20s）重载后生效 |
| `run_now` | 插入 `manual` 的 `pending` Run；由持有者在同步时启动 |
| `cancel_run` | 写 `cancel_requested_at`；持有者在同步时对该 Run 调 `Dispatcher.Cancel` |
| 观察执行过程 | Run 在持有者进程的会话里执行，事件流只在持有者那里；备用进程返回的 Run 带 `executor`（持有者地址），客户端应连到持有者观看 |

所有调度相关响应（含工具返回）都带 `"scheduler": "active" | "standby"`。备用模式下工具结果额外附一句提示："任务已保存，将由另一个 deepai-bot 实例（pid@host）执行"，避免模型误以为已在本实例生效。备用进程每 20s 尝试抢租约，持有者退出 60s 内自动接管（按 4.3 恢复）。

### 4.5 并发与资源

- 全局并发上限 `schedule.max_concurrent_runs`（默认 2），超出的 Run 以 `pending(concurrency)` 排队（4.1）；排队时间不计入 `timeout`。
- 每个任务单次运行受 `timeout`（7.2）和 `max_tool_calls` 约束。
- 全局活跃任务数上限 `schedule.max_schedules`（默认 50）；单任务每日运行上限 `schedule.max_runs_per_day`（默认 48，`manual` 也计入）。

## 5. cron 规则（自写）

只实现确定需要的子集，规则写死、行为可预测；不支持的写法在创建时报错并给出提示，由模型改写。

P2 提供；P1 对 `cron` 类型返回 `trigger_not_supported`（3.3）。

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
- **时区**：按 Schedule 的 `timezone`（3.3）的本地时间匹配。
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
- **目标会话正忙**（用户正在该会话里聊天，或 CLI 持有会话锁）：这不是任务自身的重叠，不看 `overlap`；Run 以 `pending(session_busy)` 排队，该会话 `turn_end` 后由 `tryStart` 启动，`queue_deadline = slot + timeout`（4.1）。

## 7. 无人值守与提问

定时运行时用户大概率不在，模型调用 `ask_clarification` 不能把 turn 永远挂住，也不能让"还在等用户回答"被算成超时。

### 7.1 流程

1. 模型提问 → Run → `waiting_input`；会话流照常发出 `question` 事件；全局流发出 `schedule_run_updated`；hook 推送"任务「每日工作总结」需要你回答：…"（第 10 节）。
2. 任意客户端用现有的 `answer` 接口回答 → Run 回到 `running`，turn 继续。
3. 某个提问超过 `answer_timeout` 无人回答：
   - `on_answer_timeout=assume` 且 `auto_answers < max_auto_answers`：以 bot 身份自动回答"用户暂时无法回应。请按最合理的假设继续完成任务，并在结果中列出你做的假设。"；`question_answered` 事件带 `auto: true`；`auto_answers+1`；Run 回到 `running`。
   - 否则（`fail`，或自动回答次数已用尽）：以 `cancel_source=scheduler` 取消该 turn，Run 记 `failed`（`error_code=answer_timeout`，`error="no answer within 30m"`）。

### 7.2 时钟：等待回答时暂停执行超时

一个 Run 涉及三个时长，互不叠加：

| 时钟 | 计时区间 | 到期结果 |
|---|---|---|
| 排队（`queue_deadline`） | `pending` 期间 | `skipped(queue_timeout)` |
| 执行（`timeout`） | `running` 期间；**`waiting_input` 期间暂停** | `timed_out` |
| 回答（`answer_timeout`） | 每个提问从发出到被回答 | 见 7.1 第 3 步 |

- **执行时钟可暂停**：Manager 的 turn 超时从 `context.WithTimeout` 改为"剩余额度 + 定时器"的 `turnClock`，提供 `Pause()`/`Resume()`（引用计数：有任一提问未答即暂停，全部答完才恢复，覆盖子代理并发提问）。到期时以 `context.WithCancelCause` 取消，cause 为 `context.DeadlineExceeded`；状态映射改看 `context.Cause(ctx)`，结果仍是 `timed_out`。
- **同一时刻只有一个时钟能取消 turn**：`waiting_input` 时执行时钟已暂停，只有回答时钟在走；回答后回答时钟停止，执行时钟从剩余额度继续。因此不会出现"用户还没答就 `timed_out`"，也不会双重取消。
- 用户在 `waiting_input` 期间 `cancel_run` → 停掉所有时钟，Run 记 `cancelled`。
- **生效范围**：暂停规则对所有 turn 生效，包括交互会话里的 `--turn-timeout`（这是对 #40 Manager 行为的一处修改，交互 turn 不做自动回答）。
- **两个上限同时存在时**：定时 turn 的执行额度 = `min(Schedule.timeout, --turn-timeout)`（后者为 0 表示不限）；通过 Manager 的内部 Send 选项传入，不暴露给普通客户端。

### 7.3 实现位置

回答时钟和自动回答由 bot 层的 `scheduleDispatcher` 实现：Manager 在 `turnInteraction` 上暴露 `turnHooks`（`QuestionAsked` / `QuestionSettled` / `TurnEnded`），调度器为每个定时 turn 注入 `scheduleQuestionHooks`，负责每个提问的回答时钟、`assume` 自动回答、`auto_answers` 计数，并通过 `Service.UpdateRunInteraction` 回写 `waiting_input` ↔ `running`。执行时钟的 `Pause()` / `Resume()` 由 Manager 在提问时统一处理（对所有 turn 生效），因此定时 turn 不需要包一层 `UserInteraction`，`pkg/agent` 也不需要修改。调度循环不参与等待（4.1）。

## 8. 内置工具 `schedule`

一个工具，`action` 区分操作（与现有 `task` 工具风格一致，避免多个工具定义占用上下文）。只在 bot 进程注册。

### 8.1 参数

```jsonc
{
  "action": "create | list | get | update | pause | resume | delete | run_now | runs",
  "id": "sch_…",                          // get/update/pause/resume/delete/run_now/runs
  "name": "每日工作总结",                   // create/update
  "prompt": "汇总今天在本仓库的提交、PR 和未完成事项，按项目分组，最后列出明天的优先事项",
  "trigger": {"type": "cron", "expr": "0 22 * * *"},
  "timezone": "Asia/Taipei",              // 缺省 schedule.timezone → 系统时区
  "session_mode": "dedicated",
  "notify": "always",
  "timeout": "30m",                       // 执行时长上限，等待回答期间暂停
  "answer_timeout": "30m",                // 每个提问的等待上限
  "on_answer_timeout": "assume",          // assume | fail
  "max_auto_answers": 3,
  "max_tool_calls": 0,                    // 0 = 沿用 bot 配置
  "end_at": null, "max_runs": null,
  "request_id": "…",                      // run_now 幂等键，缺省由工具按 turn_id 生成
  "limit": 20                             // list/runs
}
```

### 8.2 返回（create 示例）

```json
{
  "id": "sch_3f9a1c",
  "state": "active",
  "scheduler": "active",
  "timezone": "Asia/Taipei",
  "summary": "每天 22:00（Asia/Taipei）",
  "next_runs": ["2026-11-07T22:00:00+08:00", "2026-11-08T22:00:00+08:00", "2026-11-09T22:00:00+08:00"],
  "session_mode": "dedicated"
}
```

校验失败返回结构化错误（`invalid_cron`、`trigger_not_supported`、`invalid_timezone`、`interval_too_short`、`time_in_past`、`limit_exceeded`、`not_allowed_in_scheduled_run`、`confirmation_not_received`、`run_in_progress`…）和修正提示，由模型自行改正重试。

### 8.3 确认策略（`schedule.confirm`，默认 `ask`）

| 情况 | `ask`（默认） | `auto` |
|---|---|---|
| 交互 turn 中创建**周期**任务 | 工具内调用 `AskQuestion`，原文展示名称、规则描述、未来 3 次时间、prompt，选项「确认创建 / 取消」；确认才写入为 `active` | 直接 `active` |
| 交互 turn 中创建**一次性**任务 | 直接 `active`（用户刚提出，且执行前可随时取消） | 直接 `active` |
| `update` 修改 `trigger`、`timezone` 或 `prompt` | 同创建周期任务 | 直接生效 |
| 定时运行中调用 `create` | 拒绝（`not_allowed_in_scheduled_run`），可通过 `schedule.allow_nested` 放开，放开后也只能进入 `pending_approval` | 同左 |

`pending_approval` 的任务由用户在客户端批准（`POST /v1/schedules/{id}/approve`）或拒绝（删除）。

**确认不留半成品**：`ask` 流程是"校验 → `AskQuestion` → 用户确认 → 再次校验 → 单个事务写入"。确认之前库里没有任何记录；用户选择取消、turn 被取消、或超过 `schedule.confirm_timeout`（默认 10m）未确认，工具都直接返回 `confirmation_not_received`，不写库。确认后的再次校验用于处理等待期间的变化（一次性任务时间已过、任务数已达上限等），失败则返回对应错误。`update` 同理：确认前不修改原任务。

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
| POST | `/v1/schedules/{id}/run` | 立即执行一次（`trigger=manual`，受 overlap 约束，见 4.2）；可带 `request_id` 幂等 |
| POST | `/v1/schedules/{id}/skip` | 跳过下一次（插入 `skipped` Run 占住该槽位，见 3.2.1） |
| GET | `/v1/schedules/{id}/runs?limit=20` | 历史 Run |
| GET | `/v1/runs/{id}` | Run 详情 |
| POST | `/v1/runs/{id}/cancel` | 中止 Run（内部即会话 cancel → `cancelled`） |
| POST | `/v1/schedule/validate` | 只校验触发规则，返回描述与未来 N 次时间（客户端编辑器用） |

鉴权、Origin/Host 校验、JSON content-type 要求与现有接口一致。备用模式（4.4）下所有接口照常可用，响应带 `scheduler: "standby"`。

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

`notify` 的含义：

| 设置 | 推送 `schedule_run_end` 的终态 |
|---|---|
| `always` | `completed`、`failed`、`timed_out`、`missed`、`cancelled` |
| `on_failure` | `failed`（含 `answer_timeout`、`bot_restarted`、`bot_shutdown`）、`timed_out`、`missed` |
| `never` | 不推送 |

`cancelled` 是用户主动操作，不算失败；`skipped` 是策略的预期结果，任何设置都不推送。`schedule_ask` 和 `schedule_approval` 需要人处理，除 `never` 外总是推送。

`Event` 增加可选字段 `schedule_id`、`run_id`，便于 webhook 端做深链接。bot 当前尚未接入 `pkg/hook`，P2 一并接入（同时覆盖 BOT_DESIGN.md 后续路线第 8 条）。

## 11. 配置

```yaml
# ~/.deepai/config.yaml
schedule:
  enabled: true              # false = 不注册工具、不启动调度器（接口返回 404）
  timezone: Asia/Taipei      # 缺省 = 系统时区
  confirm: ask               # ask | auto
  confirm_timeout: 10m
  allow_nested: false        # 定时运行中是否允许创建任务（仍需批准）
  min_interval: 5m
  max_schedules: 50
  max_concurrent_runs: 2
  max_runs_per_day: 48
  run_history: 200
  default_timeout: 30m
  default_answer_timeout: 30m
  default_max_auto_answers: 3
```

`deepai-bot serve` 另加 `--no-schedule` 临时关闭。

## 12. 分阶段实现

| 阶段 | 内容 |
|---|---|
| **P1** | `pkg/schedule`：模型、存储与迁移、`once`/`interval` 触发（`cron` 明确拒绝）、任务级 `timezone`、槽位与唯一约束、`Service`、非阻塞 `Scheduler`（overlap、并发、会话忙排队）、租约与备用模式、启动恢复第 1–3 步（错过的槽位只记 `missed`）；`schedule` 工具（`ask` 确认、嵌套限制）；系统提示注入当前时间；`bot.scheduleDispatcher`；Manager 可暂停的 `turnClock` 与 `cancel_source`；`waiting_input` 与 `answer_timeout`；REST + WS + 全局事件；单元测试 + 假 Dispatcher/假时钟的调度测试 |
| **P2** | 自写 cron（第 5 节）及表驱动测试；misfire 补跑（`run_once`）；`pkg/hook` 接入 bot 与三个新事件；Web 控制台任务页签；文档与 E2E（mock LLM） |
| **P3** | 单任务工具白名单；运行中 steer（需 `pkg/agent` 配合）；任务链（A 完成后触发 B）；按结果条件通知 |

**测试策略**：`Scheduler` 依赖可注入的 `Clock` 接口（`Now()`、`After(d)`），测试中用假时钟推进时间，不 sleep；Dispatcher 用假实现记录调用；bot 层集成测试复用现有 `fakeRuntime`。

### 12.1 P1 实现状态（2026-11-07，已全部完成）

| 已落地 | 位置 |
|---|---|
| 模型、`Store` 接口、内存 + SQLite 存储与迁移（含 `UNIQUE(schedule_id, slot)` / `request_id`、租约、version、历史清理） | `pkg/schedule/{types,trigger,store,store_sqlite}.go` |
| `once` / `interval` 触发、任务级时区、`cron` 明确拒绝、槽位唯一与 skip 占槽 | `pkg/schedule/trigger.go`、`service.go` |
| `Service`（CRUD / pause / resume / approve / skip / run_now / cancel / validate / finish / 启动恢复 / 每日上限 / `UpdateRunInteraction`） | `pkg/schedule/service.go` |
| 非阻塞 `Scheduler`（最小堆 + 单定时器 + kick、overlap、并发、会话忙排队、queue_deadline、租约与备用模式） | `pkg/schedule/scheduler.go` |
| `schedule` 工具（`action` 全套、`ask` 确认不留半成品、嵌套限制、返回当前时间与 `next_runs`、无人值守参数） | `pkg/schedule/tool.go`、`api.go` |
| REST（§9.2 全部路径）、WS（§9.3 全部 ops）、6 类全局 `schedule_*` 事件 | `pkg/bot/schedule_http.go`、`protocol.go`、`server.go`、`ws.go` |
| `bot.scheduleDispatcher`：`Manager.SendWithOptions` → 普通 turn → `turn_end` 回写 `FinishRun`；`dedicated/new/origin` 会话；`bot_shutdown` / `answer_timeout` / `replaced` 映射 | `pkg/bot/{schedule,schedule_questions}.go`、`manager.go` |
| §7.2 可暂停 `turnClock`（剩余额度 + 引用计数 `Pause/Resume` + `WithCancelCause`）与 `cancel_source`（client / shutdown / scheduler）；执行额度 = `min(Schedule.timeout, --turn-timeout)` | `pkg/bot/{turnclock,manager,protocol}.go` |
| §7.1 `waiting_input`、`answer_timeout`、`assume` 自动回答（`question_answered.auto`）、`max_auto_answers` 上限、客户端回答后恢复 `running` | `pkg/bot/schedule_questions.go` |
| 单任务 `max_tool_calls`（`TurnInput` per-turn 覆盖，不改 `pkg/agent`） | `pkg/bot/{runtime,agentruntime,schedule}.go` |
| 装配：`schedule:` 配置段、`--no-schedule`、工具注册、调度器启停、系统提示注入当前时间、优雅关闭顺序 | `cmd/deepai-bot/{schedule,runtime,serve}.go`、`pkg/commands/setup.go` |

**说明与后续**：

1. 备用进程只写库不执行；`run_now` / 修改 / `cancel_requested_at` 由租约持有者在下一次同步 tick（≤ 20s）重载后生效，响应统一带 `scheduler: "standby"`。
2. 按 §12 属 **P2**：`notify` 与 `pkg/hook` 的三个新事件（含 `schedule_ask` 推送）、Web 控制台任务页签、`misfire=run_once` 补跑、自写 cron。
3. `answer_timeout` 到期后 `assume` 自动回答会写回 `auto_answers`，`fail` / 次数用尽时以 `cancel_source=scheduler` 取消 turn，Run 记 `failed(error_code=answer_timeout)`；等待回答期间执行时钟暂停，不会误判 `timed_out`。

### 12.2 PR #43 评审 round 1 修复（2026-11-08）

| 级别 | 问题 | 修复 |
|---|---|---|
| H1 | `RenewLease` 瞬时错误即降级 standby，接管方 `Recover` 判死仍在执行的 Run（双跑） | `syncLease` 区分 `err`（租约未到期则保持 active、下个 sync 重试）、`ok=false`（确认被接管才降级）与"错误持续超过 leaseTTL"；新增 `RecoverAfterTakeover` / `Reconcile`：接管时按 `StartedAt + leaseTTL + 任务 timeout` 给在跑的 Run 宽限期，到期后由租约持有者补判死，迟到的 `FinishRun` 优先 |
| H2 | dedicated 会话被删后重建的新会话不回写 Schedule，每轮泄漏一个会话 | `tryStartLocked` 回写条件改为 `SessionMode==dedicated && sid != "" && v.SessionID != sid`，Fire 返回的新 ID 每轮都会固定下来 |
| M1 | 激活已过期的 `once` → active 且 `NextRunAt=nil` 死端 | `Approve` 复用 `checkOnceNotPast`（与 `Resume` 一致），返回 `time_in_past` |
| M2 | standby 堆顶已到期 → `delay=0` 热自旋 | standby 下 delay 施加 1s 下限，接管仍由 sync tick 驱动 |
| M3 | 先 Fire 后落 `running`，崩溃后 pending Run 被重放 | Fire 前静默预写 `running`（含 `StartedAt`），`ErrBusy` 回滚 pending 并保留原 deadline；恢复只判死、不重放 |
| M4 | 并发提问 `waiting_input`/`running` 回写乱序 | `scheduleQuestionHooks` 的状态写入与 pending map 变更同处 `h.mu` 临界区 |
| L1 | `FireSlot` 吞掉一切落库错误 | 只吞 `ErrConflict`，其余记 Warn 并返回错误 |
| L2 | `Contains(a, "no")` 把肯定回答误判为取消 | `classifyConfirmation`：选项值精确匹配 + 英文整词/短语匹配，歧义回答仍不创建 |
| L3 | `answer_timeout(fail)` 后 settle 仍写一次 `running` | `h.failure` 非空时 settle 不回写 running |
| L4 | `Closing()` 兜底把关停窗口内的客户端取消误标 `bot_shutdown` | `finalizeOutcome` 仅在 `cancel_source` 为空且正在关闭时才兜底 |

回归测试集中在 `pkg/schedule/review_test.go` 与 `pkg/bot/review_test.go`。

## 13. 关键取舍

| 取舍 | 理由 |
|---|---|
| 触发 = 普通 turn | 观察、干预、持久化、锁、压缩全部复用，调度器保持很小；客户端无需学新的执行协议 |
| 单个 `schedule` 工具 + `action` | 少占上下文；与 `task` 工具一致 |
| 服务端规范化并回显未来时间 | 模型算时间容易错，让服务端算、模型复述、用户确认 |
| 自写 cron 子集 | 零新依赖；规则写死、可完整测试；不支持的写法明确报错而不是猜 |
| 先持久化再触发 + 槽位唯一约束 | 宁可漏一次（可见、可补跑），不重复执行（可能有副作用） |
| 调度循环永不阻塞，等待全部表示为 `pending` | 一个任务排队不会拖住其它任务；所有等待都可见、可持久化、可恢复 |
| 等待回答时暂停执行超时 | 无人值守时提问很常见，不能把"等人"算成"执行太慢" |
| 备用进程可写库、不执行 | 多实例下模型创建任务不会静默失败；执行仍只有一个进程 |
| `dedicated` 默认 | 周期任务大多需要参考往次结果；上下文增长由现有压缩处理 |
| 周期任务默认需确认 | 周期任务会无人值守地反复执行，代价高；一次性任务风险低且执行前可取消 |
| 只在 bot 内 | CLI 是前台交互进程，无法保证到点时还在运行；两者独立，避免共享调度状态 |

## 附录：评审修订记录

| 评审意见（#41） | 修订位置 |
|---|---|
| `overlap=queue` 不能堵调度 goroutine | 4.1 重写为非阻塞循环 + `tryStart`；4.2 overlap 小结；2.1 `Dispatcher` 约束 |
| `answer_timeout` 与 turn `timeout` 的关系 | 7.2：等待回答时暂停执行时钟，同一时刻只有一个时钟能取消 turn |
| `UNIQUE` 与 `run_now` / `skip` | 3.2.1：唯一约束改加在 `slot` 上；manual 无槽位 + `request_id` 幂等；skip 插 `skipped` 占槽 |
| 无租约时的"只读" | 4.4：备用进程照常读写库、不执行；响应带 `scheduler` 字段 |
| 任务级时区 | 3.1 / 3.3：Schedule 增加 `timezone`，三种触发共用，触发规则不再带 `tz` |
| `assume` 轮次上限 | 3.1 `max_auto_answers`、7.1 |
| 确认超时不留半成品 | 8.3 |
| `on_failure` 的范围 | 第 10 节表格；3.2 `cancel_source` |
| P1 拒绝 `cron` | 3.3、第 5 节、第 12 节 |
| resume 时 `next_run_at` 已过 | 3.1 状态迁移：从现在重算，不走 misfire |
