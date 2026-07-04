# Claude Plugin Commands 使用文档

本文说明如何在 deepai 中编写、安装和使用 Claude 风格的插件命令（file-command）。

## 1. 这是什么

deepai 支持从 Markdown 文件加载 slash 命令。

当你在 REPL 中输入一个命令，例如：

```text
/demo:greet Alice
```

deepai 会：

1. 找到对应的命令文件。
2. 展开正文里的 `$ARGUMENTS`、`$1`、`$2` 等参数占位符。
3. 把展开后的正文当作一条新的用户消息注入当前对话。
4. 正常跑一轮 agent。

这类命令本身不直接执行 shell，也不直接改文件；它只是把一段预定义 prompt 注入到对话里。

## 2. 支持哪些来源

deepai 会从这三类目录加载命令：

1. 用户级：`~/.deepai/commands/*.md`
2. 项目级：`<workdir>/.deepai/commands/*.md`
3. 插件级：`<plugin>/commands/*.md`

加载顺序：用户级 → 项目级 → 插件级。

说明：

- 用户级和项目级命令使用裸名，例如 `/review-pr`
- 插件命令强制带插件名前缀，例如 `/demo:greet`
- 插件命令不会和内置命令、用户命令、项目命令同名冲突
- 如果文件命令和内置 slash 命令重名，例如 `help.md`，该命令会被跳过

## 3. 插件命令的目录结构

一个 Claude 插件目录里，命令放在 `commands/` 目录下：

```text
my-plugin/
├── .claude-plugin/
│   └── plugin.json
└── commands/
    ├── greet.md
    └── review-pr.md
```

如果插件名是 `demo`，那么：

- `commands/greet.md` 的调用名是 `/demo:greet`
- `commands/review-pr.md` 的调用名是 `/demo:review-pr`

## 4. 命令文件格式

命令文件是一个 Markdown 文件，支持可选 frontmatter 和正文。

示例：

```md
---
description: 生成一个礼貌的问候
argument-hint: [name]
allowed-tools: Read, Grep
model: sonnet
---
请用中文向 $1 打招呼，并结合以下上下文：$ARGUMENTS
```

当前 deepai 的实际支持如下：

- `description`：支持，用于 `/help` 展示
- 正文：支持，作为真正注入对话的 prompt
- `argument-hint`：可写，但当前仅容忍解析，不参与行为
- `allowed-tools`：可写，但当前不生效
- `model`：可写，但当前不生效
- 未知字段：会被忽略，不会导致命令整体失效

如果没有 frontmatter，整个文件正文仍然可以作为命令使用。

## 5. 参数展开规则

支持两类占位符：

1. `$ARGUMENTS`
2. `$1`、`$2`、`$3` ...

规则如下：

- `$ARGUMENTS`：替换成命令名后面的整段原始文本
- `$1`、`$2`：按空白切分后的第 1、2 个位置参数
- 越界参数会替换为空串

例子：

命令正文：

```text
Review PR #$1. Extra context: $ARGUMENTS
```

用户输入：

```text
/demo:review-pr 123 payment timeout bug
```

展开结果：

```text
Review PR #123. Extra context: 123 payment timeout bug
```

## 6. 没有 `description` 时会怎样

如果 frontmatter 里没有 `description`，deepai 会用正文的第一条非空行作为命令说明，并在 `/help` 中展示。

例如：

```md
请审查当前变更，重点关注安全和回归风险。

输出结论和关键问题。
```

那么该命令在 `/help` 中的描述会是：

```text
请审查当前变更，重点关注安全和回归风险。
```

## 7. 如何安装带命令的插件

### 方式 1：安装远程仓库

```bash
deepai plugin install <git-url>
```

如果插件不在仓库根目录，可以用：

```bash
deepai plugin install <git-url> --subdir path/to/plugin
```

### 方式 2：链接本地目录（开发推荐）

```bash
deepai plugin add /path/to/plugin
```

链接后，修改本地插件目录内容会立即生效。

### 查看已安装插件

```bash
deepai plugin list
```

### 删除插件

```bash
deepai plugin remove <plugin-name>
```

## 8. 如何在 REPL 中使用

启动 `deepai` 后：

1. 输入 `/help`
2. 在 `Custom commands:` 区域查看已加载的文件命令
3. 直接输入命令

例如：

```text
/demo:greet Alice
/demo:review-pr 123
```

命令命中后，deepai 不会走内置 slash 动作分支，而是会把命令正文展开后注入为当前回合的用户输入。

## 9. 一个最小可用示例

插件目录：

```text
demo-plugin/
├── .claude-plugin/
│   └── plugin.json
└── commands/
    └── greet.md
```

`plugin.json`：

```json
{
  "name": "demo"
}
```

`commands/greet.md`：

```md
---
description: 生成一个问候语
---
请用中文向 $1 打招呼。如果没有名字，就生成一个通用问候。
```

安装：

```bash
deepai plugin add /path/to/demo-plugin
```

使用：

```text
/demo:greet Alice
```

## 10. 当前明确不支持的特性

为了降低复杂度和安全风险，当前版本明确不支持以下 Claude command 能力：

1. `` !`cmd` `` 内联 shell 执行
2. `@file` 文件内容内联
3. per-command `model`
4. `disable-model-invocation`
5. 模型主动调用 slash command 工具

也就是说，当前命令系统只支持：

- 用户手动输入 `/...`
- 参数展开
- 将正文注入成一轮用户消息

## 11. 命令命名规则

命令文件名会变成命令名，因此请遵守这些约束：

- 不能为空
- 不能包含空格
- 不能包含 `/`、`\`
- 不能包含 `..`

建议使用：

```text
review-pr.md
greet.md
fix-tests.md
```

不建议使用：

```text
my command.md
../hack.md
foo/bar.md
```

## 12. 故障排查

### `/help` 里看不到命令

优先检查：

1. 文件是否放在正确目录
2. 插件是否已通过 `deepai plugin list` 显示为 `ok`
3. 文件名是否合法
4. 是否与内置命令重名，例如 `help.md`、`clear.md`

### 命令触发了，但效果不对

优先检查：

1. `$ARGUMENTS` 和 `$1` 的使用是否符合预期
2. 正文是否真的写成了你想注入的 prompt
3. frontmatter 是否缺失关闭分隔符 `---`

### 插件命令为什么必须带前缀

这是当前 deepai 的设计选择：

- `/plugin:cmd` 可以避免和内置命令冲突
- 避免和项目命令、用户命令冲突
- 行为更稳定，调试更简单

## 13. 推荐实践

1. 一个命令只做一件事，正文尽量短而明确。
2. 用 `description` 写清楚“何时调用”。
3. 需要用户输入参数时，优先用 `$1`、`$2`；需要保留整段文本时，用 `$ARGUMENTS`。
4. 插件命令适合封装高频 prompt 模板，不适合承载复杂执行逻辑。
5. 复杂逻辑优先放到 MCP、agent 或 skill；command 更适合做“快捷入口”。
