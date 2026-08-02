# DeepAI 架构 Review 与优化路线

> 基于 2026-08-02 的代码现状（HEAD `f793e8c`），聚焦多 agent 协同与"团队意识"改动。
> 所有结论均经代码核实，引用格式为 `file:line`。

---

## TL;DR

1. **"团队意识"改动（`f793e8c`）方向正确**：动态 catalog 注入、防递归、plan-mode 逃逸防护、测试覆盖都到位。但它建立在一条**实际是串行**的委派链路上——`task` 工具未标 `ParallelSafe`，ReAct 循环永远串行执行它，池的并发 4 从 CLI 路径不可达。"团队协作"目前是排队。
2. **两个 P0 级 bug**：① 共享工具注册表被每回合新建的 agent 污染（`enter_plan_mode` 闭包绑定死 agent、子代理从第 2 回合起继承它）；② 并行工具路径漏掉了重复调用熔断器（注释声称与串行一致，实际只抄了一半）。
3. **子代理是成本黑洞**：token 用量在 executor 层被完全丢弃，TUI 统计不含任何子代理消耗；无跨 agent 树预算。
4. **文档严重过时**：`AUTONOMOUS_MULTI_AGENT.md` §9 声称已落地的 orchestrator（implement_task/design_task/build_task/黑板）已于 `87772b6` 整体删除；`MULTI_AGENT.md` 描述的 Environment 消息总线从未存在。
5. **死代码约占非测试代码的 20%**：`pkg/gateway`（无命令接线）、`pkg/checkpoint`（仅被 gateway 引用）、`pkg/plugin`（in-process 插件框架，零调用方）、clarification 的 async Manager 分支、`builtin.GitTools()` 空壳。
6. 建议路线：**不要重建确定性编排层**（已被实测否决一次），继续 model-driven 路线，但把投入放在三个缺失的原语上：**真并发委派、可信的结构化交接、跨树 token 预算**。

---

## 1. 架构总览

```
cmd/deepai
└── pkg/commands (CLI 装配: config → llm.ModelRegistry → tools.Registry
    │             → claudeplugin 发现 → agent catalog → skills → MCP → SQLite/memory)
    └── pkg/chat (REPL/TUI: 每回合 new 一个一次性 Agent)
        └── pkg/agent (ReAct 循环 + plan mode + 压缩/aging/offload + SubagentExecutor)
            ├── pkg/tools (注册表 + builtin 工具 + task 工具)
            ├── pkg/subagent (任务池: 信号量并发 + 生命周期事件)
            ├── pkg/llm (Anthropic / OpenAI-compat 两个 provider + 别名注册表)
            ├── pkg/memory (SQLite 记忆: 注入 + 异步抽取)
            ├── pkg/skill (SKILL.md → 系统提示注入)
            └── pkg/sandbox
    [未接线] pkg/gateway (+pkg/checkpoint) / pkg/plugin / pkg/clarification.Manager
```

**关键设计事实**（决定多 agent 上限）：

- Agent 实例**一次性使用**（`react.go:242-248`），REPL 每回合重建（`chat/repl.go:539`），会话状态以消息列表形式在回合间传递。
- 多 agent 是**严格两层、同步阻塞**：主 agent → `task` 工具 → `pool.StartTask` + `Wait`（`tools/subagent.go:62-84`）→ 子 agent 从单条 prompt 从零起跑（`agent/subagent.go:171-179`），无历史、无记忆（`subagent.go:136-147` 未传 `MemoryService`）、结果为纯文本。
- 递归深度**结构性封顶为 1**：`selectSubagentTools` 无条件剥离 `task`（`agent/subagent.go:217-219`），两条 fallback 也过滤（`:201, :236`）。
- 委派"意识"由 `f793e8c` 注入：交互式主 agent + 持有 task 工具 + catalog 非空时，系统提示追加策略文字 + 动态渲染的 agent 目录（`react.go:896-905, 983-1021`）。

---

## 2. 多 agent 协同专题

