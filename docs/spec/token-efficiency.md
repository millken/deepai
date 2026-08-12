# 设计文档：Agent Token 效率优化

> 状态：设计文档（问题分析 + 策略 + 集成点 + 分阶段计划）
> 范围：Agent 运行时全场景 token 来源
> 原则：减少 token 使用，但不以降低质量为代价

---

## 1. 问题定义

### 1.1 目标

降低 Agent 每次运行的总 token 消耗（input + output），在不降低任务完成质量的前提下。

"不降低质量"的硬约束意味着：压缩策略必须是**有损但可恢复的**。可恢复性要求按信息
来源分层（见原则 2）—— 工具结果必须有确定性的重取路径；AI 文本允许 best-effort
（保留结论，截断展开），与现有 compaction 行为一致。不可恢复的信息丢弃（例如静默
删除历史消息）不在本设计范围内。

### 1.2 Agent 的 token 来源全景

一次 LLM 请求的 input token 由以下五部分构成。本设计逐一分析每部分的现状与优化空间。

```
┌─────────────────────────────────────────────────────┐
│  单次 LLM 请求 input token                          │
├──────────────┬──────────────────────────────────────┤
│ ① system     │  基础 prompt + 日期 + 记忆注入 +      │
│   prompt     │  技能描述 + 文件操作规则 + 计划模式    │
│              │  (~1-3K tokens, 每 turn 重发)         │
├──────────────┼──────────────────────────────────────┤
│ ② 工具       │  所有注册工具的 name + description +   │
│   schema     │  JSON schema                          │
│              │  (~2-5K tokens, 每 turn 重发, 固定)   │
├──────────────┼──────────────────────────────────────┤
│ ③ 对话       │  human 消息 + ai 文本回复              │
│   历史       │  (随 turn 线性增长)                    │
├──────────────┼──────────────────────────────────────┤
│ ④ 工具       │  历史 tool_result（已完成的工具输出）  │
│   结果历史   │  (随 turn 累积, 探索阶段主导膨胀)      │
├──────────────┼──────────────────────────────────────┤
│ ⑤ 当前 turn  │  本轮工具调用的结果                    │
│   新增       │  (单次可能很大: read_file 全文等)      │
└──────────────┴──────────────────────────────────────┘
```

output token（模型生成的文本 + tool_call 参数）相对较小且难以压缩（压缩输出等于
限制模型能力），本设计不针对 output。

### 1.3 现有防御机制（已实现）

代码库已有一层防御，本设计是它的补充而非替代：

| 机制 | 位置 | 行为 | 不足 |
|------|------|------|------|
| 上下文压缩 | `compact.go` | 估算 token 超过窗口 75% 时，压缩旧消息（tool_result 截断到 300 字节，ai 文本截断到 200 字节） | 阈值触发，触发前已膨胀；只做"删/截"，不做"替" |
| 单结果硬上限 | `react.go:868` `toolMessageContent` | 单条 tool_result 超 50KB 时截断 | 50KB 仍然很大；且只防极端情况 |
| 工具结果估算 | `react.go:970` `toolSchemaTokens` | 把工具 schema 纳入 token 估算 | 仅用于估算，不优化 schema 本身 |
| 溢出重试 | `react.go:1055` `compactOnOverflow` | provider 报溢出时压缩重试 | 事后补救，用户可见延迟 |

**核心洞察**：现有机制全是**事后补救**（阈值触发或溢出触发）。本设计引入**事前控制** ——
在 token 膨胀发生之前就约束它，让"事后补救"成为二道防线而非唯一防线。

---

## 2. 策略分析：五个 token 来源逐一拆解

### 2.1 来源 ①：system prompt（每 turn 重发）

**现状**：

`BuildSystemPrompt`（react.go:715）每个 turn 都重新拼接 system prompt 并发送。构成：

- 基础 prompt（agent-type profile，如 coderSystemPrompt ~500 字符）
- 日期标记（~50 字符）
- 记忆注入（`<memory-context>` 块，上限 2000 tokens，见 `prompt.go:67`）
- 技能描述列表（`Available skills:` 段，技能多时膨胀）
- 文件操作规则（~400 字符，硬编码）
- 计划模式提示（plan mode 时追加）

**膨胀点**：

1. 记忆注入：2000 token 上限，且每 turn 重发。记忆事实多时接近上限。
2. 技能描述：每个技能一段描述，技能多时累积。
3. 文件操作规则：固定开销，每个 agent type 都带。

**优化空间**：中等。system prompt 相对消息历史通常不是大头，但它是**固定开销**（每 turn 都有），长会话里累积效应明显。

**质量约束**：system prompt 里的指令性内容（行为规则、工具偏好）不能压缩 —— 压缩等于改变模型行为。可优化的是**描述性内容**（技能列表、记忆事实）。

### 2.2 来源 ②：工具 schema（每 turn 重发，固定）

**现状**：

每次请求带上所有注册工具的 `name + description + InputSchema`（JSON schema）。
`toolSchemaTokens`（react.go:970）估算这部分成本。典型 coder agent 注册约 20 个工具。

**膨胀点**：

1. 工具描述冗长（如 bash 工具描述 ~500 字符，含大量"不要用 bash 做 X"的引导）。
2. JSON schema 的 `description` 字段重复信息。
3. **所有工具每 turn 都带**，即使本 turn 只可能用其中几个。

**优化空间**：高。这是**纯固定开销**，且 provider 通常对 system/tool 部分无缓存。

**质量约束**：工具 schema 是模型正确调用工具的唯一依据。不能删除，但可以：
- 精简描述（去掉冗余措辞）
- 动态裁剪（根据上下文只带相关工具）—— 但这有风险，模型可能"忘记"它本来能用的工具

### 2.3 来源 ③：对话历史（human + ai 文本）

**现状**：

`runMessages` 里的 `RoleHuman` 和 `RoleAI`（纯文本部分）消息。随 turn 线性增长。
compaction 时 ai 文本截断到 200 字节（`compactAssistantTextKeep`）。

**膨胀点**：

1. 长文本回复（模型解释一大段）。
2. human 消息含粘贴的代码/日志/错误堆栈。
3. compaction 只在 75% 阈值触发，触发前全部保留。

**优化空间**：中等。对话历史是模型理解任务上下文的核心，压缩风险高。

**质量约束**：human 消息通常不能压缩（是用户原始意图）。ai 文本回复里，早期的详细解释在后续 turn 可能不再需要，但判断"不再需要"本身需要理解力。

### 2.4 来源 ④：工具结果历史（核心膨胀源）

**现状**：

`runMessages` 里的 `RoleTool` 消息。每完成一次工具调用就追加一条。compaction 时
截断到 300 字节（`compactToolResultKeep`）。

**膨胀点**：

1. `read_file` 大文件全文（单次可达 20KB+）
2. `grep`/`find` 命中过多（默认上限 100-200 条）
3. `web_fetch` 网页内容（默认 4096 字符，但仍可观）
4. **跨 turn 累积**：第 5 turn 仍带着第 1-4 turn 的所有 tool_result

这是**最大的膨胀源**。现有 compaction 的 300 字节截断是事后补救，且只在 75% 阈值触发。

**优化空间**：极高。这是本设计的重点。

**质量约束**：tool_result 是模型获取环境信息的唯一渠道。但"历史"tool_result（已被
模型消化过的）可以激进压缩 —— 模型如果需要细节，可以重新调用工具。关键是区分
"当前 turn 需要"和"历史已消化"。

### 2.5 来源 ⑤：当前 turn 新增（单次工具输出）

**现状**：

本轮工具调用的结果。`toolMessageContent`（react.go:863）在写入前做 50KB 硬上限。

**膨胀点**：

1. `read_file` 无行范围限制时读全文
2. `grep`/`find` 默认返回大量命中
3. `list_dir` 列大目录（含 vendor/node_modules）

**优化空间**：高。这是"事前控制"最直接的着力点 —— 在工具返回结果前就约束其大小。

**质量约束**：当前 turn 的结果模型需要完整看到。但"完整"不等于"原始" —— 一个 2000
行文件的结构大纲（函数签名列表 + 首尾）对模型定位问题通常足够，定位后再精确读取。

---

## 3. 设计原则

基于以上分析，确立五条设计原则：

### 原则 1：事前控制优于事后补救

现有机制（compaction）是阈值触发的事后补救。本设计优先在 token 进入上下文之前就
约束它。事前控制让事后补救成为真正的"最后防线"。

### 原则 2：有损但可恢复（按信息来源分层）

压缩策略的可恢复性要求按信息来源分层，而非一刀切：

**严格可重取**（必须有确定性的重取路径）：
- **工具结果**（T1/T2）：被压缩的工具输出可通过重新调用同一工具恢复。例如截断的
  `read_file` 结果可通过 `read_file(path, start_line, end_line)` 精确重取。这类信息
  的压缩附带明确的重取提示（如 `[...aged: re-call {toolName}]`）。

**Best-effort 可恢复**（无确定性重取路径，但信息丢失后果可控）：
- **AI 文本回复**（T4）：模型的解释、推理、计划承诺只存在于文本中，无法通过重新
  调用工具确定性重建。对这类信息，T4 采用 best-effort 策略 —— 保留首段（通常含
  结论），截断中段展开。这与现有 compaction（`compactAssistantTextKeep=200`）的
  行为一致，只是从事后阈值触发变为持续生效。

> **为什么接受 AI 文本的 best-effort**：现有 compaction 已经对 AI 文本做 200 字节
> 截断，且只在 75% 阈值触发 —— 那才是真正的"截到只剩结论"。T4 的预算梯度
> （age=2-3 保留 500 字符，age≥4 保留 200 字节）比现有 compaction 更宽松。
> 如果现有 compaction 的 best-effort 截断是可接受的（它已在生产中运行），那么
> T4 更宽松的梯度也是可接受的。T4 不引入新的信息丢失级别，只是让丢失更早、
> 更可控地发生。

**明确排除出 T4 视图层**（T4 的 `buildPromptView` 不压缩）：
- **ToolCalls.Arguments**：工具参数（如 edit_file 的 old_string）执行后无法重建，
  且 provider 会重新序列化并发送。见 §9.3 的详细论证与范围限定。
- 注意：此排除**仅限 T4 视图层**。现有 compaction（compact.go:136-144）仍会剥离
  Arguments，本设计不改 compaction。

### 原则 3：按"信息时效"分级

信息越旧，压缩越激进。当前 turn 的工具结果完整保留；上一 turn 的保留大部分；
3 turn 前的激进压缩。这模拟人类记忆的衰减 —— 最近的细节清晰，久远的只记要点。

### 原则 4：不引入额外 LLM 调用

本设计的所有策略都是**确定性的、无 LLM 的**变换（截断、大纲提取、schema 精简）。
不引入"用小模型压缩再用大模型推理"的二级 LLM 结构 —— 那会增加复杂度和新的 token
成本，且收益不确定。

### 原则 5：渐进式、可开关、可度量

每个策略都是独立可开关的配置项，默认行为不变（向后兼容）。实施前先度量（Phase 0），
实施后验证不降质（每阶段含质量回归）。

### 原则 6：分离 canonical 消息与 prompt 视图（关键架构约束）

> ⚠ 这条原则是对早期设计的根本修正。早期版本设想"给 `models.Message` 加运行时
> 字段 `BornAtTurn`，每 turn 就地重写 `Content`"。经代码审查确认这是错的：
> `Message` 是**跨子系统的 canonical（权威）数据结构**，就地修改会破坏多个不变性。

