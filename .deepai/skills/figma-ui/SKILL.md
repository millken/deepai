---
name: figma-ui
description: "Use when a Figma link needs full UI code, or when designing in Figma — 实现设计/设计比对/导出素材/在 Figma 搭建或改稿/建 variables 与组件/从零探索. For Tailwind CSS v4 class strings only use figma-tailwind instead."
---

# Figma UI Assistant

Turn Figma designs into faithful UI code, build and iterate designs in Figma
that convert cleanly to code (Figma ↔ code), and verify the result visually.
Figma data is ground truth — never guess values that the file can tell you.

参考文件(按需读):
- [bridge-recipes.md](references/bridge-recipes.md) — 原生桥 JS 配方:取数(含变量名)、token 表、SVG 导出、构建辅助、variables 绑定、按规格构建组件、交付体检、**量测验收**
- [component-specs.md](references/component-specs.md) — Button/Input 尺寸规格与验收不变量(搭控件前必读)
- [tailwind-v4-map.md](../figma-tailwind/references/tailwind-v4-map.md) — Figma 属性 → Tailwind v4 映射与刻度(目标项目用 Tailwind 时必读;非 Tailwind 项目也可作为 CSS 语义对照)

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

## Figma 设计端规范(B/D 搭建时遵守,也是 A 取数质量的前提)

- **Auto Layout**：容器、卡片、页面全部使用，并正确设置 Hug / Fill / Fixed；绝对定位仅限角标、浮层。不要 Detach 实例。
- **命名**：英文语义命名（`UserProfileCard`），禁止 `Rectangle 1`、`Group 3` 等默认名；图标统一 `Icon/xxx`，且为单一矢量。
- **组件与变体**：按钮、输入框、标签等必须是 Component，变体属性与代码 props 同名（`variant`、`size`、`state`：default / hover / active / focus / disabled）。布尔差异用 Boolean Property，内容差异用 Text / Instance Swap Property。
- **Variables / Styles**：颜色、圆角、阴影、字体、间距全部绑定变量，不用裸值。命名对应 `@theme` 命名空间，如 `color/primary/500` → `--color-primary-500` → `bg-primary-500`；`radius/md` → `rounded-md`；`shadow/card` → `shadow-card`。
- **栅格**：间距基于 4px（4、8、12、16、24、32…）；控件内部由规格推出的 2px 倍数（如按钮垂直内边距 6/10）除外。
- **控件按规格**：Button/Input 尺寸取 [component-specs.md](references/component-specs.md) 档位，内容靠 auto-layout 居中，禁止手摆文字坐标。
- **文本框自适应**：单行 `WIDTH_AND_HEIGHT`，段落定宽 `HEIGHT`；需要截断时设 `textTruncation` + `maxLines`。
- **响应式**：提供 Mobile / Desktop 两套 Frame，或标注断点行为。

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
| 设计→代码数据 | `run` 取数脚本(recipes §1,大画板传 `node` 分块) | `figma__get_design_context` |
| Tokens | `run` token 表(recipes §2,variables + styles,含 mode 值与 codeSyntax) | `figma__get_variable_defs`(变量唯一依据) |
| 截图/视觉基准 | `run` exportAsync → preview.png | `figma__get_screenshot` |
| 资产导出 | `run` exportAsync(recipes §3:`SVG_STRING` 图标 / PNG 2x) | `figma__download_figma_images` |
| 写 Figma | `run`(确定性 JS,单飞+证据;recipes §4–6) | `figma__use_figma`(无 Desktop 时) |
| 交付体检 | `run` design lint(recipes §7) | ❌(用 `get_metadata` 人工核查) |
| 量测验收 | `run` 验收脚本(recipes §8) | ❌(用 `get_metadata` 坐标逐项核算) |
| 生成式草稿 | ❌ | `figma__generate_figma_design` |
| 新文件/传素材 | ❌ | `figma__create_new_file` / `figma__upload_assets` |
| 设计系统/动效 | ❌ | `figma__search_design_system` / `figma__get_motion_context` |

`before` 快照只含尺寸、填充 hex、基础 auto-layout,**不含描边/阴影/行高/字距/FILL·HUG/变量名**——写代码取数一律用 recipes §1,不要拿快照凑。

