# Multi-Agent 协同设计

## 概述

deepai 多 agent 系统通过三个层次实现 agent 协同：

1. **Pipeline**（`/pipeline`）— 简单的 actor + 并行 reviewers + 重试循环
2. **Workflow**（`/workflow`）— 通用 DAG 工作流引擎，支持条件分支、重试、并行执行
3. **Environment** — 发布/订阅消息总线，agent 间异步通信，消息历史追溯

## 快速开始

### Pipeline 模式

Pipeline 适用于「一个执行者 + 多个审查者」的简单场景：

```
/pipeline list                        # 查看可用 pipeline
/pipeline run code-with-review "实现一个 HTTP 限流中间件"   # 执行
```

内置 Pipeline：

| 名称 | 描述 | Actor | Reviewers | 策略 |
|------|------|-------|-----------|------|
| `code-with-review` | 实现 + 并行安全/架构审查 + 自动修复 | coder | security, arch, perf | retry (最多 3 轮) |
| `code-quick` | 快速实现，无审查 | coder | 无 | report |

### Workflow 模式

Workflow 适用于复杂的多阶段流程：

```
/workflow list                         # 查看可用 workflow
/workflow run code-with-review "实现一个 HTTP 限流中间件"
/workflow run feature-planning "用户注册功能"
```

内置 Workflow：

| 名称 | 描述 | 阶段 |
|------|------|------|
| `code-with-review` | 实现 → 并行安全+架构审查 → 条件修复 | implement → security ∥ arch → fix |
| `feature-planning` | PRD → 设计 → 实现 → 审查 → 条件修复 | prd → design → implement → review → fix |

执行时实时显示每个阶段的状态：

```
  Running workflow "code-with-review" (4 stages)...
  [implement] running...
  [implement] done
  [security] running...
  [arch] running...
  [security] done
  [arch] done
  [fix] skipped
  Workflow: code-with-review  Status: COMPLETED
  Stages:
    [implement] done
    [security] done
    [arch] done
    [fix] skipped
```

## Agent 类型

| Agent Type | 角色 | 工具 | 温度 | 用途 |
|---|---|---|---|---|
| `general-purpose` | 通用助手 | 全部 | 0.7 | 日常对话 |
| `researcher` | 研究员 | 全部 | 0.3 | 信息收集与综合 |
| `coder` | 编码 | bash, file_ops, git | 0.7 | 代码实现、调试 |
| `analyst` | 分析师 | 全部 | 0.3 | 数据分析 |
| `security-reviewer` | 安全审查 | read_file, grep, glob, list_dir, find | 0.2 | 漏洞、注入、权限 |
| `arch-reviewer` | 架构审查 | read_file, grep, glob, list_dir, find | 0.2 | 设计模式、耦合度 |
| `perf-reviewer` | 性能审查 | read_file, grep, glob, list_dir, find, bash | 0.2 | 算法复杂度、内存 |
| `product-manager` | 产品经理 | read_file, grep, glob, list_dir, find, ask_clarification | 0.15 | 需求分析、功能拆解 |
| `architect` | 架构师 | read_file, grep, glob, list_dir, find | 0.2 | 系统设计、接口定义 |
| `bash` | 命令执行 | bash | 0.0 | 仅执行 shell 命令 |
| `frontend` | 前端开发 | bash, file_ops, web_search, web_fetch, image_search | 0.15 | HTML/CSS/JS、React/Vue/Angular、响应式设计、无障碍 |
| `ui-designer` | UI 设计 | file_ops, web_search, web_fetch, image_search | 0.2 | 设计系统、线框图、组件规范、色彩、排版 |
| `news` | 新闻获取 | web_search, web_fetch, web_fetch_batch | 0.1 | 新闻搜索、来源验证、结构化报道 |

审查类 agent（security/arch/perf-reviewer）自动配置 `OutputSchema`，要求输出结构化 JSON：

```json
{
  "verdict": "pass",
  "summary": "代码安全性良好",
  "issues": [
    {
      "severity": "critical",
      "file": "handler.go",
      "line": 42,
      "message": "SQL 拼接注入风险",
      "suggestion": "使用参数化查询"
    }
  ]
}
```

