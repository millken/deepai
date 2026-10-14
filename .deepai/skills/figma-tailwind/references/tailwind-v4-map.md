# Figma 属性 → Tailwind CSS v4 映射表

字段名对应原生桥取数脚本(figma-ui `references/bridge-recipes.md` §1)的输出;官方远程 `figma__get_design_context` 的参考代码按同一语义回译。
吸附顺序始终是:**变量 → 标准刻度 → 任意值**。`n` 表示 px,`n/4` 为 spacing 刻度(v4 `--spacing: 0.25rem`,任意 4 的倍数都合法:44px → `p-11`;2px 倍数可用 `.5`:6px → `p-1.5`)。

## 1. Auto Layout → Flex / Grid

| Figma | Tailwind v4 | 注意 |
|---|---|---|
| `layout.mode` HORIZONTAL / VERTICAL | `flex flex-row` / `flex flex-col` | |
| `layout.mode` GRID | `grid grid-cols-{gridColumnCount} gap-x-{colGap/4} gap-y-{rowGap/4}` | |
| `gap` n | `gap-{n/4}` | `gap:'auto'`(SPACE_BETWEEN)→ 只写 `justify-between`,不写 gap |
| `wrap` + `rowGap` | `flex-wrap gap-x-{gap/4} gap-y-{rowGap/4}` | |
| `pad` [t,r,b,l] | 四值相等 `p-`;t=b 且 l=r `px- py-`;否则 `pt- pr- pb- pl-` | |
| `justify` MIN / CENTER / MAX / SPACE_BETWEEN | `justify-start` / `justify-center` / `justify-end` / `justify-between` | MIN 可省略 |
| `align` MIN / CENTER / MAX / BASELINE | `items-start` / `items-center` / `items-end` / `items-baseline` | **CSS 默认是 stretch,Figma 默认是 MIN——MIN 必须显式写 `items-start`** |
| 子级主轴 `FILL` | `flex-1 min-w-0`(横向)/ `flex-1 min-h-0`(纵向) | `min-w-0` 让内部 `truncate` 生效 |
| 子级交叉轴 `FILL` | `self-stretch`(父级已写 items-*)或 `w-full`(flex-col 内) | |
| `HUG` | 不写尺寸;父级 stretch 时需 `w-fit` / `self-start` | |
| `FIXED` | `w-{n/4}` / `h-{n/4}`,宽高相等用 `size-{n/4}`;主轴固定尺寸加 `shrink-0` | 屏幕级宽度(375/390/1440)不要写死,用 `w-full` / `max-w-*` |
| `minWidth` / `maxWidth` 等 | `min-w-` / `max-w-` / `min-h-` / `max-h-` | |
| `absolute` + x/y | 父级 `relative`,子级 `absolute left-{x/4} top-{y/4}`(贴右/下边改 `right-`/`bottom-`) | |
| 父级 `layout` 缺失(NONE)但子级明显成行/列 | 按坐标推断成 flex,不要整片 absolute | 子级重叠才用 absolute |
| `clip` | `overflow-hidden` | |

## 2. 填充 / 描边 / 圆角 / 效果

