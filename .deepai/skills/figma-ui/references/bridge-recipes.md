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
      if (x === undefined || x === null) vals.push(null);
      else if (x.type === 'VARIABLE_ALIAS') { const a = await figma.variables.getVariableByIdAsync(x.id); vals.push('→' + (a ? a.name : x.id)); }
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

反向导入(`figma.createNodeFromSvg`)时**禁用 `A/a` 圆弧参数**:实测 16×16 viewBox 的 3/4 圆弧 `M12 3a9 9 0 1 0 9 9` 导入后矢量包围盒 13×39,超出 viewBox 且比例错误;同形改用三段 C 贝塞尔 `M12 3 C17 3 21 7 21 12 C21 17 17 21 12 21 C7 21 3 17 3 12` 导入后 12×12 规整。纯线段/折线路径不受影响,spinner/loading 类图标是重灾区。导入后读 after 快照核对矢量尺寸是否落在 viewBox 内。

## 4. 构建辅助(写 Figma 时整段贴在脚本开头)

```js
const rgb = h => { const n = parseInt(h.slice(1, 7), 16); return { r: (n >> 16 & 255) / 255, g: (n >> 8 & 255) / 255, b: (n & 255) / 255 }; };
const solid = h => ({ type: 'SOLID', color: rgb(h) });
const needAuto = (p, what) => { if (!('layoutMode' in p) || p.layoutMode === 'NONE') throw Error(what + ' 需要父级 ' + p.name + ' 是 auto-layout(先 ensureAuto)'); };
const ensureAuto = (f, dir) => {
  if (f.layoutMode !== 'NONE') return;
  if (f.children.length) throw Error(f.name + ' 非 auto-layout 且已有内容,直接转换会重排;在其内新建 auto-layout 容器,或先与用户确认');
  const w = f.width, h = f.height;
  f.layoutMode = dir || 'VERTICAL';
  f.resize(w, h);
  f.layoutSizingHorizontal = 'FIXED'; f.layoutSizingVertical = 'FIXED';
};
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
  if (o.w === 'FILL' || o.h === 'FILL') needAuto(parent, name + ' 的 FILL');
  parent.appendChild(f);
  if (typeof o.w === 'number' || typeof o.h === 'number') f.resize(typeof o.w === 'number' ? o.w : f.width, typeof o.h === 'number' ? o.h : f.height);
  f.layoutSizingHorizontal = o.w === 'FILL' ? 'FILL' : typeof o.w === 'number' ? 'FIXED' : 'HUG';
  f.layoutSizingVertical = o.h === 'FILL' ? 'FILL' : typeof o.h === 'number' ? 'FIXED' : 'HUG';
  return f;
};
const FONTS = (await figma.listAvailableFontsAsync()).map(f => f.fontName.family + '|' + f.fontName.style);
const STYLES = {
  100: ['Thin', 'Hairline'], 200: ['ExtraLight', 'Extra Light', 'UltraLight', 'Ultralight'], 300: ['Light'],
  400: ['Regular', 'Normal'], 500: ['Medium'], 600: ['Semi Bold', 'SemiBold', 'Semibold', 'DemiBold'],
  700: ['Bold'], 800: ['ExtraBold', 'Extra Bold', 'Heavy'], 900: ['Black', 'Heavy'],
};
const LH = { 12: 16, 13: 18, 14: 20, 16: 24, 18: 28, 20: 28, 24: 32, 30: 36, 36: 40 };
const HANGUL = /[\u1100-\u11ff\u3130-\u318f\uac00-\ud7af]/, KANA = /[\u3040-\u30ff]/;
const CJK = /[\u1100-\u11ff\u3000-\u30ff\u3130-\u318f\u3400-\u9fff\uac00-\ud7af\uf900-\ufaff\uff00-\uffef]/;
const CJK_FONT = /PingFang|Noto Sans (SC|TC|HK|JP|KR|CJK)|Noto Serif (SC|TC|JP|KR|CJK)|Source Han|思源|Hiragino|YaHei|HarmonyOS|MiSans|PuHuiTi|Heiti|Songti|Apple SD Gothic|Pretendard|Malgun|Yu Gothic|Meiryo/i;
const CJK_FAMILIES = {
  ko: ['Apple SD Gothic Neo', 'Noto Sans KR', 'Pretendard', 'Malgun Gothic'],
  ja: ['Hiragino Sans', 'Hiragino Kaku Gothic ProN', 'Noto Sans JP', 'Yu Gothic'],
  zh: ['PingFang SC', 'Noto Sans SC', 'Source Han Sans SC', 'Source Han Sans CN', 'Noto Sans CJK SC', 'HarmonyOS Sans SC', 'MiSans', 'Alibaba PuHuiTi', 'Hiragino Sans GB', 'Microsoft YaHei'],
};
const fontFor = (chars, family, weight) => {
  const w = weight || 400;
  if (!STYLES[w]) throw Error('不支持的字重 ' + w + ',用 100–900 的整百值');
  let fams = [family || 'Inter'];
  if (CJK.test(chars)) {
    fams = CJK_FAMILIES[HANGUL.test(chars) ? 'ko' : KANA.test(chars) ? 'ja' : 'zh'];
    if (family && CJK_FONT.test(family)) fams = [family].concat(fams);
  }
  for (const fam of fams) for (const s of STYLES[w]) if (FONTS.indexOf(fam + '|' + s) >= 0) return { family: fam, style: s };
  throw Error('无可用字体:' + fams.join('/') + ' weight ' + w + ';先 listAvailableFontsAsync 核对,不要回退到无对应字形的字体');
};
const label = async (chars, o, parent) => {
  if (o.w === 'FILL') needAuto(parent, (o.name || chars) + ' 的 FILL');
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

`label` 按字重选字体样式(各字体的 600 叫法不同:`Semi Bold` / `SemiBold` / `Semibold`),含中日韩字符时只在对应文种字体里选(调用方传入的 `family` 是中日韩字体时优先)——Inter 等拉丁字体没有这些字形,Figma 回退渲染会让文字视觉偏移,这是「按钮文字位置不对」的常见根因。行高总是显式像素值。

示例(在绑定画板内搭一张卡片;Workflow D 新建的空画板默认不是 auto-layout,先 `ensureAuto`):

```js
// ……粘贴 §4 辅助……
ensureAuto(target, 'VERTICAL');
target.paddingTop = target.paddingBottom = 24; target.paddingLeft = target.paddingRight = 16;
const card = box('OrderCard', { dir: 'VERTICAL', gap: 12, pad: [16, 16, 16, 16], fill: '#FFFFFF', radius: 16, stroke: '#E5E7EB', w: 'FILL' }, target);
const head = box('Header', { dir: 'HORIZONTAL', justify: 'SPACE_BETWEEN', align: 'CENTER', w: 'FILL' }, card);
await label('订单 #1024', { size: 16, lh: 24, weight: 600, name: 'Title' }, head);
await label('已发货', { size: 12, lh: 16, color: '#047857', name: 'Status' }, head);
await label('预计 3 天内送达,请保持电话畅通。', { size: 14, lh: 20, color: '#4B5563', w: 'FILL', name: 'Body' }, card);
return await card.exportAsync({ format: 'PNG', constraint: { type: 'SCALE', value: 2 } });
```

新节点 ID 从 after 快照读。**预览图必须是 `return` 的顶层值**:旧版插件只把顶层 Uint8Array 送进图像通道,嵌在对象里会被序列化成巨大的数字键对象并撞上 24KB 截断;需要同时拿数据就分两次 `run`。

顺序铁律:`layoutMode` → `appendChild` → `resize(显式尺寸)` → `layoutSizing*`。`resize()` 会把改动的轴锁回 FIXED,所以必须在设置 HUG/FILL **之前**调用;需要按内容撑开的轴(如控件高度)在子节点加入后再设 HUG。`FILL` 只能在父级是 auto-layout 且**已 append** 之后设置,否则抛错;`HUG` 只对 auto-layout 容器和文字有效。每次构建后跑 §8 验收。

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
const V = name => { const v = vars.find(x => x.name === name); if (!v) throw Error('变量不存在:' + name + ',先跑上面的创建脚本'); return v; };
const card = target.findOne(n => n.name === 'OrderCard');
if (!card) throw Error('未找到 OrderCard');
card.fills = [figma.variables.setBoundVariableForPaint({ type: 'SOLID', color: { r: 1, g: 1, b: 1 } }, 'color', V('color/surface'))];
for (const f of ['itemSpacing', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft']) card.setBoundVariable(f, V('spacing/4'));
for (const f of ['topLeftRadius', 'topRightRadius', 'bottomRightRadius', 'bottomLeftRadius']) card.setBoundVariable(f, V('radius/lg'));
for (const t of card.findAll(n => n.type === 'TEXT' && Array.isArray(n.fills) && n.fills.length))
  t.fills = [figma.variables.setBoundVariableForPaint(t.fills[0], 'color', V('color/text/primary'))];
return card.id;
```

