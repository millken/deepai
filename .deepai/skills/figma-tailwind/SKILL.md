---
name: figma-tailwind
description: "Use when a Figma link needs Tailwind CSS v4 utility classes only (no template/script/style) — 设计稿转 Tailwind 类名、提取样式清单. Maps Figma variables to v4 @theme tokens; weapp-tailwindcss constraints for 小程序/UniApp."
---

# Figma → Tailwind CSS v4 样式提取

读取 Figma 画布，把布局、间距、颜色、排版、效果转成符合 Tailwind CSS v4 语法的原子类组合，供用户直接粘贴到目标组件。**只交付 class 字符串与 token 声明块，不生成页面/模板/逻辑。**

逐属性映射与 v4 默认刻度见 [tailwind-v4-map.md](references/tailwind-v4-map.md)——动手前先读。

## 1. 取数（先判别数据源，两条路径）

优先级：**原生桥 > 官方远程**。若 builtin `figma` 工具（deepai 内嵌桥，Figma Desktop + 插件）`status` 为 connected:true 且已绑定目标文件，走内嵌桥；否则用官方远程 MCP（mcp.figma.com，docs/figma_mcp.md §三）。

> 社区版 figma-developer-mcp 已从 skills 移除，以官方为准。识别到环境只配了社区版（mcp.json 里 npx figma-developer-mcp）时**不要静默当官方用**：提示停用社区版，按 docs/figma_mcp.md §三 配置官方远程，或改走原生桥。

**官方远程（完整工具名带 `figma__` 前缀）：**
`figma__get_metadata` 定位 → `figma__get_variable_defs` 取变量表（token 映射唯一依据）→ `figma__get_design_context` 按 section 分块取数 → `figma__get_screenshot` 视觉核对。
`get_design_context` 返回的是 React + Tailwind **参考代码**：其中的绝对定位、魔法数要按映射表回译为 flex + 刻度 + token，不要原样转抄。

**原生桥（builtin `figma`）：**
1. 取数用 [bridge-recipes.md](../figma-ui/references/bridge-recipes.md) §1 的取数脚本，**不要只看 `return null` 的 before 快照**——快照缺描边/阴影/行高/字距/FILL·HUG，也没有变量名。
2. 脚本输出把绑定变量解析成名字（`"#4f46e5 @color/brand/primary"`），按变量硬映射；需要全量 token 时跑 §2 Token 表。
3. 大画板传 `node` 按 section 分块；输出被截断（`output_truncated`）时读 evidence 目录的 `result.json`。
4. PNG 核对：`run` {code:'return await target.exportAsync({format:"PNG"})'}。

## 2. 映射流程

1. **先结构后样式**：从外到内逐层，`layout` → flex/grid 容器类，子级 `sizing` → `flex-1` / `self-stretch` / 固定尺寸。Figma 默认 MIN 对齐而 CSS 默认 stretch：容器 `items-start` 不可省略。
2. **再视觉**：填充 → 描边（v4 `border` 默认 currentColor，必须给色）→ 圆角 → 阴影/模糊 → 不透明度。
3. **再排版**：字号与行高成对输出（v4 `text-*` 自带行高，用 `text-sm/6` 简写）；字重取数字；字距 % ÷100 → em；`maxLines` → `truncate` / `line-clamp-N`。
4. **吸附顺序：变量 → 标准刻度 → 任意值。**
   - 绑定了变量：用 token 类名（命名规则见映射表 §5）；`@theme` 尚无该变量时先补一行。
   - 未绑定：吸附 v4 刻度（任何 4 的倍数 px 都是合法 spacing：44px → `p-11`）。
   - 都不行且视觉关键（1px 分隔线 `h-px`、品牌专色）才写任意值，行内注明「未绑定变量」。
5. **图标/矢量**（取数输出 `asset:'svg'`）：只给尺寸与颜色类（`size-5 text-gray-500`），并说明需按 bridge-recipes §3 导出 SVG，不要用 div 拼形状。

## 3. Tailwind v4 语法硬规则

- **禁止生成 `tailwind.config.js`**：v4 已废弃。设计系统扩展以 `@theme` 区块给出：
  ```css
  @theme {
    --color-brand-primary: #4F46E5;  /* Figma 变量 color/brand/primary */
    --radius-card: 1rem;             /* Figma 变量 radius/card */
  }
  ```