| Figma | Tailwind v4 | 注意 |
|---|---|---|
| fill `#hex @var` | `bg-{token}`(TEXT 节点为 `text-{token}`) | token 名见 §5 |
| fill `/N%`(paint opacity) | `bg-{token}/N` | 小程序见 SKILL 的 oklch 规则 |
| 多层 fill | Figma 数组末项在最上层,CSS 背景首项在最上层——顺序反转;无法用单类表达时嵌一层元素 | |
| GRADIENT_LINEAR | `bg-linear-to-{t,tr,r,br,b,bl,l,tl} from-x via-y to-z`;非 45° 倍数用 `bg-linear-{deg}` | v3 的 `bg-gradient-to-*` 不要输出 |
| GRADIENT_RADIAL / ANGULAR | `bg-radial from-… to-…` / `bg-conic from-… to-…` | |
| IMAGE `scaleMode` FILL / FIT | `object-cover` / `object-contain`(配合 `<img>`) | |
| stroke INSIDE,weight w | `border`(1px)/ `border-{w}` + **`border-{color}`** | v4 `border` 默认色是 currentColor,必须给色 |
| stroke OUTSIDE | `outline outline-{w} outline-{color}` 或 `ring-{w} ring-{color}` | 不占布局;v4 `ring` 默认 1px |
| stroke CENTER | 按 INSIDE 处理,宽度视觉差 ≤0.5px 可忽略 | |
| 单边描边 [t,r,b,l] | `border-t` / `border-b` / `border-x` … | |
| `dash` | `border-dashed` | |
| radius 单值 | §4 刻度;≥ min(w,h)/2 → `rounded-full` | |
| radius [tl,tr,br,bl] | `rounded-t-` / `rounded-b-` / `rounded-tl-` … | |
| DROP_SHADOW `x y blur spread #rgba` | 与 §4 阴影刻度逐项吻合才用 `shadow-*`;否则 `shadow-[x_y_blur_spread_#rgba]`(多层逗号分隔,空格换下划线) | |
| INNER_SHADOW | `inset-shadow-[…]` | |
| LAYER_BLUR / BACKGROUND_BLUR | `blur-*` / `backdrop-blur-*` | Figma 模糊半径与 CSS `blur()` 非 1:1(Dev Mode 导出约 ÷2);官方远程以 `get_design_context` 为准 |
| `opacity` | `opacity-{round(v*100)}` | |
| `blend` | `mix-blend-{multiply,screen,overlay,…}` | |
| `rotation` r | r>0 → `-rotate-{r}`;r<0 → `rotate-{abs(r)}` | Figma 逆时针为正,CSS 顺时针为正——取反 |

## 3. 文字

| Figma | Tailwind v4 | 注意 |
|---|---|---|
| `size` + `lh` px | `text-{size}/{lh/4}` 简写(如 `text-sm/6`)或 `text-* leading-{lh/4}` | **v4 的 `text-*` 自带行高**,Figma 行高与默认值不同就必须显式写 |
| `lh` % | `leading-{named}`(吻合时)或 `leading-[1.4]` | |
| `lh` auto | `leading-[normal]` | `leading-normal` 是 1.5,不是 CSS normal |
| `weight`(取数字,别解析 style 名) | 100 `font-thin` · 200 `font-extralight` · 300 `font-light` · 400 `font-normal` · 500 `font-medium` · 600 `font-semibold` · 700 `font-bold` · 800 `font-extrabold` · 900 `font-black` | |
| `family` | 项目已有 `--font-*` 时 `font-{name}`;否则补 `@theme --font-{name}` | |
| `ls` % | ÷100 → em:吻合 §4 用 `tracking-*`,否则 `tracking-[-0.02em]` | |
| `ls` px | `tracking-[0.5px]` | |
| `case` UPPER / LOWER / TITLE | `uppercase` / `lowercase` / `capitalize` | |
| `deco` UNDERLINE / STRIKETHROUGH | `underline` / `line-through` | |
| `align` | `text-left` / `text-center` / `text-right` / `text-justify` | |
| `maxLines` 1 / N | `truncate` / `line-clamp-N` | 所在 flex 子级需 `min-w-0` |
| `autoResize` WIDTH_AND_HEIGHT | `whitespace-nowrap` | 单行自适应 |
| 多段 `font[]` | 外层类给主样式,差异段落用内联元素单独列一行 | |

## 4. v4 默认刻度(吸附用)

