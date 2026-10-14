---
name: figma-ui
description: "Use when a Figma link needs full UI code — 实现设计/设计比对/导出素材/写回 Figma/从零探索. For Tailwind CSS v4 class strings only use figma-tailwind instead."
---

# Figma UI Assistant

Turn Figma designs into faithful UI code, iterate on designs in both directions
(Figma ↔ code), and verify the result visually. Figma data is ground truth —
never guess values that the file can tell you.

## When to use

- A `figma.com/design/<fileKey>/...` link appears (any `?node-id=` scopes it)
- "Implement / 还原 / 按这个设计写" a screen, component, or icon
- "Adjust the design" / "update the Figma file" / "add a variant in Figma"
- "Check my implementation against the design" (visual diff)
- "Export this icon/logo/frame" as SVG/PNG
- Design exploration from zero ("mock up a dashboard", with Figma as canvas)

## When NOT to use

- Tailwind CSS v4 class-string-only extraction from a Figma design (any
  target — web, 小程序/UniApp) → use `figma-tailwind` (its red lines forbid
  the full-code workflows this skill performs). Primary routing lives in the
  catalog descriptions; this section is the fallback once this skill is
  already loaded.

## Data source: which path am I on? (先判路径,再选工具)

两条路径,优先级:**原生桥 > 官方远程**。原生桥可用(已绑定 + 插件面板开着)时永远首选——读写走同一实况文档,证据落盘。

1. **原生桥**(builtin `figma` 工具):`action=status` 返回 `connected:true` 即可用;绑定与插件导入见 docs/figma_bridge.md。
2. **官方远程 MCP**(mcp.figma.com,41 tools):`~/.deepai/mcp.json` 配有该 url 时可用;401 时跑 `scripts/figma-mcp-token.py ensure` 续期后重试。

> 社区版 figma-developer-mcp 已从 skills 移除,以官方为准。识别到环境只配了社区版(mcp.json 里 npx figma-developer-mcp)时**不要静默当官方用**:提示停用社区版,按 docs/figma_mcp.md §三 配置官方远程,或改走原生桥。

URL 解析(通用):`fileKey` = `/design/` 后段;`?node-id=1-2` → `1:2`(破折号换冒号)。

## Tool map by path

| Goal | 原生桥(builtin `figma`,首选) | 官方远程 MCP(完整工具名带 `figma__` 前缀) |
|---|---|---|
| 结构总览 | `run` {code:'return null'} → before 快照 | `figma__get_metadata` |
| 设计→代码数据 | `run` 读节点树(大画板分块) | `figma__get_design_context` |
| Tokens | `run` 读 `getLocalPaintStylesAsync` / `getLocalTextStylesAsync`(样式≈token 近似,无变量名→按 figma-tailwind 降级) | `figma__get_variable_defs`(变量唯一依据) |
| 截图/视觉基准 | `run` exportAsync → preview.png | `figma__get_screenshot` |
| 资产导出 | `run` exportAsync(SVG 图标/PNG 2x) | `figma__download_figma_images` |
| 写 Figma | `run`(确定性 JS,单飞+证据) | `figma__use_figma`(无 Desktop 时) |
| 生成式草稿 | ❌ | `figma__generate_figma_design` |
| 新文件/传素材 | ❌ | `figma__create_new_file` / `figma__upload_assets` |
| 设计系统/动效 | ❌ | `figma__search_design_system` / `figma__get_motion_context` |

## Workflow A — implement a design (design → code)

1. 结构与样式走首选路径取数;大画板按 section 分块,不一次拉全树。
2. Tokens 优先(变量/样式名 → `--color-primary` 等语义名),不要魔法数。
3. 实现。图标用 SVG 内联(super-frontend-design 的图标规则)。
4. 验证闭环:设计侧导出基准 PNG + 实现侧同视口截图 → 逐层比对
   (全图差异% → 分区统计 → 关键色块包围盒几何),不要只看百分比。
   差异可能在任一侧:代码 bug 或 Figma 构建缺陷(见 gotchas),按用户意图修对应侧。
5. 动效:仅官方远程 `figma__get_motion_context` → CSS,尊重 `prefers-reduced-motion`。

## Workflow B — adjust the design in Figma (code → design)

原生桥路径(首选,要求 Figma Desktop + 已绑定):

