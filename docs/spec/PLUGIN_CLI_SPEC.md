# Spec: `deepai plugin` 命令（插件安装/管理 CLI）

## 背景与现状

2a/2b/2c 让 deepai 能加载 `~/.deepai/plugins/` 与 `<project>/.deepai/plugins/` 下的插件（skills/mcp/agents/commands）。但**没有安装命令**——目前只能手动 `git clone` / `ln -s` 进扫描目录，且：

- 名字/路径要自己拼对；
- clone 进来的目录是否真是插件（含 `.claude-plugin/plugin.json`）要自己验证；
- Claude **marketplace** 多插件仓库 clone 进去会被一层扫描漏掉（插件在二层）。

本 spec 加一个最小 `deepai plugin` CLI，把"单插件安装"变成一条命令，并显式把 marketplace 排除在外。

## 目标 / 非目标

**目标**

- `deepai plugin install <git-url>` —— clone 到扫描目录，自动校验是合法插件。
- `deepai plugin add <本地路径>` —— 软链本地插件目录（开发用）。
- `deepai plugin list` —— 列出已发现插件 + 来源 + 是否有效。
- `deepai plugin remove <name>` —— 移除已装插件（clone 或软链）。

**非目标（显式排除）**

- **marketplace.json 解析 / 仓库级安装**：多插件仓库直接装不在本轮。只提供 `--subdir` 作为单插件抽取的逃生口（见下）。
- 版本锁定 / `plugin update` / `plugin enable|disable` 配置持久化。
- 插件签名 / 信任校验（与 MCP 一致，用户自担风险）。

## 扫描路径与作用域

- 全局：`~/.deepai/plugins/`（**默认**作用域）。
- 项目：`<cwd>/.deepai/plugins/`（`--project` 指定）。
- 命令操作的目标目录由此决定；`list` 同时反映两层。

## 子命令

### `plugin install <git-url> [--name <name>] [--project] [--force] [--subdir <path>]`

1. 名字：`--name` 给定，否则取 URL 仓库 basename（去 `.git`）。
2. 目标目录：`<scope>/plugins/<name>`（scope = `~/.deepai` 或 `<cwd>/.deepai`）。
3. 已存在 → 报错；`--force` 先删除再装。
4. `git clone --depth 1 <url> <staging>`（浅克隆到临时目录；git 不在 PATH → 清晰报错）。
5. `--subdir <path>`：clone 后取仓库内 `<path>` 子目录作为插件根（marketplace 单插件逃生口）；校验该子目录。
6. 校验 staging 是合法插件（`.claude-plugin/plugin.json` 存在、可解析、有 `name`）；不合法 → 删 staging + 报错，**不污染**目标目录。
7. 合法 → 把 staging 移到目标目录（`--subdir` 时只移子目录）。
8. 打印结果：`installed <name> -> <dir>` 或 `installed <name> (subdir <path>) -> <dir>`。

### `plugin add <本地路径> [--name <name>] [--project] [--force]`

1. 解析 `<本地路径>` 为绝对路径；名字同上（默认取路径 basename）。
2. 校验该路径是合法插件（同上）。
3. 在目标目录创建**符号链接**：`<scope>/plugins/<name>` → `<绝对路径>`。已存在 → 报错；`--force` 覆盖。
4. 开发场景：源目录改了即生效（软链）。

### `plugin list [--project]`

逐目录扫描两层（`~/.deepai/plugins/` 全局、`<cwd>/.deepai/plugins/` 项目），对每个直接子项调 `claudeplugin.LoadPlugin` 校验，打印 `name  <scope>  <dir>  (status)`——`status` 为 `ok` 或 `invalid: <原因>`。这样能看到**所有已装条目**，包括被项目级覆盖的全局插件和清单损坏的插件（管理视角比"生效集"更有用）。不聚合去重，不展示覆盖关系。

### `plugin remove <name> [--project]`

1. 目标 = `<scope>/plugins/<name>`。
2. **安全**：解析后必须是 `<scope>/plugins/` 的直接子项（`filepath.Rel` 校验，拒 `..`、拒软链逃逸到区外）；不满足 → 报错不动。
3. 是软链 → 只删软链（不动源目录）；是普通目录 → `os.RemoveAll`。
4. 不存在 → 报错。

## 名字校验

插件名（`--name` 或 basename）须匹配 `^[a-z0-9][a-z0-9-]*$`（与 plugin.json `name` 规范一致）；不合法 → 报错（不静默改成别的）。

## 失败处理

- git clone 失败 / 目标已存在无 `--force` / 名字非法 / 校验不过 / remove 路径越界 → 非零退出码 + 清晰错误，**不留半成品**（staging 清理、目标不被污染）。

## 代码改动点

1. **新增** `pkg/commands/plugin.go`：
   - `PluginCommand(topLevel *cobra.Command)` 注册 `plugin` 及四个子命令。
   - `install/runInstall`、`add/runAdd`、`list/runList`、`remove/runRemove`。
   - 辅助：`scopeDir(project bool) string`、`pluginNameFromURL(url) string`、`validatePluginDir(dir) error`、`safeRemove(base, name) error`。
2. **改** `pkg/claudeplugin/loader.go`：导出 `LoadPlugin(dir string) (Plugin, problem string)`（即现有 `loadPlugin` 的导出别名），供 CLI 与发现共用校验；加 `Plugin.Version()`、`Plugin.Description()` 访问器（list 展示用）。
3. **改** `pkg/commands/commands.go`：`AddCommands` 注册 `plugin`。
4. **文档**：README 补 `deepai plugin` 用法 + marketplace 的 `--subdir` 逃生口说明。

## 安全模型

- `install` 执行 `git clone`（用户权限），与手动 clone 等价；插件内 MCP/agent/命令的代码执行仍受 deepai 既有工具/沙箱约束。
- `add`/`remove` 限定在 `<scope>/plugins/` 内，`filepath.Rel` 拒绝越界；`remove` 软链只删链接。
- 不做来源信任校验（与 MCP 一致）；`list` 显示来源路径供用户自查。

## 测试计划

- `pkg/commands/plugin_test.go`：
  - `validatePluginDir`：合法插件通过；缺 manifest / 坏 JSON / 缺 name → 报错。
  - `add`：软链创建 + 校验失败不创建 + `--force` 覆盖；软链只删链接。
  - `remove`：`safeRemove` 拒绝 `..` 与区外路径；删软链不动源；删普通目录。
  - `list`：用 temp 目录构造全局+项目插件，断言输出含名字与 problems。
  - `install` 的 git 部分用 fake 仓库目录验证 staging→校验→移入流程（git clone 本身用冒烟覆盖）。
  - `pluginNameFromURL`：各种 URL 形态 → basename。
- 冒烟（实现后）：`deepai plugin install <真实单插件仓库>` → `deepai plugin list` 可见 → 启动 REPL 确认组件加载。

## 决策（已定）

1. **默认作用域**：全局 `~/.deepai/plugins/`（"装一次、多项目可用"心智）；`--project` 显式切项目级。
2. **`--subdir`**：首轮就做——低成本，明显缓解多插件仓库可用性。
3. **`remove`**：不交互确认，直接删（CLI 自动化场景；靠 `safeRemove` 边界校验兜底）。
4. **`plugin update`**：首轮**不做**（git 状态/非 git 源/软链源/subdir 源分支太多，超出最小可用目标）。
