# Multi-Agent 协同设计

> 与 `pkg/agent/types_config.go` 的 `BuiltinAgentTypes` 对齐于 2026-09-13。「工具」列是精确的
> `DefaultTools` 白名单；改了那张表就要回来改这里。
>
> 历史勘误（留着是因为旧文档和旧会话里还有这个说法）：「Environment 发布/订阅消息总线」从未实现，
> 代码中不存在 Publish/Subscribe/MessageBus；agent 间没有 peer 通信，唯一的协同通道是父 → 子的
> 一次性委派。

## 概述

deepai 的多 agent 协同只有一条通道：**主 agent 通过 `task` 工具委派子 agent**。子 agent 之间不通信，
也看不见彼此；父 agent 用 `context_files` 显式传递子 agent 需要的上下文，不做隐式共享。

## Agent 类型

内置 profile **一律不设温度**：Claude 4.7+ 直接拒绝采样参数，其余现代模型忽略它，所以除非有人显式
要求（项目 YAML/MD 里写 `temperature:`，或 `task` 工具传参），请求里不带这个字段——见
`ApplyAgentType` 中 `profile.temperatureSet` 的判断。

| Agent Type | 角色 | 工具（`DefaultTools` 白名单） | 工具上限 | 结构化输出 |
|---|---|---|---|---|
| `general-purpose` | 通用助手 | bash, read_file, write_file, edit_file, list_dir, glob, grep, find, code_map, present_file, ask_clarification, skill, web_search, web_fetch | 不限 | — |
| `researcher` | 研究员（带出处的取证，不改文件） | read_file, list_dir, glob, grep, find, code_map, present_file, ask_clarification | 不限 | — |
| `coder` | 编码 | 上述通用集 + git_auto_commit | 不限 | — |
| `analyst` | 分析师（Method + Caveats） | read_file, write_file, edit_file, list_dir, glob, grep, find, code_map, present_file, ask_clarification | 不限 | — |
| `product-manager` | 产品经理（可证伪的验收） | read_file, grep, glob, list_dir, find, code_map, ask_clarification | 不限 | — |
| `architect` | 架构师（设计决策记录） | read_file, grep, glob, list_dir, find, code_map | 不限 | — |
| `security-reviewer` | 安全审查 | read_file, grep, glob, list_dir, find, code_map | 20 | ReviewResult |
| `arch-reviewer` | 架构审查 | read_file, grep, glob, list_dir, find, code_map | 20 | ReviewResult |
| `perf-reviewer` | 性能审查 | 上列 + bash | 20 | ReviewResult |
| `correctness-reviewer` | 对抗式正确性审查（评审门的 reviewer） | 上列 + bash | 20 | ReviewResult |
| `design-reviewer` | 对抗式方案审查（判 brief vs plan，不看代码） | read_file, grep, glob, list_dir, find, code_map | 20 | DesignReviewResult |
| `document-editor` | .docx 编辑（默认开修订痕迹） | docx_read, docx_edit, docx_format, docx_write, read_file, write_file, ask_clarification | 30 | — |
| `frontend` | 前端开发 | 通用集 + web_search, web_fetch, image_search | 不限 | — |
| `ui-designer` | UI 设计 | read_file, write_file, edit_file, list_dir, glob, grep, find, code_map, present_file, ask_clarification, web_search, web_fetch, image_search | 不限 | — |
| `news` | 新闻获取 | web_search, web_fetch, web_fetch_batch, read_file, present_file, ask_clarification | 不限 | — |
| `bash` | 命令执行 | bash | 3 | — |

五个审查类 agent（security / arch / perf / correctness / design-reviewer）自动配置 `OutputSchema`，要求输出结构化 JSON。前四个用 `ReviewResult`：

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

注意主 agent（REPL）建 agent 时**不声明** `AgentType`：它仍以 `general-purpose` 的 prompt 为基线，
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
用户 → REPL → Agent ──task──→ Subagent pool (无本地并发上限，单次 Run 内 task 调用封顶 20)
                                 ├── agent_type    → profile（prompt / 工具白名单 / 上限 / schema）
                                 ├── context_files → 父显式传入的 <context-files> 块
                                 └── 结果          → FinalOutput + subagent_usage / subagent_stats
```

子 agent 的工具集恒不含 `task`（委派不可嵌套），子 agent 之间没有通道。
