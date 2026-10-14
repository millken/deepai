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
const FONTS = (await figma.listAvailableFontsAsync()).map(f => f.fontName.family + '|' + f.fontName.style);
const STYLES = { 400: ['Regular'], 500: ['Medium'], 600: ['Semi Bold', 'SemiBold', 'Semibold'], 700: ['Bold'] };
const LH = { 12: 16, 13: 18, 14: 20, 16: 24, 18: 28, 20: 28, 24: 32, 30: 36, 36: 40 };
const CJK = /[\u3000-\u303f\u3400-\u9fff\uf900-\ufaff\uff00-\uffef]/;
const CJK_FAMILIES = ['PingFang SC', 'Noto Sans SC', 'Source Han Sans SC', 'Noto Sans CJK SC', 'Microsoft YaHei'];
const fontFor = (chars, family, weight) => {
  const fams = CJK.test(chars) ? CJK_FAMILIES : [family || 'Inter'];
  for (const fam of fams) for (const s of STYLES[weight || 400]) if (FONTS.indexOf(fam + '|' + s) >= 0) return { family: fam, style: s };
  throw Error('无可用字体:' + fams.join('/') + ' weight ' + (weight || 400) + ';先 listAvailableFontsAsync 核对,不要回退到无中文字形的字体');
};
const label = async (chars, o, parent) => {
  const font = fontFor(chars, o.family, o.weight);
  await figma.loadFontAsync(font);
  const t = figma.createText();
  t.fontName = font;
  t.characters = chars;
  t.fontSize = o.size || 14;
  t.lineHeight = { value: o.lh || LH[t.fontSize] || Math.round(t.fontSize * 1.4), unit: 'PIXELS' };
  t.fills = [solid(o.color || '#111827')];
  t.name = o.name || chars.slice(0, 24);
  parent.appendChild(t);
  if (o.w === 'FILL') { t.textAutoResize = 'HEIGHT'; t.layoutSizingHorizontal = 'FILL'; }
  return t;
};
```

`label` 按字重选字体样式(各字体的 600 叫法不同:`Semi Bold` / `SemiBold` / `Semibold`),含中文时强制选中文字体——Inter 等拉丁字体没有中文字形,Figma 回退渲染会让文字视觉偏移,这是「按钮文字位置不对」的常见根因。行高总是显式像素值。

示例(在绑定画板内搭一张卡片):

```js
// ……粘贴 §4 辅助……
const card = box('Card / Order', { dir: 'VERTICAL', gap: 12, pad: [16, 16, 16, 16], fill: '#FFFFFF', radius: 16, stroke: '#E5E7EB', w: 'FILL' }, target);
const head = box('Header', { dir: 'HORIZONTAL', justify: 'SPACE_BETWEEN', align: 'CENTER', w: 'FILL' }, card);
await label('订单 #1024', { size: 16, lh: 24, weight: 600, name: 'Title' }, head);
await label('已发货', { size: 12, lh: 16, color: '#059669', name: 'Status' }, head);
await label('预计 3 天内送达,请保持电话畅通。', { size: 14, lh: 20, color: '#4B5563', w: 'FILL', name: 'Body' }, card);
return { id: card.id, png: await card.exportAsync({ format: 'PNG', constraint: { type: 'SCALE', value: 2 } }) };
```

顺序铁律:`layoutMode` → `appendChild` → `layoutSizing*` → `resize`。`FILL` 只能在父级是 auto-layout 且**已 append** 之后设置,否则抛错;`HUG` 只对 auto-layout 容器和文字有效。每次构建后跑 §8 验收。

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

## 6. 组件与变体(按规格构建 Button / Input)

尺寸只从规格表取([component-specs.md](component-specs.md)),不现场估算;高度由「行高 + 2×垂直内边距」推出,文字靠 auto-layout 居中,绝不手摆坐标。

```js
// ……粘贴 §4 辅助……
const BTN = { sm: { h: 32, px: 12, size: 14, lh: 20, weight: 500, r: 6 }, md: { h: 40, px: 16, size: 14, lh: 20, weight: 500, r: 8 }, lg: { h: 48, px: 20, size: 16, lh: 24, weight: 600, r: 8 } };
const INPUT = { sm: { h: 32, px: 10, size: 14, lh: 20, r: 6 }, md: { h: 40, px: 12, size: 14, lh: 20, r: 8 }, lg: { h: 48, px: 14, size: 16, lh: 24, r: 8 } };
const control = (c, s) => {
  c.layoutMode = 'HORIZONTAL';
  c.counterAxisAlignItems = 'CENTER';
  c.paddingLeft = c.paddingRight = s.px;
  c.paddingTop = c.paddingBottom = (s.h - s.lh) / 2;
  c.cornerRadius = s.r;
};
const button = async (text, size, v) => {
  const s = BTN[size];
  const c = figma.createComponent();
  c.name = 'Size=' + size + ', Variant=' + v.name;
  control(c, s);
  c.primaryAxisAlignItems = 'CENTER';
  c.itemSpacing = size === 'sm' ? 6 : 8;
  c.fills = [solid(v.bg)];
  if (v.border) { c.strokes = [solid(v.border)]; c.strokeWeight = 1; c.strokeAlign = 'INSIDE'; }
  target.appendChild(c);
  c.layoutSizingHorizontal = 'HUG'; c.layoutSizingVertical = 'HUG';
  await label(text, { size: s.size, lh: s.lh, weight: s.weight, color: v.fg, name: 'Label' }, c);
  return c;
};
const input = async (placeholder, size, state) => {
  const s = INPUT[size];
  const c = figma.createComponent();
  c.name = 'Size=' + size + ', State=' + state.name;
  control(c, s);
  c.itemSpacing = 8;
  c.fills = [solid('#FFFFFF')];
  c.strokes = [solid(state.border)]; c.strokeWeight = state.borderWidth || 1; c.strokeAlign = 'INSIDE';
  target.appendChild(c);
  c.layoutSizingHorizontal = 'FIXED'; c.layoutSizingVertical = 'HUG';
  c.resize(280, c.height);
  const t = await label(placeholder, { size: s.size, lh: s.lh, color: state.text || '#6B7280', name: 'Placeholder', w: 'FILL' }, c);
  t.textTruncation = 'ENDING'; t.maxLines = 1;
  return c;
};
const VARIANTS = [{ name: 'primary', bg: '#4F46E5', fg: '#FFFFFF' }, { name: 'secondary', bg: '#FFFFFF', fg: '#111827', border: '#D1D5DB' }];
const btns = [];
for (const size of ['sm', 'md', 'lg']) for (const v of VARIANTS) btns.push(await button('确认', size, v));
const btnSet = figma.combineAsVariants(btns, target);
btnSet.name = 'Button';
btnSet.layoutMode = 'HORIZONTAL'; btnSet.layoutWrap = 'WRAP'; btnSet.itemSpacing = 16; btnSet.counterAxisSpacing = 16;
btnSet.paddingTop = btnSet.paddingRight = btnSet.paddingBottom = btnSet.paddingLeft = 16;
const key = btnSet.addComponentProperty('Label', 'TEXT', '确认');
for (const c of btnSet.children) c.findOne(n => n.name === 'Label').componentPropertyReferences = { characters: key };
const ins = [];
for (const st of [{ name: 'default', border: '#D1D5DB' }, { name: 'focus', border: '#4F46E5', borderWidth: 2 }, { name: 'error', border: '#DC2626' }])
  ins.push(await input('请输入手机号', 'md', st));