## Verification gate(验收闸门:量测数字才是证据)

**你看不到 preview.png。** deepai 的工具结果只带图片路径,像素不进模型;「改后看 PNG 确认无误」在这里等于没验证。脚本 `ok:true` 也只说明 JS 没抛错,不说明布局正确。
因此任何设计产出都走「规格 → 构建 → 量测 → 修正」闭环:

1. **先定规格**:动手前写出本次要用的尺寸表(高度/内边距/字号/行高/圆角/间距),取自文件已有组件或 [component-specs.md](references/component-specs.md);不现场目测取值。
2. **按规格构建**:控件用 recipes §6 的规格化构建(高度 = 行高 + 2×垂直内边距,auto-layout 居中),文字用 §4 `label`(自动选中文字体、显式像素行高)。
3. **量测**:每次写入后对改动子树跑 recipes §8;它检查控件几何居中、内边距对称、字号/高度比、折行、溢出、
   文字重叠、近似未对齐、胶囊收缩/居中(pillCollapsed/pillOffY)、图标栈居中(stackOffX)、图标矢量畸变
   (iconBBox)、对比度、中日韩字体、变体一致性。先看结果里的 `checked`:构建了控件却 `controls:0`
   (命名未识别)或 `truncated:true` 时,`pass` 不构成证据;页面级结构问题另由 recipes §7 体检覆盖。
4. **修正到通过**:`errors` 逐条修(通常是按规格重建,而非挪坐标)并重跑同一子树,直到 `pass:true`
   (页面级再加 §7 归零);3 轮不过则如实报告。
5. **交付报告**:附通过的验收 job_id、`controls` 实测表、未消除的 `warn` 及理由,并把 preview.png 路径交给用户目视确认。没有通过的验收结果,不得说「已完成 / 已对齐 / 效果良好」。

## Workflow A — implement a design (design → code)

1. **先读项目,再读设计**:确认框架与样式方案(package.json、`@import "tailwindcss"` 即 v4、CSS Modules 等)、
   现有 token 文件、现有组件库。设计里的 INSTANCE(取数输出 `component` + `props`)优先映射到已有组件,
   props 直接对应变体属性(`variant=secondary` → `variant="secondary"`);官方远程可先查
   `figma__get_code_connect_map`(若工具列表中有)。
2. **取数**:`get_metadata` / `return null` 看全貌 → 按 section 分块取详细数据(官方远程每个子树一次
   `get_design_context`;原生桥 recipes §1 传 `node`)。同时导出该区块 PNG 作为对照基准。
3. **Tokens 优先**:变量名 → 项目语义 token(`color/brand/primary` → `--color-brand-primary`);
   项目已有同值 token 时复用,不另起名;无绑定的原始值才落刻度,不写魔法数。
4. **结构翻译**(核心规则,细节见 tailwind-v4-map §1–3):
   - Auto Layout → flex/grid;`FILL` → 弹性(`flex: 1` / stretch),`HUG` → 内容自适应,`FIXED` → 固定尺寸;
     屏幕级宽度(375/390/1440)是样稿尺寸,实现用流式宽度 + max-width。
   - Figma 默认 MIN 对齐,CSS flex 默认 stretch——对齐方式显式写出。
   - `absolute` 只用于角标/浮层/装饰;父级无 auto-layout 的区块按坐标推断行列,不要整片绝对定位。
   - `asset:'svg'` 的节点导出 SVG 内联(recipes §3),单色图标改 `currentColor`;位图导出 PNG 2x。
   - 文本按角色选语义标签(标题层级、`button`、`label`、`a`),不按字号猜;`maxLines` → 截断/行数限制。
   - 同一组件的 Mobile/Desktop 两张画板 → 一套响应式代码(断点),不要写两份。
   - `get_design_context` 的输出是参考代码:改写进项目的框架、组件与 token,不要原样粘贴。
5. **状态与可访问性**:组件集里的 hover/pressed/disabled/focus 变体映射为交互状态;补 `focus-visible`、
   图片 alt、表单 label;正文对比度不足时告知用户(不擅自改色)。
