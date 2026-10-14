# 原生桥脚本配方(builtin `figma` 工具 `run`)

每段都是 `run` 的 `code` 参数,在 `(figma, target)` 作用域执行;`target` 默认是绑定画板,传 `node` 参数则为该子树。
脚本彼此独立,不共享变量——辅助函数需要时整段复制进同一次 `run`。

通用约束:

- 输出上限:工具结果里 output 截断 24KB、before/after 截断 8KB(`*_truncated:true`);**完整内容在 `evidence` 目录的 `result.json`**,截断时用 read_file 读它,或用 `node` 缩小子树。
- 沙箱兼容:不用 `?.` / `??`,用 `&&` 守卫;节点不具备的属性直接读会抛异常,一律经 `get()` 读。
- 写操作只动 `target` 子树内的节点,before/after 快照才能覆盖改动,证据完整。

## 1. 设计→代码取数(样式 + 变量名,替代 `return null` 快照)

before 快照只有尺寸/填充 hex/基础 auto-layout;下面脚本补齐描边、阴影、文字排版、FILL/HUG、组件属性,并把绑定的变量**解析成名字**(`"#4f46e5 @color/brand/primary"`)。大画板按 section 传 `node` 分块,`MAX` 防输出爆量。

```js
const MAX = 250;
let count = 0;
const get = (n, k) => { try { return n[k]; } catch (e) { return undefined; } };
const r2 = v => Math.round(v * 100) / 100;
const hex = c => '#' + [c.r, c.g, c.b].map(v => Math.round(v * 255).toString(16).padStart(2, '0')).join('') +
  (c.a !== undefined && c.a < 1 ? Math.round(c.a * 255).toString(16).padStart(2, '0') : '');
const names = new Map();
const varName = async id => {
  if (!names.has(id)) { const v = await figma.variables.getVariableByIdAsync(id); names.set(id, v ? v.name : id); }
  return names.get(id);
};
const bound = async (n, f) => {
  const b = get(n, 'boundVariables'); const a = b && b[f]; const x = Array.isArray(a) ? a[0] : a;
  return x ? ' @' + await varName(x.id) : '';
};
const num = async (n, f) => {
  const v = get(n, f); if (typeof v !== 'number') return undefined;
  const b = await bound(n, f); return b ? r2(v) + b : r2(v);
};
const styleName = async id => {
  if (!id || typeof id !== 'string') return undefined;
  const s = await figma.getStyleByIdAsync(id); return s ? s.name : undefined;
};
const paint = async p => {
  if (p.visible === false) return null;
  const op = p.opacity !== undefined && p.opacity < 1 ? ' /' + Math.round(p.opacity * 100) + '%' : '';
  if (p.type === 'SOLID') {
    const b = p.boundVariables && p.boundVariables.color;
    return hex(p.color) + op + (b ? ' @' + await varName(b.id) : '');
  }
  if (p.type.indexOf('GRADIENT') === 0) {
    const stops = [];
    for (const s of p.gradientStops) stops.push(hex(s.color) + ' ' + Math.round(s.position * 100) + '%');
    return { type: p.type, stops, transform: p.gradientTransform.map(row => row.map(r2)) };
  }
  if (p.type === 'IMAGE') return { type: 'IMAGE', scaleMode: p.scaleMode };
  return p.type;
};
const paints = async arr => {
  if (!Array.isArray(arr) || !arr.length) return undefined;
  const out = (await Promise.all(arr.map(paint))).filter(Boolean);
  return out.length ? out : undefined;
};
const VECTOR = new Set(['VECTOR', 'BOOLEAN_OPERATION', 'STAR', 'POLYGON']);
const isIcon = n => VECTOR.has(n.type) || (typeof n.findOne === 'function' && n.width <= 64 && n.height <= 64 &&
  !n.findOne(c => c.type === 'TEXT') && !!n.findOne(c => VECTOR.has(c.type)));

async function ex(n, parentAuto) {
  if (count++ >= MAX) return { id: n.id, cut: true };
  const o = { id: n.id, name: n.name, type: n.type, w: Math.round(n.width), h: Math.round(n.height) };
  if (get(n, 'layoutPositioning') === 'ABSOLUTE' || !parentAuto) { o.x = Math.round(n.x); o.y = Math.round(n.y); }
  if (get(n, 'layoutPositioning') === 'ABSOLUTE') o.absolute = true;
  if (parentAuto || get(n, 'layoutMode') && n.layoutMode !== 'NONE') {
    const sh = get(n, 'layoutSizingHorizontal'), sv = get(n, 'layoutSizingVertical');
    if (sh) o.sizing = [sh, sv];
  }
  for (const f of ['minWidth', 'maxWidth', 'minHeight', 'maxHeight']) if (typeof get(n, f) === 'number') o[f] = get(n, f);
  if (n.type === 'INSTANCE') {
    const mc = await n.getMainComponentAsync();
    if (mc) o.component = mc.parent && mc.parent.type === 'COMPONENT_SET' ? mc.parent.name : mc.name;
    const props = get(n, 'componentProperties');
    if (props) { o.props = {}; for (const k of Object.keys(props)) o.props[k.split('#')[0]] = props[k].value; }
  }
  if (isIcon(n)) { o.asset = 'svg'; o.fills = await paints(get(n, 'fills')); return o; }

  const mode = get(n, 'layoutMode');
  if (mode && mode !== 'NONE') {
    const L = { mode };
    if (mode === 'GRID') {
      for (const f of ['gridColumnCount', 'gridRowCount', 'gridColumnGap', 'gridRowGap']) if (get(n, f) !== undefined) L[f] = get(n, f);
    } else {
      L.justify = n.primaryAxisAlignItems;
      L.align = n.counterAxisAlignItems;
      L.gap = n.primaryAxisAlignItems === 'SPACE_BETWEEN' ? 'auto' : await num(n, 'itemSpacing');
      L.pad = [await num(n, 'paddingTop'), await num(n, 'paddingRight'), await num(n, 'paddingBottom'), await num(n, 'paddingLeft')];
      if (n.layoutWrap === 'WRAP') { L.wrap = true; L.rowGap = await num(n, 'counterAxisSpacing'); }
    }
    o.layout = L;
  }
  if (get(n, 'clipsContent')) o.clip = true;

  const fills = await paints(get(n, 'fills'));
  if (fills) o.fills = fills;
  const strokes = await paints(get(n, 'strokes'));
  if (strokes) {
    const sw = get(n, 'strokeWeight');
    o.stroke = {
      paints: strokes, align: n.strokeAlign,
      weight: sw === figma.mixed ? ['strokeTopWeight', 'strokeRightWeight', 'strokeBottomWeight', 'strokeLeftWeight'].map(f => get(n, f)) : await num(n, 'strokeWeight'),
    };
    if (get(n, 'dashPattern') && n.dashPattern.length) o.stroke.dash = n.dashPattern;
  }
  const cr = get(n, 'cornerRadius');
  if (cr === figma.mixed) o.radius = [await num(n, 'topLeftRadius'), await num(n, 'topRightRadius'), await num(n, 'bottomRightRadius'), await num(n, 'bottomLeftRadius')];
  else if (cr) { const b = await bound(n, 'topLeftRadius'); o.radius = b ? r2(cr) + b : r2(cr); }
  const effects = (get(n, 'effects') || []).filter(e => e.visible !== false);
  if (effects.length) o.effects = effects.map(e => e.type.indexOf('SHADOW') > 0
    ? (e.type === 'INNER_SHADOW' ? 'inset ' : '') + [e.offset.x, e.offset.y, e.radius, e.spread || 0].map(r2).join(' ') + ' ' + hex(e.color)
    : e.type + ' ' + r2(e.radius));
  if (get(n, 'opacity') < 1) o.opacity = r2(n.opacity);
  const blend = get(n, 'blendMode');
  if (blend && blend !== 'PASS_THROUGH' && blend !== 'NORMAL') o.blend = blend;
  if (get(n, 'rotation')) o.rotation = r2(n.rotation);
  const styles = {
    fill: await styleName(get(n, 'fillStyleId')), stroke: await styleName(get(n, 'strokeStyleId')),
    effect: await styleName(get(n, 'effectStyleId')), text: await styleName(get(n, 'textStyleId')),
  };
  if (Object.values(styles).some(Boolean)) o.styles = styles;

  if (n.type === 'TEXT') {
    o.text = n.characters.length > 160 ? n.characters.slice(0, 160) + '…' : n.characters;
    const segs = n.getStyledTextSegments(['fontName', 'fontSize', 'fontWeight', 'lineHeight', 'letterSpacing', 'fills', 'textCase', 'textDecoration']);
    o.font = [];
    for (const s of segs.slice(0, 6)) {
      const lh = s.lineHeight.unit === 'AUTO' ? 'auto' : r2(s.lineHeight.value) + (s.lineHeight.unit === 'PERCENT' ? '%' : 'px');
      o.font.push({
        chars: segs.length > 1 ? s.characters.slice(0, 24) : undefined,
        family: s.fontName.family, style: s.fontName.style, weight: s.fontWeight, size: s.fontSize, lh,
        ls: s.letterSpacing.value ? r2(s.letterSpacing.value) + (s.letterSpacing.unit === 'PERCENT' ? '%' : 'px') : undefined,
        color: await paints(s.fills),
        case: s.textCase !== 'ORIGINAL' ? s.textCase : undefined,
        deco: s.textDecoration !== 'NONE' ? s.textDecoration : undefined,
      });
    }
    const sizeVar = await bound(n, 'fontSize');
    if (sizeVar) o.fontSizeVar = sizeVar.slice(2);
    o.align = n.textAlignHorizontal;
    o.autoResize = n.textAutoResize;
    if (get(n, 'textTruncation') === 'ENDING') o.maxLines = get(n, 'maxLines') || 1;
  }

  if ('children' in n) {
    const auto = !!(mode && mode !== 'NONE');
    const kids = n.children.filter(c => c.visible !== false);
    if (kids.length < n.children.length) o.hiddenChildren = n.children.length - kids.length;
    if (kids.length) { o.children = []; for (const c of kids) o.children.push(await ex(c, auto)); }
  }
  return o;
}
return await ex(target, false);
```

