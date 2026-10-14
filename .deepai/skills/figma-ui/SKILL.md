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

三条路径,优先级:**原生桥 > 官方远程 > 社区版**。原生桥可用(已绑定 + 插件面板开着)时永远首选——读写走同一实况文档,证据落盘。

```sh
grep -A3 '"figma"' ~/.deepai/mcp.json
# "url": "https://mcp.figma.com/mcp"      → 官方远程(41 tools,OAuth)
# "command": "npx" + figma-developer-mcp  → 社区版(2 tools,PAT,只读)
# 无 figma 条目                            → 只剩原生桥
```

原生桥可用性:`figma` action=`status`,`connected:true` 即可用;绑定/导入插件见 docs/figma_bridge.md。

## Tool map by path

| Goal | 原生桥(builtin `figma`,首选) | 官方远程 MCP | 社区版 MCP |
|---|---|---|---|
| 结构总览 | `run` {code:'return null'} → before 快照 | `get_metadata` | `get_figma_data` |
| 设计→代码数据 | `run` 读节点树(大画板分块) | `get_design_context` | `get_figma_data` |
| Tokens | `run` 读 `getLocalPaintStylesAsync` / `getLocalTextStylesAsync` | `get_variable_defs` | 降级:实测 hex |
| 截图/视觉基准 | `run` exportAsync → preview.png | `get_screenshot` | 无(用响应内嵌图) |
| 资产导出 | `run` exportAsync(SVG 图标/PNG 2x) | `download_figma_images` | `download_figma_images` |
| 写 Figma | `run`(确定性 JS,单飞+证据) | `use_figma`(无 Desktop 时回退) | ❌ 无写路径 |
| 生成式草稿 | ❌ | `generate_figma_design` | ❌ |
| 新文件/传素材 | ❌ | `create_new_file` / `upload_assets` | ❌ |
| 设计系统/动效 | ❌ | `search_design_system` / `get_motion_context` | ❌ |

URL 解析(所有路径通用):`fileKey` = `/design/` 后段;`?node-id=1-2` → `1:2`(破折号换冒号)。

## Workflow A — implement a design (design → code)

1. 结构与样式走首选路径取数;大画板按 section 分块,不一次拉全树。
2. Tokens 优先(变量/样式名 → `--color-primary` 等语义名),不要魔法数。
3. 实现。图标用 SVG 内联(super-frontend-design 的图标规则)。
4. 验证闭环:设计侧导出基准 PNG + 实现侧同视口截图 → 逐层比对
   (全图差异% → 分区统计 → 关键色块包围盒几何),不要只看百分比。
   差异可能在任一侧:代码 bug 或 Figma 构建缺陷(见 gotchas),按用户意图修对应侧。
5. 动效:`get_motion_context`(官方远程)→ CSS,尊重 `prefers-reduced-motion`。

## Workflow B — adjust the design in Figma (code → design)

原生桥路径(首选,要求 Figma Desktop + 已绑定):

1. `doctor`/`status` 确认连接;`run` {code:'return null'} 先读结构。
2. 小步编辑:一次 `run` 一个有界变更;写文字前 `await figma.loadFontAsync`;
   改前重读、改后读 output/before/after 快照 + 新鲜 PNG,报告视觉结果而非 "done"。
3. 恢复纪律:任务单飞、绝不重放。超时先 `result` 读回;delivered 无结果的
   任务会阻断新写入,直到完成的 inspect:true `run` + `ack(job_id,
   inspect_job_id, note)` 记录核实结论。断连超时按提示区分:queued=面板
   没开(重开后自动执行),delivered=执行中失联(重开后待回传)。

MCP 回退路径(无 Figma Desktop,官方远程):`use_figma` 小步 JS 编辑,改前重读;`generate_figma_design` 只用于生成式草稿,不做手术式修改。

## Workflow C — verify / iterate an existing implementation

1. 同视口宽度截两图:设计节点 PNG + 运行页面截图。
2. 比对定位差异 → 按用户意图修代码或修设计(Workflow B)。
3. 再截图闭环;汇报遗留的、有意为之的偏差。

## Workflow D — design from scratch

1. 意图不明先 ask_clarification(用途/调性/框架)。
2. 参考图:`generate_image`(官方远程)或跳过(纯排版方向)。
3. 画布:官方远程 `create_new_file` + `use_figma`;或原生桥 `figma init`
   绑定目标 frame 后 `run` 构建(本机 Desktop 场景更顺手);也可先代码原型后移植。
4. 先 `search_design_system` 再造组件;tokens 走 `get_variable_defs`。
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

## Hard rules

- Never invent image URLs — only ones returned by the tools.
- Never guess values a tool can answer (spacing, color, radius, font).
- 官方远程:one `get_design_context` per node subtree,相关节点合并调用(限流)。
- 写 Figma:确定性编辑(`run` / `use_figma`)优先于生成式,除非用户要想法。
- UI aesthetics/iconography rules come from super-frontend-design; this skill
  supplies the ground-truth data. Compose both when building UI.
- 401 排查按路径:官方远程 → `scripts/figma-mcp-token.py ensure` 后重试;
  社区版 → `FIGMA_API_KEY` 失效,figma.com → Settings → Security 重新生成。
- 原生桥任务单飞、绝不重放;结果未知时先读回——绝不复述提交同一修改。
- ack 不是橡皮章:必须附完成的 inspect:true 任务 ID + 实际观察到的文档现状说明。