`models.Message`（`message.go:118`）不是 agent 内部的私有对象，它同时被以下路径消费：

| 消费者 | 位置 | 对 Message 的依赖 |
|--------|------|-------------------|
| 会话持久化 | `chat/session.go:271` `AppendMessage` | `Content` + `ToolResult` 落库 SQLite |
| 会话回放 | `chat/session.go:217` `LoadMessages` | 从库重建 Message，无运行时字段 |
| 异步记忆抽取 | `memory/queue.go:481` `prepareAsyncMessages` | `cloneMessages` 后送 LLM 抽取事实 |
| 上下文压缩 | `compact.go:106` `compactToolMessage` | **构造新 Message 副本**，只复制显式字段 |
| 溢出重试 | `react.go:348` `compactOnOverflow` | 调用 compactMessages，产生新副本 |
| subagent 结果 | `subagent.go` | Message 跨 agent 边界传递 |

**三个致命问题**（如果就地修改 canonical Message）：

1. **compaction 丢失运行时字段**：`compactToolMessage`（compact.go:106）和
   `compactAssistantMessage`（compact.go:128）构造**新** Message，只复制 ID/SessionID/
   Role/Content/ToolResult。任何加在 Message 上的运行时字段（如 `BornAtTurn`）在第一次
   compaction 后就被静默抹掉，后续 age 计算全部失真。

2. **双份存储导致收益高估**：tool 消息同时写 `Message.Content`（react.go:517）和
   `ToolResult.Content`（message.go:87）。只改 `Message.Content` 不改 `ToolResult.Content`，
   则落库和记忆抽取仍携带完整内容 —— prompt token 降了，但存储/记忆管道没降。

3. **就地重写破坏回放一致性**：如果老化改写了 `runMessages` 里的 Content，而落库的是
   改写前的版本（或反之），会话回放时会看到与原运行不同的内容。

**因此，本设计确立 canonical/view 分离原则**：

- **canonical 消息**（`runMessages`、`models.Message`）：**永不修改**。保持完整、
  可持久化、可回放。所有写入点（react.go:443/513/620）写入的是 canonical 消息。
- **prompt 视图**（request-scoped）：每次 LLM 请求前，从 canonical 消息**派生**一份
  压缩视图，只用于组装 `ChatRequest`。视图用完即弃，不影响 canonical 状态。

这意味着 T1/T4 的老化逻辑作用于**派生视图**，而非 canonical 消息。age 信息不存放在
Message 上，而是由视图层扫描 canonical 消息的 `userTurnIndex` 序号计算（见 §T1
"age 时间轴定义"），不依赖任何运行时字段或外部传入的 turn 编号。

这条原则使 T1/T4 的"复用同一基础设施"成为真正的同源 —— 它们是同一个视图派生函数
对不同 Role 的不同预算策略。

---

## 4. 优化策略

五个策略，按收益预期从高到低排序。每个策略对应一个或多个 token 来源。

### 策略 T1：工具结果时效老化（对应来源 ④）

> ✅ **已实现（含 code review 修正）**。`buildPromptView` + `AgingConfig` 位于
> `pkg/agent/aging.go`，接入点在 `react.go` 组装 `ChatRequest` 处
> （`Messages: promptView`）。默认关闭（`AgentConfig.Aging == nil` → 零拷贝直通，
> 向后兼容）。含压力门槛。测试 `aging_test.go`（12 项单元）+
> `aging_integration_test.go`（2 项端到端）全绿。
> T4 分支（RoleAI）已随 `buildPromptView` 建好但默认不启用（Phase 2 校准）。
>
> **review 修正 ×2**：(1) 压力门槛在 `contextWindow<=0`（合法的"未知窗口"状态,
> 如未链 WithContextWindow 的 subagent）时**fail-safe 跳过老化**,而非门槛失效导致
> 从第 1 turn 就无条件压缩;(2) 截断改用 `truncateRuneSafe` 对齐 UTF-8 rune 边界,
> 避免把 CJK 字符切半产生非法 UTF-8 送给 provider。

**核心机制**：每次组装 `ChatRequest` 前，从 canonical `runMessages` **派生**一份
prompt 视图，其中历史 `RoleTool` 消息的 `Content` 按"年龄"递减预算。视图只用于组装
`ChatRequest`，canonical 消息不变。

**为什么不是写入时压缩**：写入时新结果年龄为 0（当前 turn），而当前 turn 不压缩，
所以写入时压缩无法削减历史累积。必须在**每次请求前**对已有历史做派生压缩。

**为什么不是就地重写 canonical 消息**：见原则 6。`Message` 是跨子系统 canonical
结构，就地改 Content 会破坏持久化一致性、被 compaction 抹掉运行时字段、且
`ToolResult.Content`（message.go:87）与 `Message.Content` 双份存储导致只改一份无效。

#### age 时间轴定义（关键算法，修正版 v2）

> ⚠ 早期版本的 age 算法有两个致命错误：
> 1. `turnOfLastAI` 初始化为 `currentTurn` 再 `++`，导致 age 恒为 0 或负数
> 2. 假设"每个 turn 都有含 tool_calls 的 RoleAI 作为边界"，但实际无工具 turn 产出
>    纯文本 AI 消息（react.go:433），且新 Run 会带入整个 session 历史（repl.go:517），
>    `turn` 变量只计当前 Run 的循环，不是绝对位置。

> ⚠ **v2 修正（读取死循环事故）**：v1 把**每条 RoleAI 消息**当作 age 边界。这在
> "一条 assistant 消息批量发多个 tool_call"的假设下成立，但实测模型（glm-5.2 等）
> **一条消息只发一个 tool_call**，于是"age 3"只需 3 次工具调用就到了 —— 而
> `read_file` 在 age≥3 的预算是 300 字节，还附带"re-call read_file"提示。后果：
> 连读第 4 个文件就把第 1 个挤没，模型照提示重读，重读又挤掉下一个，形成**无法
> 自行退出的读取循环**。真实事故：session `20260812_093415_fc6e` 在 7 个文件之间
> 发了 1136 次 `read_file`（其中同一个文件 858 次），整个会话报废。
>
> 根因不是预算数值，而是 age 轴选错了：**同一个用户请求内的工作集必须完整保留**，
> 否则模型没有任何出路，只能重新获取被删掉的信息。因此 v2 把 age 轴改为
> **用户轮次**：age = "几个用户请求之前"。请求内的膨胀交给 compaction（75% 的
> 二道防线）处理，而不是靠悄悄删掉模型正在用的东西来处理。

**正确的 age 定义：基于扫描的绝对 user-turn 序号**

age 不依赖 `Run` 的 `turn` 变量，而是由视图函数扫描 `runMessages` 独立计算：

**第一步：定义"user-turn"**

每一条 `RoleHuman` 消息开启一个新 turn。扫描时为每条 RoleHuman 消息分配一个
**递增的序号**（从 0 开始）；它触发的所有 assistant 消息与 tool 结果都继承这个
序号，因此"agent 为同一个用户请求做的全部工作"共享同一个 age。

```
消息序列示例（跨多次 Run 的 session 历史）：
  [0] human        "帮我看看这个项目"        userTurnIndex=0
  [1] AI (tools)   属于 userTurnIndex=0     ← 调用 list_dir
  [2] tool         属于 userTurnIndex=0
  [3] AI (tools)   属于 userTurnIndex=0     ← 调用 read_file
  [4] tool         属于 userTurnIndex=0
  [5] AI (tools)   属于 userTurnIndex=0     ← 调用 grep
  [6] tool         属于 userTurnIndex=0
  [7] AI (text)    属于 userTurnIndex=0     ← 纯文本回复，Run 结束
  --- 下一次 Run（新用户消息）---
  [8] human        "再看看那个函数"          userTurnIndex=1
  [9] AI (tools)   属于 userTurnIndex=1     ← 调用 read_file
  [10] tool        属于 userTurnIndex=1
  [11] AI (text)   属于 userTurnIndex=1     ← 纯文本回复，Run 结束（当前请求前）
```

**第二步：tool 消息归属**

每条 `RoleTool` 消息归属于它**前面最近的** RoleHuman 消息的 `userTurnIndex`
（等价于：归属它所在的那个用户请求）。第一条 RoleHuman 之前的消息（system
prompt、带入的状态）按 turn 0 处理。

**第三步：age 计算**

```
totalTurns = 扫描得到的 RoleHuman 消息总数（上例中 = 2）
对每条消息 M（归属 userTurnIndex = K）：
  age = totalTurns - 1 - K
```

上例中：
- 消息 [2]/[4]/[6]（userTurnIndex=0）：age = 2-1-0 = **1** → 轻度压缩（8KB）
- 消息 [10]（userTurnIndex=1）：age = 2-1-1 = **0** → 当前请求，不压缩

**为什么这个定义正确**：

1. **不依赖 `Run` 的 `turn` 变量**：`totalTurns` 由扫描得出，跨 Run 的 session
   历史也有正确的绝对位置。
2. **不假设模型每条消息发几个 tool_call**：这是 v1 致命错误的来源。一个用户请求
   里模型是发 3 次单调用还是 1 次三调用，对 age 不再有任何影响 —— 而 v1 下这个
   差异等于 3 倍的老化速度。
3. **当前请求的所有工作 age=0**：模型正在做的这件事的全部工具结果都归属最后一个
   `userTurnIndex`，age = 0，不压缩。**这是防死循环的核心不变量**：绝不在模型还
   需要某份内容时把它删掉再叫它重新获取。
4. **compaction 安全**：compaction 保留 Role 和相对顺序，扫描结果在 compaction 后
   仍然正确（只是消息变少，userTurnIndex 重新编号，但相对 age 关系不变）。
5. **请求内膨胀有人管**：单个请求内不老化，膨胀由 compaction 在 75% 处接手。分工
   清晰：aging 管**跨请求**衰减（无损、只作用于视图），compaction 管**请求内**溢出
   （有损、最后手段）。

#### 上下文压力门槛（避免短会话过度压缩）

> ⚠ 这是对早期"每 turn 无条件按 age 压缩"的修正。纯 age 驱动的老化有一个隐患：
> 它把"事前控制"（原则 1）推过头，滑向了"过早控制"。

**问题**：如果视图层只要启用就在 age≥1 时开始压缩，那么第 2 turn 就会把第 1 turn
的工具结果压到 8KB —— 但此时上下文窗口可能才用了 10%,完全没有膨胀压力。这是
**无收益的信息损失**：压缩本身不省钱（离窗口上限还很远），却让模型少看到细节，
可能诱发不必要的工具重调用（§10 的"turn 数增加"风险）。

**修正**：age 老化只在**上下文压力达标后**才启动。引入 `MinContextPressure`
（占窗口比例，默认 0.4）作为门槛：

- 当前上下文估算 token < `MinContextPressure × contextWindow`：视图层**不压缩**，
  直接返回 canonical 消息（零拷贝）。短会话、探索初期天然豁免。
- 压力达标后：按下方 age 预算梯度压缩。

这把 T1 从"每 turn 无条件压"改为"**压力触发 + age 排序**"两级决策：**压力**决定
"压不压"，**age** 决定"先压谁、压多狠"。两个维度正交互补 —— 这是从学术界的
Adaptive Context Compaction（按 70%/80%/… 压力阈值渐进压缩）借鉴的思路，但保持
本设计的确定性、无 LLM 特性。