字段速读:`sizing=[横,纵]` 取值 FIXED/HUG/FILL;`layout.pad=[上,右,下,左]`;`gap:'auto'` 即 SPACE_BETWEEN;`asset:'svg'` 表示图标/矢量,走 §3 导出,不要手写 div 还原;`x/y` 只在父级非 auto-layout 或 absolute 时给出(相对父级)。

## 2. Token 表(本地 variables + styles)

```js
const hex = c => '#' + [c.r, c.g, c.b].map(v => Math.round(v * 255).toString(16).padStart(2, '0')).join('') +
  (c.a !== undefined && c.a < 1 ? Math.round(c.a * 255).toString(16).padStart(2, '0') : '');
const out = { collections: [], textStyles: [], effectStyles: [], paintStyles: [] };
for (const c of await figma.variables.getLocalVariableCollectionsAsync()) {
  const vars = [];
  for (const id of c.variableIds) {
    const v = await figma.variables.getVariableByIdAsync(id);
    const vals = [];
    for (const m of c.modes) {
      const x = v.valuesByMode[m.modeId];
      if (x && x.type === 'VARIABLE_ALIAS') { const a = await figma.variables.getVariableByIdAsync(x.id); vals.push('→' + (a ? a.name : x.id)); }
      else vals.push(v.resolvedType === 'COLOR' ? hex(x) : x);
    }
    vars.push([v.name, v.resolvedType, vals, v.codeSyntax.WEB]);
  }
  out.collections.push({ name: c.name, modes: c.modes.map(m => m.name), vars });
}
for (const s of await figma.getLocalTextStylesAsync())
  out.textStyles.push([s.name, s.fontName.family, s.fontName.style, s.fontSize, s.lineHeight, s.letterSpacing]);
for (const s of await figma.getLocalEffectStylesAsync()) out.effectStyles.push([s.name, s.effects.map(e => e.type)]);
for (const s of await figma.getLocalPaintStylesAsync())
  out.paintStyles.push([s.name, s.paints.map(p => p.type === 'SOLID' ? hex(p.color) : p.type)]);
return out;
```

