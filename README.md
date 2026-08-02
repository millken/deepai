# DeepAI

一个基于 Go 的 AI 编码助手，采用 ReAct（Reasoning + Acting）模式，内置多 LLM 提供商支持、子代理（subagent）编排、工具调用、技能（skill）系统、MCP 集成、会话持久化与长期记忆。

DeepAI 在终端中以交互式 REPL 运行，也可作为 HTTP 网关服务对外提供会话能力。它把"大模型 + 工具 + 沙箱 + 记忆"组合成一个可扩展、可插拔的智能体运行时。

---

## 目录

- [核心特性](#核心特性)
- [架构总览](#架构总览)
- [快速开始](#快速开始)
- [配置](#配置)
- [命令行参考](#命令行参考)
- [LLM 提供商](#llm-提供商)
- [工具](#工具)
- [子代理（Subagent）](#子代理subagent)
- [技能（Skill）](#技能skill)
- [插件系统](#插件系统)
- [MCP 集成](#mcp-集成)
- [会话与记忆](#会话与记忆)
- [沙箱与安全](#沙箱与安全)
- [Token 效率](#token-效率)
- [API 代理（开发调试）](#api-代理开发调试)
- [项目结构](#项目结构)
- [开发](#开发)
- [文档](#文档)
- [许可证](#许可证)

---

## 核心特性

- **ReAct 智能体循环**：思考 → 工具调用 → 观察 → 再思考，支持并行与串行工具调用、流式输出、上下文压缩。
- **多 LLM 提供商**：OpenAI、Anthropic、Qwen、DeepSeek、Gemini、Groq、GLM、Ollama、Bedrock、OpenAI 兼容网关，统一抽象。
- **子代理编排**：通过 `task` 工具把任务委派给独立的专家代理（coder、researcher、security-reviewer 等 13 种内置类型），池化并发、超时控制、事件流回传。
- **丰富的内置工具**：bash、文件读写编辑、代码地图、git 全家桶、web 搜索/抓取/图片搜索、图像查看、记忆等。
- **技能系统**：用 `SKILL.md` + YAML frontmatter 定义可复用工作流，支持 hooks、动态注入、模型/effort 覆盖、fork 上下文。
- **Claude 插件兼容**：发现并加载 Claude Code 风格的插件包（skills、agents、commands、MCP servers）。
- **MCP 集成**：通过 `.mcp.json` 连接任意外部 MCP 服务器（stdio / SSE / HTTP），自动注册为工具。
- **会话持久化**：SQLite 存储会话历史，支持恢复、续聊、导出、重命名、清理、统计。
- **长期记忆**：跨会话的用户偏好与事实记忆，LLM 自动抽取，注入系统提示。
- **沙箱隔离**：Landlock → bubblewrap（bwrap）→ direct 三级降级，限制 bash 与文件操作的作用域。
- **Token 效率**：可选的工具结果老化（T1）、对话压缩（T4）、Phase 0 指标采集，用于优化长会话成本。
- **交互式 TUI**：基于 bubbletea 的终端界面，语法高亮、Markdown 渲染、斜杠命令、会话选择器。
- **HTTP 网关**：把 agent 能力作为 REST/SSE 服务暴露，支持多用户、多会话、Postgres 持久化。

---

## 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                      用户入口                                │
│   deepai (REPL TUI)          deepai -q (单次)    Gateway HTTP │
└──────────────┬──────────────────┬──────────────────┬────────┘
               │                  │                  │
               ▼                  ▼                  ▼
┌──────────────────────────────────────────────────────────────┐
│                    pkg/agent (ReAct 引擎)                     │
│  系统提示构建 → LLM 调用 → 工具调用 → 上下文压缩 → 记忆注入    │
└──────┬────────────────┬───────────────┬──────────────┬───────┘
       │                │               │              │
       ▼                ▼               ▼              ▼
┌────────────┐  ┌──────────────┐ ┌────────────┐ ┌────────────┐
│ pkg/llm    │  │ pkg/tools    │ │ pkg/subagent│ │ pkg/memory │
│ 多提供商抽象│  │ 内置+MCP+插件│ │ 子代理池    │ │ 长期记忆   │
└────────────┘  └──────┬───────┘ └──────┬─────┘ └────────────┘
                       │                │
                       ▼                ▼
                ┌────────────┐  ┌────────────────┐
                │pkg/sandbox │  │ 新建独立 Agent  │
                │Landlock/bwrap│ │ (复用 ReAct)   │
                └────────────┘  └────────────────┘
```

**核心设计**：子代理不是简化版，而是新建一个完整的 `agent.New` 实例，拥有独立的工具子集、系统提示、模型与超时，复用同一套 ReAct 引擎。

---

## 快速开始

### 前置要求

- Go 1.26+（见 `go.mod`）
- Linux / macOS / Windows（沙箱完整功能推荐 Linux，支持 Landlock）
- 至少一个 LLM 提供商的 API Key

### 安装

```bash
git clone <repo-url>
cd deepai
make build          # 生成 ./bin/deepai
# 或安装到 ~/.local/bin
make install
```

### 首次配置

```bash
deepai setup
```

交互式向导会引导你完成：
1. 选择 LLM 提供商（openai / anthropic / qwen / deepseek / gemini / groq / glm / ollama / bedrock / openai-compat）
2. 填入 API Key（存入 `~/.deepai/.env`，与配置分离）
3. 选择模型
4. 可选：配置数据库连接

配置完成后，直接运行：

```bash
deepai                    # 启动交互式 REPL
deepai -q "解释这个项目"  # 单次查询
deepai -m claude-sonnet-4-20250514   # 覆盖模型
```

---

## 配置

DeepAI 的配置分三层，按优先级从高到低：

### 1. 命令行参数

见[命令行参考](#命令行参考)。

### 2. 配置文件 `~/.deepai/config.yaml`

```yaml
provider: openai                    # LLM 提供商
model: gpt-4o                       # 默认模型
base_url: ""                        # 可选，自定义 API 端点
context_window: 192000              # 模型上下文窗口（token）
database_url: ""                    # 可选，Postgres 连接串
request_timeout: 30                 # agent 单次运行超时（分钟，0=无限）
mode: ""                            # "" 或 "interactive"（默认）；"autonomous" 跳过用户确认

# Token 效率（可选，默认关闭）
token_metrics: ""                   # "1" 写入默认路径；或指定 JSONL 文件路径
token_aging: false                  # true 启用 T1 工具结果老化
```

### 3. 环境变量 `~/.deepai/.env`

API Key 等敏感信息存放于此，不会被提交：

```bash
OPENAI_API_KEY=sk-...
ANTHROPIC_API_KEY=sk-ant-...
DEEPSEEK_API_KEY=sk-...
# ... 其他提供商
```

**Token 效率环境变量**（优先级高于 config.yaml）：

| 变量 | 说明 |
|------|------|
| `DEEPAI_TOKEN_METRICS` | `1`/`true` 写入 `$TMPDIR/deepai-token-metrics.jsonl`；其他非空值作为输出路径；空=关闭 |
| `DEEPAI_TOKEN_AGING` | `1`/`true` 启用 T1 工具结果老化 |

### 项目级指令 `DEEPAI.md`

在项目根目录放置 `DEEPAI.md`，内容会注入为系统提示，用于给 agent 项目特定的指令：

```markdown
# DEEPAI.md

## 规范
- 使用 Go 1.26
- 遵循现有项目约定
- 不要自动 commit
```

全局指令放在 `~/.deepai/DEEPAI.md`，项目指令覆盖全局。

---

## 命令行参考

DeepAI 基于 cobra 构建。根命令直接进入 REPL，子命令提供管理功能。

### 交互模式（默认）

```bash
deepai                    # 启动 REPL
deepai -v                 # 启用调试日志
```

### 单次查询

```bash
deepai -q "你的问题"      # 执行一次后退出
```

### 全局标志

| 标志 | 说明 |
|------|------|
| `-q, --query <text>` | 单次查询（非交互模式） |
| `-r, --resume [id]` | 恢复会话；不带参数打开交互式选择器 |
| `-c, --continue` | 继续最近一次会话 |
| `-m, --model <name>` | 覆盖配置中的模型 |
| `--max-turns <n>` | 单次运行最大 agent 轮次（0=无限） |
| `-v, --verbose` | 启用调试日志 |

### `deepai setup`

交互式配置向导。子命令：

- `deepai setup provider` — 仅配置提供商
- `deepai setup model` — 仅配置模型
- `deepai setup database` — 仅配置数据库

### `deepai session`

管理会话历史（SQLite 存储）：

```bash
deepai session list              # 列出会话
deepai session show <id>         # 查看会话内容
deepai session rename <id> <title>
deepai session export <id>       # 导出为 JSON/Markdown
deepai session delete <id>       # 删除会话
deepai session prune             # 清理空/旧会话
deepai session stats             # 会话统计
```

### `deepai plugin`

管理 Claude 风格插件：

```bash
deepai plugin install <git-url> [--name N] [--subdir S] [--project] [--force]
deepai plugin add <path>         # 符号链接本地插件（开发用）
deepai plugin list               # 列出已安装插件
deepai plugin remove <name>      # 移除插件
```

`--project` 把插件装到 `<cwd>/.deepai/plugins` 而非全局 `~/.deepai/plugins`。

### `deepai version`

打印版本信息。

### REPL 内斜杠命令

在 REPL 中输入 `/` 触发：

| 命令 | 说明 |
|------|------|
| `/help` | 显示帮助 |
| `/clear` | 清空当前会话历史 |
| `/history` | 显示对话历史 |
| `/sessions` | 列出最近会话 |
| `/new` | 开启新会话 |
| `/title` | 设置会话标题 |
| `/save` | 保存会话元数据 |
| `/undo` | 撤销上一轮 |
| `/compact` | 立即压缩上下文 |
| `/plan` | 进入计划模式（只读探索） |
| `/run` | 退出计划模式（完整工具权限） |
| `/model [name]` | 查看/切换模型 |
| `/status` | 显示已加载工具/插件与调用统计 |
| `/exit` | 退出 REPL |

---

## LLM 提供商

DeepAI 通过 `pkg/llm` 抽象所有提供商，统一为 `LLMProvider` 接口（`Chat` / `Stream`）。底层分为两种协议实现：`anthropic` 原生协议与 `openai` 兼容协议。

| 提供商 | API Key 环境变量 | 协议 | 备注 |
|--------|------------------|------|------|
| `openai` | `OPENAI_API_KEY` | openai | 支持 `OPENAI_BASE_URL` 自定义端点 |
| `anthropic` | `ANTHROPIC_API_KEY` | anthropic | 支持 `ANTHROPIC_BASE_URL` |
| `qwen` | `QWEN_API_KEY` | openai | 通义千问 |
| `deepseek` | `DEEPSEEK_API_KEY` | openai | DeepSeek |
| `gemini` | `GEMINI_API_KEY` | openai | Google Gemini |
| `groq` | `GROQ_API_KEY` | openai | Groq |
| `glm` | `GLM_API_KEY` | openai | 智谱 GLM |
| `ollama` | `OLLAMA_API_KEY` | openai | 本地 Ollama |
| `bedrock` | `BEDROCK_API_KEY` | openai | AWS Bedrock |
| `openai-compat` | `OPENAI_API_KEY` | openai | 任意 OpenAI 兼容网关 |

切换提供商只需修改 `config.yaml` 的 `provider` 字段或重新运行 `deepai setup provider`。

---

## 工具

工具是 agent 感知与操作世界的接口，统一注册到 `tools.Registry`。每个工具声明 `Name`、`Description`、`Groups`、`InputSchema`（JSON Schema）与 `Handler`。

### 内置工具

| 分类 | 工具 | 说明 |
|------|------|------|
| **Shell** | `bash` | 执行 shell 命令，返回 stdout/stderr/exit_code |
| **文件** | `read_file` `write_file` `edit_file` `list_dir` `glob` `grep` `find` `code_map` | 代码地图按符号签名索引，避免读全文 |
| **Git** | `git_status` `git_diff` `git_log` `git_add` `git_commit` `git_reset` `git_auto_commit` `git_push` | git_auto_commit 支持 AI 生成提交信息 |
| **Web** | `web_search` `web_fetch` `web_fetch_batch` `image_search` | DuckDuckGo 搜索，SSRF 防护 |
| **图像** | `view_image` | 查看本地/远程图像 |
| **记忆** | `memory` | 读写长期记忆 |
| **展示** | `present_file` | 向用户展示文件内容 |
| **代理** | `task` | 委派子代理（见下节） |
| **交互** | `ask_clarification` | 向用户提问（autonomous 模式下走 best-judgment） |
| **技能** | `skill` | 调用已加载的技能 |
| **计划** | `enter_plan_mode` | 进入只读计划模式（非交互模式禁用） |

### 工具描述优化

工具描述经过精心裁剪（T3 策略）：文件操作路由规则集中在系统提示，避免在每个工具描述中重复，节省 token。

---

## 子代理（Subagent）

子代理是 DeepAI 的核心编排能力。主 agent 通过 `task` 工具把子任务委派给独立的专家代理，每个子代理拥有自己的工具子集、系统提示、模型与超时。

### 工作方式

1. 主 agent 调用 `task` 工具，传入 `description`、`prompt`、`agent_type`
2. `SubagentPool` 创建任务，经信号量限流后异步执行
3. `SubagentExecutor` 新建一个完整的 `agent.New` 实例（复用 ReAct 引擎）
4. 子代理独立运行，事件流回传给主 agent
5. 主 agent 阻塞等待结果，把最终输出作为工具返回值

### 内置 Agent 类型（13 种）

| 类型 | 用途 | 默认工具集 |
|------|------|-----------|
| `general-purpose` | 通用平衡助手 | 全部 |
| `coder` | 编码、调试、实现 | bash+文件+git+task+skill |
| `researcher` | 研究、阅读、综合 | 只读工具+task |
| `analyst` | 结构化分析 | 文件+分析工具 |
| `security-reviewer` | 安全评审 | 只读+JSON OutputSchema |
| `arch-reviewer` | 架构评审 | 只读+JSON OutputSchema |
| `perf-reviewer` | 性能评审 | 只读+bash+JSON OutputSchema |
| `product-manager` | 产品规划 | 只读+ask_clarification |
| `architect` | 系统设计 | 只读工具 |
| `bash` | 命令执行 | 仅 bash |
| `frontend` | 前端开发 | 全栈前端工具 |
| `ui-designer` | UI/UX 设计 | 设计+web 工具 |
| `news` | 新闻研究 | web 搜索+抓取 |

### 关键设计

- **递归防护**：子代理的工具集中自动剔除 `task`，防止无限嵌套。
- **用户交互隔离**：子代理剥离 `UserInteraction`，永不阻塞等待用户——计划自动批准，澄清走 best-judgment。
- **并发控制**：默认 4 个并发子代理，每个最长 15 分钟。
- **独立模型**：`SubagentConfig.Model` 支持为子代理指定不同模型（双模型路由基础）。

### 自定义 Agent 类型

在 `.deepai/agents/` 下放置 YAML 或 MD 文件即可定义新类型：

`.deepai/agents/my-reviewer.yaml`:
```yaml
type: my-reviewer
name: My Reviewer
description: 自定义代码评审
system_prompt: "你是一个代码评审专家..."
tools: ["read_file", "grep", "code_map"]
max_turns: 8
temperature: 0.2
```

解析优先级：**项目 YAML > 项目 MD > 插件 MD > 内置 > general 回退**。

详见 [`pkg/subagent/README.md`](pkg/subagent/README.md) 与 [`docs/MULTI_AGENT.md`](docs/MULTI_AGENT.md)。

---

## 技能（Skill）

技能是可复用的、参数化的工作流，用 `SKILL.md` + YAML frontmatter 定义。技能可以由用户显式调用（`/skill-name`），也可以由 LLM 自动调用。

### 技能目录

技能从以下位置加载（后者覆盖前者）：
- `~/.deepai/skills/<name>/SKILL.md` — 全局
- `<project>/.deepai/skills/<name>/SKILL.md` — 项目
- `<plugin>/skills/<name>/SKILL.md` — 插件

### Frontmatter 字段

```yaml
---
name: my-skill
description: 做某件事
argument-hint: "<target>"
disable-model-invocation: false   # true 则只能用户调用
user-invocable: true              # nil/true 允许用户调用
allowed-tools: [Read, Grep, Bash] # 限制技能内可用工具
model: claude-sonnet-4-20250514   # 覆盖模型
effort: high                      # 推理强度
context: fork                     # "" 继承；"fork" 复制上下文
agent: coder                      # 指定 agent 类型
max-turns: 10                     # DeepAI 扩展
temperature: 0.1                  # DeepAI 扩展
hooks:
  - event: PreToolUse
    command: "./check.sh"
    on_error: abort
paths:
  - "src/**/*.go"
shell: bash
---
技能正文（Markdown），支持 !`command` 动态注入与 $ARGUMENTS。
```

详见 [`docs/SKILL_DESIGN.md`](docs/SKILL_DESIGN.md)。

---

## 插件系统

DeepAI 兼容 Claude Code 插件包格式。一个插件是包含 `.claude-plugin/plugin.json` 清单的目录，可捆绑：

- **skills** — `<plugin>/skills/` 下的技能
- **agents** — `<plugin>/agents/` 下的自定义 agent 类型（MD 格式）
- **commands** — `<plugin>/commands/` 下的斜杠命令（注册为 `plugin:cmd`）
- **MCP servers** — 清单中的 `mcpServers` 配置

### 安装插件

```bash
# 从 git 仓库克隆
deepai plugin install https://github.com/user/my-plugin
deepai plugin install https://github.com/user/marketplace --subdir plugins/foo

# 本地开发（符号链接）
deepai plugin add ./my-plugin

# 列出 / 移除
deepai plugin list
deepai plugin remove my-plugin
```

插件发现顺序：全局 `~/.deepai/plugins` → 项目 `<cwd>/.deepai/plugins`。

详见 [`docs/PLUGIN_SYSTEM_DESIGN.md`](docs/PLUGIN_SYSTEM_DESIGN.md) 与 [`docs/spec/PLUGIN_INTEGRATION_SPEC.md`](docs/spec/PLUGIN_INTEGRATION_SPEC.md)。

> 早前基于 `.so`/`.dll` 的原生工具插件加载（`pkg/plugin`）已移除，改用上述 Claude 插件包格式。

---

## MCP 集成

DeepAI 是 MCP（Model Context Protocol）客户端，可连接任意外部 MCP 服务器并将其工具注册到 agent。

### 配置

在项目根目录放置 `.mcp.json`（项目级）或 `~/.deepai/mcp.json`（全局）：

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    "remote-server": {
      "type": "http",
      "url": "https://mcp.example.com/sse",
      "headers": { "Authorization": "Bearer ..." }
    }
  }
}
```

支持三种传输：`stdio`（默认）、`sse`、`http`。环境变量支持 `${VAR}` 展开。插件捆绑的 MCP servers 会自动合并。

启动时 DeepAI 会连接所有配置的服务器，调用 `tools/list`，把工具注册到 registry，并在启动报告中汇总状态。

详见 [`docs/spec/MCP_INTEGRATION_SPEC.md`](docs/spec/MCP_INTEGRATION_SPEC.md)。

---

## 会话与记忆

### 会话持久化

会话默认存储在 SQLite 数据库 `~/.deepai/deepai.db`（WAL 模式）。每次 REPL 运行自动创建/恢复会话。

- **恢复**：`deepai -r`（选择器）、`deepai -r <id>`、`deepai -c`（最近一次）
- **管理**：`deepai session list/show/rename/export/delete/prune/stats`
- **REPL 内**：`/sessions`、`/new`、`/undo`、`/clear`

可选配置 `database_url` 指向 Postgres，用于网关多用户场景。

### 长期记忆

DeepAI 维护跨会话的用户记忆（偏好、事实、反馈）：

- **自动抽取**：每轮对话后，LLM 抽取值得记住的事实与偏好。
- **注入**：相关记忆在下次对话时注入系统提示。
- **作用域**：支持全局（`UserScope`）与项目级记忆。
- **偏好调度**：`PreferenceScheduler` 管理偏好的应用与冲突解决。
- **工具**：`memory` 工具让 agent 显式读写记忆。

记忆与偏好存储复用同一个 SQLite/Postgres 数据库。

详见 [`docs/PERSONALIZED_AGENT_ROADMAP.md`](docs/PERSONALIZED_AGENT_ROADMAP.md) 与 [`docs/hermes-agent-memory-analysis.md`](docs/hermes-agent-memory-analysis.md)。

---

## 沙箱与安全

`pkg/sandbox` 为 bash 执行与文件操作提供隔离，按可用性降级：

1. **Landlock**（Linux 内核 5.13+）— 细粒度路径访问控制，无需外部依赖。
2. **bubblewrap**（`bwrap`）— 容器化隔离，需系统安装。
3. **direct** — 直接执行（无隔离，仅限可信环境）。

每个会话有独立的沙箱目录（`~/.deepai/sandbox/<session>/`）。bash 工具默认通过沙箱执行；`ExecDirect` 用于需要当前工作目录上下文的命令。

### Web 工具安全

`web_fetch` / `web_fetch_batch` 内置 SSRF 防护：
- DNS 解析时拒绝私有/回环/链路本地地址
- 拒绝云元数据地址（`169.254.169.254`）
- 重定向目标同样校验，阻止重定向到内网

---

## Token 效率

长会话的 token 成本是 agent 系统的核心挑战。DeepAI 提供可选的优化机制（默认关闭，需经 Phase 0 指标校准后启用）：

| 机制 | 说明 | 启用方式 |
|------|------|---------|
| **Phase 0 指标** | 每轮记录 provider token 数与各工具结果字节桶，输出为 JSONL | `token_metrics: "1"` 或 `DEEPAI_TOKEN_METRICS=1` |
| **T1 工具结果老化** | 历史工具结果按年龄压缩（上下文压力 >40% 时触发） | `token_aging: true` 或 `DEEPAI_TOKEN_AGING=1` |
| **T4 对话压缩** | 历史 AI 消息文本与 ToolCall 参数按年龄压缩 | 待校准（配置预留） |
| **上下文压缩** | 上下文窗口 75% 时自动压缩，保留最近 N 条 | 默认启用（需 `context_window > 0`） |

详见 [`docs/spec/token-efficiency.md`](docs/spec/token-efficiency.md)。

---

## API 代理（开发调试）

> 早前的 HTTP/SSE 网关服务（`pkg/gateway`）已移除（未接入主 CLI，无命令注册它）。

`cmd/proxy` 提供独立的 LLM API 反向代理，转发 OpenAI/Anthropic 请求并记录完整 JSONL 事件流，用于调试与性能分析：

```bash
go run ./cmd/proxy --openai-key sk-... --log-file /tmp/proxy.jsonl
```

---

## 项目结构

```
deepai/
├── cmd/
│   ├── deepai/              # 主 CLI 入口（REPL）
│   │   ├── main.go
│   │   └── help/            # 文档生成工具
│   ├── proxy/               # LLM API 调试代理
│   └── mcp-example/         # MCP 服务器示例
├── pkg/
│   ├── agent/               # ReAct 引擎、子代理执行器、配置、老化、指标
│   ├── chat/                # REPL TUI、会话、斜杠命令、渲染
│   ├── commands/            # cobra 命令（chat/setup/session/plugin/version）
│   ├── llm/                 # LLM 提供商抽象与实现（openai/anthropic）
│   ├── tools/               # 工具注册表、task 工具、present、git_auto_commit
│   │   └── builtin/         # 内置工具（bash/file/git/web/...）
│   ├── subagent/            # 子代理池、任务、事件、执行接口
│   ├── skill/               # 技能注册表、解析、hooks、渲染
│   ├── plugin/              # 原生 .so 插件加载（实验性）
│   ├── claudeplugin/        # Claude 插件发现与解析
│   ├── mcp/                 # MCP 客户端与配置加载
│   ├── memory/              # 长期记忆、偏好、抽取、存储
│   ├── models/              # 共享类型（Message/Session/Tool/...）
│   ├── sandbox/             # Landlock/bwrap/direct 沙箱
│   ├── clarification/       # ask_clarification 工具与交互
│   ├── checkpoint/          # Postgres 检查点（网关用）
│   ├── gateway/             # HTTP 网关服务
│   ├── proxy/               # API 代理实现
│   └── logs/                # 结构化日志（异步、fanout）
├── plugins/                 # 原生插件示例（echo/web_fetch/weather_rust）
├── docs/                    # 设计文档与规格说明
├── Makefile
├── go.mod
└── LICENSE
```

---

## 开发

### 构建

```bash
make build              # 构建 ./bin/deepai
make build-proxy        # 构建 ./bin/proxy
make install            # 构建并安装到 ~/.local/bin
```

### 测试

```bash
make test               # 全部测试
go test ./pkg/agent/... # 单包测试
```

### 代码检查

```bash
make lint               # golangci-lint
```

### 调试

```bash
deepai -v               # 启用调试日志（写入 $TMPDIR/deepai-debug.log）
DEEPAI_TOKEN_METRICS=1 deepai   # 采集 token 指标
```

### 生成命令文档

```bash
go run ./cmd/deepai/help -d ./docs/cmd
```

---

## 文档

设计与规格文档位于 [`docs/`](docs/)：

| 文档 | 说明 |
|------|------|
| [`INSTALL_AND_RUN.md`](docs/INSTALL_AND_RUN.md) | 安装与运行（注意：部分内容可能过时） |
| [`WORKFLOW.md`](docs/WORKFLOW.md) | 端到端工作流 |
| [`MULTI_AGENT.md`](docs/MULTI_AGENT.md) | 多代理概述 |
| [`AUTONOMOUS_MULTI_AGENT.md`](docs/AUTONOMOUS_MULTI_AGENT.md) | 自主多代理设计 |
| [`SKILL_DESIGN.md`](docs/SKILL_DESIGN.md) | 技能系统设计 |
| [`PLUGIN_SYSTEM_DESIGN.md`](docs/PLUGIN_SYSTEM_DESIGN.md) | 插件系统设计 |
| [`SESSION_DESIGN.md`](docs/SESSION_DESIGN.md) | 会话设计 |
| [`PERSONALIZED_AGENT_ROADMAP.md`](docs/PERSONALIZED_AGENT_ROADMAP.md) | 个性化代理路线图 |
| [`CLAUDE_PLUGIN_COMMANDS_GUIDE.md`](docs/CLAUDE_PLUGIN_COMMANDS_GUIDE.md) | Claude 插件命令指南 |
| [`pkg-guide-cn.md`](docs/pkg-guide-cn.md) | 包指南（中文） |
| [`spec/token-efficiency.md`](docs/spec/token-efficiency.md) | Token 效率规格 |
| [`spec/MCP_INTEGRATION_SPEC.md`](docs/spec/MCP_INTEGRATION_SPEC.md) | MCP 集成规格 |
| [`spec/PLUGIN_*.md`](docs/spec/) | 插件相关规格 |

---

## 许可证

[Apache License 2.0](LICENSE)
