# Spec: 插件 Commands（阶段 2c）

## 背景与现状

2c 让插件捆绑的 **slash 命令**（`commands/*.md`）可用。Claude 的 file-command 是一段 markdown，用户键入 `/cmd args` 时，其正文（展开参数后）作为**用户轮 prompt 注入**对话。

代码审计揭示 deepai 现有 slash 系统两件事：

1. **命令是硬编码的**：`var slashCommands = []slashCmd{...}`（[slashcommands.go:10](../../pkg/chat/slashcommands.go#L10)）13 个名字，dispatch 是 `handleSlashCommand` 的 switch（[repl.go](../../pkg/chat/repl.go)）。无注册表、无文件加载器。
2. **没有 prompt 注入路径**：现有 slash 命令都是 REPL 动作（clear/history/model…），没有"把正文当用户消息跑一回合"的机制。注入点已存在：`runTurn(ctx, line, false)`（[repl.go:421](../../pkg/chat/repl.go#L421)）。

所以 2c = 新建 file-command 注册表 + markdown 加载 + 参数展开 + 复用 `runTurn` 注入。

## Claude 命令格式（官方）

`commands/*.md`，可选 frontmatter + 正文（= 注入的 prompt）：

```
---
description: 简述
argument-hint: [pr-number]
allowed-tools: Bash(git:*), Read
model: sonnet
disable-model-invocation: false
---
Review PR #$1. Context: $ARGUMENTS
```

特性：`$ARGUMENTS`（全部参数）、`$1`/`$2`（位置参数）、`` !`cmd` ``（执行 bash 并内联输出）、`@file`（内联文件内容）。插件命令可命名空间化为 `/plugin:command`。

## 目标 / 非目标

**目标**

- 加载 `<plugin>/commands/*.md`、项目级 `<workDir>/.deepai/commands/*.md`、用户级 `~/.deepai/commands/*.md` 为 file-command。
- 解析 frontmatter（`description`、`argument-hint`）+ 正文。
- `/cmd args` → 展开 `$ARGUMENTS`/`$N` → 把正文作为用户消息跑一回合（复用 `runTurn`）。
- file-command 列入 `/help`。
- 单命令解析失败不阻断其余。

**非目标（本轮显式排除，降低风险/复杂度）**

- `` !`cmd` `` bash 内联执行（需要 `allowed-tools` 解析 + 额外安全面，单列）。
- `@file` 文件引用内联。
- `model` frontmatter（deepai 无 per-command model）。
- `disable-model-invocation` 与 `SlashCommand` 工具（模型主动调用命令）——本轮只做**用户键入**触发。
- 子目录命名空间（Claude 用子目录组织但不影响命令名）。

## 命名空间

- 项目/用户命令：裸名 `/cmd`。
- 插件命令：**强制** `/pluginname:cmd`（避免与内置/项目命令碰撞；省去碰撞检测逻辑）。`ParseSlashCommand` 已按首个空格切分，名字可含 `:`。

> 是否对插件命令也允许裸名（碰撞时才加前缀）见「决策」。

## 参数展开

`$ARGUMENTS` → 用户在命令名后输入的全部文本；`$1`/`$2`/… → 按空白切分的位置参数（越界返回空串）。展开在注入前对正文做字符串替换。无参数时 `$ARGUMENTS`→""。

## 注入

REPL 主循环（[repl.go](../../pkg/chat/repl.go)）：识别 slash 后，**先查 file-command 注册表**，命中则 `r.runTurn(ctx, expandedBody, false)`（把展开后的正文当用户消息跑一回合，会持久化）；未命中再走既有 `handleSlashCommand`（内置动作命令）。这样 file-command 与内置命令并存。

## 失败隔离

单命令 `.md` 解析失败 → `slog.Warn` + 不注册，其余命令正常。命令名非法（含空格/路径分隔符）→ 拒绝注册并 warn。

## 代码改动点

1. **新增** `pkg/chat/command.go`：
   - `type Command struct { Name, Description, Body, Source string }`。
   - `LoadCommands(workDir string, pluginCommandDirs []string) (map[string]Command, []string)` —— 扫描 `<workDir>/.deepai/commands/*.md` + 各 `<plugin>/commands/*.md`，解析 frontmatter+正文，返回命令表 + 问题列表（供 report）。插件命令的注册名带 `pluginname:` 前缀。
   - `Expand(body, args string) string` —— `$ARGUMENTS` / `$N` 展开。
2. **改** `pkg/claudeplugin`：`Plugin.CommandDir() string`（`<plugin>/commands`）+ `Plugin.Name` 已有（作前缀）。
3. **改** `pkg/chat/repl.go`：
   - `ReplConfig` 加 `Commands map[string]Command`。
   - 主循环 slash 分支：先查 `Commands`（命中 → `runTurn(ctx, Expand(body, args), false)`），再走 `handleSlashCommand`。
4. **改** `pkg/chat/slashcommands.go`：`slashHelpText` 追加 file-command 清单（标注来源）。
5. **改** `pkg/commands/chat.go`：插件发现时收集 `CommandDir()` → `chat.LoadCommands(workDir, dirs)` → 传入 `ReplConfig.Commands`；问题并入启动 report。

## 安全模型

file-command 的正文是**文本 prompt**，不直接执行代码（本轮不做 `` !`bash` ``）。真正的代码执行只发生在随后 agent 回合调用的工具里，受 deepai 既有工具/沙箱约束。`.md` 路径来自插件目录枚举，无路径穿越面。

## 测试计划

- `pkg/chat/command_test.go`：
  - `LoadCommands`：项目 + 插件来源合并；插件命令带 `plugin:` 前缀；同名（项目 vs 插件）项目优先。
  - `Expand`：`$ARGUMENTS` 全量、`$1`/`$2` 位置、越界为空、无参数。
  - 坏 `.md`（坏 frontmatter）→ 不注册 + warn，不 panic。
  - 命令名非法 → 拒绝。
- REPL 集成（既有 repl_test 风格）：键入 `/proj-cmd foo` → 注入展开后的正文为一回合。
- 交互冒烟（实现后）：插件 `/demo:greet` 命令，确认 `/demo:greet Alice` 注入并跑通。

## 决策（已定）

1. **范围**：只做**用户键入触发**的 prompt 注入命令；`` !`bash` ``/`@file`/`model`/`SlashCommand` 工具显式排除。
2. **插件命令命名**：**强制 `/plugin:cmd`**（避免与内置 slash、项目命令、用户级命令碰撞，复杂度最低）。项目/用户命令用裸名 `/cmd`。
3. **用户级命令**：支持 `~/.deepai/commands/`，与项目级并列加载（和全局 skills / 全局 mcp 心智一致）。