> **门槛用字节启发式是可接受的**：压力估算复用现有 `estimateTokens`
> （compact.go:195，与 compaction 的 75% 阈值同源）。Phase 0 已论证字节启发式
> 不适合作**决策主指标**，但作为一个**粗粒度门槛**（"窗口用了不到 4 成就别压"）
> 它足够可靠 —— compaction 的 75% 触发本就基于同一估算，多年在生产运行。门槛只需
> 区分"远未膨胀 / 已经膨胀"，不需要精确。

> **与 compaction 阈值的关系**：`MinContextPressure`（默认 0.4）远低于 compaction
> 的 0.75 触发点。这形成三段梯度：**<40% 完全不压**（视图零拷贝）→ **40%~75%
> 视图层按 age 老化**（本设计的主要工作区间，事前控制）→ **≥75% compaction 介入**
> （事后补救，二道防线）。视图层老化填补了"膨胀已开始但尚未触发 compaction"的空窗。

#### 预算梯度（默认，可配置）

以下梯度**仅在上下文压力达到 `MinContextPressure` 后生效**（见上）：

```
age=0 (当前 turn)    : 不压缩（模型本轮需要完整结果）
age=1                : 8 KB   （上一轮，保留大部分）
age=2                : 2 KB
age>=3               : 300 字节（等同现有 compaction 的 compactToolResultKeep）
```

#### 视图派生函数

```go
// buildPromptView 从 canonical runMessages 派生一份 prompt 视图。
// 视图中的 RoleTool Content 和 RoleAI Content 可能被压缩，但 ToolResult 结构
// （CallID/ToolName/Status）和 ToolCalls 结构（ID/Name/Arguments）保留不动。
// canonical runMessages 不被修改。T1（RoleTool）和 T4（RoleAI）在同一次遍历完成。
// contextWindow 是当前模型的上下文窗口 token 数，用于计算压力门槛。
func buildPromptView(messages []models.Message, cfg *AgingConfig, contextWindow int) []models.Message {
    if cfg == nil || !cfg.Enabled {
        return messages // 不启用时直接返回原切片（零拷贝）
    }

    // 第零步：上下文压力门槛。远未膨胀时（占窗口比例 < MinContextPressure）
    // 不做任何压缩，避免短会话/探索初期的无收益信息损失。复用 estimateTokens
    // （与 compaction 75% 阈值同源的字节启发式），仅作粗粒度门槛。
    if cfg.MinContextPressure > 0 && contextWindow > 0 {
        estimated := estimateMessagesTokens(messages) // 复用 compact.go 的估算
        if float64(estimated) < cfg.MinContextPressure*float64(contextWindow) {
            return messages // 压力未达标，零拷贝返回
        }
    }

    // 第一步：扫描建立 userTurnIndex，统计 totalTurns
    turnIndex := -1
    ownerTurn := make([]int, len(messages)) // 每条消息归属的 userTurnIndex
    for i, msg := range messages {
        if msg.Role == models.RoleHuman {
            turnIndex++
        }
        ownerTurn[i] = turnIndex
        if ownerTurn[i] < 0 {
            ownerTurn[i] = 0 // 第一条 human 之前的消息按 turn 0 处理
        }
    }
    totalTurns := turnIndex + 1 // -1 表示还没有用户消息
    if totalTurns == 0 {
        return messages // 无用户请求，无需压缩
    }

    // 第二步：派生视图，按 age 压缩（T1: RoleTool + T4: RoleAI 同一遍历）
    view := make([]models.Message, len(messages))
    copy(view, messages) // 浅拷贝；Content/Arguments 只在压缩时新建

    for i := range view {
        msg := &view[i]
        age := totalTurns - 1 - ownerTurn[i]
        if age <= 0 {
            continue // 当前 turn，不压缩
        }

        switch msg.Role {
        case models.RoleTool:
            // T1：压缩工具结果 Content
            budget := cfg.toolResultBudget(age)
            if budget > 0 && len(msg.Content) > budget {
                toolName := ""
                if msg.ToolResult != nil {
                    toolName = msg.ToolResult.ToolName
                }
                msg.Content = msg.Content[:budget] +
                    "\n[...aged: re-call " + toolName + " to see full output]"
            }

        case models.RoleAI:
            // T4：仅压缩文本 Content；ToolCalls 结构（含 Arguments）完全不动
            budget := cfg.conversationBudget(age)
            if budget > 0 && len(msg.Content) > budget {
                msg.Content = msg.Content[:budget] + "\n[...earlier response truncated]"
            }
        }
    }
    return view
}
```

> **关于 ToolResult.Content 双份存储**：视图层只压缩 `Message.Content`（prompt 实际
> 发送的字段），不改 `ToolResult.Content`。这是**有意为之**：
> - `Message.Content` 是 prompt-facing 文本（`toolMessageContent` 函数的产物，
>   react.go:863），provider 看到的是它。
> - `ToolResult.Content` 是 canonical 结构化数据，用于持久化和事件回放。
> - 两者本就允许不同（`toolMessageContent` 已经可能截断 Content 到 50KB）。
> - 视图层压缩 prompt-facing 副本，canonical 副本保持完整 —— 这正是原则 6 的体现。

**与现有 compaction 的关系**：正交互补。

- compaction 作用于 canonical `runMessages`（删消息/截断），是**不可逆的状态变更**。
- 老化作用于 prompt 视图（派生压缩），是**可逆的、每 turn 重做的**。
- compaction 后 canonical 消息变少，视图派生的扫描输入也变少 —— 两者叠加协同。
- 关键：compaction 的 `compactToolMessage` 不需要保真任何运行时字段，因为 age 信息
  不在 Message 上，而在视图层每次扫描时重新计算。

**接入点**（1 处，对照 react.go 核实）：

| 接入点 | 位置 | 职责 |
|--------|------|------|
| 视图派生点 | react.go L335 组装 `ChatRequest` 前 | 调用 `buildPromptView(runMessages, cfg, contextWindow)`，将返回的视图传入 `ChatRequest.Messages`。`contextWindow` 取自当前模型配置（compaction 已用同一值算 75% 阈值） |

**不需要改动的点**（相比早期设计大幅简化）：

- ~~写入点 L513/L620~~：不需要设任何字段，canonical 消息不变。
- ~~`models.Message` 加字段~~：不需要。age 由扫描计算。
- ~~compaction 保真运行时字段~~：不需要。没有运行时字段。
- ~~session 持久化~~：不受影响。canonical 消息不变。
- ~~memory queue~~：不受影响。`prepareAsyncMessages` 拿到的是 canonical 消息。

**预期收益**：高。直接砍掉跨 turn 累积 —— 这是长会话 token 膨胀的主因。且由于不修改
canonical 消息，实现风险大幅降低。

### 策略 T2：工具输出事前塑形（对应来源 ⑤）

> ✅ **已实现（T2a/T2b/T2c，含 code review 修正）**。配置与大纲构造在
> `pkg/tools/builtin/shaping.go`。
> - **T2a**：`read_file` 大**代码**文件（>500 行、扩展名有符号提取器、无有效
>   range/limit、非 `full=true`）返回"首 50 行 + 符号签名（带行号，复用 T6
>   `extractSymbols`）+ 尾 20 行 + 行数提示"。实测 `react.go` 大纲 5073 字节 vs
>   全文 41459 字节（**↓88%**）。`full=true` 或 range 读绕过。
>   **review 修正 ×2**：(1) 大纲**仅限代码文件** —— CSV/YAML/词表等无符号可提取,
>   大纲对它们是纯数据丢失,保持全文返回（早期"未知语言回退首尾截断"的设计被否决）；
>   (2) `limit=0` 视为"无 limit"（与下方 limit 分支一致）,不再兼作大纲旁路。
> - **T2b**：`grep` 默认 100→50、`find` 默认 200→50，截断消息附重取提示。
> - **T2c**：`list_dir` 折叠 vendor/node_modules/.git/__pycache__ 为
>   `[name/ (N entries, omitted)]`，新增 `expand_dirs` 参数展开；`Data["entries"]`
>   保持完整。**review 修正**：默认列表**不含 dist/build/target** —— 很多项目在
>   build/ 放手写 CI 脚本、dist/ 放已提交产物,按名字折叠会藏住真源码。
> - **T2d**：`web_fetch` 维持现状（4096 字符已合理，历史由 T1 老化）。
> 测试 `shaping_test.go`（11 项）+ 现有工具测试全绿。

**核心机制**：在工具 Handler 返回结果前，对大结果做结构化塑形，让模型拿到的是
"够用且紧凑"的信息，而非原始全量。

这不是一个统一的后处理，而是**每个工具各自优化返回策略**：

**T2a. `read_file` 结构大纲模式**（`pkg/tools/builtin/file.go`）

- 文件 ≤ 500 行：原样返回（现状不变）
- 文件 > 500 行且未指定行范围：返回**结构大纲**
  - 首 50 行（带行号）+ 函数/类型/方法签名列表（正则提取 `^func|^type|^class|^def`，
    **每项附带行号**）+ 尾 20 行（带行号）
  - 签名列表格式示例：`L  42  func (a *Agent) Run(ctx context.Context, ...)`
  - 末尾提示：`[File has N lines. Use read_file with start_line/end_line for specific sections.]`
- 指定了行范围：原样返回（现状不变）

> **行号是恢复路径的关键**：`read_file` 的精确读取是行号驱动的（file.go:48-86）。
> 大纲中的签名必须附带行号，否则模型只知道"有这个函数"却不知道在第几行，无法直接
> 发起 `read_file(start_line=X, end_line=Y)` 的确定性 range read，只能再多走一次
> grep。带行号的签名让模型从大纲直接跳到精确读取，满足原则 2 的"严格可重取"。

**T2b. `grep`/`find` 结果收紧**（`pkg/tools/builtin/grep.go`、`find.go`）

- 现状：grep 默认 100 条，find 默认 200 条 —— 偏大
- 调整：默认降到 50 条；超量时返回 top-50 + `[Showing 50 of N matches. Refine your pattern.]`
- 主模型仍可通过 `max_results` 参数显式请求更多

**T2c. `list_dir` 折叠生成物目录**（`pkg/tools/builtin/listdir.go`）

- 对 `vendor/`、`node_modules/`、`.git/`、`__pycache__/`、`dist/`、`build/` 默认折叠
  为 `[dir/ (N entries, omitted)]`
- **恢复路径**：新增 `expand_dirs` 参数（布尔，默认 false）。设为 true 时展开被折叠
  的目录。不复用现有 `all` 参数 —— `all` 的语义是"显示隐藏项（.claude, .git）"
  （listdir.go:88），与"展开折叠目录"是不同概念。混用会导致 `all=true` 既有隐藏项
  又有折叠目录，语义不清。
- 折叠提示：`[dir/ (N entries, omitted). Use list_dir with expand_dirs=true to expand.]`

**T2d. `web_fetch` 默认字符数收紧**（`pkg/tools/builtin/web.go`）

- 现状：`defaultWebFetchMaxChars = 4096` —— 合理，但可考虑按年龄老化（T1 会覆盖）

**预期收益**：中-高。单次工具调用就少带很多 token，且这些 token 本来会进入历史（T1
再压缩它们，但事前就少带更好）。

**质量保证**：每个塑形都保留"重新获取完整信息"的路径（行范围、max_results、all 参数）。

### 策略 T3：工具 schema 精简与按需裁剪（对应来源 ②）

