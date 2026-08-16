# 子代理可见性与取消 — 设计

- 日期：2026-08-16
- 状态：已实现
- 涉及：`pkg/subagent`、`pkg/agent`、`pkg/chat`、`pkg/commands`

## 问题

并行分派多个 `task` 后，子代理运行期间对用户是黑盒。一次真实会话（`20260815_144657_1d76`）分派 7 个只读审查子代理，49 分钟后只有 1 个返回结果，期间无法判断其余的是在正常工作还是已经卡死，也无法只终止卡住的那个——只能 Ctrl+C 连同已经跑了几分钟的其他任务一起丢掉。

三个具体缺口：

1. **只显示"此刻在调什么"**，看不到历史。`task_running` 事件只带一个自由文本 `Message`，TUI 直接整行替换。
2. **完成即消失**。`clearSubagentLine` 在 `task_completed` 时立刻移除该行（`pkg/chat/tui.go`），先结束的任务从画面上蒸发，看起来像"只分派了一个"。
3. **取消只有全局**。`InterruptCh` 是唯一入口，粒度是整轮。

## 目标

- 每个运行中的子代理能看到：当前工具、累计工具调用数、累计 token、耗时。
- 能展开单个子代理，查看它到目前为止的完整活动流。
- 能只取消其中一个子代理，其余继续运行。

## 非目标

- 不把子代理的完整消息流实时透传到主界面（Claude Code 的 `ProgressMessage` 模型）。4 路并发时刷屏会淹没主对话，收益不抵成本。
- 不做后台/异步子代理（Claude Code 的 async agent + 双击 Esc 杀后台）。当前所有子代理都与父轮次同生命周期。
- 不改子代理的调度与并发策略。

## 设计

### A. 事件层：`TaskEvent` 结构化

`pkg/subagent/events.go` 当前是纯字符串事件：

```go
type TaskEvent struct {
    Type, TaskID, RequestID, Description, Message, Result, Error string
}
```

新增字段：

| 字段 | 用途 |
|---|---|
| `AgentType` | 子代理类型 |
| `ToolName` | 本次事件对应的工具名 |
| `ToolArgs` | 简短参数摘要（如 `src/app.zig`） |
| `ToolStatus` | `running` / `ok` / `error` |
| `DurationMS` | 该工具耗时 |
| `ToolCalls` | 该子代理累计工具调用数 |
| `Tokens` | 该子代理累计 token |

字段全部可选，旧的 `Message` 保留——降级路径不变，没有新字段的事件仍按原样渲染。

**顺带修掉的脆弱代码**：TUI 目前从 `Description` 里用字符串抠 `[...]` 得到 agent 类型（`pkg/chat/tui.go` 的 `task_started` 分支）。描述里只要出现方括号就会抠错。有了 `AgentType` 字段后删除这段。

发射点：`pkg/subagent/pool.go` 与 `pkg/agent/subagent.go` 的 `task_running` 处。

### B. 展示层

`subagentTaskLine` 扩展：`agentType`、`currentTool`、`toolCalls`、`tokens`、`status`、`history []toolEntry`。

`history` 是**环形缓冲，上限 50 条**——长跑子代理的活动流必须有界，否则一个跑两小时的任务会无限增长。超出时丢最旧的，展开视图顶部标注"更早的 N 条已省略"。

渲染改为树形块：

```
并行分派 4 个 review 任务：
  ├─ ⠙ [zi-core]  Zig 生命周期与内存安全
  │     read_file src/app.zig · 12 工具 · 47k · 2m14s
  ├─ ✓ [ui-perf]  ui 组件性能
  │     完成 · 23 工具 · 88k · 1m52s
  └─ ⠹ [gradient] 渐变解析器
        edit_file gradient.zig · 5 工具 · 19k · 2m14s
```

**行为变更**：完成的任务不再立即移除，改为标记 resolved 后继续留在块内；整轮结束时把整块一次性提交到 scrollback。这是问题 2 的直接修复。

`maxLiveSubagentLines`（当前 5）继续生效并保留溢出提示；展开态下被展开的那一个不受该上限约束。

### C. 交互：独立的任务模式

`↑`/`↓` 在 TUI 里已被占用——先是补全候选，其次是输入历史（`pkg/chat/tui.go` 的 `"up"` / `"down"` 分支）。任务一运行就抢走方向键会破坏日常输入，因此**不劫持默认行为**。

新增 `Ctrl+T` 进入/退出「任务模式」，仅在该模式内：