预览暗色(独立脚本,各次 `run` 不共享变量,collection 需重新查):

```js
const col = (await figma.variables.getLocalVariableCollectionsAsync()).find(c => c.name === 'Tokens');
const dark = col && col.modes.find(m => m.name === 'Dark');
if (!dark) throw Error('没有 Tokens 集合或 Dark mode');
target.setExplicitVariableModeForCollection(col, dark.modeId);
return await target.exportAsync({ format: 'PNG', constraint: { type: 'SCALE', value: 2 } });
```

核对完复原:`const col = (await figma.variables.getLocalVariableCollectionsAsync()).find(c => c.name === 'Tokens'); target.clearExplicitVariableModeForCollection(col); return null`。文字绑定可用 `t.setBoundVariable('fontSize', v)`(亦支持 lineHeight / letterSpacing / fontWeight / fontFamily)。

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
  c.name = 'size=' + size + ', variant=' + v.name;
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
  c.name = 'size=' + size + ', state=' + state.name;
  control(c, s);
  c.itemSpacing = 8;
  c.fills = [solid('#FFFFFF')];
  c.strokes = [solid(state.border)]; c.strokeWeight = state.borderWidth || 1; c.strokeAlign = 'INSIDE';
  target.appendChild(c);
  c.resize(280, s.h);
  c.layoutSizingHorizontal = 'FIXED';
  const t = await label(placeholder, { size: s.size, lh: s.lh, color: state.text || '#6B7280', name: 'Placeholder', w: 'FILL' }, c);
  t.textTruncation = 'ENDING'; t.maxLines = 1;
  c.layoutSizingVertical = 'HUG';
  return c;
};
const VARIANTS = [{ name: 'primary', bg: '#4F46E5', fg: '#FFFFFF' }, { name: 'secondary', bg: '#FFFFFF', fg: '#111827', border: '#D1D5DB' }];
const btns = [];
for (const size of ['sm', 'md', 'lg']) for (const v of VARIANTS) btns.push(await button('确认', size, v));
const btnSet = figma.combineAsVariants(btns, target);
btnSet.name = 'Button';
btnSet.layoutMode = 'HORIZONTAL'; btnSet.itemSpacing = 16; btnSet.counterAxisAlignItems = 'CENTER';
btnSet.paddingTop = btnSet.paddingRight = btnSet.paddingBottom = btnSet.paddingLeft = 16;
btnSet.layoutSizingHorizontal = 'HUG'; btnSet.layoutSizingVertical = 'HUG';
const key = btnSet.addComponentProperty('Label', 'TEXT', '确认');
for (const c of btnSet.children) c.findOne(n => n.name === 'Label').componentPropertyReferences = { characters: key };
const ins = [];
for (const st of [{ name: 'default', border: '#D1D5DB' }, { name: 'focus', border: '#4F46E5', borderWidth: 2 }, { name: 'error', border: '#DC2626' }])
  ins.push(await input('请输入手机号', 'md', st));