> ✅ **已实现（T3a）/ 已澄清（T3b、T3c）**。
> - **T3a ✅**：删除各工具 description 里与 system prompt"文件操作规则"重复的
>   bash 路由文本。system prompt 的规则在 `BuildSystemPrompt`（react.go:775）**无条件
>   注入**每个 agent，因此每工具再列一遍"read_file (not cat/head/tail)…"是纯冗余。
>   改动:`bash.go`（最大头，删整张路由表，保留"别用 bash 做文件操作"的核心提示）、
>   `read_file`/`write_file`/`edit_file`/`list_dir`/`glob`/`grep`/`find` 各删"…via bash"
>   子句。实测描述字节 ~3200→2673（**↓约 16%**,≈175 tokens/turn 固定开销）。
>   测试 `descriptions_test.go`（3 项：无冗余路由 / bash 保留核心提示 / 各工具功能语义
>   不丢失）。
> - **T3b（澄清，不新增代码）**：按需裁剪只在 plan mode 做——现有 `enterPlanMode`
>   已将工具集限制为只读。常规 ReAct 中裁剪风险高（模型"忘记"工具），本设计明确
>   不做（见下方 T3b 风险）。故 T3b 由既有 plan mode 满足。
> - **T3c（澄清，无改动）**：审查确认各参数的 schema description（如"Regex pattern
>   to search for"）均具体简洁，**不与工具 Description 重复**，无可去重项。
>   `file_path`/`query` 等 deprecated 别名虽占字节，但删除会破坏依赖它们的 tooling
>   兼容性，故保留。符合设计对 T3c"收益小"的预判。

**核心机制**：减少每次请求携带的工具 schema 体积。

**T3a. 描述精简（低风险）**

现有工具描述含大量"不要用 bash 做 X，改用 Y"的引导文本。这些引导在 system prompt
的"文件操作规则"段已有重复。审查每个工具描述，去除与 system prompt 重复的引导，
保留功能描述和参数说明。

例：bash 工具描述（~500 字符）中"Do NOT use bash for file operations: prefer
read_file..."与 system prompt 的文件操作规则几乎完全重复，可精简。

**T3b. 按需裁剪（中风险，需验证）**

根据当前 agent type 和任务阶段，只携带相关工具的 schema：

- plan mode 下只带只读工具（现有机制，`enterPlanMode` 已做）
- coder agent 做探索阶段时，可只带 read_file/grep/find/glob/list_dir

**风险**：模型可能"忘记"它本来能用的工具，导致该写文件时不写。缓解：只在明确的
只读阶段（plan mode）裁剪，不在常规 ReAct 中裁剪。

**T3c. schema 压缩（低风险，收益小）**

JSON schema 的 `description` 字段有时与工具 `Description` 重复。审查并去重。

**预期收益**：中。schema 是固定开销，精简后每个 turn 都受益。但绝对值不如 T1/T2 大。

### 策略 T4：对话历史摘要（对应来源 ③）

**核心机制**：对早期的 ai 文本回复做渐进式摘要。

**现状**：compaction 时 ai 文本截断到 200 字节（`compactAssistantTextKeep`），但只在
75% 阈值触发。

**T4 方案**：与 T1 共用同一个 `buildPromptView` 函数及其 age 时间轴（§T1 的
`userTurnIndex`/`totalTurns` 扫描）。在同一个遍历中，对 `RoleAI` 消息的文本 Content
做渐进式压缩。ToolCalls.Arguments 在 T4 视图层不压缩（见 §9.3，含与 compaction 的
范围限定）。

**为什么 T4 现在能真正复用 T1 基础设施**：

T1 的 age 时间轴（§T1 "age 时间轴定义"）为**每条**消息都算出了所属的
`userTurnIndex` —— RoleAI 消息也一样。因此 RoleAI 消息自身的 age 直接可得：
`age = totalTurns - 1 - 自身的userTurnIndex`。T4 不需要任何额外的扫描或字段。

**压缩范围：仅 Content（文本），不含 ToolCalls.Arguments**

历史 assistant 消息的 token 来自两部分：

1. **Content（文本部分）**：模型的文本回复（解释、推理、总结）—— **T4 压缩这部分**
2. **ToolCalls.Arguments（参数部分）**：历史 tool call 的参数 JSON —— **T4 视图层不压缩**（见下方说明）

> ⚠ 关于 ToolCalls.Arguments 的有意排除（修正早期版本的错误设计）：
>
> 早期版本曾把 Arguments 截断纳入 T4。经审查确认这是错的，原因有二：
>
> 1. **不可恢复**：`edit_file` 的 `old_string`/`new_string`、`write_file` 的 `content`
>    都在 Arguments 里（message.go:55），工具执行后无法从当前环境确定性重建原始值。
>    这违反 §1.1 的硬约束"主模型需要时总能重新获取被压缩的信息"。与 T1 不同（T1 可
>    重新调用工具拿回结果），历史参数截断是真正的信息丢失。
>
> 2. **不安全**：provider 映射层会把历史 assistant tool-calls 的参数**重新序列化并发送**
>    给 provider —— OpenAI 路径编成 `Function.Arguments`（openai_compat.go:291-299），
>    Anthropic 路径作为 `ToolUseBlockParam.Input` 送出（anthropic.go:262-270）。
>    截断后产生的无效 JSON 会被 provider 看到，可能导致 provider 报错或模型行为异常。
>    早期文档"provider 不再解析历史 arguments"的论证不成立。
>
> 因此 Arguments 截断从 T4 视图层移除。
>
> **范围限定**："T4 不压缩 Arguments"这一保证**仅适用于 T4 的 prompt 视图层**
> （`buildPromptView`）。它**不覆盖**现有 compaction（`compact.go`）的行为 ——
> compaction 在阈值触发时已经会剥离 Arguments（`compactAssistantMessage`，
> compact.go:136-144，只保留 ID+Name+Status）。本设计不改 compaction（Phase 1
> 明确排除），因此一旦会话触发 compaction 阈值或 overflow 重试，历史 Arguments
> 仍会被 compaction 剥离。这是现有机制的行为，不在本设计的变更范围内。
>
> 如果未来需要让 compaction 也保留 Arguments，那是独立的 compaction 增强议题，
> 不在本 spec 范围内。

**预算梯度**（仅 Content）：

```
age=0-1   : 不压缩
age=2-3   : 保留首 500 字符 + [...]
age>=4    : 保留首 200 字节（等同 compaction 现状）
```

**视图派生规则**（T4 部分，嵌入 buildPromptView 的第二步遍历）：

```go
// 在 buildPromptView 第二步遍历中，与 RoleTool 分支并列：
if msg.Role == models.RoleAI {
    age := totalTurns - 1 - ownerTurn[i] // ownerTurn[i] 即所属的 userTurnIndex

    // 仅压缩 Content（文本）；ToolCalls 结构（含 Arguments）完全不动
    budget := cfg.conversationBudget(age)
    if budget > 0 && len(msg.Content) > budget {
        msg.Content = msg.Content[:budget] + "\n[...earlier response truncated]"
    }
}
```

**链完整性保证**：

T4 只压缩 `Content`（文本），`ToolCalls` 结构（ID/Name/Arguments）完全不动。这维持
tool_call → tool_result 链完整性，且 provider 收到的 arguments 是原始完整值。

**质量约束**（best-effort 可恢复，见原则 2）：

- 截断只作用于**历史** AI 文本（age ≥ 2）。当前 turn 的 AI 消息不经过压缩。
- AI 文本属于 best-effort 可恢复类别：架构结论、计划承诺、decision rationale 只存在
  于文本中，无确定性重取路径。T4 的策略是保留首段（通常含结论），截断中段展开。
- 这与现有 compaction（`compactAssistantTextKeep=200`，75% 阈值触发）的行为类别
  相同，只是 T4 让它更早、更可控地发生，且预算梯度更宽松（age=2-3 保留 500 字符
  vs compaction 的 200 字节）。如果现有 compaction 的 best-effort 截断是可接受的，
  T4 更宽松的梯度也是可接受的。

**预期收益**：低-中。T4 只压缩 AI 文本，不触及 Arguments。实际收益取决于任务类型 ——
如果模型产出大量解释性文本（如架构分析、代码审查），T4 有意义；如果模型回复简短
（如 coder agent 的"已修复"），T4 收益薄。Phase 0 的 `ai_content_bytes` 分桶数据
会校准实际收益。

**质量风险**：ai 回复里的推理过程被截断后，模型可能丢失"我之前为什么这么决定"的上下文。
缓解：保留首段（通常含结论），截断的是中段展开。

### 策略 T5：system prompt 分层与去重（对应来源 ①）

> **状态**：T5c ✅ 已实现；T5a/T5b 依 Phase 3 门槛**推迟到 Phase 0 数据到位后**。
> - **T5c ✅**（安全、无需数据）：(1) 从 `generalPurposeSystemPrompt`（types_config.go）
>   删除与 `BuildSystemPrompt` 权威规则重复的"Tool preference…"整句；(2)
>   `BuildSystemPrompt`（react.go）仅在 agent 注册了**任一文件工具**
>   （read_file/edit_file/write_file/list_dir/find/grep,`hasAnyFileTool`）时才追加
>   那条 ~400 字符的文件操作规则 —— 纯 bash-only agent 不再携带无关规则。
>   **review 修正**:gate 从"仅看 read_file"改为"任一文件工具"——只有 edit_file
>   没 read_file 的 agent 仍需要"用 edit_file 别用 sed -i"的指引。
>   测试 `systemprompt_test.go`(5 项)。
> - **T5a/T5b ⏸ 推迟(有意)**：二者都是**条件性收益 + 质量敏感 + 数据门槛**,不宜在
>   假设未被数据验证前投机实现(见下"为何推迟")。用户正在采集的 Phase 0 `system_bytes`
>   分桶会直接判定是否值得做。

**核心机制**：减少 system prompt 的固定开销。

**T5a. 记忆注入预算收紧**

现状记忆注入上限 2000 tokens（`prompt.go:67`）。对短会话偏大。考虑按会话长度动态
调整：前 5 turn 用 500 token 上限，之后渐增到 2000。

**T5b. 技能描述延迟加载**

现状技能注册后描述进入 system prompt（`Available skills:` 段）。技能多时膨胀。

**与现有 skill 逻辑的协同约束**（必须明确，否则 system prompt 不可预测）：

现有 skill 逻辑不是纯静态列表，而是有状态的：

1. 技能注册时，描述追加到 `agent.systemPrompt`（`AppendSystemPrompt`，react.go:160）
2. 加载某个 skill 后，`removeSkillDescriptions`（react.go:174）会**移除整个
   `Available skills:` 段**（因为技能已加载，描述不再需要）
3. skill body 通过 `AppendSystemPrompt` 注入（react.go:609）

因此 T5b 的"按相关性注入 top-K"必须明确：

- **时机**：在 `BuildSystemPrompt`（react.go:715）中动态拼接，而非修改
  `agent.systemPrompt` 字段。现有 `AppendSystemPrompt` 写入的是静态字段，
  T5b 应改为在每 turn 的 `BuildSystemPrompt` 里动态计算 top-K。
- **与 removeSkillDescriptions 的关系**：skill 加载后仍移除描述段（现状不变）。
  T5b 只影响"skill 未加载时展示哪些描述"。
- **无缓存、纯每 turn 计算**：top-K 选择是轻量关键词匹配（见下），每 turn 重算，
  不引入缓存。这避免了缓存键/失效/与 `removeSkillDescriptions` 的状态同步问题，
  也与 §11"不引入缓存层"一致。技能数量通常 ≤ 20，关键词匹配开销可忽略。