### 2.1 演进史（重要背景）

| 时间 | 事件 |
|---|---|
| 2026-06 | 确定性编排层完整落地：`pkg/orchestrator`（implement-verify-fix 闭环、design 面板、build 串联、共享黑板、多评委投票、`MaxAgentCalls` 预算）+ `/design` `/implement` `/build` 命令 |
| 2026-06-09 | **`87772b6` 整体删除（-1975 行）**，提交信息："实测编排功能不可用" |
| 2026-08 | `f793e8c` 转向 model-driven 路线：注入团队委派提示词，让主模型自主选择 agent |

这个反复是本 review 最重要的输入：**确定性流水线已被实践否决**，新路线把编排决策交还给模型。因此优化方向应该是"把模型需要的原语做扎实"，而不是再造流程引擎（详见 §5.4）。

### 2.2 团队意识改动（`f793e8c`）评估

**做对的**：

- catalog 动态渲染自 `EnumerateAgents`（项目 > 插件 > 内置），与 task 工具描述同源，不会漂移（`react.go:1007-1021`）。
- 注入条件三重门：交互式 + 持有 task 工具 + catalog 非空（`react.go:903`）；子代理（NonInteractive）不注入，避免无意义 token。
- plan mode 通过工具集替换天然移除 task 工具 → 委派提示消失 + 无法借子代理绕过只读限制，且已有测试锁住（`systemprompt_test.go` 的 `TestTeamDelegation_OmittedInPlanMode`）。
- 两条 fallback 路径补上了 `filterTaskTool`，递归口子闭合。

**边界与遗留**：

- 提示词教模型"lead a team"，但底层是串行阻塞委派（见 2.3）——模型即使想 fan-out 也做不到，提示词与能力不匹配。
- `coder`/`researcher`/`frontend` 的 `DefaultTools` 仍列着 `"task"`（`types_config.go:84,93,165`），执行时恒被剥离，属死配置，应删除以免误导。
- 委派提示 + catalog 每回合每请求都随系统提示重建，叠加记忆注入的变动，**provider 侧 prompt cache 基本必然 miss**（系统提示前缀不稳定，`react.go:145` 的日期 + `react.go:862-881` 的记忆注入）。

### 2.3 委派链路的五个结构性缺口

**① 假并行（最重要）。** `task` 工具未声明 `ParallelSafe`（`tools/subagent.go:30-45`），而并行批量执行要求批内**全部**工具 ParallelSafe（`react.go:560, 1417-1428`）。模型一次发出 3 个 task 调用会落入串行循环逐个阻塞执行。`NewSubagentPool(subExecutor, 4, …)`（`commands/chat.go:342`）的并发 4 在 CLI 路径**不可达**——唯一调用方 `StartTask` 后立即 `Wait`，且被 ReAct 循环串行化。唯一曾用到并发的 `fanOutReviews` 已随 orchestrator 删除。

**② 成本黑洞。** `SubagentExecutor.Execute` 丢弃 `result.Usage`，只返回 `FinalOutput + Messages`（`agent/subagent.go:184-187`）；子代理 `AgentConfig` 不设 `MaxTokensBudget`（预算检查 `react.go:307-313` 对子代理失效）；无任何机制把子代理 token 上卷到父回合，TUI 统计行少报整棵子代理树的消耗。也没有单回合 task 调用次数上限（orchestrator 的 `MaxAgentCalls` 随删除消失）。

**③ 单任务 UI。** `m.subagentStatus` 是单个字符串（`chat/tui.go:781-804`），两个并发任务互相覆盖状态行，第一个结束的会把所有任务的状态清空。这是 ① 修复的前置依赖。