- **多 mode（Light/Dark）**：默认 mode 进 `@theme`，其余 mode 写成同一 token 块里的 `.dark { --color-…: … }` 变量覆盖（映射表 §5），类名保持不变。
- **v3→v4 改名一律用 v4 名**：`rounded-sm`（=v3 `rounded`）、`rounded-xs`、`shadow-xs`、`blur-sm`、`outline-hidden`、`bg-linear-to-*`、`bg-(--x)`。bare 名仅为兼容保留，不要输出。
- **任意值语法**：空格换下划线，如 `shadow-[0_4px_12px_0_#00000014]`。

## 4. 目标环境约束（按目标分支）

**默认（Web / H5）**：v4 全量工具类可用，含 `space-y-*`、`divide-*`。

**UniApp / 微信小程序（weapp-tailwindcss）**，叠加：

- **全量 Flexbox/Grid，禁止绝对定位**：对齐间距用 `flex flex-col gap-4 items-center justify-between`，严格对齐 Auto Layout（direction→`flex-row/flex-col`，gap→`gap-*`，padding→`p-*`，align→`items-*`，justify→`justify-*`）
- **禁用依赖兄弟/子组合器与 `:not()/:where()` 的工具类**（`space-y-*`、`divide-*`）——间距一律 `gap-*` + flex。注意这与 `before:`/`after:` 伪元素变体是两回事，后者另行判断
- **特殊字符**：`.`（`p-2.5`）、`/`（`bg-x/10`、`text-sm/6`）、`[]`（`text-[#…]`）同属需要转义的字符，weapp-tailwindcss 的核心功能就是转换它们——机械上可用，但优先整数刻度与标准类（可读性），构建中应确认转换实际生效
- **oklch / `color-mix()` 风险（杀伤力最大）**：v4 默认色板是 oklch，透明度修饰符编译为 `color-mix(in oklab,…)`，低版本小程序基础库可能整条规则失效（颜色直接不生效）。不确定目标基础库支持度时：`@theme` 用 hex/rgb 定义颜色、透明度用预先算好的色值变量，不用 `/N` 修饰符
- **750 设计稿基准**：750 宽的稿间距/字号先 ÷2 再吸附（H5 以 375 基准）；375 宽直接吸附
- **组件语义**：输出对象是 `<view>`/`<text>`/`<image>`——容器 `flex …`、文本 `text-base …`、图片 `w-10 h-10 rounded-full`

## 5. 输出格式（结构化样式清单）

禁止输出 `<template>` / `<script>` / `<style>` / 完整 HTML。按节点层级缩进列清单，每行 = 语义名 + 节点 ID + class 字符串：

```
- 外层包裹容器 Wrapper `12:40`: `flex flex-col gap-3 p-4 bg-surface rounded-2xl border border-gray-200 shadow-sm`
  - 头部行 Header `12:41`: `flex items-center justify-between`
    - 标题 Title `12:42`: `text-base/6 font-semibold text-text-primary truncate`
    - 状态 Badge `12:43`: `shrink-0 px-2 py-0.5 rounded-full text-xs/4 font-medium text-emerald-600 bg-emerald-50`
  - 正文 Body `12:44`: `text-sm/5 text-gray-600 line-clamp-2`
  - 图标 Icon `12:45`: `size-5 text-gray-400`（SVG 需导出）
```

清单前按需附 token 声明块（仅列本次新映射的变量）；清单后附**待确认项**：所有任意值及原因、未绑定变量的品牌色、取数被截断未覆盖的子树。小程序目标且基础库支持度不明时，`/N` 修饰符按 §4 规则替换。

## 6. 红线

1. **只出 class 字符串与 token 声明块**（`@theme` 及其 mode 覆盖）：任何 Vue/HTML/JS/CSS 结构代码一律不生成。token 块是唯一豁免——它是类名映射的前置契约，不是结构样式。用户明确要完整页面时，说明本 skill 职责并建议改用 figma-ui 工作流 A。
2. **PII 脱敏**：清单的语义名与示例文本中出现任何个人敏感信息（身份证、护照、政府 ID、卡号、手机号等）一律掩码（如 `**** **** **** 1234`）；截图仅用于布局核对，不复述其中敏感值。
3. **不臆造值**：颜色/间距/字号/圆角/阴影全部来自工具实测数据（原生桥取数脚本输出，官方远程 `figma__get_design_context` / `figma__get_variable_defs`）；工具调用失败时说明失败，不用记忆兜底。
4. **401**（官方远程，docs/figma_mcp.md §三）：跑 deepai 仓库根的 `scripts/figma-mcp-token.py` ensure 后重试。