- **top-K 选择**：基于用户最近消息与技能 description 的关键词匹配（简单子串/TF-IDF），
  不引入 embedding 依赖。K = min(技能总数, 5)。

**风险**：相关性判断错误导致模型"看不到"该用的技能。缓解：匹配阈值低（宁可多带
不可漏带）；用户仍可通过 `/skill <name>` 显式加载。

**T5c. 文件操作规则去重 ✅ 已实现**

`BuildSystemPrompt`（react.go）无条件追加一段 ~400 字符的文件操作规则。审查发现
只有 `generalPurposeSystemPrompt` 重复了它（coder/researcher/reviewer 等其它 profile
不含 bash 路由）。改动：(1) 删除 `generalPurposeSystemPrompt` 里的重复句；(2) 该规则
改为**仅当 agent 注册了 `read_file` 时**才追加（`a.tools.Get("read_file") != nil`）——
无文件工具的 agent 不再携带无关规则。

**预期收益**：低。system prompt 相对不大，但它是每 turn 固定开销，精简后长会话累积受益。

**为何 T5a/T5b 推迟（有意的范围纪律）**：

- **数据门槛**：Phase 3 明文"仅在 Phase 0 显示 system prompt 占比高时实施"。用户正在
  采集的 `system_bytes`/`ai_content_bytes` 分桶会判定 memory/skill 是否真的膨胀。在数据
  到位前投机实现,违背设计自身的门槛。
- **T5a 是条件性收益**：记忆注入的 2000 是**上限**,只有当记忆 > 2000 token 才截断;
  多数会话根本触不到。把它按 turn 调小(早期 500)只在"早期就有大量记忆"这一窄场景有益,
  却有**丢事实的质量风险**。且需跨包把 `maxTokens` 从 agent 传入 `memory.Service.Inject*`。
  → 待 Phase 0 数据确认 memory 占比高再做。
  - **就绪方案**:在 `BuildSystemPrompt` 用 `len(runMessages)` 估会话长度算动态预算
    (如 `min(2000, 400 + turns*200)`),经新增的 `Inject*WithBudget(maxTokens)` 传入;
    env `DEEPAI_TOKEN_MEMORY_BUDGET=1` 开关,默认关(=当前 2000 平铺)。
- **T5b 是质量敏感 + 大改**：技能描述目前在 REPL 构造时一次性注入(`repl.go` →
  `SkillRegistry.Descriptions()` → `AppendSystemPrompt`),是烘焙进 `a.systemPrompt` 的
  静态串。做**按相关性 top-K** 必须改成 per-turn 在 `BuildSystemPrompt` 动态计算,需让
  agent 持有结构化技能列表(name+desc)、与 `removeSkillDescriptions` 协调。风险:模型
  看不到本该用的技能。收益又只在"技能很多"时显著。
  → 待 Phase 0 确认 skill 段占比高再做。
  - **就绪方案**:纯函数 `selectRelevantSkills(descs, recentUserText, k=min(n,5))` 关键词
    匹配(低阈值,宁多勿漏),per-turn 在 `BuildSystemPrompt` 调用;env 开关,默认关(=全带)。

### 策略 T6：仓库结构地图（对应来源 ④/⑤，探索阶段专项）

> ✅ **已实现**（提升为最高优先级先行落地）。工具 `code_map` 位于
> `pkg/tools/builtin/codemap.go`，已注册进 `FileTools()`，测试
> `codemap_test.go`（10 项）全绿。实测：`grep.go` 的完整符号地图 910 字节，
> 相较读取该 376 行文件全文（~11KB）,探索阶段 token 降幅显著。以下为设计说明。

**问题**：Agent 探索一个陌生代码库时，对"哪个文件负责什么"一无所知，只能靠连续的
`grep`/`glob`/`read_file` 盲目试探。这些试探性调用的结果全部进入历史，是探索阶段
token 膨胀的一大来源 —— 且很多是"读了才发现不相关"的无效读取。

**核心机制**：新增一个按需调用的只读工具 `code_map`，一次返回仓库（或子树）的
**结构化符号索引**（文件 → 符号签名，带行号），让模型在**不读文件全文**的前提下
建立导航图，然后直接精确读取目标位置。

这是 **T2a（单文件大纲）提升到仓库级**的自然延伸 —— 复用同一个签名提取器
（正则 `^func|^type|^class|^def` + 行号），只是跨文件聚合。

**与 grep 的分工**：不替代 grep，补上空档。`grep` 回答"符号 X 在哪一行"；`code_map`
回答"这个仓库/目录大致有什么结构"。探索初期一次 `code_map` 可替代十几次试探性
grep/read，这是本策略的收益来源。

**符号级，非散文级（关键取舍）**：

`code_map` 只做**确定性符号提取**（函数/类型/方法签名 + 行号 + 文件路径），
**不生成散文式"功能描述"**（如"这个文件负责鉴权"）。理由：

- 散文功能描述需要 LLM 逐文件总结（违背原则 4）或人工维护注解（引入 staleness +
  维护负担）。
- 符号签名对"我要改鉴权，该看哪个文件"这个导航问题已经足够 —— 模型看到
  `auth.go: func Login / func validateToken` 自然知道去哪。散文描述的边际价值
  撑不起它引入的 LLM 依赖或维护成本。

**无持久化、无缓存、每次现算**（与 §11.5 一致）：

正则扫全仓是毫秒级，CPU 成本可忽略。**不落地为 map 文件**，因此没有 staleness
和失效管理问题 —— 每次调用反映的都是磁盘当前状态。这是本方案区别于"持久化
repomap"（如 aider）的关键：持久化 map 需要缓存失效，且注入每 turn 会变成新的
固定开销，反而与本设计的减 token 目标冲突。

**输出预算（地图本身可能很大，必须自我约束）**：

真正的成本是地图的**输出 token**，而非扫描 CPU。因此 `code_map` 自带三重约束：

- **分层（depth 参数）**：默认第一级只给 `目录树 + 每文件一行（路径 + 符号数）`；
  模型选定目录/文件后再 drill-in 到符号列表（`depth=symbols`）。避免一次吐出全仓所有签名。
- **可 scope（path 参数）**：`code_map(path="pkg/agent")` 只映射子树。
- **可排序 + 分页（max_files 参数）**：按相关性（用户最近消息关键词）或 git 近期
  活跃度排前，截断尾部并提示 `[N more files. Narrow with path= or raise max_files.]`
  —— 复用 T2b 的分页 + 重取提示范式。
- 默认跳过 `vendor/node_modules/.git/dist/build/target` 等生成物目录;
  **`include_hidden=true` 会全部下钻（含 fold 目录,review 修正）**,这是"真源码
  住在 build/ 里"这类仓库的恢复路径。

**恢复路径（原则 2 的"严格可重取"）**：签名带行号 → 模型可直接
`read_file(path, start_line, end_line)` 精确读取具体实现。地图末尾提示：
`[Symbol outline only. Use read_file with start_line/end_line to see implementations.]`

**质量约束**：符号提取对非主流语言、宏密集代码（Go generate、Rust 宏、模板引擎）
会漏。缓解同 T2a —— 漏了模型仍可 grep/read 兜底，地图只求"够用的导航"，不求完备。
未知语言的文件回退为"文件名 + 行数"一行，不提取符号。

**与 T1/T2 的协同**：

- `code_map` 的结果是普通 tool_result，进入历史后由 **T1 自然老化**（探索完成后
  这张地图不再需要完整保留），不占永久预算。
- 与 T2 同类（工具层事前塑形），但粒度是仓库/目录而非单文件。

**预期收益**：高。直接砍掉探索阶段的盲目遍历 —— Phase 0 的"单次工具结果字节"
辅助指标很可能显示这是一大膨胀源（大量试探性 read_file / 宽 grep）。

---

## 5. 策略优先级与依赖

```
收益预期
  高 │  T6 (仓库结构地图)  ◄── ✅ 已实现（最高优先级，先行落地）
     │  T1 (工具结果老化)  ◄── ✅ 已实现（本设计核心）
     │  T2 (工具输出塑形)  ◄── ✅ 已实现（T2a/b/c）
     │  T3 (schema 精简)   ◄── ✅ 已实现（T3a；T3b/c 已澄清）
  中 │  T4 (对话历史摘要) ◄── 依赖 T1 基础设施
     │  T5 (system prompt)  ◄── ✅ T5c 已实现；T5a/b 待 Phase 0 数据
  低 │
     └──────────────────────────────► 实施顺序
        Phase 0    Phase 1     Phase 2   Phase 3
        度量       T6→T1+T2    T3+T4     T5
       (T6 可独立于度量先行，因它是新增只读工具，不改任何现有路径)
```

**依赖关系**：

- T4 与 T1 共用 `buildPromptView` 视图函数（同一次遍历，不同 Role 的预算策略）
- T2 与 T1 协同：T2 事前塑形减少单次结果体积，T1 视图层老化压缩历史累积
- T6 复用 T2a 的签名提取器（跨文件聚合）和 T2c 的 fold 列表；结果由 T1 老化
- T3、T5 独立，可并行实施

---

## 6. 分阶段实施计划

### Phase 0：度量框架（前置，必做）

> ✅ **已实现**。框架在 `pkg/agent/metrics.go`：`MetricsSink` 接口 +
> `TurnMetrics`（主指标 provider InputTokens/OutputTokens + 辅助字节分桶
> `ContextBytes`）+ `ToolResultMetric`（单工具结果字节）+ 现成的
> `LoggingMetricsSink`（结构化日志，可 grep 成报告）。接入点：`react.go` 三处
> （请求前算字节分桶、streamUsage 处发主指标、两个 tool append 处发工具字节）。
> 默认 `AgentConfig.Metrics == nil` = 零开销。测试 `metrics_test.go`（4 项，含
> 端到端验证三处埋点全部触发）全绿。启用方式：`AgentConfig.Metrics =
> agent.LoggingMetricsSink{}`（或自定义 sink 收集结构化记录做报告）。

**目标**：用真实数据验证哪些 token 来源是膨胀主因，校准策略优先级。

**主指标 vs 辅助指标**（修正：字节启发式不可靠，不能作为决策主指标）：

现有 `estimateTokens`（compact.go:195）是字节启发式（bytes/3），对中英混合、代码块、
JSON schema 的真实 token 成本不稳定。system prompt 是 provider 请求的独立字段，
tools schema 走 provider-specific 映射（provider.go:15），字节估算尤其不准。

因此 Phase 0 采用**provider 真实 token 为主指标，字节分桶为辅助解释指标**。

**主指标：provider 真实 InputTokens**（响应时）

provider 返回的 `Usage.InputTokens`（react.go:417 已有 `streamUsage.InputTokens`）
是模型真实 tokenizer 的计数，对 CJK/代码/schema 都准确。每 turn 记录：

- turn 编号、`streamUsage.InputTokens`、`streamUsage.OutputTokens`

这是**决策的唯一权威指标**。退出条件基于它判断。

**辅助指标 1：每 turn 上下文构成**（请求前，字节分桶）

在组装 `ChatRequest` 前（react.go:335），按消息角色分桶估算字节：

- system_bytes / human_bytes / ai_bytes / tool_bytes / total_bytes / tool_fraction

其中 `ai_bytes` 进一步拆分为 `ai_content_bytes`（文本）和 `ai_args_bytes`
（所有 ToolCalls.Arguments 的 JSON 总字节）。这决定了 T4（仅压 Content）的实际收益，
以及 Arguments 是否大到值得作为后续可选实验项重新评估（见 §9.3）。