## 自定义 Agent

在项目根目录创建 `.deepai/agents/{type}.yaml`：

```yaml
# .deepai/agents/db-reviewer.yaml
type: db-reviewer
name: Database Reviewer
description: 审查数据库查询性能和安全
system_prompt: |
  你是数据库审查专家。关注：SQL 注入、索引使用、N+1 查询、事务隔离级别。
  输出 JSON 格式的 ReviewResult。
tools:
  - read_file
  - grep
  - glob
temperature: 0.2
max_turns: 10
```

或者使用外部 prompt 文件：

```yaml
# .deepai/agents/api-reviewer.yaml
type: api-reviewer
name: API Reviewer
system_prompt_file: prompts/api-reviewer.md
tools:
  - read_file
  - grep
```

加载优先级：`.deepai/agents/{type}.yaml` > 内置配置 > `general-purpose` 兜底。

## 自定义 Pipeline

在 `.deepai/pipelines/{name}.yaml` 中定义：

```yaml
# .deepai/pipelines/db-review.yaml
name: db-review
actor:
  agent_type: coder
  prompt: "{{.UserInput}}"
reviewers:
  - agent_type: db-reviewer
    name: db-security
    prompt: "Review database security:\n{{.diff}}"
  - agent_type: perf-reviewer
    name: db-perf
    prompt: "Review query performance:\n{{.diff}}"
on_issues: retry
max_rounds: 2
```

模板变量：
- `{{.UserInput}}` — 用户输入
- `{{.diff}}` — git diff（相对于 baseline）
- `{{.output}}` — actor 的输出文本

## 自定义 Workflow

在 `.deepai/workflows/{name}.yaml`（支持 `.yml`）中定义：

```yaml
# .deepai/workflows/full-review.yaml
name: full-review
description: 实现 + 安全/架构/性能并行审查 + 条件修复
stages:
  - name: implement
    role: coder
    prompt: "{{.UserInput}}"

  - name: security
    role: security-reviewer
    input_from: [implement]
    prompt: "Review for security issues:\n{{.outputs.implement}}"

  - name: arch
    role: arch-reviewer
    input_from: [implement]
    prompt: "Review for architecture issues:\n{{.outputs.implement}}"

  - name: perf
    role: perf-reviewer
    input_from: [implement]
    prompt: "Review for performance issues:\n{{.outputs.implement}}"

  - name: fix
    role: coder
    input_from: [implement, security, arch, perf]
    condition: has_critical_issues
    max_retries: 3
    prompt: |
      Fix the issues found by reviewers.
      Implementation: {{.outputs.implement}}
      Security: {{.outputs.security}}
      Architecture: {{.outputs.arch}}
      Performance: {{.outputs.perf}}
```

### Workflow 字段说明

**WorkflowStage 字段：**

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | 是 | 阶段名称，必须唯一 |
| `role` | 是 | 执行的 agent type |
| `prompt` | 是 | Prompt 模板 |
| `input_from` | 否 | 依赖的前序阶段名称列表 |
| `condition` | 否 | 执行条件（`has_critical_issues` / `always` / `never` / 自定义） |
| `max_retries` | 否 | 失败重试次数（默认 0） |

**模板变量：**
- `{{.UserInput}}` — 用户原始输入
- `{{.outputs.<stagename>}}` — 前序阶段的输出文本
- `{{.diff}}` — git diff（相对于 workflow 开始时）

**执行逻辑：**
1. 根据 `input_from` 构建依赖 DAG
2. 拓扑排序为执行波次（同波次内并行执行）
3. 每个阶段：检查 condition → 构建 prompt → 执行 agent → 失败则重试
4. 条件为 false 的阶段标记为 skipped，不影响 workflow 最终状态
5. 阶段输出超过 10000 字符自动截断

**内置条件：**

| 条件 | 说明 |
|------|------|
| `has_critical_issues` | 前序阶段存在 severity=critical 的审查结果 |
| `always` | 始终执行 |
| `never` | 始终跳过 |

通过 `workflow.RegisterCondition()` 可注册自定义条件。

## Environment 消息总线

Workflow 执行时自动创建 Environment，每个阶段完成后发布结构化消息：