**④ 池的生命周期毛刺。**
- 任务 map 只 `Store` 不 `Delete`（`pool.go:88`），每个条目持有完整子代理转写，长会话进程级泄漏。
- 超时时钟在信号量**之后**才启动（`pool.go:136-160`），排队等待不计入 deadline，且无排队上限。
- 任务超时被复用为子代理**单次 LLM 请求**超时（`RequestTimeout: task.Config.Timeout`，`agent/subagent.go:143`）——CLI 的 15 分钟意味着一个挂死的 HTTP 请求可吃掉整个任务预算。
- 无 `cancelled` 状态：父取消被归类为 `failed`（`pool.go:176-185`）；无按任务取消 API。
- `Wait` 因 ctx 取消先返回后，任务继续运行并向已结束回合的 sink 发事件（`pool.go:111-112, 173`）。

**⑤ 子代理零上下文。** 从单条 prompt 起跑，无对话历史、无记忆、无共享黑板。委派提示已正确告知模型这一点（"Sub-agents cannot see your conversation history"），但对多步项目意味着父 agent 必须在每个 prompt 里重复搬运上下文，token 成本高且易丢信息。

---

## 3. 缺陷清单

### P0 — 正确性 bug，建议立即修

| # | 缺陷 | 位置 | 影响 |
|---|---|---|---|
| P0-1 | **共享注册表污染**：`New()` 直接使用 `cfg.Tools` 指针（`react.go:118-124`），`registerPlanTools` 向其注册 `enter_plan_mode` 且**丢弃错误**（`plan.go:103`，`Register` 拒绝重复 `registry.go:64-66`）。REPL 每回合用同一个进程级注册表新建 agent（`repl.go:521,539`） | `pkg/agent/react.go`, `plan.go` | ① 第 1 回合的 `enter_plan_mode` 闭包**绑定已死的 turn-1 agent**，后续回合模型调用它时翻转的是死对象的 planMode，活 agent 工具集不受限、REPL 的 `r.planMode` 不同步；② 子代理注册表从父的 `List()` 构建且只过滤 `task`（`agent/subagent.go:97-100, 217`），**从第 2 回合起子代理继承 `enter_plan_mode`**，与 `NonInteractive` 设计意图直接矛盾；③ gateway 的 `Restrict(nil)` 返回原指针（`registry.go:331-333`），同样被逐请求污染 |
| P0-2 | **并行路径缺重复调用熔断器**：注释声称"same logic as serial path"（`react.go:628`），实际只有验证熔断（`629-660`）；`repeatCalls`/`repeatFails`/`prevRepeatKey` 只在串行路径维护（`746-791`） | `pkg/agent/react.go:628-660` | 模型批量重复只读调用（如 2×read_file 同参数循环）永不被拦截；默认 `MaxTurns=0`、`RequestTimeout=0` 时无任何兜底，无人值守场景成本无界 |

### P1 — 功能性缺陷 / 静默降级