6. **验证闭环(量测对量测)**:设计侧用 recipes §1/§8 取关键元素实测值(尺寸、内边距、字号/行高、
   相对位置),实现侧在浏览器里量同一批元素(如 Playwright `getBoundingClientRect` + `getComputedStyle`),
   逐项对数字,差异 > 1px 必须解释或修正。像素截图比对(全图差异% → 分区统计 → 关键色块包围盒)
   仅在能用程序算出差异时使用,不要凭「看截图」下结论。
   差异可能在任一侧:代码 bug 或 Figma 构建缺陷(见 gotchas),按用户意图修对应侧。
7. 动效:仅官方远程 `figma__get_motion_context` → CSS,尊重 `prefers-reduced-motion`。

图标/插画规则与整体审美来自 super-frontend-design;本 skill 提供 ground-truth 数据,两者组合使用。

## Workflow B — adjust the design in Figma (code → design)

原生桥路径(首选,要求 Figma Desktop + 已绑定):

1. `doctor`/`status` 确认连接;`run` {code:'return null'} 先读结构;陌生文件先跑 recipes §2(已有
   variables/styles)与 §7(体检),**复用文件已有的 token、text style、组件**,不平行再造一套。
2. 小步编辑:一次 `run` 一个有界变更(一个区块/一个组件);写文字前 `await figma.loadFontAsync`;
   新值绑定 variable / style 而不是写死 hex;改后对改动子树跑 recipes §8 验收,按 Verification gate 报告。
3. 恢复纪律:任务单飞、绝不重放。超时先 `result` 读回;delivered 无结果的
   任务会阻断新写入,直到完成的 inspect:true `run` + `ack(job_id,
   inspect_job_id, note)` 记录核实结论。断连超时按提示区分:queued=面板
   没开(重开后自动执行),delivered=执行中失联(重开后待回传)。

官方远程回退(无 Figma Desktop):`figma__use_figma` 小步 JS 编辑,改前重读;
`figma__generate_figma_design` 只用于生成式草稿,不做手术式修改。

## Workflow C — verify / iterate an existing implementation

1. 同视口宽度取两侧量测:设计侧 recipes §1/§8,实现侧浏览器 DOM 量测(截图留给用户目视)。
2. 逐项对数字定位差异 → 按用户意图修代码或修设计(Workflow B)。
3. 再量测闭环;汇报遗留的、有意为之的偏差(附数字)。

## Workflow D — design from scratch

1. 意图不明先 ask_clarification(用途/调性/框架/目标端);**新设计先提 2 个结构方向**(文字或
   线框)供用户选定,再动手构建。
2. 参考图:仅官方远程 `figma__generate_image` 或跳过(纯排版方向)。
3. 画布:原生桥让用户建一个空画板(移动端 390×844 / 桌面 1440×900)并 `figma init` 绑定,所有节点建在
   `target` 内;或官方远程 `figma__create_new_file` + `figma__use_figma`;也可先代码原型后移植。
4. **按 token → 组件 → 页面的顺序搭建**(每步一次 `run`,每步跑 recipes §8 验收到 `pass:true` 再进下一步):
   - Tokens:recipes §5 建 variables(语义色 + 4px 间距刻度 + 圆角,需要暗色时加 Dark mode),设
     `codeSyntax.WEB`;文字建 text style(字阶 ≤ 6–7 档)。官方远程先 `figma__search_design_system` 复用。
   - 组件:先按 component-specs.md 写出尺寸表,再用 recipes §6 规格化构建 Button/Input 等组件集,变体命名
     `属性=值`,文案用 TEXT 组件属性;**组件验收通过后才允许在页面里使用**。
   - 页面:recipes §4 辅助按区块搭建(Header → 内容区 → 底部),区块内放组件实例,值全部绑定 token。
5. 交付前对整屏跑 recipes §7 体检(修到 `noAutoLayout`/`group`/`rawColor`/`rawText` 归零或说明原因)
   与 §8 验收(`pass:true`);交付文件 URL + 已建 tokens/组件清单 + 验收报告(Verification gate 第 5 条)。

## Bridge gotchas(原生桥实战踩坑,全部实测验证)