| 键 | 行为 |
|---|---|
| `↑` / `↓` | 选择任务 |
| `Enter` | 展开 / 折叠选中项 |
| `Ctrl+X` | 取消选中项 |
| `Esc` | 退出任务模式 |

模式行：`[任务模式 · ↑↓ 选择 · Enter 展开 · Ctrl+X 取消 · Esc 退出]`。

无运行中任务时 `Ctrl+T` 无效果。代价是多一次按键，换来的是任务运行期间输入历史仍可用。

### D. 取消链路

`pool.go` 的 `runTask` 已经为每个任务建了独立 ctx，但 cancel 就地 `defer` 丢弃。改为存到 `Task` 上，新增：

```go
func (p *Pool) CancelTask(taskID string) bool
```

取消后走既有的 `TaskStatusCancelled` 路径，发 `task_cancelled` 事件——**tool_result 配对不变**，因为取消的任务仍会由 `runOneTool` 返回一个失败结果，既有不变量（每个 tool_use ID 必须有配对的 tool_result）自动成立。

TUI 侧新增 `CancelTaskCh() <-chan string`，加入 `pkg/chat/repl.go` 的 ui 接口。

**原未决点已解决，用直连，不需要进程内注册表**：pool 在 `pkg/commands/chat.go` 的 `registerChatTools` 里创建，而该函数在 `chat.NewRepl` **之前**调用——组合根同时持有两者。改为让 `registerChatTools` 返回 pool，经 `ReplConfig.TaskCanceller` 传入。该字段的类型是只含 `CancelTask(string) bool` 的窄接口，REPL 因此不依赖 pool 的具体类型，nil 时退化为「只有 Ctrl+C 整轮取消」。

**单点取消不结束整轮**：repl 的信号监听 goroutine 从一次性 select 改为循环——收到 taskID 只调 `CancelTask`，只有 `interruptCh` 才 `turnCancel()`。丢掉卡死的那一个、让其余跑完，正是这个功能的全部意义。

## 测试

| 用例 | 钉住的行为 |
|---|---|
| 事件字段端到端 | 子代理工具调用 → `TaskEvent` 带 `ToolName`/`ToolCalls`/`Tokens` |
| resolved 保留 | 完成的任务在整轮结束前不从块内消失 |
| scrollback 不重复 | 延迟提交后，完成信息只输出一次 |
| 方向键回归 | 任务运行中，非任务模式下 `↑`/`↓` 仍走补全/历史 |
| 历史有界 | 超过 50 条后丢最旧，不无限增长 |
| 单点取消 | `CancelTask` 只终止目标，其余任务跑完 |
| 配对不变量 | 取消后每个 tool_use 仍有配对 tool_result |

## 风险

**完成行延迟提交改变 scrollback 时序**，可能与现有 `commitWithFlush` 重复输出。这是本设计里最容易出岔子的地方，用上表"scrollback 不重复"一条专门钉住。

其余改动都是加法（新字段、新模式、新方法），旧路径在字段缺失时按原样降级。

## 实现记录

设计之外、实现时才发现的四点：

**1. 自由文本渲染路径险些被丢掉。** 新渲染器全面改用结构化字段后，只填 `Message`、不填 `ToolName` 的事件不再显示任何进度——直接违背了本文「Message 保留，旧消费方渲染不变」的承诺。补 `lastMessage` 兜底字段修复，由 `TestHandleSubagentEvent_MultiTask_RunningUpdatesOnlyThatTask` 钉住。

**2. 两处渲染缺陷只有跑真实渲染才看得出来**，读代码看不出来：最后一项的详情行在 `└─` 之后仍画竖线；已完成任务的详情行把自己的描述重复打印一遍。详情行改为接收树形前缀，已完成状态显示「完成」而非回显描述。

**3. `Pool.Wait` 会回收任务条目**（有意为之：否则整份 transcript 保留到进程退出），所以 `Wait` 之后 `GetTask` 必然返回 false。对应的真实场景是「用户在任务刚好自己结束的瞬间按下 Ctrl+X」——`CancelTask` 对已回收的 ID 返回 false 而非 panic。

**4. 四个既有 TUI 测试编码的是被本设计取代的旧行为**（终止事件立即提交并清除），按新语义更新了断言、保留其原本意图（任务间互不干扰）。其中超出显示上限的那条更严格了：断言「从未在实时区渲染过的任务，仍必须出现在轮次结束的块里」。
