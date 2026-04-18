# Hermes 命令体系分析

> 分析对象: hermes-agent v0.10.0 (2026.4.16)
> 分析时间: 2026-04-17
> 用途: 为 deepai 命令设计提供参考

## 1. 命令总览

Hermes 共有 **35 个命令**，按功能域分为 7 大类：

| 分类 | 命令数 | 命令 |
|------|--------|------|
| 核心运行 | 3 | `chat`(默认), `model`, `status` |
| 网关/消息 | 4 | `gateway`, `whatsapp`, `webhook`, `pairing` |
| 配置管理 | 4 | `setup`, `config`, `login/logout`, `auth` |
| 插件/技能 | 4 | `skills`, `plugins`, `memory`, `tools` |
| 会话/数据 | 4 | `sessions`, `cron`, `insights`, `backup/import` |
| 诊断调试 | 4 | `doctor`, `dump`, `debug`, `logs` |
| 系统 | 5 | `version`, `update`, `uninstall`, `profile`, `completion` |

## 2. 核心运行命令

### 2.1 `chat` — 主命令（默认行为）

运行裸 `hermes` 等价于 `hermes chat`，启动交互式 REPL。

**核心流程：**
1. 检查 provider 是否已配置，未配置则引导运行 `setup`
2. 后台检查版本更新
3. 同步内置 skills
4. 创建 `HermesCLI` 实例（provider 解析、API Key 轮换、session 管理）
5. 进入交互式 REPL，支持 tool calling

**关键 flags：**
- `--query/-q` — 单次查询（非交互模式）
- `--resume/-r` — 恢复指定 session
- `--continue/-c` — 恢复最近 session
- `--model/-m` — 覆盖模型
- `--provider` — 覆盖提供商
- `--skills/-s` — 预加载 skill
- `--yolo` — 跳过所有危险命令确认
- `--worktree/-w` — 隔离 git worktree 运行
- `--max-turns` — 单轮最大 tool 调用次数（默认 90）

### 2.2 `model` — 切换模型

交互式选择 provider + model 向导。与 setup 中的 model 模块共享代码路径。

### 2.3 `status` — 系统状态

显示所有组件状态：provider 配置、gateway 运行状态、内存服务等。

## 3. 网关/消息平台

### 3.1 `gateway` — 消息网关

支持 16 种平台（Telegram/Discord/Slack/微信/钉钉/飞书等）。

**子命令：** `run`, `start`, `stop`, `restart`, `status`, `install`, `uninstall`, `setup`

关键设计：支持安装为 systemd/launchd 服务，开机自启。

### 3.2 `webhook` — 事件驱动

通过 HTTP webhook 激活 agent，支持自定义事件过滤和 prompt。

**子命令：** `subscribe`, `list`, `remove`, `test`

### 3.3 `cron` — 定时任务

**子命令：** `list`, `create`, `edit`, `pause`, `resume`, `run`, `remove`, `status`, `tick`

支持通过 `--deliver` 将结果推送到消息平台。

## 4. 会话与数据管理

### 4.1 `sessions` — 会话历史

基于 SQLite 的会话存储。

**子命令：** `list`, `browse`, `export`, `delete`, `prune`, `stats`, `rename`

关键：`browse` 用 `os.execvp` 替换进程为 `hermes --resume <id>`。

### 4.2 `insights` — 使用分析

Token 用量、费用、工具使用模式、活跃趋势统计。

### 4.3 `backup/import` — 备份恢复

ZIP 格式备份所有配置、session、记忆数据。

## 5. 插件/技能系统

### 5.1 `skills` — 技能市场

支持多注册源（skills.sh, GitHub, ClawHub, lobehub）。

**子命令：** `browse`, `search`, `install`, `inspect`, `list`, `check`, `update`, `audit`, `uninstall`, `publish`, `config`, `tap add/remove/list`

### 5.2 `plugins` — Git 插件

**子命令：** `install`, `update`, `remove`, `list`, `enable`, `disable`

### 5.3 `memory` — 记忆系统

支持外部记忆 provider（honcho, mem0, hindsight 等）。

**子命令：** `setup`, `status`, `off`, `reset`

### 5.4 `tools` — 工具配置

**子命令：** `list`, `enable`, `disable`

按平台（cli/gateway/webhook）独立配置启用的 toolset。

### 5.5 `mcp` — MCP 服务器管理

**子命令：** `serve`, `add`, `remove`, `list`, `test`, `configure`

## 6. 诊断调试

| 命令 | 用途 |
|------|------|
| `doctor` | 自动诊断 + `--fix` 自动修复 |
| `dump` | 输出精简状态摘要（供支持/调试用） |
| `debug share` | 上传调试报告到 paste 服务 |
| `logs` | 查看/过滤日志（支持 follow、按 session/level/component 过滤） |

## 7. 多实例管理

### 7.1 `profile` — 配置隔离

每个 profile 拥有独立的 HERMES_HOME。

**子命令：** `list`, `use`, `create`, `delete`, `show`, `rename`, `export/import`, `alias`

## 8. 对 deepai 的设计启示

### 8.1 优先级划分

**P0 — 核心必须：**
- `chat` — 主交互命令（默认行为）
- `setup` — 首次配置 ✅ 已完成
- `version` — 版本信息 ✅ 已完成
- `config` — 查看/修改配置

**P1 — 重要：**
- `sessions` — 会话管理（list/resume/delete）
- `model` — 快速切换模型
- `status` / `doctor` — 诊断
- `memory` — 记忆系统管理

**P2 — 增值：**
- `skills` — 技能管理
- `tools` — 工具配置
- `mcp` — MCP 服务器管理
- `insights` — 使用统计
- `logs` — 日志查看

**P3 — 未来：**
- `gateway` — 消息平台网关
- `cron` — 定时任务
- `webhook` — 事件驱动
- `profile` — 多实例

### 8.2 关键设计模式

1. **默认命令模式** — 裸 `hermes` = `hermes chat`，降低使用门槛
2. **子命令自治** — 每个子命令自行确保前置条件（如目录初始化）
3. **交互/非交互双模式** — `--query` 单次查询、`--resume` 恢复 session
4. **分层配置** — 全局 flags 与子命令 flags 分离（`PersistentFlags` vs `Flags`）
5. **动态命令注册** — 插件可注册自己的 CLI 命令（如 honcho）
6. **配置与密钥分离** — `config.yaml` 存结构化配置，`.env` 存密钥

### 8.3 命令树建议（deepai）

```
deepai                          → deepai chat（默认）
deepai chat [--query|-q]        → 交互式 REPL / 单次查询
deepai setup [provider|model|database]  → 配置向导 ✅
deepai config [show|set|edit]   → 配置管理
deepai model                    → 切换模型
deepai sessions [list|browse|delete]    → 会话管理
deepai memory [setup|status|reset]      → 记忆系统
deepai status                   → 系统状态
deepai doctor                   → 诊断检查
deepai skills [list|install|search]     → 技能管理
deepai tools [list|enable|disable]      → 工具配置
deepai mcp [add|list|remove]            → MCP 管理
deepai version                  → 版本信息 ✅
deepai logs                     → 日志查看
```
