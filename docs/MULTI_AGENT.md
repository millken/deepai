# Multi-Agent 协同设计

## 现状

当前 deepai 采用**主从模式**：

```
用户 → REPL → Agent(ReAct loop) → LLM + Tools
                              ↓
                         Subagent(task) → 独立 Agent 实例 → 返回结果文本
```

- Agent 是单次使用的（`started` 标志），一个 turn 创建一个
- Subagent 通过 `task` 工具启动，主 agent 拿到的是**结果文本**，不是结构化上下文
- Subagent 之间没有通信通道，只能通过主 agent 中转
- 已有 agent type：`general-purpose`、`researcher`、`coder`、`analyst`

## 目标场景

1. **多角色审查**：代码完成后，安全/架构/性能 agent 从不同视角审查，反馈给 coder 修改，多轮迭代直到通过
2. **功能规划**：产品 agent 制定功能计划 → coder 实现 → 审查 → 迭代
3. **持续改进**：agent 之间形成反馈闭环，不断优化产出质量

## MetaGPT 架构研究

### 核心概念

MetaGPT 的多 agent 协同基于三个核心抽象：

**1. Role（角色）**
- 每个 Role 有独立的 `profile`（角色描述）、`goal`（目标）、`constraints`（约束）
- Role 通过 `_watch` 订阅特定类型的消息（如 ProductManager watch `UserRequirement`，Architect watch `WritePRD`）
- Role 内部维护 `memory`（记忆）和 `working_memory`（工作记忆），区分长期和短期上下文
- 支持三种运行模式：`REACT`（自由推理）、`BY_ORDER`（按顺序执行 action）、`PLAN_AND_ACT`（先规划再执行）

**2. Environment（环境）**
- 所有 Role 注册到同一个 Environment
- Role 通过 `publish_message` 发布消息到 Environment
- Environment 根据消息的 `send_to` / `cause_by` 路由到目标 Role
- 本质是一个**消息总线 + 路由器**

**3. Action（动作）**
- 每个 Action 是一个原子操作（如 `WritePRD`、`WriteDesign`、`WriteCode`、`WriteCodeReview`）
- Action 的输出通过 `ActionOutput` 结构化，下游 Role 可以精确消费
- Action 之间通过 `cause_by` 建立依赖关系（如 `WriteCodeReview` 的输入是 `WriteCode` 的输出）

### 协同流程

```
用户需求 → Environment.publish_message(Message)
  → ProductManager._observe() → _think() → _act(WritePRD)
    → Environment.publish_message(Message, cause_by=WritePRD)
      → Architect._observe() → _think() → _act(WriteDesign)
        → Environment.publish_message(Message, cause_by=WriteDesign)
          → Engineer._observe() → _think() → _act(WriteCode)
            → WriteCodeReview (内部迭代，最多 k 轮)
              → LGTM → 完成
              → LBTM → 重写代码 → 再次审查
```

### 关键设计决策

| 决策 | MetaGPT 方案 | 启示 |
|------|-------------|------|
| Agent 间通信 | 通过 Environment 消息总线，非直接调用 | 解耦发送者和接收者 |
| 流程编排 | 每个 Role 声明式订阅（`_watch`），非命令式编排 | 新增 Role 不需要修改其他 Role |
| 上下文传递 | `Message` 携带结构化 `ActionOutput`，非纯文本 | 下游可精确消费 |
| 审查迭代 | `WriteCodeReview` 内部循环（最多 k 次），非跨 Role 循环 | 审查逻辑封装在 Action 内 |
| 成本控制 | `Team.invest()` 设置预算，超支抛异常 | 全局 token 预算管理 |
| 记忆管理 | `memory`（长期）+ `working_memory`（短期） | 避免上下文膨胀 |

### 与 deepai 的差异

| 维度 | MetaGPT | deepai |
|------|---------|--------|
| 语言 | Python | Go |
| Agent 生命周期 | 持久（多轮对话） | 单次使用（一个 turn 一个） |
| 通信模型 | 异步消息总线 | 同步函数调用（task 工具） |
| 流程定义 | 声明式（watch + cause_by） | 隐式（system prompt 指导） |
| 输出格式 | 结构化（ActionOutput） | 纯文本 |
| 并发 | asyncio 并行运行多个 Role | 串行 subagent |

## 方案设计

借鉴 MetaGPT 的优点，同时适配 deepai 的 Go 同步架构和 ReAct 模式。

### 核心抽象

#### 1. Message（结构化消息）