const inputSet = figma.combineAsVariants(ins, target);
inputSet.name = 'Input';
inputSet.layoutMode = 'VERTICAL'; inputSet.itemSpacing = 16;
inputSet.paddingTop = inputSet.paddingRight = inputSet.paddingBottom = inputSet.paddingLeft = 16;
return { button: btnSet.id, input: inputSet.id, labelKey: key };
```

使用实例:`const i = btnSet.defaultVariant.createInstance(); parent.appendChild(i); const k = Object.keys(i.componentProperties).find(k => k.indexOf('Label#') === 0); i.setProperties({ Size: 'lg', Variant: 'secondary', [k]: '保存' });`。
命名约定 `属性=值`(如 `Size=md, State=hover`)——代码侧直接映射为组件 props;§8 验收也靠 `Size=` 分组比对变体高度。

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

## 8. 量测验收(每次构建/修改后必跑,error 清零才算完成)

模型看不到 preview.png——deepai 的工具结果只带图片路径,不把像素送进模型。所以「看起来对」不是证据,**本脚本的量测数字才是**。
不变量与阈值见 [component-specs.md](component-specs.md) §3;控件按命名识别(`button|btn|按钮`、`input|field|search|输入|搜索`)。`node` 传刚构建/修改的子树。

```js
const ERR = [], WARN = [], controls = [];
let seen = 0;
const get = (n, k) => { try { return n[k]; } catch (e) { return undefined; } };
const r1 = v => Math.round(v * 10) / 10;
const err = (n, code, msg) => { if (ERR.length < 60) ERR.push(n.id + ' ' + n.name + ' [' + code + '] ' + msg); };
const warn = (n, code, msg) => { if (WARN.length < 40) WARN.push(n.id + ' ' + n.name + ' [' + code + '] ' + msg); };
const cx = b => b.x + b.width / 2, cy = b => b.y + b.height / 2;
const CJK = /[\u3000-\u303f\u3400-\u9fff\uf900-\ufaff\uff00-\uffef]/;
const CJK_FONT = /PingFang|Noto Sans (SC|TC|HK|JP|KR|CJK)|Noto Serif (SC|TC|JP|KR|CJK)|Source Han|思源|Hiragino|YaHei|HarmonyOS|MiSans|PuHuiTi|Heiti|Songti/i;
const BTN = /button|btn|按钮/i, INPUT = /input|field|search|输入|搜索/i;
const STD_H = [24, 28, 32, 36, 40, 44, 48, 52, 56];
const MOBILE = !!target.absoluteBoundingBox && target.absoluteBoundingBox.width <= 480;
const lum = c => { const f = v => v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b); };
const contrast = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
const solidOf = n => {
  const f = get(n, 'fills'); if (!Array.isArray(f)) return null;
  for (let i = f.length - 1; i >= 0; i--) { const p = f[i]; if (p.type === 'SOLID' && p.visible !== false && (p.opacity === undefined || p.opacity > 0.95)) return p.color; }
  return null;
};
const bgOf = n => { for (let p = n.parent; p && p.type !== 'PAGE'; p = p.parent) { const c = solidOf(p); if (c) return c; } return null; };
const lhPx = t => { const l = get(t, 'lineHeight'); return l && l !== figma.mixed && l.unit === 'PIXELS' ? l.value : null; };
const textNodes = n => typeof n.findAll === 'function' ? n.findAll(c => c.type === 'TEXT' && c.visible !== false) : [];