- **dynamic-page 模式**:跨页读前必须 `await figma.loadAllPagesAsync()`;
  `node.mainComponent` 直接读会抛,用 `await node.getMainComponentAsync()`;
  样式同理用 `getStyleByIdAsync` / `setTextStyleIdAsync` 等异步版本。
- **字体**:沙箱/容器里 PingFang SC 等系统字体可能不可加载;先
  `await figma.listAvailableFontsAsync()` 探测(注意 "Alegreya SC" 这类拉丁
  小型大写字体会干扰关键词匹配——用精确字体名列表查,不要模糊正则)。
  中文文字用 Inter 等拉丁字体时 Figma 回退渲染,字形基线与行框错位,表现为按钮文字偏上/偏下;
  字重样式名因字体而异(`Semi Bold`/`SemiBold`/`Semibold`)。recipes §4 `label` 已处理这两点。
- **auto-layout 塌陷(最大坑)**:`resize()` 后开 `layoutMode`,或容器未显式
  声明尺寸模式,会被重置为 AUTO 收缩(实测整屏 844px 塌到 154px 且 ok:true
  静默通过)。顺序固定为 `layoutMode` → `appendChild` → `resize(显式尺寸)` →
  `layoutSizingHorizontal/Vertical`(`resize()` 会把改动的轴锁回 FIXED);按内容撑开的轴在子节点加入后再设 HUG。
  构建后必读 after 快照的 `layout.sizing` 确认。
- **FILL 时机**:`layoutSizing*='FILL'` 只能在节点已 append 进 auto-layout 父级之后设置,否则抛错(新建空画板默认非 auto-layout,先 recipes §4 `ensureAuto`);
  `HUG` 只对 auto-layout 容器与文字有效。
 - **对齐语义**：控件居中首选正解是容器对齐属性——HORIZONTAL 容器的垂直居中、VERTICAL 容器的水平居中都是
   `counterAxisAlignItems='CENTER'`，主轴用 `primaryAxisAlignItems`（实测批量设 102 个容器全部生效，偏差归零）。
   字段名写错（如 `*AlignWidgets`）赋值不报错、读回 undefined，构建 ok 但视觉全错——写完读回确认。
   仅当字段名正确仍不生效（罕见）或 `layoutMode='NONE'` 场景才用兜底：双 `layoutGrow=1` spacer / 文字
   `layoutAlign='STRETCH'`+`textAlignVertical='CENTER'`，详见 recipes §6。
- **多行文本**:`textAutoResize='HEIGHT'` + `resize(定宽, 初始行高)` 实现固宽换行撑高;
  在 auto-layout 里则 `textAutoResize='HEIGHT'` 后设 `layoutSizingHorizontal='FILL'`。
- **返回值**:预览图让 `return` 的值本身就是 `exportAsync` 的 Uint8Array(走图像通道落盘
  preview.png)。较新插件会把嵌套的 Uint8Array 替换为 `{__png,bytes}` 占位,但旧版插件只认顶层,
  嵌套会序列化成巨大的数字键对象并撞上截断——一律顶层返回,数据另起一次 `run`。
  SVG 用 `format:'SVG_STRING'` 直接得字符串。
- **SVG 导入**:`createNodeFromSvg` 解析 `A/a` 圆弧参数会产生畸形矢量,圆/弧改写为 C 贝塞尔(recipes §3)。
- **截断**:工具结果里 output 限 24KB、before/after 限 8KB(`*_truncated`),完整内容在
  evidence 目录 `result.json`;取数脚本要控制节点数并按 `node` 分块。
- **写入范围**:新节点建在 `target` 子树内,否则 before/after 快照看不到改动、证据不完整。
- **Variables**:免费版每个 collection 只有 1 个 mode,`addMode` 会抛;绑定颜色用
  `figma.variables.setBoundVariableForPaint` 生成新 paint 再整体赋给 `fills`。
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
- 没有通过的 recipes §8 验收 job_id,不得宣称设计「完成 / 对齐 / 效果良好」;PNG 只交给用户看,不作为模型自证。
- 原生桥任务单飞、绝不重放;结果未知时先读回——绝不重新提交同一修改。
- ack 不是橡皮章:必须附完成的 inspect:true 任务 ID + 实际观察到的文档现状说明。