```go
// AgentMessage 是 agent 间传递的结构化消息。
// 替代当前的纯文本结果，支持精确消费。
type AgentMessage struct {
    From      string         `json:"from"`       // 发送者 agent type
    To        string         `json:"to"`         // 接收者 agent type（空=广播）
    Type      string         `json:"type"`       // 消息类型（如 "review_result", "code_change"）
    Content   string         `json:"content"`    // 自然语言内容
    Artifacts map[string]any `json:"artifacts"`  // 结构化产物（如 ReviewResult, PRD）
}
```

#### 2. AgentRole（角色定义）

```go
// AgentRole 定义一个 agent 角色的行为规范。
// 借鉴 MetaGPT 的 Role 概念，但适配 deepai 的 ReAct 模式。
type AgentRole struct {
    Type         AgentType      // 角色类型
    Name         string         // 显示名称
    Profile      string         // 角色描述（用于 system prompt）
    Goal         string         // 目标
    Constraints  string         // 约束
    Watch        []string       // 订阅的消息类型（触发此角色）
    Tools        []string       // 可用工具
    MaxTurns     int            // 最大轮次
    Temperature  float64        // 温度
}
```

#### 3. Workflow（工作流）

```go
// Workflow 定义多 agent 协同的流程。
// 借鉴 MetaGPT 的 Team + Environment 概念。
type Workflow struct {
    Name        string
    Description string
    Roles       []AgentRole     // 参与的角色
    Stages      []WorkflowStage // 执行阶段
}

type WorkflowStage struct {
    Name       string            // 阶段名称
    Role       AgentType         // 执行角色
    InputFrom  []string          // 输入来源（前序 stage 名称）
    Prompt     string            // prompt 模板（可引用前序 stage 输出）
    Condition  string            // 执行条件（可选）
    MaxRetries int               // 最大重试次数
}
```

### 预定义角色

借鉴 MetaGPT 的角色体系，定义 deepai 的多角色：

| Agent Type | 角色 | Profile | Watch | Tools |
|---|---|---|---|---|
| `coder` | 编码 | 代码实现、调试、重构 | `user_request`, `review_feedback` | bash, file_ops, git |
| `security-reviewer` | 安全审查 | 关注漏洞、注入、权限、敏感数据 | `code_change` | read_file, grep, glob |
| `arch-reviewer` | 架构审查 | 关注设计模式、耦合度、可扩展性 | `code_change` | read_file, grep, glob |
| `perf-reviewer` | 性能审查 | 关注算法复杂度、内存、并发 | `code_change` | read_file, grep, glob, bash |
| `product-manager` | 产品 | 需求分析、功能拆解、优先级 | `user_request` | read_file, grep, ask_clarification |
| `architect` | 架构设计 | 系统设计、模块划分、接口定义 | `prd`, `user_request` | read_file, grep, glob, list_dir |

### 预定义 Workflow

#### code-with-review

```
用户需求 → coder(实现) → security-reviewer(审查) → arch-reviewer(审查)
         ↑                                              ↓
         └──────────── 有问题则修改并重新审查 ←───────────┘
```

```go
Workflow{
    Name: "code-with-review",
    Stages: []WorkflowStage{
        {Name: "implement", Role: AgentTypeCoder, Prompt: "{{.UserInput}}"},
        {Name: "security",  Role: AgentTypeSecurityReviewer, InputFrom: []string{"implement"}},
        {Name: "arch",      Role: AgentTypeArchReviewer, InputFrom: []string{"implement"}},
        {Name: "fix",       Role: AgentTypeCoder, InputFrom: []string{"security", "arch"},
            Condition: "has_critical_issues", MaxRetries: 3},
    },
}
```

#### feature-planning

```
用户需求 → product-manager(PRD) → architect(设计) → coder(实现) → review → 迭代
```

```go
Workflow{
    Name: "feature-planning",
    Stages: []WorkflowStage{
        {Name: "prd",    Role: AgentTypeProductManager, Prompt: "{{.UserInput}}"},
        {Name: "design", Role: AgentTypeArchitect, InputFrom: []string{"prd"}},
        {Name: "implement", Role: AgentTypeCoder, InputFrom: []string{"prd", "design"}},
        {Name: "review", Role: AgentTypeSecurityReviewer, InputFrom: []string{"implement"}},
        {Name: "fix",    Role: AgentTypeCoder, InputFrom: []string{"review"},
            Condition: "has_critical_issues", MaxRetries: 2},
    },
}
```

## 实施计划

### Phase 1：多角色 Agent + 结构化输出（最小可用）

改动范围：`pkg/agent/`、`pkg/subagent/`

**1.1 新增 Agent Type**

在 `pkg/agent/types_config.go` 中新增 `security-reviewer`、`arch-reviewer`、`perf-reviewer`、`product-manager`、`architect`。