`vars` 每行 = `[名, 类型, 各 mode 值(与 modes 同序), codeSyntax.WEB]`;`codeSyntax.WEB` 是设计师指定的代码名,存在时优先采用。团队库变量不在本地表里,但 §1 的 `@名字` 照样能解析出来。

## 3. 资产导出

```js
return await target.exportAsync({ format: 'SVG_STRING', svgIdAttribute: false })
```

`node` 传图标节点 ID。批量:

```js
const out = {};
for (const n of target.findAll(n => /^icon[\/ -]/i.test(n.name)).slice(0, 30))
  out[n.name] = await n.exportAsync({ format: 'SVG_STRING' });
return out;
```

位图/照片:`return await target.exportAsync({ format: 'PNG', constraint: { type: 'SCALE', value: 2 } })` → 结果中的 `preview.png` 路径即文件,复制进项目资源目录。SVG 内的固定色按需换成 `currentColor` 以便代码着色。

## 4. 构建辅助(写 Figma 时整段贴在脚本开头)

```js
const rgb = h => { const n = parseInt(h.slice(1, 7), 16); return { r: (n >> 16 & 255) / 255, g: (n >> 8 & 255) / 255, b: (n & 255) / 255 }; };
const solid = h => ({ type: 'SOLID', color: rgb(h) });
const box = (name, o, parent) => {
  const f = figma.createFrame();
  f.name = name;
  f.layoutMode = o.dir || 'VERTICAL';
  f.itemSpacing = o.gap || 0;
  const p = o.pad || [0, 0, 0, 0];
  f.paddingTop = p[0]; f.paddingRight = p[1]; f.paddingBottom = p[2]; f.paddingLeft = p[3];
  f.primaryAxisAlignItems = o.justify || 'MIN';
  f.counterAxisAlignItems = o.align || 'MIN';
  f.fills = o.fill ? [solid(o.fill)] : [];
  if (o.radius) f.cornerRadius = o.radius;
  if (o.stroke) { f.strokes = [solid(o.stroke)]; f.strokeWeight = 1; f.strokeAlign = 'INSIDE'; }
  parent.appendChild(f);
  f.layoutSizingHorizontal = o.w === 'FILL' ? 'FILL' : typeof o.w === 'number' ? 'FIXED' : 'HUG';
  f.layoutSizingVertical = o.h === 'FILL' ? 'FILL' : typeof o.h === 'number' ? 'FIXED' : 'HUG';
  if (typeof o.w === 'number' || typeof o.h === 'number') f.resize(typeof o.w === 'number' ? o.w : f.width, typeof o.h === 'number' ? o.h : f.height);
  return f;
};
const label = async (chars, o, parent) => {
  const font = { family: o.family || 'Inter', style: o.style || 'Regular' };
  await figma.loadFontAsync(font);
  const t = figma.createText();
  t.fontName = font;
  t.characters = chars;
  t.fontSize = o.size || 14;
  if (o.lh) t.lineHeight = { value: o.lh, unit: 'PIXELS' };
  t.fills = [solid(o.color || '#111827')];
  t.name = o.name || chars.slice(0, 24);
  parent.appendChild(t);
  if (o.w === 'FILL') { t.textAutoResize = 'HEIGHT'; t.layoutSizingHorizontal = 'FILL'; }
  return t;
};
```