用途：**解释**主指标的变化原因（"InputTokens 上涨是因为 tool_bytes 上涨"），
而非直接决策。字节分桶与主指标的比值还能校准 `estimateTokens` 的误差率。

**辅助指标 2：单次工具结果**（写入时）

在 `runOneTool` 返回后（react.go:513 并行 / react.go:620 串行两处）记录：

- turn 编号、工具名、结果字节数

用途：识别哪些工具产出大结果（指导 T2 的工具塑形优先级）。

**产出**：10-20 个真实会话的指标报告，含：

- **InputTokens vs turn 曲线**（主指标，决策依据）
- tool_fraction（字节）vs turn 曲线（辅助，解释工具结果占比趋势）
- 各工具类型的平均/最大结果字节（指导 T2）
- system prompt + schema 的字节估算占比（辅助，指导 T3/T5）

**退出条件**（基于主指标 InputTokens）：

- 第 5 turn 的 InputTokens 相对第 1 turn 增长超过 3 倍 → 历史累积严重，
  T1/T2 高优先级，进入 Phase 1
- 增长温和（< 2 倍）但第 1 turn 的 InputTokens 基线就很高 → 固定开销（schema/
  system prompt）是大头，T3/T5 提前到 Phase 1
- 用辅助指标定位：若 tool_fraction（字节）高且 InputTokens 增长快 → 确认 T1/T2；
  若 tool_fraction 低但 InputTokens 基线高 → 确认 T3/T5

### Phase 1：T1 + T2（核心收益）

**T1：工具结果时效老化（视图层方案）—— ✅ 已完成**

1. ✅ 实现 `buildPromptView(messages, cfg, contextWindow)`（`pkg/agent/aging.go`）——
   从 canonical 消息派生 prompt 视图，扫描建立 userTurnIndex，按 age 压缩历史
   RoleTool 的 Content。函数内已含 T4 的 `case RoleAI` 分支，默认不启用。
2. ✅ 接入点：`react.go` 组装 `ChatRequest` 处，`Messages: promptView`
   （`promptView = buildPromptView(runMessages, a.aging, a.contextWindow)`；
   canonical `runMessages` 不变）。
3. ✅ 配置：`AgingConfig`（Enabled + `MinContextPressure` + `ToolResultBudgets` +
   `ConversationBudgets`），挂在 `AgentConfig.Aging`，`New()` 读入 `a.aging`。
   默认 nil = 关闭。Phase 0 的 InputTokens vs turn 曲线可校准 `MinContextPressure`。
4. ✅ T4 置空：集成时 `ConversationBudgets` 设为空 map（T1-only），T4 不生效。
5. ✅ 测试：`aging_test.go`（单元：步进预算/canonical 不变/压力门槛/纯文本边界/
   T4 开关/结构保真）+ `aging_integration_test.go`（端到端 + 默认关闭向后兼容）。
6. ✅ 未改动 `models.Message`、写入点、compaction、session 持久化、memory queue。
4. **Phase 1 的 AgingConfig 必须显式置空 `ConversationBudgets`**（设为空 map），
   确保 T4 不随 T1 生效。这样 Phase 1 的 token 下降和质量变化可纯净归因到 T1/T2。
5. 默认关闭（向后兼容），需显式启用
6. **不修改** `models.Message`、写入点、compaction、session 持久化、memory queue

**T2：工具输出事前塑形 —— ✅ 已完成**

1. ✅ T2a：`read_file` 大纲模式（`file.go` ReadFileHandler + `shaping.go`
   `buildFileOutline`，复用 T6 `extractSymbols`）；`full=true`/range 绕过。
2. ✅ T2b：`grep` 100→50、`find` 200→50（`shaping.go` `GrepMaxResults`/
   `FindMaxResults`），截断消息附重取提示。
3. ✅ T2c：`list_dir` 折叠生成物目录 + `expand_dirs` 参数（`Data["entries"]` 完整）。
4. ✅ 测试 `shaping_test.go`（8 项：大纲/full/range/小文件、grep/find 上限、
   折叠/展开），现有工具测试全绿无回归。

**T6：仓库结构地图 —— ✅ 已完成（先于 T1/T2 落地）**

1. ✅ 实现 `extractSymbols(content, ext) []symbol`（`codemap.go`）——确定性正则提取，
   覆盖 go/python/js/ts/rust/zig/java/c/cpp/ruby/php；未知扩展名返回 nil。后续 T2a
   的 read_file 大纲直接复用此函数（同 package）。
2. ✅ 新增只读工具 `code_map`（`path`/`depth`/`max_files`/`include_hidden` 参数）：
   `depth=tree`（默认）列文件+符号数，`depth=symbols` 列带行号签名，单文件目标直接
   出签名；折叠 `.git/node_modules/vendor/dist/build/...`；分页 + 重取提示齐全。
3. ✅ 注册进 `FileTools()`（`file.go`），自动进入 gateway server 与 chat 命令的
   工具集；`ParallelSafe=true`（只读）。结果由 T1 自然老化（T1 落地后生效）。
   **review 修正**：补进 `planToolNames`（plan.go）与所有含 read_file+grep 的
   agent-type DefaultTools（types_config.go）—— 此前它恰好在"探索专用"场景
   （plan mode、只读 reviewer/researcher profile）里被 Restrict 掉。
4. ✅ TDD：`codemap_test.go` 12 项测试覆盖两种 depth、折叠、include_hidden 下钻、
   分页、路径 scope、单文件、空目录、注册与多语言提取，全绿。
5. 说明：T6 是新增只读工具，**不改任何现有路径**（不改 message/compaction/持久化/
   memory），因此可独立于 Phase 0 度量先行落地，风险最低。

**质量验证**：

- 10 个真实任务，对比启用前后的任务完成率（不因截断而失败）
- 监控平均 turn 数变化（截断不应导致显著重试增加）

**退出条件**：

- input token ↓ ≥ 20% 且任务完成率不降 → 达标
- 否则分析瓶颈，调整预算梯度或进入 Phase 2

### Phase 2：T3 + T4（schema 与对话历史）

**T3：工具 schema 精简 —— ✅ 已完成**

1. ✅ T3a：删除各工具描述里与 system prompt 文件操作规则重复的 bash 路由文本
   （`bash.go`/`file.go`/`edit.go`/`listdir.go`/`grep.go`/`find.go`）。描述字节 ↓约 16%。
2. ✅ T3c：审查确认参数 schema description 不与工具 Description 重复，无可去重项。
3. ✅ T3b：确认由现有 plan mode（`enterPlanMode` 只读裁剪）满足；常规 ReAct 不裁剪
   （风险高，本设计明确排除）。
4. ✅ 测试 `descriptions_test.go`（3 项回归守卫）。

**T4：对话历史摘要（仅 Content）—— 在此阶段启用**

T4 的代码分支在 Phase 1 已建好（`buildPromptView` 的 `case RoleAI`），但 Phase 1
通过置空 `ConversationBudgets` 确保它不生效。Phase 2 的工作是**启用并校准** T4：

1. 将 `ConversationBudgets` 从空 map 改为基于 Phase 0 数据的默认值
   （`ai_content_bytes` 分桶数据指导预算梯度）
2. 在 Phase 1 的纯净基线上测量 T4 的增量效果（token 下降 + 质量变化可归因到 T4）
3. 如果 Phase 0 显示 `ai_args_bytes` 占比很高，记录为后续可选实验项（见 §9.3），
   但本期不实现 Arguments 压缩
4. 无需额外基础设施 —— age 计算和遍历复用 T1，同一次 `buildPromptView` 调用完成

**退出条件**：累计 input token ↓ ≥ 30%，任务完成率不降。

### Phase 3：T5（system prompt 优化）

仅在 Phase 0 显示 system prompt 占比高、或前两阶段收益不足时实施。

1. T5a：记忆注入动态预算 —— ⏸ 待 Phase 0 数据（就绪方案见 §4 T5）
2. T5b：技能描述延迟加载 —— ⏸ 待 Phase 0 数据（就绪方案见 §4 T5）
3. T5c：文件操作规则去重 —— ✅ 已实现（安全、无需数据，已先行落地）

---

## 6.5 启用与接线（✅ 已实现）

T2/T3 是默认生效的工具层改动（已在 `FileTools()`/`BashTool()` 生效）。T1 与 Phase 0
度量是**默认关闭**的，通过 `AgentConfig` 字段或环境变量启用。

**单一接线点**：`agent.New()`（react.go）是 REPL（`chat/repl.go:484`）、gateway
（`gateway/server.go:300`）、subagent（`agent/subagent.go:118`）三条路径的共同入口。
`New()` 调用 `applyTokenEfficiencyDefaults(&cfg)`（`pkg/agent/config_env.go`），据环境
变量填充未显式设置的 `Metrics`/`Aging`。显式 `AgentConfig` 值永远优先。

| 环境变量 | 效果 | 默认 |
|----------|------|------|
| `DEEPAI_TOKEN_METRICS=1` | 挂 `FileMetricsSink`，写 JSONL 到 `$TMPDIR/deepai-token-metrics.jsonl` | 关 |
| `DEEPAI_TOKEN_METRICS=/path.jsonl` | 同上，写到指定文件 | 关 |
| `DEEPAI_TOKEN_AGING=1` | 启用 T1 工具结果老化（T1-only：`MinContextPressure=0.4`，`ConversationBudgets` 空 → T4 不生效） | 关 |

> falsy 值（`0`/`false`/`no`/`off`）对两个变量都表示**关闭**（review 修正:早期版本
> 会把 `DEEPAI_TOKEN_METRICS=0` 的 `0` 当作输出文件路径）。agent 测试包有 `TestMain`
> 统一剥离环境里的 `DEEPAI_TOKEN_*`,开发者 shell 里的 export 不会让 `go test` 假失败。

**持久化配置（推荐，免 shell export）**：`~/.deepai/config.yaml` 支持同义键，
`deepai chat` 启动时经 `applyTokenEfficiencyEnv`（`pkg/commands/setup.go`）桥接到上述
env（仅当对应 env 未设置——**显式 env 永远优先**，符合"env 覆盖配置文件"惯例）：

```yaml
# ~/.deepai/config.yaml
token_metrics: "1"                  # 或写具体路径 "~/.deepai-metrics.jsonl"
token_aging: true
```

测试：`pkg/commands/setup_test.go`（config→env 桥接 / env 优先 / 未配置时 no-op /
YAML 解析往返）。注：桥接目前接在 `deepai chat` 入口；gateway 走独立配置源，需要时
在其入口调用同一 `applyTokenEfficiencyEnv` 即可。

> ⚠ **为什么用专用文件而非 slog**：早期版本用 `LoggingMetricsSink`（slog Info），但
> 默认 `deepai chat` 的 stderr 级别是 Error、且无 debug 文件，Info 度量行会被**静默
> 丢弃**（既不写文件也不显示）。`FileMetricsSink`（`pkg/agent/metrics_file.go`）直接写
> **独立 JSONL 文件**，绕开 slog 级别路由，每条一行、带 `type` 字段（`turn`/`tool`），
> open-append-close + O_APPEND 保证多 agent/subagent 并发写同一文件不串行。
> `LoggingMetricsSink` 仍保留供程序化使用。

**JSONL 样例**（每行一条记录）：