const inputSet = figma.combineAsVariants(ins, target);
inputSet.name = 'Input';
inputSet.layoutMode = 'VERTICAL'; inputSet.itemSpacing = 16;
inputSet.paddingTop = inputSet.paddingRight = inputSet.paddingBottom = inputSet.paddingLeft = 16;
inputSet.layoutSizingHorizontal = 'HUG'; inputSet.layoutSizingVertical = 'HUG';
return { button: btnSet.id, input: inputSet.id, labelKey: key };
```

使用实例:`const i = btnSet.defaultVariant.createInstance(); parent.appendChild(i); const k = Object.keys(i.componentProperties).find(k => k.indexOf('Label#') === 0); i.setProperties({ size: 'lg', variant: 'secondary', [k]: '保存' });`。
命名约定 `属性=值`,属性名与代码 props 同名(`size` / `variant` / `state`,见 SKILL「Figma 设计端规范」);§8 验收靠 `size=` 分组比对变体高度。

组件集开启 auto-layout 后显式设两轴 HUG,否则集合保持合并时的固定尺寸,变体溢出(§8 报 `overflow`)。
Input 的顺序:`resize(280, s.h)` 在设 sizing 之前(resize 会锁回 FIXED),文字加入后再把纵向设回 HUG,高度由「行高 + 2×内边距」得出,恰等于 `s.h`。

**控件居中（正解优先）**：对齐字段名是 `primaryAxisAlignItems` / `counterAxisAlignItems`——这是 auto-layout 居中的**首选正解**（实测全页 102 个 HORIZONTAL 容器一次性设 `counterAxisAlignItems='CENTER'` 全部生效，chip/pill/按钮文字几何偏差归零）。交叉轴记住一句：HORIZONTAL 容器的垂直居中与 VERTICAL 容器的水平居中都是 `counterAxisAlignItems='CENTER'`；主轴用 `primaryAxisAlignItems`。写成 `*AlignWidgets` 等不存在的名字，赋值**不报错**、读回 undefined，构建 ok:true 但视觉全错（实测按钮文字贴左上、输入框文字贴顶、chip 收缩贴文字）——写完读回确认，§8 也会以 `labelOffY` / `contentOffX` 拦下。仅当字段名正确仍不生效（罕见）或 `layoutMode='NONE'` 绝对布局场景，才用兜底三板斧：
- 水平居中：两个 `layoutGrow=1` 的空 spacer frame 夹住内容组（主轴为 MIN 时）；
- 文字垂直居中：文字子级 `layoutAlign='STRETCH'` 拉满容器高 + `textAlignVertical='CENTER'`；
- SVG/图标垂直居中：非 auto-layout 的固定高包装 frame + 内部矢量按 `(高 − 图标高)/2` 绝对定位。

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
      if (n[f] % 2 && !bound(n, f)) { add('offGrid', n); break; }
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
| `offGrid` | 奇数 px 无法吸附 Tailwind 刻度(2px 倍数可用 `.5` 档,如 6 → `p-1.5`) | 调到 2 的倍数(布局间距优先 4 的倍数)或绑定 spacing 变量 |
| `defaultName` | 代码命名无语义 | 英文语义命名(`Header`、`Price`、`UserProfileCard`) |
| `fixedTextBox` | 文案变长溢出 | `textAutoResize` 改 HEIGHT 或 WIDTH_AND_HEIGHT |
| `fontSizes` 超过 6–7 档 | 字阶混乱 | 收敛到字阶表 |

## 8. 量测验收(每次构建/修改后必跑,error 清零才算完成)

模型看不到 preview.png——deepai 的工具结果只带图片路径,不把像素送进模型。所以「看起来对」不是证据,**本脚本的量测数字才是**。
不变量与阈值见 [component-specs.md](component-specs.md) §3;控件按命名识别(`button|btn|按钮`、`input|field|search|输入|搜索`)。`node` 传刚构建/修改的子树。

```js
const ERR = [], WARN = [], controls = [];
const LIMIT = 3000;
let seen = 0, texts = 0, truncated = false;
const get = (n, k) => { try { return n[k]; } catch (e) { return undefined; } };
const r1 = v => Math.round(v * 10) / 10;
const err = (n, code, msg) => { if (ERR.length < 60) ERR.push(n.id + ' ' + n.name + ' [' + code + '] ' + msg); };
const warn = (n, code, msg) => { if (WARN.length < 40) WARN.push(n.id + ' ' + n.name + ' [' + code + '] ' + msg); };
const cx = b => b.x + b.width / 2, cy = b => b.y + b.height / 2;
const CJK = /[\u1100-\u11ff\u3000-\u30ff\u3130-\u318f\u3400-\u9fff\uac00-\ud7af\uf900-\ufaff\uff00-\uffef]/;
const CJK_FONT = /PingFang|Noto Sans (SC|TC|HK|JP|KR|CJK)|Noto Serif (SC|TC|JP|KR|CJK)|Source Han|思源|Hiragino|YaHei|HarmonyOS|MiSans|PuHuiTi|Heiti|Songti|Apple SD Gothic|Pretendard|Malgun|Yu Gothic|Meiryo/i;
const BTN = /button|btn|按钮/i, INPUT = /input|field|search|输入|搜索/i;
const STD_H = [24, 28, 32, 36, 40, 44, 48, 52, 56];
let screen = target;
while (screen.parent && screen.parent.type !== 'PAGE' && screen.parent.type !== 'SECTION') screen = screen.parent;
const MOBILE = screen.type === 'FRAME' && screen.width <= 480 && screen.height >= 560;
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