1. `doctor`/`status` 确认连接;`run` {code:'return null'} 先读结构。
2. 小步编辑:一次 `run` 一个有界变更;写文字前 `await figma.loadFontAsync`;
   改前重读、改后读 output/before/after 快照 + 新鲜 PNG,报告视觉结果而非 "done"。
3. 恢复纪律:任务单飞、绝不重放。超时先 `result` 读回;delivered 无结果的
   任务会阻断新写入,直到完成的 inspect:true `run` + `ack(job_id,
   inspect_job_id, note)` 记录核实结论。断连超时按提示区分:queued=面板
   没开(重开后自动执行),delivered=执行中失联(重开后待回传)。

官方远程回退(无 Figma Desktop):`figma__use_figma` 小步 JS 编辑,改前重读;
`figma__generate_figma_design` 只用于生成式草稿,不做手术式修改。

## Workflow C — verify / iterate an existing implementation

1. 同视口宽度截两图:设计节点 PNG + 运行页面截图。
2. 比对定位差异 → 按用户意图修代码或修设计(Workflow B)。
3. 再截图闭环;汇报遗留的、有意为之的偏差。

## Workflow D — design from scratch

1. 意图不明先 ask_clarification(用途/调性/框架);**新设计先提 2 个结构方向**(文字或
   线框)供用户选定,再动手构建。
2. 参考图:仅官方远程 `figma__generate_image` 或跳过(纯排版方向)。
3. 画布:官方远程 `figma__create_new_file` + `figma__use_figma`;或原生桥 `figma init`
   绑定目标 frame 后 `run` 构建(本机 Desktop 场景更顺手);也可先代码原型后移植。
4. 先 `figma__search_design_system` 再造组件;tokens 走 `figma__get_variable_defs`。
5. 截图迭代;交付文件 URL。

## Bridge gotchas(原生桥实战踩坑,全部实测验证)

- **dynamic-page 模式**:跨页读前必须 `await figma.loadAllPagesAsync()`;
  `node.mainComponent` 直接读会抛,用 `await node.getMainComponentAsync()`。
- **字体**:沙箱/容器里 PingFang SC 等系统字体可能不可加载;先
  `await figma.listAvailableFontsAsync()` 探测(注意 "Alegreya SC" 这类拉丁
  小型大写字体会干扰关键词匹配——用精确字体名列表查,不要模糊正则)。
- **auto-layout 塌陷(最大坑)**:`resize()` 后开 `layoutMode`,或容器未显式
  声明尺寸模式,会被重置为 AUTO 收缩(实测整屏 844px 塌到 154px 且 ok:true
  静默通过)。构建容器时显式设 `primaryAxisSizingMode`/`counterAxisSizingMode`
  再 resize;构建后必读 after 快照的 `layout.sizing` 确认。
- **对齐语义**:VERTICAL 布局里 `primaryAxisAlignWidgets` 是垂直方向,水平
  居中要看 counter 轴;两者都不生效时(实测发生过)用子级
  `layoutAlign='CENTER'`,仍不行就 `layoutMode='NONE'` + 绝对定位兜底。
- **多行文本**:`textAutoResize='HEIGHT'` + `resize(定宽, 初始行高)` 实现固宽换行撑高。
- **返回值**:`exportAsync` 的 Uint8Array 可直接 return(走图像通道落盘
  preview*.png);嵌套在对象里返回则输出 `{__png,bytes}` 占位。不要把大
  字节数组塞进普通 JSON 字段。
- **本地 .fig**:无 fileKey,绑定校验靠 nodeId 兜底(放行);本地副本与云端
  原件勿同时开插件面板,避免任务串入另一份。

## Hard rules

- Never invent image URLs — only ones returned by the tools.
- Never guess values a tool can answer (spacing, color, radius, font).
- 官方远程:one `figma__get_design_context` per node subtree,相关节点合并调用(限流)。
- 写 Figma:确定性编辑(`run` / `figma__use_figma`)优先于生成式,除非用户要想法。
- UI aesthetics/iconography rules come from super-frontend-design; this skill
  supplies the ground-truth data. Compose both when building UI.
- 401(官方远程)→ `scripts/figma-mcp-token.py ensure` 续期后重试
  (docs/figma_mcp.md §三)。
- 原生桥任务单飞、绝不重放;结果未知时先读回——绝不重新提交同一修改。
- ack 不是橡皮章:必须附完成的 inspect:true 任务 ID + 实际观察到的文档现状说明。