```json
{"type":"turn","turn":1,"input_tokens":3400,"output_tokens":120,"context":{"system_bytes":900,"schema_bytes":2100,"human_bytes":60,"ai_content_bytes":0,"ai_args_bytes":0,"tool_bytes":5100,"total_bytes":8160}}
{"type":"tool","turn":0,"tool_name":"read_file","result_bytes":5073}
```

**出报告（jq 一行）**：

```sh
# InputTokens vs turn + tool 占比曲线（tool_fraction 现算，JSONL 里不含该字段）
jq -c 'select(.type=="turn")|{turn,input_tokens,tf:(.context.tool_bytes/.context.total_bytes)}' \
  $TMPDIR/deepai-token-metrics.jsonl
# 各工具平均结果字节（指导 T2 优先级）
jq -s 'map(select(.type=="tool"))|group_by(.tool_name)|map({tool:.[0].tool_name,avg:(map(.result_bytes)|add/length)})' \
  $TMPDIR/deepai-token-metrics.jsonl
```

**推荐流程**：先 `DEEPAI_TOKEN_METRICS=1` 跑 10-20 个真实会话 → 用上面 jq 出报告 →
据 InputTokens/tool_fraction 曲线校准 `MinContextPressure` 与预算梯度 → 再
`DEEPAI_TOKEN_AGING=1` 启用 T1。程序化定制（自定义 sink、非默认预算、启用 T4）仍走
`AgentConfig.Metrics`/`.Aging`。

测试：`config_env_test.go`（默认关闭 / metrics 默认路径 + 显式路径 / aging 启用 / 显式配置
优先）、`metrics_file_test.go`（JSONL 格式 / 跨 sink 追加 / 建父目录 / 写错误非致命 /
env→New→Run→落盘端到端）。

---

## 7. 关键集成点（代码级）

所有集成点已对照代码核实：

| 策略 | 文件 | 位置 | 改动 |
|------|------|------|------|
| T1 视图派生 | `pkg/agent/react.go` | 组装 ChatRequest 处 | ✅ `Messages: buildPromptView(runMessages, a.aging, a.contextWindow)` |
| T1 视图函数 | `pkg/agent/aging.go` | — | ✅ `buildPromptView(messages, cfg, contextWindow)`；含压力门槛（复用 `estimateTokens`） |
| T1/T4 配置 | `pkg/agent/types.go` | AgentConfig | ✅ 加 `Aging *AgingConfig`；`react.go` New() 读入 `a.aging` |
| T4 | `pkg/agent/react.go` | `buildPromptView` 的 `case RoleAI` | 仅 Content 压缩（Phase 1 已建分支，Phase 2 校准参数） |
| T2 配置 | `pkg/tools/builtin/shaping.go` | — | ✅ `ReadFileOutlineThreshold`/`GrepMaxResults`/`FindMaxResults`/`listDirFoldDirs` + `buildFileOutline` |
| T2a | `pkg/tools/builtin/file.go` | ReadFileHandler | ✅ 大纲模式（>500 行）+ `full` 参数 |
| T2b | `pkg/tools/builtin/grep.go` | 默认 maxResults | ✅ 100 → 50 + 重取提示 |
| T2b | `pkg/tools/builtin/find.go` | 默认 maxResults | ✅ 200 → 50 + 重取提示 |
| T2c | `pkg/tools/builtin/listdir.go` | ListDirHandler + InputSchema | ✅ 折叠生成物 + `expand_dirs` 参数 |
| T6 提取器 | `pkg/tools/builtin/codemap.go` | `extractSymbols(content, ext)` | ✅ 已实现；同 package，T2a read_file 大纲后续直接复用 |
| T6 工具 | `pkg/tools/builtin/codemap.go` | `CodeMapHandler` + `CodeMapTool` | ✅ 已实现：跨文件符号聚合，分层/分页/折叠/行号；已注册进 `FileTools()` |
| T3a | `pkg/tools/builtin/{bash,file,edit,listdir,grep,find}.go` | 各工具 Description | ✅ 删除与 system prompt 重复的 bash 路由文本（描述字节 ↓~16%） |
| Phase 0 框架 | `pkg/agent/metrics.go` | — | ✅ `MetricsSink`/`TurnMetrics`/`ToolResultMetric`/`computeContextBytes`/`LoggingMetricsSink` |
| Phase 0 主指标 | `pkg/agent/react.go` | streamUsage 处 | ✅ 发 `TurnMetrics`（provider tokens + 字节分桶） |
| Phase 0 辅助 1 | `pkg/agent/react.go` | ChatRequest 组装前 | ✅ `computeContextBytes(promptView, systemPrompt, toolSchemaBytes)` |
| Phase 0 辅助 2 | `pkg/agent/react.go` | 两处 tool append 后 | ✅ 发 `ToolResultMetric`（工具名 + 结果字节） |
| Phase 0 配置 | `pkg/agent/types.go` | AgentConfig | ✅ 加 `Metrics MetricsSink`；`New()` 读入 `a.metrics` |
| 现有 compaction | `pkg/agent/compact.go` | compactMessages | 不改（T1 视图层与之正交） |
| 现有硬上限 | `pkg/agent/react.go` | L868 toolMessageContent | 不改（T1 保留） |
| canonical 消息 | `pkg/models/message.go` | Message 结构 | **不改**（无运行时字段） |
| 会话持久化 | `pkg/chat/session.go` | AppendMessage | **不改**（canonical 不受影响） |
| memory queue | `pkg/memory/queue.go` | prepareAsyncMessages | **不改**（canonical 不受影响） |

---

## 8. 质量保障设计

"不以降低质量为代价"是硬约束。每个策略的质量保障机制：

### 8.1 可恢复性保证（分层，见原则 2）

**严格可重取**（T1/T2）—— 压缩时附带明确的重取提示：

| 策略 | 压缩时提示 | 重取路径 |
|------|-----------|----------|
| T1 老化 | `[...aged: re-call {toolName} to see full output]` | 重新调用同一工具 |
| T2a 大纲 | `[File has N lines. Use read_file with start_line/end_line.]` | 按行范围重读 |
| T2b 分页 | `[Showing 50 of N matches. Refine your pattern.]` | 收窄 pattern 或加大 max_results |
| T2c 折叠 | `[dir/ (N entries, omitted). Use list_dir with expand_dirs=true to expand.]` | expand_dirs=true 展开 |
| T6 地图 | `[Symbol outline only. Use read_file with start_line/end_line to see implementations.]` + `[N more files. Narrow with path= or raise max_files.]` | 按行号精确读取实现；缩小 path 或加大 max_files |

模型看到提示后可重新调用工具恢复完整信息。

**Best-effort 可恢复**（T4）—— 无确定性重取路径，但信息丢失后果可控：

| 策略 | 压缩时提示 | 可恢复性 |
|------|-----------|----------|
| T4 摘要 | `[...earlier response truncated]` | 保留首段（含结论）；与现有 compaction 行为类别相同 |

T4 不提供重取路径，因为 AI 文本（解释、推理、计划承诺）无法通过工具调用重建。
这是有意的取舍 —— 见原则 2 的分层论证。

### 8.2 当前 turn 完整性

T1 的预算梯度保证 age=0（当前 turn）不压缩。模型在本轮永远看到完整的工具结果。
压缩只作用于历史。

### 8.3 结构完整性

T1/T4 作用于 prompt 视图，只压缩视图副本的 `Content`，保留 `ToolResult` 结构
（CallID/ToolName/Status）和 `ToolCalls` 结构（ID/Name）。这维持：

- tool_call → tool_result 链不断（provider 校验要求）
- canonical 消息完整不变（持久化/回放/memory 管道看到的是原始内容）

### 8.4 canonical 不变性保证

视图层方案的核心保证：**canonical `runMessages` 在整个 Run 期间不被 T1/T4 修改**。

- `buildPromptView` 返回新切片，canonical 切片不变
- 视图副本的 `Content` 被压缩，但 `ToolResult` 指针共享（浅拷贝）—— 指针指向的
  canonical `ToolResult.Content` 不被修改
- compaction（compact.go）仍作用于 canonical 消息，是独立的状态变更，与视图层无关

这意味着：session 持久化（session.go:271）、memory 抽取（queue.go:481）、
会话回放（session.go:217）全部看到完整的 canonical 内容，不受 T1/T4 影响。

### 8.5 质量回归测试

每阶段含：

1. **功能测试**：被压缩后，agent 仍能完成需要该信息的任务（通过重调用工具）
2. **链完整性测试**：视图压缩后 tool_call → tool_result 链不断
3. **canonical 不变性测试**：启用 T1 后，`runMessages` 的 Content 与未启用时完全一致
4. **compaction 交互测试**：T1 视图 + compaction 同时启用时，行为正确且不互相破坏
5. **端到端对比**：10 个真实任务，启用前后完成率 + 平均 turn 数 + token 总量对比

---

## 9. 配置设计

### 9.1 统一配置接口

T1 和 T4 共用同一个 `buildPromptView` 函数，因此共用同一个聚合配置类型。
`buildPromptView` 的签名是 `(messages, cfg *AgingConfig)`，`AgingConfig` 是唯一入口：

```go
// AgingConfig 控制 buildPromptView 的老化压缩行为（T1 + T4 共用）。
// nil 或 Enabled=false 时 buildPromptView 直接返回原消息（零拷贝）。
type AgingConfig struct {
    Enabled bool

    // MinContextPressure 是启动老化的上下文压力门槛（占窗口比例，默认 0.4）。
    // 当前上下文估算 token < MinContextPressure × contextWindow 时，buildPromptView
    // 零拷贝返回、不做任何压缩。这避免短会话/探索初期在窗口远未膨胀时过度压缩。
    // 0 表示无门槛（一律按 age 压缩，回到"每 turn 无条件压"的旧行为）。
    MinContextPressure float64

    // ToolResultBudgets 按年龄设定 RoleTool 消息 Content 的字节上限（T1）。
    // nil 时使用默认: {1: 8192, 2: 2048, 3: 300}
    // age=0 永远不压缩（当前 turn）。
    ToolResultBudgets map[int]int

    // ConversationBudgets 按年龄设定 RoleAI 消息 Content 的字节上限（T4）。
    // nil 时使用默认: {2: 500, 4: 200}（age 0-1 不压缩）
    // 仅压缩 Content（文本部分），不影响 ToolCalls 结构。
    ConversationBudgets map[int]int
}

// 以下方法返回指定年龄的 Content 字节上限，0 表示不压缩。
func (c *AgingConfig) toolResultBudget(age int) int { ... }
func (c *AgingConfig) conversationBudget(age int) int { ... }
```

### 9.2 与 AgentConfig 的关系

`AgingConfig` 作为 `AgentConfig` 的一个字段：

```go
type AgentConfig struct {
    // ...existing fields...

    // Aging 控制 prompt 视图的老化压缩（T1 工具结果 + T4 对话历史文本）。
    // nil = 禁用，行为不变。作用于 buildPromptView 派生的 prompt 视图，
    // 不修改 canonical 消息。
    Aging *AgingConfig
}
```

T1 和 T4 **不能分别启用** —— 它们是同一个 `buildPromptView` 遍历里的两个分支，
由同一个 `AgingConfig` 驱动。如果需要"只启用 T1 不启用 T4"，将 `ConversationBudgets`
设为空 map（所有 age 返回 0 = 不压缩）。

> **设计理由**：T1/T4 共用 age 时间轴（`userTurnIndex` 扫描）和视图派生基础设施。
> 拆成两个独立配置会导致两个 `buildPromptView` 或两次扫描，违背"同一次遍历"的设计。
> 用一个 `AgingConfig` 里的两组 budget map 表达"对哪类内容、在哪个 age、压到多少"。