async function kindOf(n) {
  if (n.type !== 'FRAME' && n.type !== 'COMPONENT' && n.type !== 'INSTANCE') return null;
  let name = n.name;
  if (n.type === 'COMPONENT' && n.parent && n.parent.type === 'COMPONENT_SET') name += ' ' + n.parent.name;
  if (n.type === 'INSTANCE') { const mc = await n.getMainComponentAsync(); if (mc) name += ' ' + mc.name + ' ' + (mc.parent && mc.parent.type === 'COMPONENT_SET' ? mc.parent.name : ''); }
  const texts = textNodes(n).length;
  if (BTN.test(name) && texts <= 1) return 'button';
  if (INPUT.test(name) && texts <= 2) return 'input';
  return null;
}

function checkText(t) {
  if (get(t, 'hasMissingFont')) err(t, 'missingFont', '字体缺失,渲染为替代字体');
  const fams = t.getStyledTextSegments(['fontName']).filter(s => CJK.test(s.characters)).map(s => s.fontName.family);
  for (const f of fams) if (!CJK_FONT.test(f)) { err(t, 'cjkFont', '中文文本使用 ' + f + '(无中文字形,回退渲染会偏移),换 PingFang SC / Noto Sans SC'); break; }
  const box = t.absoluteBoundingBox, ink = get(t, 'absoluteRenderBounds');
  if (box && ink && (ink.height > box.height + 2 || ink.width > box.width + 2)) err(t, 'textClipped', '字形超出文字框(行高过小或固定框过窄):ink ' + r1(ink.width) + '×' + r1(ink.height) + ' > box ' + r1(box.width) + '×' + r1(box.height));
  let frame = t.parent; while (frame && !('clipsContent' in frame)) frame = frame.parent;
  if (box && frame && frame.absoluteBoundingBox) {
    const f = frame.absoluteBoundingBox;
    if (box.x < f.x - 0.5 || box.y < f.y - 0.5 || box.x + box.width > f.x + f.width + 0.5 || box.y + box.height > f.y + f.height + 0.5)
      err(t, 'textOverflow', '文字框超出父容器 ' + frame.name);
  }
  const fg = solidOf(t), bg = bgOf(t), fs = get(t, 'fontSize');
  if (fg && bg && typeof fs === 'number') {
    const w = get(t, 'fontWeight'), large = fs >= 24 || (fs >= 18.66 && typeof w === 'number' && w >= 700);
    const cr = contrast(fg, bg);
    if (cr < (large ? 3 : 4.5)) err(t, 'contrast', '对比度 ' + r1(cr) + ':1 < ' + (large ? 3 : 4.5) + ':1');
  }
}

