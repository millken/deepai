# Spec: Claude 插件集成（阶段 2）

## 背景与现状

阶段 1（[MCP_INTEGRATION_SPEC.md](MCP_INTEGRATION_SPEC.md)）让 deepai 能从 `.mcp.json` 加载 MCP server。阶段 2 让 deepai 识别 **Claude Code 插件包**——一个带 `.claude-plugin/plugin.json` 清单的目录，可捆绑 5 类组件（commands / agents / skills / hooks / mcpServers），目标是复用 Claude 插件生态、不再自研对应机制。

代码审计（见下表）显示：5 类组件在 deepai 的映射成本差异极大，**不宜一次全做**。本 spec 据此把阶段 2 拆为子阶段，先把性价比最高的 2a 写成可实现规格，2b/2c/2d 留概要。

### 代码审计结论（映射成本）

| 组件 | 状态 | 关键依据 |
|---|---|---|
| Skills | **就绪** | `skill.Registry.LoadAll(projectDir, pluginDirs []string)` 已支持插件目录（[registry.go:108](../../pkg/skill/registry.go#L108)）；但 [chat.go:137](../../pkg/commands/chat.go#L137) 传 `nil`。`SkillToolWithRegistry` 暴露给 LLM。唯一缺陷：无来源命名空间（同名 last-write-wins）。 |
| MCP | **适配器（~20 行）** | loader 已生产可用（[chat.go:150](../../pkg/commands/chat.go#L150)），JSON 形状与 Claude 一致。需 `LoadWithServers` 重载合并插件的 server。 |
| Agents | **适配器** | YAML 按需加载已可用，但只读 `.deepai/agents/{type}.yaml`（[yaml_loader.go:41](../../pkg/agent/yaml_loader.go#L41)），无目录枚举/注册表。需翻译 `.md`→YAML + 加插件搜索路径。 |
| Commands | **大改** | slash 命令是硬编码 `var slashCommands` + `handleSlashCommand` switch（[repl.go:706](../../pkg/chat/repl.go#L706)），无注册表/markdown 加载/提示注入路径。 |
| Hooks | **大改 + 死代码** | `plugin.Manager.ExecuteHook` 从未在 agent loop 调用；skill `HookRunner` 在 chat.go 未配置。9 个 Claude 事件中 5 个无对应（UserPromptSubmit / SessionStart / SessionEnd / PreCompact / SubagentStop / Notification）。 |
| `pkg/plugin` 管理器 | **死代码，勿复用** | 清单是 `plugin.yaml` 单类型（[types.go:154](../../pkg/plugin/types.go#L154)），与 Claude 多组件 `plugin.json` 不兼容。阶段 2 新建独立加载器。 |

## Claude 插件格式回顾

- 清单：`.claude-plugin/plugin.json`，必填 `name`；组件路径字段类型（按官方 plugins-reference）：`commands`/`agents` = string|array（路径），`hooks`/`mcpServers` = string|object（路径或 inline）。均**补充**而非替换默认目录。
- 默认目录：`commands/*.md`、`agents/*.md`、`skills/<n>/SKILL.md`、`hooks/hooks.json`、`.mcp.json`。
- 注意：**skills 没有 manifest 字段**，固定从 `skills/` 自动发现。
- `${CLAUDE_PLUGIN_ROOT}` = 插件绝对路径，用于 hooks/mcp/脚本里的相对路径。

## 子阶段划分

| 子阶段 | 范围 | 成本 | 价值 |
|---|---|---|---|
| **2a（本 spec 详细规格）** | 插件发现 + Skills + MCP | 低 | 高——一个 Claude 插件的 skills 与 MCP server 自动加载 |
| 2b | Agents（.md→agent 适配 + 插件搜索路径） | 中 | 中 |
| 2c | Commands（新建命令注册表 + markdown 加载 + 提示注入） | 高 | 中 |
| 2d | Hooks（接 dispatch 点 + 事件映射，5/9 无对应需新建） | 高/高风险 | 中 |

---

# 阶段 2a 详细规格（本次实现范围）

## 目标 / 非目标

**目标**

- 扫描插件目录，识别含 `.claude-plugin/plugin.json` 的插件包。
- 自动加载每个插件的 `skills/`（复用 `skill.Registry`）与 mcpServers（复用阶段 1 的 MCP 加载）。
- 支持 `${CLAUDE_PLUGIN_ROOT}` 与 `${VAR}` 展开。
- 单个插件/组件失败不阻断其余。

**非目标（留给 2b/2c/2d）**

- 加载插件的 `agents/`、`commands/`、`hooks/`。
- marketplace 安装/克隆/版本解析。
- 插件启用/禁用配置（阶段 2a 加载所有已存在的插件）。

## 插件发现

扫描两个根（后者覆盖同名插件）：

1. `~/.deepai/plugins/` —— 全局
2. `<workdir>/.deepai/plugins/` —— 项目级

每个根下的**直接子目录**是一个候选插件；仅当其含 `.claude-plugin/plugin.json` 时视为有效插件。缺失/损坏的 `plugin.json` → warn 并跳过该插件，继续其余。

> 是否额外读取 `~/.claude/plugins/`（复用 Claude 已安装插件）见「待决策」。

## 清单解析

新增 `Manifest` 结构（仅解析 2a 需要的字段，其余忽略）：

```go
type Manifest struct {
    Name        string          `json:"name"`         // 必填，kebab-case
    Version     string          `json:"version"`
    Description string          `json:"description"`
    MCPServers  json.RawMessage `json:"mcpServers"`   // inline object 或 路径字符串
}
```

`mcpServers` 解析（官方形状 string|object；其他形状防御性处理，不静默丢弃）：
- manifest inline `mcpServers` 为 object → 直接当作 server map；
- manifest `mcpServers` 为 string → 当作相对插件根的路径，读该文件（内容为 `{ "mcpServers": {...} }` 包装形或直接 `{ "name": {...} }` 裸 server map 两种都接受）；路径必须落在插件目录内（`../` 逃逸被拒并计入 problem）；
- 默认 `<plugin>/.mcp.json` 存在 → 读它，与上面合并（同名后者覆盖前者）；
- 若 `mcpServers` 为 array 或其他形状（不在官方 mcpServers 规范内）→ `slog.Warn` + 跳过该插件的 MCP，**不静默丢弃**。

合并后对所有字符串值先展开 `${CLAUDE_PLUGIN_ROOT}`→插件绝对路径，再交由 MCP loader 做 `${VAR}` 环境展开。

## Skills 加载

复用 `skill.Registry.LoadAll(projectDir, pluginDirs)`：**`pluginDirs` 必须传插件根目录 `<plugin>`，不是 `<plugin>/skills`**——`LoadAll` 内部会对每个 pluginDir 再拼 `/skills`（[registry.go:122](../../pkg/skill/registry.go#L122)），传 skills 子目录会读到 `<plugin>/skills/skills` 而静默加载不到任何 skill。[chat.go:137](../../pkg/commands/chat.go#L137) 的 `nil` 改为**插件根目录**列表。`SkillToolWithRegistry` 自动让 LLM 可调用。

> 命名冲突：deepai 无来源命名空间，同名 skill last-write-wins（加载顺序：global→project→plugin）。2a **接受** last-write-wins、不加前缀（见「决策」）。

## MCP 加载

阶段 1 的 `pkg/mcp` 增加 `LoadWithServers` 重载：在 `Discover(workdir)` 结果之上合并一份 `extra map[string]ServerConfig`（插件来源），共用既有连接循环。`extra` 的值走与磁盘配置相同的 `${VAR}` 展开。

由 chat.go 把各插件解析出的 server map（已展开 `${CLAUDE_PLUGIN_ROOT}`）聚合后，一次性传给 `mcp.LoadWithServers`。`claudeplugin` 包**不直接调** skill/mcp loader、无副作用，只负责发现 + 解析 + 展开（见「代码改动点」的责任边界）。

## 失败隔离

延续阶段 1 哲学：单插件 `plugin.json` 损坏 / mcpServers 解析问题 / 某 MCP server 连接失败 → `slog.Warn` + 跳过，不中断其余插件或 session，并聚合进 `MCPReport`（如 `MCP: 2 loaded (a, b), 1 failed (x), plugins: p1 p2`）。

**skills 加载的可观测性需先补齐（现状缺口）**：现有 `skill.Registry.loadDirSilent` 在 `os.ReadDir` 失败时直接 `return`，不 warn 不回传；`LoadFromDir` 又会吞掉单个 SKILL.md 的解析错误（[registry.go:138,241](../../pkg/skill/registry.go#L138)）。若不修，插件 `skills/` 的权限问题 / 损坏目录 / 解析失败仍是静默失效。2a 分两层修：
- **目录级失败**（`ReadDir` 失败，如权限/损坏目录）：`loadDirReported` 产出 `SkillWarning`，chat.go 把**插件来源**的并入 report（global/project 仅 `slog.Warn`、不进 TUI，避免回溯性噪音）。
- **单 skill 解析失败**：`LoadFromDir` 改为 `slog.Warn` 后再 skip（不静默；不进 TUI report，以免改 `LoadFromDir` 签名牵动共享链路）。

## 代码改动点

1. **新增** `pkg/claudeplugin/`（独立包，不复用 `pkg/plugin`）。**职责仅限发现 + 解析 + 展开，不调用 skill/mcp loader、无副作用**（避免双遍历与空壳 API）：
   - `type Manifest struct`（如上）。
   - `type Plugin struct` —— 持有插件根目录 `Dir`、`Name`、解析后的 manifest。
   - `Discover(workdir string) (plugins []Plugin, problems []string)` —— 扫描两根，返回有效插件 + 坏插件/坏清单描述（供 report）。
   - `(p *Plugin) SkillRoot() string` —— 返回**插件根目录**（调用方收集后传给 `LoadAll` 的 `pluginDirs`；`LoadAll` 自己拼 `/skills`，所以这里不拼）。
   - `(p *Plugin) MCPServers() (servers map[string]mcp.ServerConfig, problem string)` —— 合并三来源、展开 `${CLAUDE_PLUGIN_ROOT}`，返回 server map + 解析问题（如 array 等非规范形状）；无 MCP 时返回 nil。
2. **改** `pkg/skill`（可观测性 + 调用）：
   - `loadDirSilent` → `loadDirReported(dir, source) []SkillWarning`：`ReadDir` 失败产出 warning（含 dir、source、err），不再 bare `return`；目录不存在仍静默（合法）。
   - `LoadFromDir` 单 skill 解析失败改为 `slog.Warn` 后 skip（不再静默吞掉）。
   - 新增 `LoadAllReported(projectDir, pluginDirs) []SkillWarning`（`SkillWarning{Source, Dir, Msg}`）暴露**目录级** warning；`LoadAll` 改为委托它并对每条 `slog.Warn`（**签名不变**，既有 4 处调用点/测试不受影响）。
   - chat.go 调用从 `LoadAll` 换成 `LoadAllReported(workDir, pluginRoots)`（**插件根目录**，非 skills 子目录）。
3. **改** `pkg/mcp`：新增 `LoadWithServers(ctx, registry, workdir, extra) (closers, report)`；把 `Load` 现有连接循环抽成内部 `loadServers(ctx, registry, servers)`，`Load` 与 `LoadWithServers` 共用。`extra` 同样走 `${VAR}` 展开。
4. **改** `pkg/commands/chat.go`（**唯一聚合点，单遍历**）：`plugins, problems := claudeplugin.Discover(workDir)` → 遍历**一次** `plugins`，收集 `SkillRoot()` 列表、合并各 `MCPServers()` 到 `pluginServers`、累加各插件 problem → `skillWarnings := skillReg.LoadAllReported(workDir, pluginRoots)`（**须在注册 `SkillToolWithRegistry` 之前**）→ 对每条 warning `slog.Warn`，`Source=="plugin"` 的并入 report → `mcp.LoadWithServers(ctx, registry, workDir, pluginServers)` → 把 `problems` + 插件 skill warnings 前置拼进 MCP report 写入 `ReplConfig.MCPReport`。
5. **文档**：新增 `pkg/claudeplugin/README.md`，说明插件目录结构与发现规则。

## 安全模型

插件内的 MCP server / skills 脚本以用户权限运行，与阶段 1 一致，不进 deepai 沙箱。`${CLAUDE_PLUGIN_ROOT}` 仅做字符串替换，不执行任意代码（真正的代码执行发生在 MCP server 进程 / skill 脚本，属用户自行安装的信任边界）。

## 测试计划

- 新增 `pkg/claudeplugin/loader_test.go`：
  - 发现：有效插件（含 plugin.json）被识别，无清单的目录被跳过；两根合并 + 项目覆盖全局。
  - 清单损坏 → warn+跳过，其余插件仍加载；problem 并入 report（非静默）。
  - mcpServers：object inline / string 路径 / 默认 .mcp.json 三来源合并；路径文件接受包装形与裸 server map 两种；`../` 路径逃逸被拒；array 与损坏文件（如 `{"foo":"bar"}`）→ 返回 problem、不静默；空 `{}` / `{"mcpServers":{}}` → 无 server、不报错。
  - `${CLAUDE_PLUGIN_ROOT}` 展开为插件绝对路径。
  - `SkillRoot()` 返回插件**根目录**（非 skills 子目录），符合 `LoadAll` 再拼 `/skills` 的约定。
- `pkg/skill` 可观测性：`loadDirSilent` 的 `ReadDir`/解析失败产出 warning（不再 bare return）；`LoadAllReported` 返回插件来源 warning 供 chat.go 并入 report；目录不存在仍静默。
- `pkg/mcp`：为 `LoadWithServers` 加测试（extra map 合并、env 展开、单 server 失败隔离）。
- 既有阶段 1 测试保持绿。

## 决策（已定）

1. **发现路径**：只读 `~/.deepai/plugins/` 与 `<workdir>/.deepai/plugins/`。**不**纳入 `~/.claude/plugins/`——边界最清楚，避免承担 Claude 本机目录的结构漂移、权限、版本差异、跨工具副作用等兼容债。真要支持 Claude 安装目录，后续单开一个兼容子阶段。
2. **skills/agents 命名空间**：2a 接受 **last-write-wins，不加前缀**。现有 skill 链路本就是按名字注册/覆盖（`registry.go` 同一模型）；现在加前缀会牵动用户可见名称、调用习惯、文档示例，影响面大于 2a 本身。"来源命名空间"作为 2b+ 统一设计，一次性覆盖 skills 与 agents。
3. **2a 范围**：只做 发现 + skills + mcp，**不纳入 agents**。agents 是单文件懒加载模型（`yaml_loader.go`），与插件目录扫描、`.md` 适配、注册表化不在一个量级，属 2b。

---

# 后续子阶段概要（非本 spec 实现范围）

## 2b — Agents

- 适配 Claude `agents/*.md`（frontmatter: name/description/tools；body=system prompt）→ deepai `AgentTypeConfig`。
- 两条路二选一：(a) 在 `loadAgentYAML` 增加 `.md` 解析与插件搜索路径；(b) 加载时把 `.md` 翻译成内存 `AgentTypeConfig`，注册进一个新 agent 注册表（当前是按 type 懒查文件，无注册表）。
- 风险：Claude agent 可能依赖 Claude Code 专属工具面，需映射或降级。

## 2c — Commands

- 新建 slash 命令注册表（替代/补充硬编码 switch）。
- markdown+frontmatter 命令加载器；`/pluginname:cmd` 触发，把 body 作为用户轮 prompt 注入（当前无此注入路径，`SkillToolWithRegistry` 是 sub-run system-prompt，不同机制）。
- dispatch 接入 [repl.go:178](../../pkg/chat/repl.go#L178)。

## 2d — Hooks

- 在 `pkg/agent/react.go` 的 `Run` 与工具调用点（约 481/596 行）新增 hook dispatch。
- 事件映射：PreToolUse/PostToolUse → 复用既有 `HookBefore/AfterToolCall`；Stop → `HookAfterAgentRun`；UserPromptSubmit/SessionStart/SessionEnd/PreCompact/SubagentStop/Notification → 需新增 HookPoint 常量与触发点。
- 高风险：hook 执行 shell 命令，需严格超时与错误隔离；多数插件 hook 假定 Claude Code 生命周期语义。

## 非目标（全阶段）

- marketplace 安装（git clone、版本解析、`marketplace.json`）。
- 插件启用/禁用 UI 与配置持久化。
- 复用 `pkg/plugin`（已判定死代码 + 格式不兼容）。
