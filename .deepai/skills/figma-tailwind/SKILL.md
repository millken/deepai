---
name: figma-tailwind
description: "Use when a Figma link needs Tailwind CSS v4 utility classes only (no template/script/style) — 设计稿转 Tailwind 类名、提取样式清单. Maps Figma variables to v4 @theme tokens; weapp-tailwindcss constraints for 小程序/UniApp."
---

# Figma → Tailwind CSS v4 样式提取

读取 Figma 画布，把布局、间距、颜色转成符合 Tailwind CSS v4 语法的原子类组合，供用户直接粘贴到目标组件。**只交付 class 字符串与 `@theme` token 声明，不生成页面/模板/逻辑。**

## 取数（先判别服务器，两套管线）

第一步：判别当前 figma MCP 是哪套（docs/figma_mcp.md）。目标环境不明时用 ask_clarification 问一次（决定 §4 分支）。

**社区版 figma-developer-mcp（默认，PAT）——只有 2 个工具：**
- `figma__get_figma_data`：唯一的结构/样式来源。URL 里 `?node-id=1-2` → nodeId `1:2`（破折号换冒号）；大画布按 section 分次调用
- `figma__download_figma_images`：SVG/PNG 资产导出
- **没有** `get_metadata` / `get_design_context` / `get_variable_defs` / `get_screenshot`——不要调用
- 变量映射降级（§2）：响应不含 Figma 变量绑定信息，所有值按「未绑定变量」处理——吸附标准刻度，品牌色用实测 hex 给出 `@theme` 行并注明「变量名不可用，建议在 Figma 中核对命名」

**官方远程 mcp.figma.com（§三 变通路径，41 工具）：**
- `get_metadata` 定位 → `get_design_context` 分块取数 → `get_variable_defs` 取变量表（token 映射唯一依据）→ `get_screenshot` 视觉核对

共享取数细节（URL 解析、分块、token 优先原则）见 figma-ui 的 Workflow A。

## Tailwind v4 语法硬规则

- **禁止生成 `tailwind.config.js`**：v4 已废弃。设计系统扩展以 `@theme` 区块给出，如：
  ```css
  @theme {
    --color-brand-primary: #4F46E5;  /* Figma 变量 brand-primary */
    --radius-card: 1rem;             /* Figma 变量 radius-card */
  }
  ```
- **变量硬映射（官方远程）**：Figma 变量直接映射类名——`brand-primary` → `text-brand-primary` / `bg-brand-primary`；`radius-lg` → `rounded-lg`。`@theme` 未定义的变量名，清单前先补 `@theme` 行。
- **v3→v4 重命名一律用 v4 名**：v3 `rounded` → v4 `rounded-sm`（原 `rounded-sm` → `rounded-xs`）；同理 `shadow-sm`（=v3 `shadow`）、`blur-sm`（=v3 `blur`）、`outline-none` → `outline-hidden` 等。bare 名仅为兼容保留，不要输出。
- **任意值是最后手段**：仅当数值未绑定变量、且无法吸附刻度时用 `text-[#1A1A1A]`，行内注明「未绑定变量」。

## 刻度吸附（优先于任意值）

吸附顺序：变量 → 标准刻度 → 任意值。

- **间距（v4 动态刻度）**：spacing 由 `--spacing: 0.25rem` 动态生成，**任何 4 的倍数 px 都是合法刻度**——一律 `p-<px/4>`（44px → `p-11`），不存在「白名单里没有就退任意值」
- **字号**：`14/16/18/20px` → `text-sm / text-base / text-lg / text-xl`
- **圆角**：`2/4/8/16px` → `rounded-xs / rounded-sm / rounded-lg / rounded-2xl`
- 非 4 倍数、非标准档且视觉关键（如 1px 分隔线 `h-px`）才保留任意值

## 目标环境约束（按目标分支）

**默认（Web / H5）**：v4 全量工具类可用，含 `space-y-*`、`divide-*`。

**UniApp / 微信小程序（weapp-tailwindcss）**，叠加：

- **全量 Flexbox/Grid，禁止绝对定位**：对齐间距用 `flex flex-col gap-4 items-center justify-between`，严格对齐 Auto Layout（direction→`flex-row/flex-col`，gap→`gap-*`，padding→`p-*`，align→`items-*`，justify→`justify-*`）
- **禁用依赖兄弟/子组合器与 `:not()/:where()` 的工具类**（`space-y-*`、`divide-*`）——间距一律 `gap-*` + flex。注意这与 `before:`/`after:` 伪元素变体是两回事，后者另行判断
- **特殊字符**：`.`（`p-2.5`）、`/`（`bg-x/10`）、`[]`（`text-[#…]`）同属需要转义的字符，weapp-tailwindcss 的核心功能就是转换它们——机械上可用，但优先整数刻度与标准类（可读性），构建中应确认转换实际生效
- **oklch / `color-mix()` 风险（杀伤力最大）**：v4 默认色板是 oklch，透明度修饰符编译为 `color-mix(in oklab,…)`，低版本小程序基础库可能整条规则失效（颜色直接不生效）。不确定目标基础库支持度时：`@theme` 用 hex/rgb 定义颜色、透明度用预先算好的色值变量，不用 `/N` 修饰符
- **750 设计稿基准**：750 宽的稿间距/字号先 ÷2 再吸附（H5 以 375 基准）；375 宽直接吸附
- **组件语义**：输出对象是 `<view>`/`<text>`/`<image>`——容器 `flex …`、文本 `text-base …`、图片 `w-10 h-10 rounded-full`

## 输出格式（结构化样式清单）

禁止输出 `<template>` / `<script>` / `<style>` / 完整 HTML。按层级列清单，每行 = 语义名 + class 字符串：

- 📦 **外层包裹容器 (Wrapper)**: `flex flex-col p-4 bg-white rounded-2xl shadow-sm`
- 🏷️ **左侧图标区 (Icon Box)**: `flex items-center justify-center w-10 h-10 bg-brand-primary/10 text-brand-primary`
- 📝 **核心文本域 (Text Body)**: `flex-1 ml-3 text-base font-medium text-gray-900`
- 💰 **右侧金额 (Badge/Amount)**: `text-lg font-semibold text-emerald-600`

清单前按需附 `@theme` 补充块（仅列本次新映射的变量）。小程序目标且基础库支持度不明时，示例中的 `/N` 修饰符按 §4 规则替换。

## 红线

1. **只出 class 字符串与 `@theme` token 声明块**：任何 Vue/HTML/JS/CSS 结构代码一律不生成。`@theme` 块是唯一豁免——它是类名映射的前置契约，不是结构样式。用户明确要完整页面时，说明本 skill 职责并建议改用 figma-ui 工作流 A。
2. **PII 脱敏**：清单的语义名与示例文本中出现任何个人敏感信息（身份证、护照、政府 ID、卡号、手机号等）一律掩码（如 `**** **** **** 1234`）；截图仅用于布局核对，不复述其中敏感值。
3. **不臆造值**：颜色/间距/字号/圆角全部来自工具实测数据（社区版 `get_figma_data`，官方远程 `get_design_context` / `get_variable_defs`）；工具调用失败时说明失败，不用记忆兜底。
4. **401 双路径**（docs/figma_mcp.md）：社区版 PAT（§二，默认）→ `FIGMA_API_KEY` 失效，去 figma.com → Settings → Security 重新生成；官方远程（§三）→ 跑 deepai 仓库根的 `scripts/figma-mcp-token.py` ensure 后重试。