| # | 缺陷 | 位置 | 影响 |
|---|---|---|---|
| P1-1 | task 非 ParallelSafe → 委派恒串行，池并发 4 不可达 | `tools/subagent.go:30-45` | 见 §2.3① |
| P1-2 | 子代理 Usage 被丢弃，无 token 上卷、无预算 | `agent/subagent.go:184-187` | 见 §2.3② |
| P1-3 | 自定义 agent 的 YAML/MD **解析错误被静默吞掉**；注释两处声称 "EnumerateAgents warns once at startup"（`yaml_loader.go:151-152`, `agentmd.go:175`），但 `EnumerateAgents` **没有任何警告代码** | `yaml_loader.go:155, 176-185` | 写错的配置静默退化为内置/general 档案，用户无从得知 |
| P1-4 | 子代理工具选择器**无一命中时静默放宽**为"全部工具减 task"（`agent/subagent.go:231-237`） | `pkg/agent/subagent.go` | `tools:` 列表打错字 → 只读 reviewer 静默获得 bash/write_file，权限放大而非收窄 |
| P1-5 | `mergeConfig` 零值陷阱：`temperature: 0`、`max_turns: 0` 无法通过覆盖设置（`yaml_loader.go:123-126`）；base 与 override 都无工具时强制只读五件套（`:133-137`）→ 项目放一个无 `tools:` 的 `general-purpose.yaml` 会**静默剥夺主 agent 的 bash/edit/write** | `pkg/agent/yaml_loader.go` | 自定义 agent 行为与预期不符且难排查 |
| P1-6 | skill 状态跨回合丢失：skill 加载改的是当回合 agent 的 systemPrompt（`react.go:695-708`），下一回合 agent 重建即遗忘；"Available skills" 目录被重新注入 | `pkg/chat/repl.go:539` + `react.go:212` | 回合 N 激活的 skill 到 N+1 静默失效 |
| P1-7 | skill 的 `context:fork`/hooks/`allowed-tools`/`model`/`max-turns` 全部是死配置：无调用方设置 `SubagentRunner`，`makeSkillHandler` 只传播 `SystemPrompt` | `skill/executor.go:80-82`, `skill/tool.go:37-40` | SKILL.md 里这些字段写了不生效 |
| P1-8 | 池任务 map 泄漏、无 cancelled 状态、超时时钟与排队解耦、任务超时误用作单请求超时 | `subagent/pool.go` | 见 §2.3④ |
| P1-9 | `AgentConfig.RequestTimeout` 文档声称"默认 10 分钟"（`types.go:42-46`），实际 `defaultRequestTimeout` 常量**从未使用**（`react.go:26,128`），默认无限 | `pkg/agent/react.go` | 文档性 bug + 无人值守挂死风险 |
| P1-10 | TUI 子代理状态行仅支持单任务 | `chat/tui.go:774-812` | 并发委派的前置缺口 |

### P2 — 质量 / 卫生

| # | 缺陷 | 位置 |
|---|---|---|
| P2-1 | `computeArgsHash` 不是哈希，把完整工具参数（bash 命令、路径、潜在密钥）明文写入 metrics JSONL 落盘 | `react.go:1527-1550`, `metrics_file.go:43-48` |
| P2-2 | 流式非 overflow 错误路径不 drain stream，goroutine 泄漏 | `react.go:443-445`（对照 `437-439`） |
| P2-3 | `view_image` 为 ParallelSafe，批量调用产生 `tool → human(images) → tool` 交错，破坏 OpenAI 1:1 顺序契约与 Anthropic "tool_result 块在前"规则 | `view_image.go:96-107`, `llm/openai_compat.go:300-304`, `llm/anthropic.go:315-321` |
| P2-4 | `toolMessage := runMessages[len(runMessages)-1]` 在结果携带图片时取到的是注入的图片消息而非工具消息，事件 MessageID 错位 | `react.go:611, 728` |
| P2-5 | 每个 delta 双发 `AgentEventChunk` + `AgentEventTextChunk`，前者无消费方，白占 128 深度事件通道 | `react.go:449-450` |
| P2-6 | `a.requests` sync.Map 只写不读；`maxValidationRetries` 双重声明（包级 + 遮蔽局部）；熔断阈值 8 魔数两处硬编码；验证熔断 ~35 行在两条路径间复制且已漂移 | `react.go:47, 1473, 796, 648, 816` |
| P2-7 | 压缩与 aging 度量口径不一致：压缩测**原始**消息（`react.go:326`）而非实际发送的 aged 视图；`estimateContextTokens` 只算基础 systemPrompt，不含记忆/规则/委派注入（`react.go:1303` vs `862-909`）；3 与 3.3 字节/token 两套启发式并存 | `pkg/agent/` |
| P2-8 | offload 目录 `~/.deepai/offload` 无 GC，无限增长 | `react.go:131-136, 1120-1142` |
| P2-9 | base64 图片在不开 aging 时永不被压缩/驱逐（`compact.go:97-99` 原样透传 Images；`transient_images` 元数据无人消费） | `pkg/agent/compact.go`, `view_image.go:66` |
| P2-10 | 记忆压缩前同步 flush 最长阻塞循环 30s | `react.go:332-343` |
| P2-11 | `react.go` 1580 行、`Run()` ~600 行、Agent 结构体 40+ 字段，god object | `pkg/agent/react.go` |

