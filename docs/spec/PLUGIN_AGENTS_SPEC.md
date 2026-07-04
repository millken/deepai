# Spec: 插件 Agents（阶段 2b）

## 背景与现状

2a 让 deepai 加载了插件的 skills 与 mcpServers。2b 让插件捆绑的 **subagents**（`agents/*.md`）可用——即 Claude 插件生态的子代理。

代码审计揭示 deepai 现有 agent 系统两个关键事实：

1. **解析是按 type 懒查、只认 YAML**：`resolveAgentTypeConfig(t, workDir)`（[yaml_loader.go:138](../../pkg/agent/yaml_loader.go#L138)）只读 `<workDir>/.deepai/agents/{type}.yaml`；找不到 → builtin → 兜底 general。插件 `agents/*.md` 既不在该路径、格式也不同，当前**永远解析不到自己的 config**，会静默退化为 general。
2. **agent 类型完全不告诉 LLM**：`task` 工具的 `agent_type` 是自由字符串（[subagent.go:28](../../pkg/tools/subagent.go#L28) 描述仅给例子），系统提示也没有可用 agent 清单。即**现有的项目级 `.deepai/agents/*.yaml` 也是不可见的**——模型除非被用户点名，否则不会调用。

结论：**只做"解析"不够，必须同时做"枚举+广告"**，否则插件 agent 加载了也无人调用。

## Claude subagent 格式（官方）

`agents/*.md`，YAML frontmatter + 正文：

```
---
name: code-reviewer              # 官方必填，小写+连字符
description: 何时调用此 agent
tools: Read, Grep, Bash          # 可选，逗号分隔；省略=继承全部
model: sonnet                    # 可选，alias 或 inherit
---
正文 = system prompt
```

> **`name` 缺省处理（写死）**：官方要求 `name` 必填，但为与 2a/2b 失败隔离策略一致，deepai 在 `name` 缺失/为空时**回退到文件名 stem** 作为 type/name，并 `slog.Warn`（兼容性回退，非拒绝）。规格各处以此为准，不出现"必填即拒绝"。

插件 agent 在 `<plugin>/agents/`，行为与用户 agent 一致。

## 目标 / 非目标

**目标**

- 解析插件 `agents/*.md` → `AgentTypeConfig`（name→type、description、tools、body→system_prompt）。
- 让插件 agent 类型可解析（`task(agent_type="code-reviewer")` 命中插件 config，不退化为 general）。
- 枚举可用 agent（builtin + 项目 yaml + 插件 md）并把清单注入 `task` 工具描述，让 LLM 知道有哪些可调用——顺带修复项目级 agent 不可见的既有缺陷。
- Claude 工具名 → deepai 工具名映射（`Read`→`read_file` 等），否则插件 agent 的 `tools` 选不到东西。
- 单个插件 agent 解析失败不阻断其余。

**非目标**

- `model` 字段（deepai `AgentTypeConfig` 无 per-type model；忽略，统一用 executor 的 model）。
- `/agents` 交互管理界面。
- skills/agents 来源命名空间（见「决策」）。

## 解析

新增 `pkg/agent` 的 markdown agent 加载：

```go
// ParseAgentMarkdown 解析 Claude 形态的 agent .md（frontmatter + 正文）。
// path 用于错误信息；typeFromName 是文件名 stem，作为 type 兜底。
func ParseAgentMarkdown(path string) (*AgentTypeConfig, error)
```

frontmatter 字段映射：

| Claude | deepai AgentTypeConfig |
|---|---|
| `name` | `Type`、`Name`（缺省→文件名 stem + `slog.Warn`） |
| `description` | `Description` |
| `tools`（逗号串）| `DefaultTools`（经工具名映射后） |
| `model` | 忽略（记录 slog.Debug） |
| 正文 | `SystemPrompt` |

frontmatter 沿用 `pkg/skill/parser.go` 的 YAML 解析风格（`---` 分隔）。

## 工具名映射

Claude agent 的 `tools` 用 Claude 工具名，需映射到 deepai 工具名，否则 `selectSubagentTools` 按名匹配会落空：

```go
var claudeToolAliases = map[string]string{
    "Read": "read_file", "Edit": "edit_file", "Write": "write_file",
    "Bash": "bash", "Grep": "grep", "Glob": "glob", "List": "list_dir",
    "Task": "task",
}
```

未命中别名则原样保留（兼容已用 deepai 名字的 agent）。映射在解析时应用。

## 解析优先级与顺序一致性（关键不变量）

**优先级**（首个命中返回；项目 > 插件 > builtin）：

1. 项目级 YAML `<workDir>/.deepai/agents/{type}.yaml`（既有）
2. 项目级 MD `<workDir>/.deepai/agents/{type}.md`（新增，与 yaml 同目录）
3. 插件 `<plugin>/agents/{type}.md`（新增，按下述 `pluginAgentDirs` 顺序）
4. builtin
5. general 兜底

> 与 2a skills 的 `plugin > project` 相反——agents 选"用户意图优先"；命名空间阶段再统一。

**顺序一致性（写死）**：`EnumerateAgents`（广告）与 `resolveAgentTypeConfig`（执行）**必须消费同一份、同一顺序的 `pluginAgentDirs`**，该切片**直接来自 2a 的 `claudeplugin.Discover` 结果**（已按插件名排序，确定性顺序）。两者都按该顺序 first-match-wins，**不允许各自再排序或按不同规则去重**。这样保证"模型广告看到的 `code-reviewer`"与"实际执行命中的 `code-reviewer`"是同一来源——否则会广告 A 插件的 agent、却执行 B 插件的同名 agent。

`EnumerateAgents` 对同名类型去重时保留**最高优先级**那条（项目 > 靠前的插件 > …），与 `resolveAgentTypeConfig` 的命中结果一一对应。

**实现保证**：`EnumerateAgents` 收集候选 type stem 后，对每个 stem **调用 `resolveAgentTypeConfigWithPlugins` 取其 description**——即枚举与执行走同一个 resolver。这样即便项目同时存在 `foo.yaml` 与 `foo.md`（执行期 yaml 优先），广告层也必然展示 yaml 的 description，杜绝"广告 .md / 执行 .yaml"的错位。

## 枚举与广告

新增 agent 枚举（启动时一次性，供广告 + 校验）：

```go
// EnumerateAgents 列出所有可用 agent 类型的 name+description（builtin + 项目 + 插件）。
// pluginAgentDirs 必须是 claudeplugin.Discover 的原顺序，且与 resolveAgentTypeConfig
// 同源——见「顺序一致性」。
func EnumerateAgents(workDir string, pluginAgentDirs []string) []AgentInfo
```

`task` 工具描述改为动态拼接：把枚举出的 `(type — description)` 清单附到描述末尾，例如：

> `agent_type`: Agent type. Available: coder — …, code-reviewer — …, devops — …, tester — ….

这需要 `TaskTool` 接受 agent 清单参数（当前是无参工厂）。注册时机：在插件发现、agent 枚举之后（chat.go 里调整 `registerChatTools` 与发现的顺序）。

## 失败隔离

单插件 agent `.md` 解析失败 → `slog.Warn` + 跳过，不进枚举/不可解析，不阻断其余。枚举/解析全程不 panic、不 abort 启动。

## 代码改动点

1. **改** `pkg/agent`：
   - 新增 `ParseAgentMarkdown(path) (*AgentTypeConfig, error)` + `claudeToolAliases` 映射。
   - 新增 `EnumerateAgents(workDir, pluginAgentDirs) []AgentInfo`（`AgentInfo{Type, Description}`）。
   - 扩展 `resolveAgentTypeConfig(t, workDir, pluginAgentDirs)`：增加项目 `.md` 与插件 `.md` 查找（`loadAgentYAML` 旁加 `loadAgentMarkdown`，复用 `mergeConfig`）。
2. **改** `pkg/tools/subagent.go`：`TaskTool(pool, agents []AgentInfo)` —— 描述末尾附可用 agent 清单（无 agent 时退化为原描述）。
3. **改** `pkg/agent/subagent.go`：`SubagentExecutor` 加 `WithPluginAgentDirs([]string)`；`Execute` 把它传给 `resolveAgentTypeConfig`。
4. **改** `pkg/claudeplugin`：`Plugin.AgentDir() string`（返回 `<plugin>/agents`，调用方收集）。
5. **改** `pkg/commands/chat.go`：插件发现后收集各 `AgentDir()` → `EnumerateAgents(workDir, agentDirs)` → 传给 `TaskTool`；executor `WithPluginAgentDirs(agentDirs)`。注意把 `registerChatTools` 里 `TaskTool` 的注册推迟到枚举之后（或拆出单独注册）。
6. **文档**：`pkg/agent/README.md`（若无则新建）补 `.md` agent 格式与发现规则。

## 安全模型

插件 agent 的 system prompt 是文本，不执行；其 `tools` 仅是名称选择，受 deepai 既有工具集约束。`.md` 路径来自插件目录枚举（非用户输入），无路径穿越面（不像 2a 的 mcpServers 路径字符串）。

## 测试计划

- `pkg/agent/agentmd_test.go`：
  - `ParseAgentMarkdown`：完整 frontmatter + 正文 → 各字段正确；`tools` 经别名映射；缺 name 用文件名；`model` 被忽略。
  - `EnumerateAgents`：builtin + 项目 yaml + 项目 md + 插件 md 汇总；同名按优先级去重。
  - `resolveAgentTypeConfig`：项目 md > 插件 md 命中；都未命中 → builtin/general。
  - 损坏 `.md`（坏 frontmatter）→ 枚举跳过、不 panic、不阻断。
- `pkg/tools/subagent_test.go`：`TaskTool` 描述含 agent 清单；无 agent 时退化。
- 交互冒烟（实现后）：装一个含 `agents/code-reviewer.md` 的插件，确认 banner/task 描述出现该 agent，且 `task(agent_type="code-reviewer")` 真正用上插件 system prompt（非 general）。

## 决策（已定）

1. **范围**：2b 只做插件 agents（解析 + 广告 + 工具映射）；skills/agents 来源命名空间延后为独立阶段（跨 skill+agent 的 UX 改动，单独设计）。
2. **解析优先级**：项目 > 插件 > builtin（用户工作区意图优先，比与 skills 表面一致更重要）。
3. **广告渠道**：拼进 `task` 工具描述（[subagent.go:17-33](../../pkg/tools/subagent.go#L17)），不注入系统提示——改动面最小、自包含。
4. **工具名映射**：维护 Claude→deepai 别名表，未命中则原样透传（否则插件 agent 的 `tools` 基本不可用）。