示例(在绑定画板内搭一张卡片):

```js
// ……粘贴 §4 辅助……
const card = box('Card / Order', { dir: 'VERTICAL', gap: 12, pad: [16, 16, 16, 16], fill: '#FFFFFF', radius: 16, stroke: '#E5E7EB', w: 'FILL' }, target);
const head = box('Header', { dir: 'HORIZONTAL', justify: 'SPACE_BETWEEN', align: 'CENTER', w: 'FILL' }, card);
await label('订单 #1024', { size: 16, lh: 24, style: 'Semi Bold', name: 'Title' }, head);
await label('已发货', { size: 12, lh: 16, color: '#059669', name: 'Status' }, head);
await label('预计 3 天内送达,请保持电话畅通。', { size: 14, lh: 20, color: '#4B5563', w: 'FILL', name: 'Body' }, card);
return { id: card.id, png: await card.exportAsync({ format: 'PNG', constraint: { type: 'SCALE', value: 2 } }) };
```

顺序铁律:`layoutMode` → `appendChild` → `layoutSizing*` → `resize`。`FILL` 只能在父级是 auto-layout 且**已 append** 之后设置,否则抛错;`HUG` 只对 auto-layout 容器和文字有效。中文字体先用 `figma.listAvailableFontsAsync()` 精确核对(如 `PingFang SC` / `Noto Sans SC`)再写入 `family`。