function checkControl(n, kind) {
  const b = n.absoluteBoundingBox, h = b.height, w = b.width;
  const texts = textNodes(n), label = texts.find(t => get(t, 'layoutSizingHorizontal') === 'FILL') || texts[0];
  const auto = n.layoutMode === 'HORIZONTAL';
  const pad = auto ? [n.paddingTop, n.paddingRight, n.paddingBottom, n.paddingLeft] : null;
  const m = { id: n.id, name: n.name, kind, w: r1(w), h: r1(h), pad };
  if (!auto) err(n, 'noAutoLayout', kind + ' 未用水平 auto-layout(文字靠手摆坐标必然偏);重建为 layoutMode=HORIZONTAL + counterAxisAlignItems=CENTER');
  if (STD_H.indexOf(Math.round(h)) < 0) warn(n, 'height', '高度 ' + r1(h) + ' 不在标准档 ' + STD_H.join('/'));
  if (MOBILE && h < 44) warn(n, 'touchTarget', '高度 ' + r1(h) + ' < 44(移动端触控目标不足)');
  if (kind === 'button' && !label) {
    if (Math.abs(w - h) > 0.5) err(n, 'iconOnlySquare', '仅图标按钮应为正方形:' + r1(w) + '×' + r1(h));
    controls.push(m); return;
  }
  if (!label) { err(n, 'noText', kind + ' 内没有文字节点'); controls.push(m); return; }
  const lb = label.absoluteBoundingBox, fs = get(label, 'fontSize'), lh = lhPx(label);
  m.font = fs + '/' + (lh || 'AUTO') + ' ' + (label.fontName !== figma.mixed ? label.fontName.family + ' ' + label.fontName.style : 'mixed');
  m.dy = r1(cy(lb) - cy(b));
  if (Math.abs(m.dy) > 0.5) err(n, 'labelOffY', '文字行框垂直偏离中心 ' + m.dy + 'px(≤0.5);检查 counterAxisAlignItems=CENTER、上下内边距相等、文字框未固定高度');
  const ink = get(label, 'absoluteRenderBounds');
  if (ink) {
    m.dyInk = r1(cy(ink) - cy(b));
    if (typeof fs === 'number' && Math.abs(m.dyInk) > Math.max(1.5, 0.12 * fs)) warn(n, 'opticalY', '字形视觉中心偏离 ' + m.dyInk + 'px:字体度量异常或回退字体');
  }
  if (!lh) warn(n, 'autoLineHeight', '文字行高为 AUTO,应设像素行高');
  else if (lb.height > lh * 1.2) err(n, 'labelWraps', '文字折行(框高 ' + r1(lb.height) + ' > 行高 ' + lh + ')');
  if (label.textAutoResize === 'NONE') err(n, 'fixedTextBox', '文字框为固定尺寸,内容与框不同步;改 WIDTH_AND_HEIGHT(按钮)或 HEIGHT+FILL(输入框)');
  if (typeof fs === 'number') {
    m.fs_h = r1(fs / h * 100) / 100;
    if (m.fs_h < 0.25 || m.fs_h > 0.5) err(n, 'fontRatio', '字号/高度 = ' + fs + '/' + r1(h) + ' = ' + m.fs_h + ',超出 0.25–0.5(按规格表重选尺寸档)');
    else if (m.fs_h < 0.28 || m.fs_h > 0.45) warn(n, 'fontRatio', '字号/高度 = ' + m.fs_h + ',偏离常见 0.28–0.45');
  }
  const padX = auto ? n.paddingLeft : lb.x - b.x;
  m.padX_h = r1(padX / h * 100) / 100;
  if (kind === 'button') {
    if (auto && (pad[1] !== pad[3] || pad[0] !== pad[2])) err(n, 'padAsym', '内边距不对称 [上右下左]=' + pad.join(','));
    const kids = n.children.filter(c => c.visible !== false).map(c => c.absoluteBoundingBox);
    const left = Math.min.apply(null, kids.map(k => k.x)), right = Math.max.apply(null, kids.map(k => k.x + k.width));
    m.dx = r1((left + right) / 2 - cx(b));
    if (Math.abs(m.dx) > 0.5) err(n, 'contentOffX', '内容水平偏离中心 ' + m.dx + 'px;检查 primaryAxisAlignItems=CENTER、左右内边距相等');
    if (get(label, 'layoutSizingHorizontal') === 'FILL' && label.textAlignHorizontal !== 'CENTER') err(n, 'labelAlign', '撑满宽度的按钮文字未居中对齐(textAlignHorizontal=' + label.textAlignHorizontal + ')');
    if (w < h - 0.5) err(n, 'narrow', '按钮宽 ' + r1(w) + ' < 高 ' + r1(h));
    if (m.padX_h < 0.2 || m.padX_h > 1) err(n, 'padRatio', '水平内边距/高度 = ' + m.padX_h + ',超出 0.2–1.0');
    else if (m.padX_h < 0.3 || m.padX_h > 0.65) warn(n, 'padRatio', '水平内边距/高度 = ' + m.padX_h + ',偏离常见 0.3–0.65');
  } else {
    const strokes = (get(n, 'strokes') || []).filter(p => p.visible !== false);
    const fill = solidOf(n), bg = bgOf(n);
    if (!(strokes.length && get(n, 'strokeWeight')) && !(fill && bg && contrast(fill, bg) > 1.1)) err(n, 'noBoundary', '输入框无描边且填充与背景无区分');
    if (padX < 8 || padX > 20) warn(n, 'padX', '水平内边距 ' + r1(padX) + ' 不在 8–20');
  }
  const fg = solidOf(label), bg = solidOf(n) || bgOf(n);
  if (fg && bg) m.contrast = r1(contrast(fg, bg));
  controls.push(m);
}

