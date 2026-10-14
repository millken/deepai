# Figma 原生桥使用指南

> 让 deepai 直接读写 Figma 原生可编辑图层：改稿、打磨、出预览图，全程证据落盘。内嵌实现，无 Node/npm/第三方 CLI 依赖；需 Figma Desktop + 一次性导入开发插件。
> 架构借鉴 [local-figma](https://github.com/denki-san/local-figma)（本地桥接 + 插件轮询 + 证据落盘），代码为 deepai 自研最小实现。

## 一、与 Figma MCP 的关系

| | Figma MCP（见 [figma_mcp.md](figma_mcp.md)） | Figma 原生桥（本文） |
|---|---|---|
| 通道 | 云端 REST / 远程 MCP，PAT 或 OAuth | 本机回环 HTTP（127.0.0.1:43187）+ Figma Desktop 插件 |
| 能力 | 读（结构/变量/切图）；官方远程另有生成式写 | **原生写**：任意 Figma Plugin API 脚本 + 读 + PNG 预览 |
| 前提 | Figma PAT | Figma Desktop 打开目标文件，插件面板保持开启 |

两者互补：MCP 是跨文件的只读取数路径；原生桥是对**已绑定画板**的可信写路径。设计师工作流（改稿→打磨→定稿→模板）走原生桥，SKILL.md（figma-ui / figma-tailwind）已内置路由。

## 二、首次设置（一次性，约 1 分钟）

1. 在 Figma 中右键目标画板 → **Copy/Paste as → Copy link to selection**，得到含 `node-id` 的链接。
2. 把链接交给 deepai，或直接说「帮我连接这个 Figma 画板」。deepai 执行 `figma init <链接>` 后返回 manifest 路径，例如：
   ```
   ~/Library/Application Support/deepai/figma/plugin/manifest.json
   ```
3. Figma Desktop → **Plugins → Development → Import plugin from manifest**，选择该文件。
4. 在 **Plugins → Development** 菜单运行 **deepai Figma Bridge**，面板显示「已连接 deepai」即完成。

说明：

- 插件**导入一次长期有效**：配对凭证持久化在状态目录，deepai 重启后无需重导，插件面板重新打开即可重连。
- **本地 .fig 可直接用**：从云端「保存本地副本 .fig」后在 Figma Desktop 打开，同样能被桥接管——.fig 保留云端节点 ID，绑定校验靠 nodeId 兑底。本地副本与云端原件是同一画板的两份内容：请只在一处开插件面板，避免任务串入另一份。
- 使用期间保持插件面板开启、目标文件在前台（后台时轮询降频，任务回传可能延迟）。
- 换目标文件或画板：直接对 deepai 说新链接重新 init——桥空闲时自动热切换（内存绑定、插件下发与 binding.json 同步更新）；有在途任务或未决核实时拒绝切换并提示先处理。
- 端口 43187 被占用时报错会提示：本机若装过 figma-local CLI 并在运行，先停其一。

## 三、Agent 用法（内置 `figma` 工具）

**JS 即接口**：没有语义子命令，一切操作都是提交一段在 `(figma, target)` 作用域执行的 Figma Plugin API 脚本（`target` 为绑定画板或 `node` 指定的子树节点）。一次调用完成提交→等待→读回。

| action | 参数 | 说明 |
|---|---|---|
| `init` | `url` | 绑定画板链接，生成插件并返回 manifest 路径与导入指引 |
| `doctor` / `status` | — | 桥健康、插件连接新鲜度、当前任务与绑定信息 |
| `run` | `code`, `node?`, `inspect?`, `timeout?` | 提交 JS 并等待（默认 150s，上限 600s）；结果含 output、before/after 结构快照（超长截断标记）、`preview` PNG 绝对路径、证据目录；`inspect:true` 为只读核实通道（见 §四） |
| `result` | `job_id` | 读回任务证据（插件断线后依然可读） |
| `history` | — | 全部任务列表 |
| `ack` | `job_id`, `inspect_job_id`, `note` | 解除「结果未知」阻断（见 §四） |

标准片段：

```
读结构：   run {code:'return null'}        → before 快照即完整结构（Auto Layout/文字/填充/坐标）
出预览图： run {code:'return await target.exportAsync({format:"PNG"})'}  → preview.png 路径
改文字：   run {code:'const t=target.findOne(n=>n.name==="标题"); await figma.loadFontAsync(t.fontName); t.characters="新文案"; return t.id'}
```

完整配方（含变量名的设计取数、token 表、SVG 导出、构建辅助、variables 绑定、组件变体、交付体检）见 `.deepai/skills/figma-ui/references/bridge-recipes.md`。

写文字前必须 `await figma.loadFontAsync(...)`；大画板用 `node` 缩小目标子树（快照上限 32 层/4000 节点，超出标记 truncated，不拒绝执行）。每次脚本前后自动划分原生 undo 边界，Figma 中一次 Ctrl+Z 即可回退单次脚本。

## 四、恢复纪律（重要）

- **任务单飞**：同一时刻只有一个在途任务；未读回结果前提交新任务会被拒绝。
- **绝不重放**：等待超时≠失败（任务未被取消）。用 `result <job_id>` 读回，或稍后再等；不要重复提交同一修改。
- **结果未知阻断**：任务已交付但 deepai 中断、结果未落盘时，该任务进入 unresolved 并阻断一切新写入（普通 run 一律被拒）。解锁流程：
  1. `run`（`inspect:true`）提交一段只读脚本（如 `return null`）核实文档现状，等待其完成；
  2. `ack` 附上该核实任务 ID 与观察结论：`figma ack <job_id> <inspect_job_id> <核实说明>`（如「inspect 确认仅按钮文字变化，无其他改动」）；核实任务未完成时 ack 被拒，两份证据落盘后才解除阻断。
- 证据目录（`~/Library/Application Support/deepai/figma/runs/<job_id>/`）保存每次任务的 script.js、result.json、preview.png，可随时人工检视。