### 9.3 ToolCalls.Arguments 的有意排除（仅限 T4 视图层）

T4 视图层**不压缩** `ToolCalls.Arguments`。这是有意的设计取舍，原因有二：

1. **不可恢复**：`edit_file` 的 `old_string`/`new_string`、`write_file` 的 `content`
   都在 Arguments 里（message.go:55），工具执行后无法从当前环境确定性重建原始值。
   这违反 §1.1 的硬约束"主模型需要时总能重新获取被压缩的信息"。与 T1 不同（T1 可
   重新调用工具拿回结果），历史参数截断是真正的信息丢失。

2. **不安全**：provider 映射层会把历史 assistant tool-calls 的参数**重新序列化并发送**
   给 provider —— OpenAI 路径编成 `Function.Arguments`（openai_compat.go:291-299），
   Anthropic 路径作为 `ToolUseBlockParam.Input` 送出（anthropic.go:262-270）。
   截断后产生的无效 JSON 会被 provider 看到，可能导致 provider 报错或模型行为异常。

**范围限定**：以上保证**仅适用于 T4 的 `buildPromptView` 视图层**。它不覆盖现有
compaction —— compaction 的 `compactAssistantMessage`（compact.go:136-144）在阈值
触发时已经会剥离 Arguments（只保留 ID+Name+Status）。本设计不改 compaction，因此
触发 compaction 后历史 Arguments 仍会被剥离。这是现有机制的行为，不在本设计变更范围内。

`AgingConfig` 不含 `ToolCallArgBudgets` 字段。

### 9.4 工具塑形配置（T2，独立于 AgingConfig）

> ✅ **已实现**：落地为 `pkg/tools/builtin/shaping.go` 的**包级变量 + 每调用参数**，
> 而非一个注入式 struct。原因：现有 builtin 工具经无参 `FooTool()` 构造，硬编码默认值
> （如 grep `maxResults:=100`）是既有风格；包级变量保留可配置性（可被未来的
> `ToolShapingConfig` 覆盖），而每调用参数（`start_line/end_line`、`full`、
> `max_results`、`expand_dirs`）提供确定性的恢复路径。实际形态：

```go
// pkg/tools/builtin/shaping.go —— 已实现
var (
    ReadFileOutlineThreshold = 500 // >此行数且无 range/limit/full 时启用大纲。0=禁用
    GrepMaxResults           = 50
    FindMaxResults           = 50
)
var listDirFoldDirs = map[string]bool{ // T2c 折叠目录集
    "vendor": true, "node_modules": true, ".git": true,
    "__pycache__": true, "dist": true, "build": true, "target": true,
}
```

> T6 `code_map` 已直接注册进 `FileTools()`（无需 `CodeMapEnabled` 开关）；其
> `path`/`depth`/`max_files` 走每调用参数，与此处包级默认值风格一致。未来若需统一的
> 注入式 `ToolShapingConfig`，把上述包级变量改为从 struct 读取即可，接口已就位。

---

## 10. 风险与缓解

| 风险 | 严重度 | 缓解 |
|------|--------|------|
| 老化截断导致模型重调用工具，turn 数增加 | 中 | 预算梯度让近期 turn 足够完整；`MinContextPressure` 门槛让窗口远未膨胀时不压缩；Phase 1 监控 turn 数 |
| 短会话/探索初期无收益地压缩历史 | 低 | `MinContextPressure`（默认 0.4）门槛：占窗口 <40% 时视图零拷贝、不压缩 |
| read_file 大纲遗漏关键代码（正则不全） | 中 | 大纲仅限有符号提取器的代码文件（非代码文件一律全文,review 修正）；大纲只求"够用" |
| schema 裁剪导致模型"忘记"工具 | 高 | T3b 只在 plan mode 等明确只读阶段裁剪 |
| 视图层与 compaction 叠加产生意外交互 | 低 | 视图只读 canonical，compaction 改 canonical，两者独立；Phase 1 测试覆盖 |
| 默认参数过激导致质量下降 | 中 | 默认关闭，需显式启用；Phase 0 数据校准参数 |
| age 扫描推断在异常消息序列下失准 | 低 | age 由扫描 RoleAI 序号计算，不依赖 Run.turn；纯文本 AI 也算边界；异常时 age 偏大只会多压缩，不破坏链 |

---

## 11. 不做什么

明确排除以下方向（避免范围蔓延）：

1. **不引入二级 LLM 调用**（如"小模型压缩 + 大模型推理"）。增加复杂度且收益不确定。
2. **不压缩 output token**。压缩输出等于限制模型能力。
3. **不静默删除消息**。所有压缩都是"有损但可恢复"，附带重获取提示。
4. **不改变工具协议**（ToolCall/ToolResult schema 不变）。
5. **不引入缓存层**。本设计的策略都是无状态变换（T1/T4 每 turn 重新扫描派生，
   T5b 每 turn 重新计算 top-K），无需缓存失效管理。
6. **不改变 ReAct 循环结构**。所有策略在现有循环的既有接入点插入。
7. **不引入向量检索式上下文管理（RAG 路线）**。把历史 tool_result 存入向量库、
   按相关性 embedding 检索回来（如 zvec、memrec 的混合检索）是另一条技术路线。
   它需要 embedding 调用（违背原则 4 的"无 LLM/无额外模型调用"）和向量索引缓存
   （违背 §11.5"不引入缓存层"），且引入检索命中率这一新的质量变量。本设计坚持
   确定性、上下文内、无外部依赖的压缩。向量记忆是**正交的互补维度**（跨会话持久
   记忆），若未来需要，应作为独立的 memory 子系统议题，不在本 spec 范围内。

---

## 12. 相关文件

**核心改动**：

1. `pkg/agent/aging.go` —— ✅ T1/T4 `buildPromptView` + `AgingConfig`（已实现）
2. `pkg/agent/metrics.go` —— ✅ Phase 0 度量框架（`MetricsSink`/`TurnMetrics`/`computeContextBytes`/`LoggingMetricsSink`）
3. `pkg/agent/metrics_file.go` —— ✅ `FileMetricsSink`（专用 JSONL 落盘，绕开 slog 级别路由）
5. `pkg/agent/config_env.go` —— ✅ 环境变量接线（`DEEPAI_TOKEN_METRICS`=路径/1、`DEEPAI_TOKEN_AGING`），在 `New()` 单点应用
6. `pkg/agent/react.go` —— ✅ T1 接入点 + `a.aging`；✅ Phase 0 三处埋点 + `a.metrics`；✅ `New()` 调 `applyTokenEfficiencyDefaults`；T4 参数校准待做
7. `pkg/agent/compact.go` —— 现有 compaction（不改，T1 视图层与之正交）
8. `pkg/agent/types.go` —— ✅ `AgentConfig.Aging` + `AgentConfig.Metrics`（已实现）
9. `pkg/tools/builtin/codemap.go` —— ✅ T6 `code_map` 工具 + 共用的 `extractSymbols`（已实现）
10. `pkg/tools/builtin/shaping.go` —— ✅ T2 配置 + `buildFileOutline` + 折叠助手（已实现）
11. `pkg/tools/builtin/file.go` —— ✅ T2a read_file 大纲（复用 `extractSymbols`）+ `full` 参数；已注册 T6
12. `pkg/tools/builtin/grep.go` —— ✅ T2b 默认上限 100→50
13. `pkg/tools/builtin/find.go` —— ✅ T2b 默认上限 200→50
14. `pkg/tools/builtin/listdir.go` —— ✅ T2c 折叠 + `expand_dirs`
15. `pkg/tools/builtin/{bash,edit}.go` —— ✅ T3a 描述精简（其余 T3a 改动在上面各文件）
16. `pkg/agent/types_config.go` —— ✅ T5c：`generalPurposeSystemPrompt` 去重（另见 §12 types.go 的 Aging/Metrics）
17. `pkg/agent/react.go`（BuildSystemPrompt）—— ✅ T5c：文件操作规则改为仅在有 `read_file` 时追加
18. `pkg/commands/setup.go` —— ✅ config.yaml `token_metrics`/`token_aging` 字段 + `applyTokenEfficiencyEnv` 桥接
19. `pkg/commands/chat.go` —— ✅ `runChat` 加载配置后调用桥接

**明确不改（视图层方案的关键优势）**：

- `pkg/models/message.go` —— Message 结构不加任何运行时字段
- `pkg/chat/session.go` —— 会话持久化不受影响（canonical 不变）
- `pkg/memory/queue.go` —— memory 抽取管道不受影响（canonical 不变）
- `pkg/agent/subagent.go` —— subagent 机制不受影响

**参考（不改）**：

- `pkg/memory/prompt.go` —— 记忆注入（T5a 考虑调整预算）
- `pkg/llm/provider.go` —— provider 接口（本设计不涉及）

---

## 附录 A：Message 生命周期路径清单

> 本附录回应 code review 的开放问题：把运行时状态放到 Message 上会跨越哪些路径。
> 视图层方案（原则 6）使本清单里的所有路径都**不受 T1/T4 影响**，因为 canonical
> Message 不被修改。列出此清单是为了显式验证这一保证。

`models.Message` 在系统中的完整流转路径：

| 路径 | 位置 | 操作 | T1/T4 影响 |
|------|------|------|-----------|
| **创建：human 消息** | `react.go:242` `Run` 入参 | 调用方传入 | 无（不老化 human） |
| **创建：AI 消息** | `react.go:433` | 构造后 append 到 runMessages | 无（canonical 写入，视图只读） |
| **创建：tool 消息（并行）** | `react.go:513` | runOneTool 返回后 append | 无（canonical 写入，视图只读） |
| **创建：tool 消息（串行）** | `react.go:620` | runOneTool 返回后 append | 无（canonical 写入，视图只读） |
| **读取：组装 ChatRequest** | `react.go:335-342` | 传给 provider | **T1/T4 唯一介入点**：此处用 `buildPromptView` 的返回值替换 Messages |
| **变更：compaction** | `compact.go:62` `compactMessages` | 构造新 Message 副本，替换 runMessages | 无（compaction 作用于 canonical；视图每 turn 从最新 canonical 重新派生） |
| **变更：溢出重试** | `react.go:348` `compactOnOverflow` | 调用 compactMessages | 无（同上） |
| **持久化：落库** | `chat/session.go:271` `AppendMessage` | Content + ToolResult JSON 写入 SQLite | 无（canonical 完整） |
| **持久化：回放** | `chat/session.go:217` `LoadMessages` | 从 SQLite 重建 Message | 无（canonical 完整） |
| **memory：异步抽取** | `memory/queue.go:481` `prepareAsyncMessages` | cloneMessages 后送 LLM | 无（canonical 完整） |
| **memory：同步 flush** | `react.go:284-294` | compaction 前同步抽取 | 无（canonical 完整） |
| **subagent：结果传递** | `subagent.go` `Execute` | Message 跨 agent 边界 | 无（canonical 完整） |
| **事件：ToolResult 事件** | `react.go:523/629` | emit AgentEvent 携带 result | 无（事件用原始 result） |

**结论**：T1/T4 的视图层方案只在"组装 ChatRequest"一处介入，其余 11 条路径全部
操作 canonical Message，不受影响。这是视图层相比"修改 canonical Message"方案的
根本优势 —— 后者需要保证本清单每条路径都保真运行时字段，前者天然规避了这个问题。
