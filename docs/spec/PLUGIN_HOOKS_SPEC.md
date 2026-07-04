# Spec: 插件 Hooks（阶段 2d）

## 背景与现状（含风险定性）

2d 让插件捆绑的 **hooks**（事件触发的 shell 命令）可用。Claude hook 在特定事件（PreToolUse/PostToolUse/Stop/SessionStart…）执行 shell 命令，可校验/拦截/通知。

**这是插件路线里最高风险、最低频的组件。** 代码审计：

1. **两套 hook 系统都是死代码**：`plugin.Manager.ExecuteHook`（[manager.go:675](../../pkg/plugin/manager.go#L675)）从未在 agent loop 调用；skill `HookRunner` 在 chat.go 未配置。即 deepai 当前**根本不跑任何 hook**。
2. **9 个 Claude 事件里 5 个无对应**：`UserPromptSubmit`/`SessionStart`/`SessionEnd`/`PreCompact`/`SubagentStop`/`Notification` 在 deepai 无 dispatch 点，需新建。
3. **hook 执行任意 shell**：安全敏感，需超时、隔离、失败不崩溃回合。

**建议：2d 优先级低于 2c，可等出现真实需求再做。** 本 spec 给出**最小可行范围**（4 个有干净对应的事件），其余显式排除，避免一次性吞下高风险大改。

## Claude hook 格式（官方）

`hooks/hooks.json` 或 plugin.json inline：

```json
{
  "hooks": {
    "PreToolUse": [{"matcher": "Write|Edit", "hooks": [{"type": "command", "command": "${CLAUDE_PLUGIN_ROOT}/scripts/fmt.sh"}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "echo done"}]}]
  }
}
```

事件：`PreToolUse`/`PostToolUse`/`UserPromptSubmit`/`Notification`/`Stop`/`SubagentStop`/`SessionStart`/`SessionEnd`/`PreCompact`。
`type: command`：执行 shell，stdin 喂事件 JSON（`session_id`/`tool_name`/`tool_input`/…），stdout 进上下文。**`PreToolUse` exit code 2 = 阻止该工具调用**；exit 0 = 放行。

## 目标 / 非目标（最小可行范围）

**目标（3 个事件，均有干净 deepai 对应）**

- `PreToolUse`：工具执行前（tool-call 点 [react.go:494/596](../../pkg/agent/react.go#L494)），exit 2 可阻止。
- `PostToolUse`：工具执行后。
- `Stop`：agent `Run` 结束。

加载 `<plugin>/hooks/hooks.json`（含 inline）+ 项目 `<workDir>/.deepai/hooks.json`；`command` 类型 shell 执行 + `${CLAUDE_PLUGIN_ROOT}` 展开；超时 + 失败隔离。

> **不包含 `SessionStart`**：deepai 的 REPL 每个用户回合都新建 agent 并调一次 `Run`（[repl.go](../../pkg/chat/repl.go)），把 SessionStart 绑到 `Run` 入口会变成"每回合触发"而非"会话开始一次"——语义错位。若将来要真正的会话级事件，应在 REPL 层（`ChatRepl.Run` 入口）dispatch，单列设计，不冒充 Claude 的 SessionStart。

**非目标（显式排除，降低风险）**

- 其余事件（`SessionStart`/`UserPromptSubmit`/`Notification`/`SubagentStop`/`SessionEnd`/`PreCompact`）——无干净对应或语义错位。
- `validation`/`notification` hook 类型（只做 `command`）。
- **hook stdout 进上下文**：本轮 stdout **仅 `slog.Debug`，不注入任何消息/note**（API 不承载 note 注入；避免在 2d 引入复杂的上下文反馈协议）。唯一对回合的行为影响是 `PreToolUse` exit 2 阻止工具。

## 事件映射

| Claude | deepai dispatch 点 | 阻止能力 |
|---|---|---|
| `PreToolUse` | tool-call 前（494/596） | exit 2 → 跳过该工具，回错误给模型 |
| `PostToolUse` | tool-call 后 | 否 |
| `Stop` | `Run` 出口 | 否 |

## 失败隔离 / 安全

- 每个 hook 独立超时（默认 60s，可调），超时 → kill + warn + 视为放行（不阻塞回合）。
- hook 执行错误 → `slog.Warn`，不 panic、不中断回合（PreToolUse 出错视为放行，避免误伤）。
- hook 命令以用户权限运行，**不进** deepai 沙箱（与 MCP 一致，属用户自行安装的信任边界）。
- `${CLAUDE_PLUGIN_ROOT}` 展开为插件绝对路径；其他 `${VAR}` 环境展开。

## 代码改动点

1. **新增** `pkg/hooks/`：
   - `type Action struct { Event, Matcher, Command string }`。
   - `Load(workDir string, pluginHookConfigs []HookSource) (Registry, []string)` —— 解析 Claude `hooks.json`（包装形）+ plugin.json inline `hooks`；按事件索引；返回问题列表。
   - `Registry.Run(ctx, event, payload) (blocked bool, reason string)` —— 匹配 matcher（工具名 glob/正则），执行 command：`exec.Command` + stdin JSON + 超时；PreToolUse 下 exit 2 → blocked=true。
2. **改** `pkg/claudeplugin`：`Plugin.Hooks() (raw []byte, problem string)` —— 读 `<plugin>/hooks/hooks.json` 或 manifest inline `hooks`，返回原始 JSON（含 `${CLAUDE_PLUGIN_ROOT}` 展开）。
3. **改** `pkg/agent/react.go`：在 `Run` 入口/出口、tool-call 前后（494/596）插 dispatch 调用（通过 `AgentConfig.Hooks hooks.Registry` 传入）。PreToolUse blocked → 跳过执行，构造一个工具错误结果回给模型。
4. **改** `pkg/agent`：`AgentConfig` 加 `Hooks hooks.Registry`（可空）。
5. **改** `pkg/commands/chat.go`：加载 hook 配置 → 建 `Registry` → 传入 agent（主 agent 与 subagent executor 各自）。
6. **文档**：`pkg/hooks/README.md` 说明支持的事件集与排除项。

## 测试计划

- `pkg/hooks/registry_test.go`：
  - 解析 hooks.json（包装形 + inline）；matcher 匹配工具名。
  - `Run`：command 执行、stdin JSON 正确、超时 → 放行、exit 2 → blocked。
  - 坏 hooks.json → 问题进列表，不 panic。
- react.go：PreToolUse blocked 时，工具不执行且回错误（用 stub registry）。
- 交互冒烟（实现后）：插件 PostToolUse hook（echo）确认在工具后触发；PreToolUse exit 2 确认阻止。

## 决策（已定）

1. **是否现在做 2d**：**不做**。先落 2c；2d 暂存 spec，待真实需求再启动。
2. **事件范围**：仅 `PreToolUse`/`PostToolUse`/`Stop`（`SessionStart` 已移除——语义错位）。
3. **PreToolUse 阻止语义**：exit 2 阻止该工具，`reason` 作为工具错误回给模型；其余情况放行。
4. **超时**：默认 60s；**超时一律放行且不进上下文**。
5. **stdout 处理**：仅 `slog.Debug`，**不注入**任何消息/note（API 不承载，避免上下文反馈协议）。

## 与 2c 的关系

相互独立。结论：**先做 2c**（可即时落地）；**2d 暂存**（高风险，最小范围已 spec 化）。
