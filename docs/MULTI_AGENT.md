# Multi-Agent 协同设计

> **⚠️ 2026-08 勘误**："Environment 发布/订阅消息总线"从未实现，代码中不存在 Publish/Subscribe/MessageBus；agent 表中的温度等参数与代码不符（以 `pkg/agent/types_config.go` 为准，如 general-purpose 实为 0.2、coder 实为 0.1）。现状评估见 [ARCHITECTURE_REVIEW.md](ARCHITECTURE_REVIEW.md)。

## 概述

deepai 多 agent 系统通过以下机制实现 agent 协同：

- **Subagent** — 主 agent 调用子 agent 执行子任务
- **Environment** — 发布/订阅消息总线，agent 间异步通信，消息历史追溯

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