### 死代码清单（建议删除或明确接线计划）

| 目标 | 证据 |
|---|---|
| `pkg/gateway` 整包 | 无命令注册它（`commands/commands.go:5-11` 仅 chat/setup/session/version/plugin），无外部 import；且存在自身缺陷（`NonInteractive` 未设、共享 registry 污染、全用户共享一个 sandbox、`ContextWindow=0` 压缩失效） |
| `pkg/checkpoint`（Postgres，44 个导出方法） | 仅被 `gateway/server.go:16` 引用 |
| `pkg/plugin`（in-process/.so 插件框架，~2900 行） | 全仓唯一 import 是 `plugins/web_fetch/integration_test.go`；与实际使用的 `pkg/claudeplugin` 同名不同物，纯粹造成困惑 |
| `clarification.Manager` async 分支 + `NewAPI` | CLI 恒传 nil（`commands/chat.go:332`），gateway 根本不注册该工具 |
| `builtin.GitTools()` 返回空切片，但 `coder` 的 `DefaultTools` 仍引用 `git_status` 等 7 个不存在的工具名 | `builtin/git.go:573-575`, `types_config.go:93` |
| `DefaultTools` 中的 `"task"`（coder/researcher/frontend） | 执行时恒被 `selectSubagentTools` 剥离 |
| `Registry.ListByGroup`、`llm.Provider.Chat`（agent 只用 Stream） | 无非测试调用方 |

---

## 4. 文档勘误表

| 文档断言 | 实际 |
|---|---|
| `AUTONOMOUS_MULTI_AGENT.md` §9：orchestrator/implement_task/design_task/build_task/黑板/多评委/`MaxAgentCalls` 已落地 | 全部于 `87772b6`（2026-06-09）删除，`pkg/orchestrator` 不存在 |
| `MULTI_AGENT.md`：Environment 发布/订阅消息总线 | 从未实现（AUTONOMOUS 文档 §3 已自我勘误，但 MULTI_AGENT.md 本体未改） |
| `MULTI_AGENT.md` agent 表：general-purpose/coder 温度 0.7 | 代码为 0.2/0.1（`types_config.go:77,95`） |
| `AUTONOMOUS_MULTI_AGENT.md:54`：CLI 并发写死 1 | 实为 4（`commands/chat.go:342`） |
| `AUTONOMOUS_MULTI_AGENT.md:70`："coder 含 task 可嵌套" | task 恒被剥离，深度封顶 1 |
| `pkg/subagent/README.md:98`：MaxConcurrent 默认 1；未知类型回退 general-purpose 配置 | 默认 3（`pool.go:28`）；回退逻辑已改为空 base 由 executor 档案决定（`pool.go:240-246`） |
| `AgentConfig.RequestTimeout` 注释："Default: 10 minutes" | 默认 0 = 无限 |
| `yaml_loader.go:151` / `agentmd.go:175` 注释："EnumerateAgents warns once at startup" | 无警告代码 |

---

## 5. 优化路线图

### M1 — 正确性修复（小改动，高回报，建议先行）

> **状态：已完成（2026-08-02，分支 `m1-correctness-fixes`）**。全部 7 项落地并经逐任务评审 + 整分支终审（含变异测试验证）。执行中的两处方案修正：① 强制只读回退改为"仅非 builtin base 适用"而非整体删除；② "子代理 RequestTimeout 解耦"经验证不可行已撤回（见 M1.4 注）。额外修复了执行中发现的熔断提示破坏 provider 消息契约问题（提示延迟到批次末尾 + Anthropic mapper 合并尾随 human 文本）与并行批次中途熔断丢弃已计算结果导致会话污染的问题。遗留小项见 `.superpowers/sdd/progress.md`。

