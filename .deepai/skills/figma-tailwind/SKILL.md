---
name: figma-tailwind
description: "Use when the user shares a Figma link and wants Tailwind CSS v4 utility classes (class strings only) extracted from the design — 设计稿转 Tailwind 类名、提取样式清单、还原界面样式. Maps Figma variables to design tokens, Auto Layout to flex/grid utilities, and outputs a structured class list. Covers web by default; applies mini-program/UniApp constraints (weapp-tailwindcss) when that is the target. Never generates templates, scripts, or style tags."
---

# Figma → Tailwind CSS v4 样式提取

读取 Figma 画布，把矢量布局、间距、变量（Tokens）转成符合 Tailwind CSS v4 语法的原子类组合，供用户直接粘贴到目标组件。**只交付 class 字符串，不生成页面/模板/逻辑。**

## 执行步骤

### 1. 取数（ground truth，禁止凭记忆）

1. 解析链接：`figma.com/design/<fileKey>/...`，`?node-id=1-2` → nodeId `1:2`。无 node-id 先 `figma__get_metadata` 定位目标 frame。
2. `figma__get_design_context` 取节点结构；大画布按 section 分块调用。
3. `figma__get_variable_defs` 取变量表——这是类名映射的唯一依据。
4. 需要视觉核对时 `figma__get_screenshot`。
5. 不确定目标环境（Web / 小程序 / H5）时用 ask_clarification 问一次——环境决定 §4 的约束分支。

### 2. Tailwind v4 语法硬规则

- **禁止生成 `tailwind.config.js`**：v4 已废弃。设计系统扩展（主题色、圆角、自定义间距）以 v4 `@theme` 区块格式给出映射建议，如：
  ```css
  @theme {
    --color-brand-primary: #4F46E5;  /* Figma 变量 brand-primary */
    --radius-card: 1rem;             /* Figma 变量 radius-card */
  }
  ```
- **变量硬映射**：Figma 变量直接映射类名——`brand-primary` → `text-brand-primary` / `bg-brand-primary`；`radius-lg` → `rounded-lg`。`@theme` 未定义的变量名，在清单前先给出对应 `@theme` 行。
- **任意值是最后手段**：仅当数值未绑定任何 Figma 变量、且无法吸附标准刻度时才用 `text-[#1A1A1A]` 形式，并在行内注明「未绑定变量」。

### 3. 刻度吸附（优先于任意值）

吸附顺序：变量 → Tailwind 标准刻度 → 任意值。

| Figma 原值 | 吸附目标 | 示例 |
|---|---|---|
| 4 的倍数 px 间距 | spacing 刻度 | `4/8/12/16/20/24` → `p-1/p-2/p-3/p-4/p-5/p-6` |
| 常用圆角 | radius 刻度 | `4/8/16px` → `rounded / rounded-lg / rounded-2xl` |
| 字号 | font-size 刻度 | `14/16/20px` → `text-sm / text-base / text-lg` |

无法整除且视觉关键（如 1px 分隔线）才保留任意值。

### 4. 目标环境约束（按目标分支）

**默认（Web / H5，标准 HTML 组件）**：标准 v4 全量工具类可用，含 `space-y-*`、`divide-*`、任意值变体。

**UniApp / 微信小程序（weapp-tailwindcss）**，在默认规则之上叠加：

- **全量 Flexbox/Grid，禁止绝对定位塌陷**：小程序环境绝对定位极易错位。对齐与间距必须用 `flex flex-col gap-4 items-center justify-between` 等实现，严格对齐 Figma 的 Auto Layout（direction → `flex-row/flex-col`，gap → `gap-*`，padding → `p-*`，align → `items-*`，justify → `justify-*`）。
- **禁用伪元素/选择器依赖类**：`space-y-*`、`divide-*` 在小程序支持不全——间距一律 `gap-*` + flex。
- **小数刻度规避**：`p-2.5` 等过多小数点变体优先吸附到整数刻度（`p-2` 或 `p-3`，视觉差异可忽略时）。
- **750 设计稿基准**：Figma 按 750 宽做的稿，间距/字号先 ÷2 再吸附（H5 以 375 基准）。375 宽的稿直接吸附。
- **组件语义**：输出对象是 `<view>`/`<text>`/`<image>` 不是 HTML 标签——容器类给 `flex ...`，文本类给 `text-base ...`，图片类给 `w-10 h-10 rounded-full`。

### 5. 输出格式（结构化样式清单）

禁止输出 `<template>` / `<script>` / `<style>` / 完整 HTML。按层级列清单，每行 = 语义名 + class 字符串：

- 📦 **外层包裹容器 (Wrapper)**: `flex flex-col p-4 bg-white rounded-2xl shadow-sm`
- 🏷️ **左侧图标区 (Icon Box)**: `flex items-center justify-center w-10 h-10 bg-brand-primary/10 text-brand-primary`
- 📝 **核心文本域 (Text Body)**: `flex-1 ml-3 text-base font-medium text-gray-900`
- 💰 **右侧金额 (Badge/Amount)**: `text-lg font-semibold text-emerald-600`

清单前按需附 `@theme` 补充块（仅列本次新映射的变量）。

## 红线

1. **只出 class 字符串**：任何 Vue/HTML/JS/CSS 结构代码一律不生成；用户明确要求完整页面时，说明本 skill 职责并建议改用 figma-ui 工作流 A。
2. **敏感信息脱敏**：分析订单、钱包、个人资产等界面时，身份证、护照、政府 ID 等占位文本在清单与示例中必须掩码（如 `**** **** **** 1234`）；截图仅用于布局核对，不复述其中敏感值。
3. **不臆造值**：颜色/间距/字号/圆角全部来自 `get_design_context` / `get_variable_defs` 实测数据；工具调用失败时说明失败，不用记忆兜底。
4. 401 → token 过期：`scripts/figma-mcp-token.py ensure` 后重试（PAT 路径见 docs/figma_mcp.md §二）。