// 胶囊(pill/chip/tag):几何识别(圆角≥高/2 且宽>高),不依赖命名——V2HUB 实测
// chip 被 AUTO 收缩到 13px 贴文字、tag 文字未垂直居中均靠这两条拦下。
const pillLike = n => (n.type === 'FRAME' || n.type === 'COMPONENT') && get(n, 'layoutMode') === 'HORIZONTAL' && get(n, 'cornerRadius') >= n.height / 2 - 1 && n.width > n.height + 4 && n.height >= 10 && n.height <= 44;
function checkPill(n) {
  if (n.height < 18) { err(n, 'pillCollapsed', '胶囊高 ' + r1(n.height) + ' < 18(counterAxis AUTO 收缩贴文字;用上下 padding 撑高,勿定宽定高)'); return; }
  for (const t of textNodes(n)) {
    const dy = r1(cy(t.absoluteBoundingBox) - cy(n.absoluteBoundingBox));
    if (Math.abs(dy) > 1) err(n, 'pillOffY', '胶囊文字垂直偏离中心 ' + dy + 'px(counterAxisAlignItems=CENTER)');
  }
}
// 图标+文字小栈(tab item 等):宽≤80 高≤64、2–3 子级、恰一个方形图标框+一个文字。
// 对 layoutMode=NONE 的绝对定位栈同样适用——V2HUB 实测 tab 文字偏离中心 26px 即此拦下。
function checkStack(n) {
  if (n.width > 80 || n.height > 64) return;
  const kids = n.children.filter(c => c.visible !== false);
  if (kids.length < 2 || kids.length > 3) return;
  const icon = kids.find(c => c.type === 'FRAME' && c.width >= 14 && c.width <= 48 && !textNodes(c).length);
  const label = kids.find(c => c.type === 'TEXT');
  if (!icon || !label) return;
  const dx = r1(cx(label.absoluteBoundingBox) - cx(n.absoluteBoundingBox));
  if (Math.abs(dx) > 1) err(n, 'stackOffX', '栈文字水平偏离中心 ' + dx + 'px(counterAxisAlignItems=CENTER;NONE 布局按 (槽宽−文字宽)/2 绝对居中)');
  const dxi = r1(cx(icon.absoluteBoundingBox) - cx(n.absoluteBoundingBox));
  if (Math.abs(dxi) > 1) err(n, 'stackOffX', '栈图标水平偏离中心 ' + dxi + 'px');
}
// 图标框(14–32 方形、仅矢量):矢量联合 bbox 应充满图标框且不越界。
// V2HUB 实测 createNodeFromSvg 对 A/a 圆弧解析出 13×39 畸形 bbox,即此拦下。
function checkIcon(n) {
  if (n.type !== 'FRAME' || Math.abs(n.width - n.height) > 1 || n.width < 14 || n.width > 32) return;
  if (textNodes(n).length) return;
  const vs = n.children.filter(c => c.type === 'VECTOR');
  if (!vs.length) return;
  const bs = vs.map(v => v.absoluteBoundingBox);
  const u = { x: Math.min.apply(null, bs.map(b => b.x)), y: Math.min.apply(null, bs.map(b => b.y)) };
  u.width = Math.max.apply(null, bs.map(b => b.x + b.width)) - u.x; u.height = Math.max.apply(null, bs.map(b => b.y + b.height)) - u.y;
  const fb = n.absoluteBoundingBox;
  if (u.x < fb.x - 1 || u.y < fb.y - 1 || u.x + u.width > fb.x + n.width + 1 || u.y + u.height > fb.y + n.height + 1)
    err(n, 'iconBBox', '矢量联合框 ' + r1(u.width) + '×' + r1(u.height) + ' 超出图标框 ' + r1(n.width) + '×' + r1(n.height) + '(SVG 导入畸变:查 A/a 圆弧,改 C 贝塞尔重写)');
  else if (u.width < n.width * 0.6 || u.height < n.height * 0.6)
    err(n, 'iconBBox', '矢量联合框 ' + r1(u.width) + '×' + r1(u.height) + ' 未充满图标框 ' + r1(n.width) + '×' + r1(n.height) + '(<60%,疑似 arc 畸变导入)');
}

