# Multi-Agent 协同设计

> **⚠️ 2026-08 勘误**："Environment 发布/订阅消息总线"从未实现，代码中不存在 Publish/Subscribe/MessageBus。下表温度已与 `pkg/agent/types_config.go` 对齐（在此之前子 agent 的温度实际全部退化为 0.2）；「工具」列仍是概述，精确列表以 `BuiltinAgentTypes` 的 `DefaultTools` 为准。现状评估见 [ARCHITECTURE_REVIEW.md](ARCHITECTURE_REVIEW.md)。

## 概述

deepai 多 agent 系统通过以下机制实现 agent 协同：

- **Subagent** — 主 agent 调用子 agent 执行子任务
- **Environment** — 发布/订阅消息总线，agent 间异步通信，消息历史追溯

## Agent 类型

| Agent Type | 角色 | 工具 | 温度 | 用途 |
|---|---|---|---|---|
| `general-purpose` | 通用助手 | file_ops, bash, web_search, web_fetch, skill, present_file, ask_clarification | 0.2 | 日常对话 |
| `researcher` | 研究员 | 全部 | 0.1 | 信息收集与综合 |
| `coder` | 编码 | bash, file_ops, git | 0.1 | 代码实现、调试 |
| `analyst` | 分析师 | 全部 | 0.15 | 数据分析 |
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
max_tool_calls: 30
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

加载优先级：`.deepai/agents/{type}.yaml` > `.deepai/agents/{type}.md` > 插件 `agents/{type}.md` > 内置配置。

以上都没有定义该类型时，**子 agent 会直接报错**（错误信息里列出所有可用类型），不会静默回落到
`general-purpose`——回落会让拼错的 `agent_type` 拿到一个没有工具白名单的 profile，也就是全部工具，
与 `tools` 写错时的硬失败策略正好相反。`agent_type` 留空才使用 `general-purpose`。

`max_tool_calls` / `tools` / `temperature` / `model` 一律由上面解析出的 profile 决定（`task` 工具的
`max_tool_calls`、`model` 等参数可显式覆盖），subagent pool 不再注入任何按类型的默认值。

`max_tool_calls` 上限的语义是**实际执行的工具调用次数**（0 = 不限制，默认）。与轮数不同，这个计数对
单轮单调用的模型（GLM/GPT）和批量并行调用的模型（Claude）等价。到达上限时子 agent 不会失败：它收到
一条收尾指令且后续请求不再附带工具，被迫输出最终总结。默认不限，运行由父级上下文（Ctrl+C）、可选的
`token_budget`、上下文压缩与重复调用熔断器约束。旧配置里的 `max_turns` 键仍然兼容读取。

每次 `task` 调用的结果在 `Data` 里携带 `subagent_usage`（token 消耗，供父级 roll-up）与
`subagent_stats`（工作量画像：`tool_calls` / `llm_turns` / `schema_retries` / `max_tool_calls` /
`budget_exhausted` / `duration_ms` / `agent_type` / `model`，随会话持久化）。事后可直接从会话 DB 统计
委派效率——例如 `tool_calls ≈ llm_turns` 说明模型单轮单调用（N 次调用 = N 个串行回合），`budget_exhausted`
配大 `max_tool_calls` 说明委派被截断、下次应收窄范围而不是加码上限。委派 guidance（系统提示词）也据此
约束父模型：结果不够深时**收窄任务范围**（具体文件/行号/符号），不要单纯调大 `max_tool_calls`。

`general-purpose` 的工具是**显式白名单**（不是"全部"）：不含 `git_auto_commit`，也不含任何 MCP 工具
——白名单无法枚举 MCP 工具名，所以 MCP 需要按 agent 类型显式开启。需要放宽就在
`.deepai/agents/general-purpose.yaml` 里写 `tools:`。

注意主 agent（REPL）建 agent 时**不声明** `AgentType`：它仍以 `general-purpose` 的 prompt/温度为基线，
但工具注册表不受白名单裁剪（否则 task / skill / MCP 工具会被剪掉）。只有显式声明了类型的 agent 才按
白名单收窄——见 `ApplyAgentType`。

## 文件结构

```
.deepai/
└── agents/           # 自定义 agent 配置
    ├── db-reviewer.yaml
    └── api-reviewer.yaml
```

## 架构参考

```
用户 → REPL → Agent → Subagent(s)

Environment (消息总线)
  ├── Register(Subscription)   → 订阅角色
  ├── Publish(AgentMessage)    → 定向/广播
  ├── Receive(role)            → 阻塞接收
  └── History(filters...)      → 历史查询
```
