# cmd/deepai

`cmd/deepai` 是一个交互式命令行工具，提供完整的 AI 代理功能：

- 交互式提示符界面，支持多轮对话
- 集成工具系统（bash、文件操作、git、web、memory、session_search）
- 支持技能系统（skill system）扩展
- 支持从 DEEPAI.md 文件加载配置
- 支持调试日志记录
- 支持脚本化 provider（用于演示）和真实 LLM provider
- 支持持久记忆系统（跨会话记忆 + 自动提取 + 主动管理）

## 功能特性

### 1. 交互式对话
- 提供 `>` 提示符进行多轮对话
- 支持 `exit` 或 `quit` 命令退出
- 支持 Ctrl+D 结束会话

### 2. 工具系统
- **bash 工具**：执行 shell 命令
- **文件工具**：读取、写入、列出文件等操作
- **git 工具**：查看状态、提交变更
- **web 工具**：网页搜索、网页内容抓取
- **memory 工具**：管理持久记忆（见下文）
- **澄清工具**：向用户请求澄清信息
- **任务工具**：启动子代理执行复杂任务

### 3. 技能系统
- 自动加载项目中的技能定义
- 支持动态注册技能工具
- 技能描述会添加到系统提示中

### 4. 记忆系统

记忆系统为 Agent 提供跨会话的持久记忆能力。启用后，Agent 能记住用户偏好、项目上下文、环境配置等信息，并在后续对话中自动利用。

**工作原理**：

```
用户对话 → LLM 自动提取记忆（异步）→ 存入数据库
                                            ↓
下次对话 ← 从数据库加载记忆 ← 注入系统提示词
```

**记忆触发方式**：

| 触发 | 说明 |
|------|------|
| 自动提取 | 每轮对话结束后，LLM 分析对话并提取有价值的记忆 |
| 主动保存 | Agent 调用 `memory` 工具写入事实 |
| Nudge 审查 | 连续 10 轮未主动保存时，强制触发一次提取 |
| 压缩前保存 | 上下文压缩前，确保即将丢失的信息被保存 |

**Agent 可通过 memory 工具主动管理记忆**：

```
# 添加事实
memory(action="add_fact", content="用户偏好暗色主题", category="preference")

# 更新事实
memory(action="replace_fact", fact_id="f_123", content="用户偏好浅色主题")

# 删除事实
memory(action="remove_fact", fact_id="f_123")

# 查看当前记忆
memory(action="read")
```

**记忆分类**：`work` / `personal` / `preference` / `project` / `other`

**容量限制**：每个会话最多 30 条事实，单条上限 500 字符。超出时自动淘汰低分事实。

**安全保护**：所有写入内容经过安全扫描，拦截提示注入、角色劫持、凭证泄露等威胁。

**跨会话记忆**：设置 `user_id` 后，同一用户的所有会话共享用户级记忆（偏好、画像等）。

### 5. 配置系统
- 支持从 `~/.deepai/DEEPAI.md` 和 `./deepai/DEEPAI.md` 加载配置
- 支持环境变量配置（见下文）

### 6. 调试功能
- 支持异步写入调试日志
- 记录工具调用、结果、文本块和错误
- 可配置日志文件路径

## 配置

### 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `DEEPAI_PROVIDER` | LLM 提供者（openai / anthropic / siliconflow / openai-compat） | 无（脚本演示模式） |
| `DEEPAI_MODEL` | 模型名称 | demo-model |
| `DEEPAI_DATABASE_URL` | 记忆存储数据库连接串 | 空（禁用记忆） |
| `DEEPAI_CONTEXT_WINDOW` | 上下文窗口大小 | 0（不限制） |
| `DEEPAI_DEBUG_FILE` | 调试日志文件路径 | - |
| `DEEPAI_DEBUG` | 启用调试日志（默认输出到临时文件） | - |

### 记忆存储后端

| `DEEPAI_DATABASE_URL` 格式 | 存储后端 |
|------|------|
| `postgres://user:pass@host/db` | PostgreSQL |
| `sqlite:///path/to/file.db` | SQLite |
| 文件路径（`.db` / `.sqlite` 后缀） | SQLite |
| 不设置 | 禁用记忆系统 |

## 运行方式

### 基础运行（无记忆、演示模式）

```bash
go run ./cmd/deepai
```

### 启用记忆 + 真实 LLM

```bash
export DEEPAI_PROVIDER=openai
export OPENAI_API_KEY=your-api-key
export DEEPAI_MODEL=gpt-4o
export DEEPAI_DATABASE_URL=postgres://user:pass@localhost/deepai

go run ./cmd/deepai
```

### 启用调试日志

```bash
export DEEPAI_DEBUG_FILE=/tmp/deepai-debug.log
go run ./cmd/deepai

# 或使用默认临时文件
export DEEPAI_DEBUG=1
go run ./cmd/deepai
```

## 输出说明

运行后你会看到以下输出：

- `[tool call]`：工具调用信息，显示工具名称和 ID
- `[tool result]`：工具执行结果（超过 200 字符会截断显示）
- `[subagent]`：子代理任务事件
- `[error]`：错误信息
- `[usage]`：token 使用统计（输入、输出、总计）
- `[memory nudge]`：记忆审查触发（连续 10 轮未主动保存时）

## 交互示例

```
deepai interactive mode — type your prompt, Ctrl+D or 'exit' to quit

> 我叫 Alice，喜欢用 Go 写后端服务
[tool result] ...

> 帮我查看当前目录的文件
[tool call] bash(call-123)
[tool result] go.mod  main.go  ...
> 退出
bye.
```

下次启动时，Agent 会记住 "用户叫 Alice，喜欢 Go，做后端"。

## 配置文件示例

### DEEPAI.md 文件示例
```markdown
# 项目配置
- 项目名称：deepai-demo
- 开发语言：Go
- 主要功能：AI 代理系统

# 使用指南
1. 使用 bash 工具执行命令
2. 使用文件工具操作文件
3. 使用 memory 工具记住重要信息
```

### 环境变量配置
```bash
# .env 文件
DEEPAI_PROVIDER=openai
DEEPAI_MODEL=gpt-4o
OPENAI_API_KEY=sk-your-key
DEEPAI_DATABASE_URL=postgres://user:pass@localhost/deepai
DEEPAI_DEBUG_FILE=/tmp/deepai.log
```

## 相关文件

- [pkg/memory/](../pkg/memory/) — 记忆系统核心（存储、注入、提取、安全）
- [pkg/agent/react.go](../pkg/agent/react.go) — Agent 执行逻辑（记忆注入、压缩前保存）
- [pkg/gateway/](../pkg/gateway/) — HTTP Gateway（记忆接线、user-scope 更新）
- [pkg/tools/builtin/](../pkg/tools/builtin/) — 内置工具（memory、search 等）
- [pkg/checkpoint/](../pkg/checkpoint/) — 会话持久化（PostgreSQL）
- [docs/hermes-agent-memory-analysis.md](../docs/hermes-agent-memory-analysis.md) — 记忆系统设计文档