| 类别 | 刻度 |
|---|---|
| 字号/行高 | `text-xs` 12/16 · `sm` 14/20 · `base` 16/24 · `lg` 18/28 · `xl` 20/28 · `2xl` 24/32 · `3xl` 30/36 · `4xl` 36/40 · `5xl` 48/1 · `6xl` 60/1 · `7xl` 72/1 · `8xl` 96/1 · `9xl` 128/1 |
| 圆角 | `rounded-xs` 2 · `sm` 4 · `md` 6 · `lg` 8 · `xl` 12 · `2xl` 16 · `3xl` 24 · `4xl` 32 · `full` |
| 行高 | `leading-none` 1 · `tight` 1.25 · `snug` 1.375 · `normal` 1.5 · `relaxed` 1.625 · `loose` 2 · `leading-{n}` = n×4px |
| 字距 | `tracking-tighter` -0.05em · `tight` -0.025em · `normal` 0 · `wide` 0.025em · `wider` 0.05em · `widest` 0.1em |
| 模糊 | `blur-xs` 4 · `sm` 8 · `md` 12 · `lg` 16 · `xl` 24 · `2xl` 40 · `3xl` 64 |
| 阴影 | `shadow-2xs` 0 1 0 #0000000d · `xs` 0 1 2 0 #0000000d · `sm` 0 1 3 0 #0000001a + 0 1 2 -1 #0000001a · `md` 0 4 6 -1 + 0 2 4 -2(#0000001a) · `lg` 0 10 15 -3 + 0 4 6 -4 · `xl` 0 20 25 -5 + 0 8 10 -6 · `2xl` 0 25 50 -12 #00000040 |

v3→v4 改名一律输出 v4 名:`rounded-sm`(=v3 `rounded`)、`rounded-xs`(=v3 `rounded-sm`)、`shadow-xs`/`shadow-sm`(=v3 `shadow-sm`/`shadow`)、`blur-sm`(=v3 `blur`)、`outline-hidden`(=v3 `outline-none`)、`bg-linear-*`(=v3 `bg-gradient-*`)、CSS 变量简写 `bg-(--x)`(=v3 `bg-[--x]`)。

## 5. Token 命名(Figma 变量 → `@theme`)

| Figma | `@theme` 变量 | 类名 |
|---|---|---|
| COLOR `color/brand/primary` | `--color-brand-primary` | `bg-brand-primary` / `text-brand-primary` / `border-brand-primary` |
| FLOAT `radius/card` | `--radius-card` | `rounded-card` |
| FLOAT `spacing/4` = 16 | 值落在标准刻度 → 不建 token,直接 `p-4` | |
| FLOAT `spacing/gutter` = 20 | 非刻度语义名 → `--spacing-gutter` | `p-gutter` / `gap-gutter` |
| text style `Body/M` 14/20/500 | `--text-body-m: 0.875rem; --text-body-m--line-height: 1.25rem; --text-body-m--font-weight: 500;` | `text-body-m` |
| effect style `Elevation/2` | `--shadow-elevation-2: …` | `shadow-elevation-2` |
| STRING `font/sans` | `--font-sans: …` | `font-sans` |

规则:`/` 与空格转 `-`、全小写;剥掉与命名空间重复的前缀(`colors/primary/500` → `--color-primary-500`)。变量设置了 `codeSyntax.WEB`(如 `var(--color-surface)`)时以它为准。项目已有同值 token 时复用现有名字,不另起新名。

**多 mode(Light/Dark)**:默认 mode 进 `@theme`,其余 mode 作为同一 token 契约块里的变量覆盖——v4 工具类引用的是 `var(--color-*)`,覆盖即生效,类名无需 `dark:`:

```css
@theme {
  --color-surface: #ffffff;      /* Figma color/surface · Light */
}
.dark {
  --color-surface: #18181b;      /* Figma color/surface · Dark */
}
```

仅个别节点需要暗色差异、且不值得建 token 时,才在类名里写 `dark:bg-zinc-900`(v4 `dark:` 默认跟随 `prefers-color-scheme`,项目用 `.dark` 类切换时需已声明 `@custom-variant dark`)。