## 5. Variables:创建、绑定、切 mode

```js
const rgb = h => { const n = parseInt(h.slice(1, 7), 16); return { r: (n >> 16 & 255) / 255, g: (n >> 8 & 255) / 255, b: (n & 255) / 255 }; };
const cols = await figma.variables.getLocalVariableCollectionsAsync();
const col = cols.find(c => c.name === 'Tokens') || figma.variables.createVariableCollection('Tokens');
const light = col.modes[0].modeId;
col.renameMode(light, 'Light');
let dark = (col.modes.find(m => m.name === 'Dark') || {}).modeId;
if (!dark) { try { dark = col.addMode('Dark'); } catch (e) { dark = null; } }
const existing = await figma.variables.getLocalVariablesAsync();
const upsert = (name, type, lightVal, darkVal, scopes, web) => {
  const v = existing.find(x => x.name === name && x.variableCollectionId === col.id) || figma.variables.createVariable(name, col, type);
  v.setValueForMode(light, lightVal);
  if (dark) v.setValueForMode(dark, darkVal === undefined ? lightVal : darkVal);
  v.scopes = scopes;
  if (web) v.setVariableCodeSyntax('WEB', web);
  return v;
};
upsert('color/surface', 'COLOR', rgb('#FFFFFF'), rgb('#18181B'), ['FRAME_FILL', 'SHAPE_FILL'], 'var(--color-surface)');
upsert('color/text/primary', 'COLOR', rgb('#111827'), rgb('#F4F4F5'), ['TEXT_FILL'], 'var(--color-text-primary)');
upsert('color/brand/primary', 'COLOR', rgb('#4F46E5'), rgb('#818CF8'), ['ALL_FILLS', 'STROKE_COLOR'], 'var(--color-brand-primary)');
upsert('spacing/4', 'FLOAT', 16, undefined, ['GAP'], 'var(--spacing-4)');
upsert('radius/lg', 'FLOAT', 8, undefined, ['CORNER_RADIUS'], 'var(--radius-lg)');
return { collection: col.id, modes: col.modes.map(m => m.name), darkMode: !!dark };
```

`addMode` 在免费版会抛(仅 1 个 mode)——`darkMode:false` 时告知用户。绑定到节点:

```js
const vars = await figma.variables.getLocalVariablesAsync();
const V = name => vars.find(v => v.name === name);
const card = target.findOne(n => n.name === 'Card / Order');
card.fills = [figma.variables.setBoundVariableForPaint({ type: 'SOLID', color: { r: 1, g: 1, b: 1 } }, 'color', V('color/surface'))];
for (const f of ['itemSpacing', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft']) card.setBoundVariable(f, V('spacing/4'));
for (const f of ['topLeftRadius', 'topRightRadius', 'bottomRightRadius', 'bottomLeftRadius']) card.setBoundVariable(f, V('radius/lg'));
for (const t of card.findAll(n => n.type === 'TEXT' && Array.isArray(n.fills) && n.fills.length))
  t.fills = [figma.variables.setBoundVariableForPaint(t.fills[0], 'color', V('color/text/primary'))];
return card.id;
```

预览暗色:`target.setExplicitVariableModeForCollection(col, darkModeId)` 后导出 PNG;核对完复原(`clearExplicitVariableModeForCollection(col)`)。文字绑定可用 `t.setBoundVariable('fontSize', v)`(亦支持 lineHeight / letterSpacing / fontWeight / fontFamily)。

## 6. 组件与变体

```js
// ……粘贴 §4 辅助……
const make = async (variant, bg, fg) => {
  const c = figma.createComponent();
  c.name = 'Variant=' + variant;
  c.layoutMode = 'HORIZONTAL';
  c.primaryAxisAlignItems = 'CENTER'; c.counterAxisAlignItems = 'CENTER';
  c.paddingTop = c.paddingBottom = 10; c.paddingLeft = c.paddingRight = 16;
  c.cornerRadius = 8;
  c.fills = [solid(bg)];
  target.appendChild(c);
  c.layoutSizingHorizontal = 'HUG'; c.layoutSizingVertical = 'HUG';
  await label('Button', { size: 14, lh: 20, style: 'Medium', color: fg, name: 'Label' }, c);
  return c;
};
const set = figma.combineAsVariants([await make('primary', '#4F46E5', '#FFFFFF'), await make('secondary', '#F3F4F6', '#111827')], target);
set.name = 'Button';
set.layoutMode = 'HORIZONTAL'; set.itemSpacing = 16;
set.paddingTop = set.paddingRight = set.paddingBottom = set.paddingLeft = 16;
const key = set.addComponentProperty('Label', 'TEXT', 'Button');
for (const c of set.children) c.findOne(n => n.name === 'Label').componentPropertyReferences = { characters: key };
return { set: set.id, key };
```