function checkVariants(set) {
  const groups = {};
  for (const c of set.children) {
    const mm = /size=([^,]+)/i.exec(c.name), k = mm ? mm[1].trim() : '*';
    (groups[k] = groups[k] || []).push(c);
  }
  for (const k of Object.keys(groups)) {
    const hs = groups[k].map(c => Math.round(c.height));
    if (Math.max.apply(null, hs) !== Math.min.apply(null, hs)) err(set, 'variantHeight', 'size=' + k + ' 的变体高度不一致:' + groups[k].map(c => c.name + '=' + Math.round(c.height)).join('; '));
  }
}

const inside = (a, b) => a.x >= b.x - 0.5 && a.y >= b.y - 0.5 && a.x + a.width <= b.x + b.width + 0.5 && a.y + a.height <= b.y + b.height + 0.5;
const overlap = (a, b) => Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x) > 1 && Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y) > 1;
function checkLayout(n) {
  const kids = n.children.filter(c => c.visible !== false && c.absoluteBoundingBox);
  const pb = n.absoluteBoundingBox, mode = get(n, 'layoutMode');
  if (!pb || !kids.length) return;
  if (mode === 'HORIZONTAL' || mode === 'VERTICAL') {
    for (const c of kids) if (get(c, 'layoutPositioning') !== 'ABSOLUTE' && c.type !== 'TEXT' && !inside(c.absoluteBoundingBox, pb))
      err(c, 'overflow', '流式子级超出 auto-layout 父级 ' + n.name + '(常见于 FIXED 子级宽于父级,改 FILL 或缩小)');
  } else if (mode === 'NONE' && n.type !== 'COMPONENT_SET') {
    const hasText = c => c.type === 'TEXT' || (typeof c.findOne === 'function' && !!c.findOne(x => x.type === 'TEXT'));
    for (let i = 0; i < kids.length; i++) for (let j = i + 1; j < kids.length; j++) {
      const a = kids[i], b = kids[j], A = a.absoluteBoundingBox, B = b.absoluteBoundingBox;
      if (inside(A, B) || inside(B, A)) continue;
      if (overlap(A, B) && hasText(a) && hasText(b)) err(a, 'overlap', '与 ' + b.name + ' 重叠且都含文字');
      for (const [ea, eb, axis] of [[A.x, B.x, '左'], [A.x + A.width, B.x + B.width, '右'], [A.y, B.y, '上']]) {
        const d = Math.abs(ea - eb);
        if (d > 0.5 && d < 3) { warn(a, 'nearMiss', '与 ' + b.name + ' 的' + axis + '边差 ' + r1(d) + 'px,疑似未对齐'); break; }
      }
    }
  }
}