1. **修 P0-1**：交互式路径在 `New()` 内克隆注册表再注册 plan 工具（给 `Registry` 加 `Clone()`，或 `registerPlanTools` 前 `registry = registry.Clone()`）。附带修复 `Restrict(nil)` 返回原指针的别名问题（返回克隆）。加回归测试：连续两回合共享 registry，断言第 2 回合的 `enter_plan_mode` 生效于当回合 agent、子代理工具列表不含它。
2. **修 P0-2**：把重复调用熔断器与验证熔断一起提取为方法（顺带消除两路径 35 行重复），并行路径在 `wg.Wait()` 后逐结果喂入。
3. **兑现警告承诺（P1-3/4/5）**：`EnumerateAgents` 对解析失败的 YAML/MD 产出启动警告；`selectSubagentTools` 无一命中时改为**保守失败**（返回错误或只读五件套），绝不放宽；`mergeConfig` 用 `*float64`/`*int` 区分"未设置"与"零值"。
4. **池卫生（P1-8）**：`Wait` 返回后 `tasks.Delete`；新增 `TaskStatusCancelled`。注意：原设想的"子代理 `RequestTimeout` 解耦"经实施验证不可行——agent 仅在 ctx 无 deadline 时应用 `RequestTimeout`（`react.go:265-270`），而子代理 runCtx 恒带任务级 deadline，改值只会让超时报错的时长失真。单请求超时需要 llm 层新机制，列为 M2 前置项。
5. **文档勘误**：按 §4 修正三份文档 + 两处注释；删除 `DefaultTools` 中的死 `"task"` 与不存在的 `git_*` 引用。
6. 顺手项：删 `a.requests` 死状态；合并双发事件；`computeArgsHash` 改真哈希（fnv/sha1 截断）。

### M2 — 让"模型自主编排"真正可用（本次团队意识路线的补全）

> **状态：已完成（2026-08-02，同分支）**。5 项全部落地并经逐任务评审 + 整阶段终审。与原方案的偏差：(a) M2.3 校验失败改为 **fail-soft**——不返回 error，而是给 payload 加 `WARNING:` 前缀后原样回传（返回 error 会导致 `runOneTool` 丢弃 Content，父模型将失去全部评审内容）；(b) M2.2 的调用次数上限装在**单次 Run**（`maxTaskCallsPerRun=20`，跨回合累计，比原"单回合"更强）；(c) M2.2 的"父预算剩余量向下传"**未实现**，`token_budget` 目前是模型显式传入的工具参数——顺延 M3；(d) M2.1 的 TUI 状态区用有序 slice 而非 map（渲染顺序稳定），实时区上限 5 行 + "+K more"。整阶段终审抓到并修复了一个逐任务评审不可见的接缝缺陷：`pool.resolveConfig` 曾丢弃 `ContextFiles` 字段，M2.4 一度端到端空转——现有跨 `StartTask` 接缝的回归测试锁住。

这是对 `f793e8c` 路线的兑现：模型已被告知"你领导一个团队"，M2 让这句话成立。

1. **真并发委派**（依赖 M1）：
   - 前置：TUI 状态区改为 `map[taskID]status` 多行渲染（P1-10）。
   - 方案 A（推荐，改动最小）：`task` 标记 `ParallelSafe: true`。批内多个 task 调用即天然并发，受池信号量（4）约束；子代理写文件互踩的风险由模型侧提示约束（委派提示中加入"并行任务必须操作不相交的文件集"）。
   - 方案 B（更强但更重）：`start_task`/`wait_tasks` 异步原语对，允许模型先 fan-out 再统一收割，中途可继续别的工作。建议先 A 后视需要再 B。
2. **成本可见与预算**：`ExecutionResult` 携带 `Usage`；task 工具结果 `Data` 带 token 数并上卷到父回合统计；`SubagentConfig` 增加 token 预算（父预算的剩余量向下传），单回合 task 调用次数上限（恢复 `MaxAgentCalls` 的精神，这次装在池/工具层而非编排层）。
3. **可信的结构化交接**：`OutputSchema` 目前只是 prompt 后缀（`agent/subagent.go:121-124`），无解析无校验。在 executor 收尾处按 schema 校验 `FinalOutput`，失败时带错误重试一次（复用 `OutputSchema.MaxRetries`）。reviewer 类 agent 的价值取决于此。
4. **委派上下文包**：task 工具增加可选 `context_files []string` 参数——父 agent 显式指定的文件路径列表，由 executor 读取后拼入子代理首条消息。吸取黑板被删的教训：**由父显式传递，不做自动共享**。
5. **委派提示词随能力更新**：M2.1 落地后，`delegationStrategy` 增加并行指导（何时 fan-out、成本提示、文件集不相交约束）。