每个角色有独立的：
- `SystemPrompt`：角色行为规范（借鉴 MetaGPT 的 profile + goal + constraints）
- `DefaultTools`：最小工具集（审查角色不需要写文件）
- `Temperature`：审查角色用低温度（0.05），产品角色用中等温度（0.3）

**1.2 结构化审查结果**

```go
type ReviewResult struct {
    Verdict    string  `json:"verdict"`     // "pass" | "issues_found"
    Summary    string  `json:"summary"`
    Issues     []Issue `json:"issues"`
}

type Issue struct {
    Severity   string `json:"severity"`    // "critical" | "warning" | "suggestion"
    File       string `json:"file"`
    Line       int    `json:"line"`
    Message    string `json:"message"`
    Suggestion string `json:"suggestion"`
}
```

审查 agent 的 system prompt 要求输出 JSON 格式的 `ReviewResult`。主 agent 解析后决定是否修改。

**1.3 主 Agent System Prompt 增强**

在 coder agent 的 system prompt 中增加协同规则（借鉴 MetaGPT Engineer 的 `use_code_review` 模式）：

```
完成代码修改后，执行审查流程：
1. 调用 task 工具，type=security-reviewer，传入变更文件和 diff
2. 调用 task 工具，type=arch-reviewer，同上
3. 如果存在 critical 级别问题：根据审查意见修改，重新提交审查
4. 所有审查通过后，执行 git commit
```

**1.4 验收标准**

- [ ] 新增 5 个 agent type，各有独立 system prompt 和工具集
- [ ] 主 agent 能调用审查 agent 并解析结构化结果
- [ ] 主 agent 能根据审查反馈修改代码并重新提交审查
- [ ] 审查结果在 REPL 中有清晰的渲染（区分 severity 颜色）

### Phase 2：Workflow 引擎

改动范围：新增 `pkg/workflow/`

**2.1 Workflow 定义与执行**

```go
// pkg/workflow/engine.go
type Engine struct {
    executor subagent.Executor  // 接口，通过 SubagentExecutor 创建独立 Agent
    pool     *subagent.Pool     // 并行 stage 执行
    workDir  string
}

func NewEngine(executor subagent.Executor, pool *subagent.Pool, workDir string) *Engine
func (e *Engine) Run(ctx context.Context, wf *Workflow, userInput string) (*WorkflowResult, error)
```

Engine 按 DAG 拓扑排序执行 stage，同 wave 内并行：
- 将前序 stage 的输出作为上下文注入
- 根据 condition 决定是否跳过
- 失败时按 MaxRetries 重试

**2.2 与 REPL 集成**

- 新增 `/workflow` 命令，列出可用 workflow
- 新增 `/workflow code-with-review` 启动指定 workflow
- Workflow 事件通过现有 AgentEvent 机制渲染

**2.3 验收标准**

- [ ] Workflow 可通过代码定义
- [ ] Engine 支持条件分支和重试
- [ ] Stage 之间传递结构化上下文
- [ ] REPL 可视化 workflow 进度

### Phase 3：Environment 消息总线（远期）

借鉴 MetaGPT 的 Environment 概念，实现 agent 间异步通信。

**3.1 消息路由**

```go
// pkg/workflow/environment.go
type Environment struct {
    roles    map[AgentType]AgentRole
    inbox    map[AgentType]chan AgentMessage
    history  []AgentMessage
}

func (e *Environment) Publish(msg AgentMessage)
func (e *Environment) Subscribe(role AgentType, msgTypes ...string)
```

**3.2 并行审查**

多个审查 agent 同时运行，结果汇总后传给 coder：

```
coder(实现) → Environment.publish(code_change)
            → security-reviewer ──┐
            → arch-reviewer ──────┤→ 汇总 → coder(修改)
            → perf-reviewer ──────┘
```

**3.3 验收标准**

- [ ] Agent 可异步收发消息
- [ ] 多个审查 agent 可并行运行
- [ ] 消息历史可追溯

## 技术约束

- **Token 预算**：多 agent 协同显著增加 token 消耗，需要预算控制（借鉴 MetaGPT 的 `invest` 机制）
- **上下文管理**：审查结果传入主 agent 时需要压缩，避免上下文溢出（借鉴 MetaGPT 的 `working_memory` 分离）
- **延迟**：串行审查增加总延迟，Phase 3 的并行审查可缓解
- **成本**：每次审查都是完整 LLM 调用，需要权衡审查深度和成本
- **Go 同步模型**：MetaGPT 基于 Python asyncio，deepai 基于 Go 同步模型，需要适配（goroutine + channel 替代 asyncio）