function checkVariants(set) {
  const groups = {};
  for (const c of set.children) {
    const mm = /Size=([^,]+)/.exec(c.name), k = mm ? mm[1].trim() : '*';
    (groups[k] = groups[k] || []).push(c);
  }
  for (const k of Object.keys(groups)) {
    const hs = groups[k].map(c => Math.round(c.height));
    if (Math.max.apply(null, hs) !== Math.min.apply(null, hs)) err(set, 'variantHeight', 'Size=' + k + ' 的变体高度不一致:' + groups[k].map(c => c.name + '=' + Math.round(c.height)).join('; '));
  }
}

async function walk(n, inControl) {
  if (n.visible === false || seen++ > 3000) return;
  if (n.type === 'TEXT') { checkText(n); return; }
  if (n.type === 'COMPONENT_SET') checkVariants(n);
  let kind = null;
  if (!inControl) { kind = await kindOf(n); if (kind) checkControl(n, kind); }
  if ('children' in n) for (const c of n.children) await walk(c, inControl || !!kind);
}
await walk(target, false);
return { pass: ERR.length === 0, errors: ERR, warns: WARN, controls };
```

读结果:`pass:false` 时逐条按 `[code]` 修(多半是重建为规格化 auto-layout,而不是挪坐标),改完**对同一子树重跑**直到 `pass:true`;最多 3 轮仍不过,带着 `errors` 原文与 `controls` 量测表向用户报告。`warn` 需要逐条给出「有意为之」的理由或修掉。`controls` 每行是控件的实测数据(尺寸、内边距、`font`、`fs_h` 字号/高度比、`padX_h`、`dx/dy` 几何偏移、`dyInk` 字形偏移、`contrast`),交付时原样列给用户。
