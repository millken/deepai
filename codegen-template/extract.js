// extract.js — 确定性抽取模板(设计→代码管线第①段)
// 本文件内容是提交给 figmabridge `run` 的 code(整段作为 run 的 code 参数),
// 不是 node 脚本。返回 { ir: {...} },由调用方落盘到 ir/design.json:
//   json.dump(ir, f, ensure_ascii=False, indent=2, sort_keys=True) + '\n'
//
// 语义化读取:图层名为主键(构建时规范命名是前提);children 按文档序;
// 组件集内递归稳定;屏级用「直接子级分区 + 每分区 findAll 取文本」扁平模式
// (不要深度递归屏级实例子树——场景代理会抛 TypeError,实测坑)。

await figma.loadAllPagesAsync();
const page = figma.root.children.find(p => p.name === 'V3 Redesign');  // ← 换成项目页面名

// 防御式属性读取:只用于【属性】——方法(findAll 等)会被 typeof 误杀,必须直调+try/catch
const safe = (o, k) => { try { const v = o[k]; return typeof v === 'function' ? undefined : v; } catch (e) { return undefined; } };
const hx = c => c ? '#' + [c.r, c.g, c.b].map(v => Math.round(v * 255).toString(16).padStart(2, '0')).toUpperCase() : null;

// 变量绑定解析:paint.boundVariables.color.id → 变量名 → var(--token-name)
const cols = await figma.variables.getLocalVariableCollectionsAsync();
const lt = cols.find(c => c.name === 'V3 Theme');                       // ← 换成项目主题集合名
const avs = await figma.variables.getLocalVariablesAsync();
const varName = {};
for (const v of avs) if (safe(v, 'variableCollectionId') === safe(lt, 'id')) varName[safe(v, 'id')] = safe(v, 'name');
const ref = p => {
  if (!p || safe(p, 'type') !== 'SOLID' || !safe(p, 'color')) return null;
  const bv = safe(p, 'boundVariables');
  const vn = bv && bv.color && bv.color.id ? varName[bv.color.id] : null;
  return vn ? 'var(--' + vn.replace(/([A-Z])/g, '-$1').toLowerCase() + ')' : hx(p.color);
};

const txt = n => {
  const fn = safe(n, 'fontName');
  const f0 = Array.isArray(safe(n, 'fills')) ? n.fills[0] : null;
  const s = safe(n, 'fontSize');
  const r = ref(f0);
  return { t: safe(n, 'characters'), size: typeof s === 'number' ? s : null,
    w: fn && fn.style === 'Bold' ? 700 : fn && fn.style === 'Medium' ? 500 : 400, c: r };
};
const box = n => {
  const o = {};
  const w = safe(n, 'width'), h = safe(n, 'height'), r = safe(n, 'cornerRadius');
  if (typeof w === 'number') o.w = Math.round(w * 10) / 10;
  if (typeof h === 'number') o.h = Math.round(h * 10) / 10;
  if (typeof r === 'number' && r) o.r = r;
  const f0 = Array.isArray(safe(n, 'fills')) ? n.fills[0] : null;
  const rr = ref(f0);
  if (rr && (f0.visible === undefined || f0.visible !== false)) o.fill = rr;
  return o;
};

// 组件集内递归(稳定;每节点防御,坏节点标 ERR 不炸整跑)
const tree = n => {
  const ty = safe(n, 'type');
  const nm = String(safe(n, 'name') || '').split('\n')[0].trim();
  if (ty === 'TEXT') return { k: 'text', ...txt(n) };
  if (ty === 'FRAME' || ty === 'COMPONENT' || ty === 'INSTANCE') {
    const o = { k: nm, ...box(n) };
    if (ty === 'INSTANCE') o.inst = true;
    const lm = safe(n, 'layoutMode');
    if (lm && lm !== 'NONE') {
      o.dir = lm === 'HORIZONTAL' ? 'h' : 'v';
      const g = safe(n, 'itemSpacing'); if (g) o.gap = g;
      const ps = [safe(n, 'paddingTop'), safe(n, 'paddingRight'), safe(n, 'paddingBottom'), safe(n, 'paddingLeft')];
      if (ps.every(x => x)) o.pad = ps;
    }
    const ks = safe(n, 'children');
    if (Array.isArray(ks) && ks.length) o.c = ks.map(k => { try { return tree(k); } catch (e) { return { k: 'ERR' }; } });
    return o;
  }
  return { k: nm, t: ty };
};

// ===== ① 组件集(每集独立 try;SETS 的 key 即 codegen 的 CSS 前缀) =====
const comps = {};
for (const [key, sid] of Object.entries({
  // ← 换成项目组件集 node-id(构建时记录;node-id 复制粘贴会变,长期以图层名为键维护)
  topicList: '985:16094',
  button: '990:16295',
})) {
  const set = await figma.getNodeByIdAsync(sid);
  const vs = safe(set, 'type') === 'COMPONENT_SET' ? set.children : [set];
  comps[key] = vs.map(v => { try { return { v: safe(v, 'name'), n: tree(v) }; } catch (e) { return { v: safe(v, 'name'), err: true }; } });
}

// ===== ② 屏(扁平:直接子级分区 + 每分区 findAll 取文本;SCREENS 换成项目屏名) =====
const screens = {};
for (const s of ['Topic-Hot']) {
  const f = page.children.find(c => c.name === s + ' / V3');
  if (!f) continue;
  const secs = [];
  for (const c of safe(f, 'children')) {
    const nm = String(safe(c, 'name') || c.type).split('\n')[0].trim();
    let texts = [];
    try { texts = c.findAll(x => safe(x, 'type') === 'TEXT').map(txt); } catch (e) {}
    secs.push({ name: nm, inst: safe(c, 'type') === 'INSTANCE', texts });
  }
  screens[s] = { w: Math.round(safe(f, 'width')), h: Math.round(safe(f, 'height')), secs };
}

return { ir: { v: 1, comps, screens } };