```go
// 消息类型映射
coder              → "code_change"
security-reviewer   → "review_result"
arch-reviewer       → "review_result"
perf-reviewer       → "review_result"
product-manager     → "prd"
architect           → "design"
```

所有消息记录在 History 中，可通过过滤器查询：

```go
// 获取所有审查结果
reviews := env.History(Type(MsgTypeReviewResult))

// 获取 coder 的输出
code := env.History(From("implement"))

// 获取某个时间之后的消息
recent := env.History(Since(startTime))
```

## 代码 API

### 直接使用 Pipeline

```go
executor := agent.NewSubagentExecutor(provider, registry, sandbox, model).
    WithWorkDir(workDir)
pool := agent.NewSubagentPool(executor, 3, 2*time.Minute)

pipeline, _ := agent.ResolvePipeline("code-with-review", workDir)
orch := agent.NewOrchestrator(executor, pool, workDir)

result, err := orch.Run(ctx, pipeline, agent.OrchestratorInput{
    UserInput: "实现一个限流中间件",
    WorkDir:   workDir,
})
// result.Verdict: "pass" | "issues_found"
// result.Reviews: map[string]ReviewResult
// result.Rounds: 实际执行轮数
```

### 直接使用 Workflow

```go
engine := workflow.NewEngine(executor, pool, workDir)

// 可选：附加 Environment
env := workflow.NewEnvironment(workflow.WithMaxHistory(100))
defer env.Close()
engine.WithEnvironment(env)

wf, _ := workflow.ResolveWorkflow("feature-planning", workDir)
result, err := engine.Run(ctx, wf, "用户注册功能")
// result.Status: "completed" | "failed"
// result.Stages: map[string]*StageResult
// result.StageOrder: []string 执行顺序
// result.FinalOutput: 最后一个完成阶段的输出
```

### 直接使用 Environment

```go
env := workflow.NewEnvironment(
    workflow.WithMaxHistory(500),
    workflow.WithInboxSize(128),
)
defer env.Close()

env.Register(workflow.Subscription{
    Role:     "monitor",
    MsgTypes: []string{workflow.MsgTypeCodeChange, workflow.MsgTypeReviewResult},
})

env.Publish(ctx, workflow.AgentMessage{
    From:    "coder",
    Type:    workflow.MsgTypeCodeChange,
    Content: "重构了中间件层",
})

// 接收（阻塞等待）
msg, err := env.Receive(ctx, "monitor")

// 查询历史
all := env.History()
fromCoder := env.History(workflow.From("coder"))
reviews := env.History(workflow.Type(workflow.MsgTypeReviewResult))
```

## 文件结构

```
.deepai/
├── agents/           # 自定义 agent 配置
│   ├── db-reviewer.yaml
│   └── api-reviewer.yaml
├── pipelines/        # 自定义 pipeline 定义
│   └── db-review.yaml
└── workflows/        # 自定义 workflow 定义
    ├── full-review.yaml
    └── custom.yml    # 同时支持 .yml 后缀
```

## 架构参考

```
用户 → REPL
        ├── /pipeline list | run    → Orchestrator → actor → reviewers(retry)
        └── /workflow list | run    → Engine
                                       ├── DAG 拓扑排序 → 波次
                                       ├── 单阶段: executor.Execute()
                                       ├── 并行: errgroup + pool
                                       ├── 条件评估: ConditionFunc
                                       ├── 消息发布: Environment.Publish()
                                       └── 事件流: EventSink → REPL 实时渲染

Environment (消息总线)
  ├── Register(Subscription)   → 订阅角色
  ├── Publish(AgentMessage)    → 定向/广播
  ├── Receive(role)            → 阻塞接收
  └── History(filters...)      → 历史查询
```

## 实施状态

| Phase | 内容 | 状态 |
|-------|------|------|
| Phase 1 | 多角色 Agent + 结构化输出 | 已完成 |
| Phase 2 | Workflow 引擎 + REPL 集成 | 已完成 |
| Phase 3 | Environment 消息总线 + 事件流 | 已完成 |

所有验收标准（14/14）已满足，`go build ./...` 和 `go test ./...` 全部通过。