async function walk(n, inControl) {
  if (n.visible === false) return;
  if (seen++ >= LIMIT) { truncated = true; return; }
  if (n.type === 'TEXT') { texts++; checkText(n); return; }
  if (n.type === 'COMPONENT_SET') checkVariants(n);
  let kind = null;
  if (!inControl) { kind = await kindOf(n); if (kind) checkControl(n, kind); }
  if ('children' in n) { checkLayout(n); if (!inControl) { if (pillLike(n)) checkPill(n); checkStack(n); checkIcon(n); } for (const c of n.children) await walk(c, inControl || !!kind); }
}
await walk(target, false);
return {
  pass: ERR.length === 0 && !truncated, truncated,
  checked: { nodes: seen, texts, controls: controls.length, mobile: MOBILE },
  errors: ERR, warns: WARN, controls,
};
```

读结果:先看 `checked`——它说明这次验收**实际覆盖了什么**。`truncated:true`(超过 3000 节点)时 `pass` 必为 false,用 `node` 拆小子树重跑;刚构建了按钮/输入框却 `checked.controls` 为 0,说明命名没被识别(按 `button|input…` 规则改名后重跑),此时的 `pass:true` 不能证明控件正确。脚本覆盖控件几何、文字、流式溢出、文字重叠、近似未对齐、胶囊收缩/居中(`pillCollapsed`/`pillOffY`)、图标栈居中(`stackOffX`)、图标矢量畸变(`iconBBox`);页面级的结构问题(Group、缺 auto-layout、原始色值)由 §7 体检负责,两者都过才算页面验收通过。
`pass:false` 时逐条按 `[code]` 修(多半是重建为规格化 auto-layout,而不是挪坐标),改完**对同一子树重跑**直到 `pass:true`;最多 3 轮仍不过,带着 `errors` 原文与 `controls` 量测表向用户报告。`warn` 需要逐条给出「有意为之」的理由或修掉。`controls` 每行是控件的实测数据(尺寸、内边距、`font`、`fs_h` 字号/高度比、`padX_h`、`dx/dy` 几何偏移、`dyInk` 字形偏移、`contrast`),交付时原样列给用户。