使用实例:`const i = set.defaultVariant.createInstance(); parent.appendChild(i); const k = Object.keys(i.componentProperties).find(k => k.indexOf('Label#') === 0); i.setProperties({ Variant: 'secondary', [k]: '保存' });`。
命名约定 `属性=值`(如 `Size=md, State=hover`)——代码侧直接映射为组件 props。

## 7. 交付前体检(design lint)

```js
const issues = {}, total = {};
const add = (k, n) => { total[k] = (total[k] || 0) + 1; (issues[k] = issues[k] || []); if (issues[k].length < 10) issues[k].push(n.id + ' ' + n.name); };
const sizes = new Set(), raw = new Set();
const DEFAULT = /^(Frame|Group|Rectangle|Ellipse|Vector|Text|Line|Polygon|Star|Union|Subtract|Intersect|Exclude)( \d+)?$/;
const hex = c => '#' + [c.r, c.g, c.b].map(v => Math.round(v * 255).toString(16).padStart(2, '0')).join('');
const get = (n, k) => { try { return n[k]; } catch (e) { return undefined; } };
const bound = (n, f) => !!(n.boundVariables && n.boundVariables[f]);
const walk = n => {
  if (n.visible === false) { add('hidden', n); return; }
  if (DEFAULT.test(n.name)) add('defaultName', n);
  if (n.type === 'GROUP') add('group', n);
  if (get(n, 'layoutPositioning') === 'ABSOLUTE') add('absolute', n);
  const mode = get(n, 'layoutMode');
  if ((n.type === 'FRAME' || n.type === 'COMPONENT') && mode === 'NONE' && n.children.length > 1 &&
      n.children.some(c => c.type === 'TEXT' || c.type === 'FRAME' || c.type === 'INSTANCE')) add('noAutoLayout', n);
  if (mode === 'HORIZONTAL' || mode === 'VERTICAL')
    for (const f of ['itemSpacing', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft'])
      if (n[f] % 4 && !bound(n, f)) { add('offGrid', n); break; }
  if ('fills' in n && Array.isArray(n.fills) && !n.fillStyleId)
    for (const p of n.fills) if (p.type === 'SOLID' && p.visible !== false && !(p.boundVariables && p.boundVariables.color)) { add('rawColor', n); raw.add(hex(p.color)); }
  if (n.type === 'TEXT') {
    if (typeof n.fontSize === 'number') sizes.add(n.fontSize);
    if (!n.textStyleId && !bound(n, 'fontSize')) add('rawText', n);
    if (n.textAutoResize === 'NONE') add('fixedTextBox', n);
  }
  if (n.type === 'INSTANCE') return;
  if ('children' in n) for (const c of n.children) walk(c);
};
walk(target);
return { total, examples: issues, fontSizes: [...sizes].sort((a, b) => a - b), rawColors: [...raw].slice(0, 24) };
```

| 项 | 对代码转换的影响 | 修法 |
|---|---|---|
| `noAutoLayout` / `group` | 只能绝对定位还原,响应式失效 | 转 Frame + auto-layout |
| `absolute` | 合理仅限角标/浮层/装饰 | 其余改回流式 |
| `rawColor` / `rawText` | 代码里只能落魔法数 | 绑定 variable / text style |
| `offGrid` | 无法吸附 Tailwind 刻度 | 调到 4 的倍数或绑定 spacing 变量 |
| `defaultName` | 代码命名无语义 | 按职责命名(`Header`、`Price`、`Button / Primary`) |
| `fixedTextBox` | 文案变长溢出 | `textAutoResize` 改 HEIGHT 或 WIDTH_AND_HEIGHT |
| `fontSizes` 超过 6–7 档 | 字阶混乱 | 收敛到字阶表 |