### M3 — 结构性重构与瘦身

> **状态：部分完成（2026-08-03，同分支）**。已落地：死代码删除（-9323 行：`pkg/plugin`+`plugins/`、`gateway`+`checkpoint`、clarification Manager、git 空壳、`ListByGroup`；go.mod -9 依赖）；`react.go` 拆分（1806→~990，新增 breaker/toolexec/promptbuild/streaming/subagent_context，multiset 证明纯移动）；度量统一（单一 `bytesPerToken=3.3`、度量组装后提示词 + aged view、`compactOnOverflow` 陈旧锚点失效、停滞守卫 + 增长解除、no-tool-calls 溢出分支收窄并接入重试）。计划外追加：工具调用契约修复（串行孤儿 tool_use 合成、hint 顺序）、父预算下传（耗尽即拒绝）、流空闲看门狗。**未完成顺延**：(a) 工具批执行两回路仍内联于 `Run()`（各 ~90 行重复簿记）；(b) 单一 `contextManager` 未建立；(c) M3.3 REPL/Agent 生命周期对齐未开始；(d) M3.4 系统提示前缀稳定化（prompt cache）未开始。

1. **拆 `react.go`**：`Run()` 按职责拆为 loop 骨架 / 熔断器 / 流式消费 / 工具批执行四块；上下文管理（压缩/aging/offload/估算）收拢进单一 `contextManager`，统一度量口径（一个 estimator、一套字节比、压缩与 aging 同一标尺，压缩应度量实际发送的视图）。
2. **删除死代码**：`pkg/plugin` 整包（或写明保留意图的 README）、`pkg/gateway`+`pkg/checkpoint`（若近期无 HTTP 计划）、clarification Manager 分支、`GitTools` 空壳。预计 -6000 行。
3. **REPL 与 Agent 生命周期对齐**：要么让 Agent 可复用（熔断计数、skill 状态、压缩锚点跨回合存活），要么把这些状态显式提升到 `ChatRepl` 层随回合传递。当前"每回合重建 + 部分状态手工回传（planMode）"是两头不靠的中间态，P0-1 和 P1-6 都是它的症状。
4. 系统提示前缀稳定化（日期移到消息层、记忆注入移到首条 human 消息前的独立消息），恢复 provider prompt cache 命中。

### 5.4 关于"要不要再做编排层"

不要按原样重建。orchestrator 被删的根因值得复盘：确定性流水线把"何时该 review、review 几轮、何时放弃"这些本质上需要判断力的决策固化成了代码，模型只能在框内挣扎。model-driven 路线把判断力还给模型是对的——但当前模型手里只有一把**串行、无预算、返回不可信文本**的 task 工具。M2 的四件事（并发、预算、结构化校验、上下文包）正是把 orchestrator 里**被验证有价值的机制**（fan-out、MaxAgentCalls、OutputSchema、黑板的显式传递版）下沉为工具原语，让模型自由组合，而非由引擎替模型做决策。若日后仍需要确定性流程（如 CI 场景），应作为独立的、调用同一套原语的薄壳存在，不与交互路径耦合。

---

## 附录 A：本次探索方法

四个并行只读探索 agent 分别覆盖 subagent 池/设计文档、ReAct 循环/上下文管理、REPL/gateway 装配层、tools/llm/memory/skill/plugin 基础层；两个 P0 结论由人工二次复核（`react.go:118-124, 555-671`、`plan.go:98-104`）。行号基于 `f793e8c`。
