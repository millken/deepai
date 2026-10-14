// deepai 原生 Figma 桥插件（借鉴 local-figma 架构思想，独立实现）。
// UI 轮询本地桥领取任务，主线程在 (figma, target) 作用域执行 JS，
// 快照与结果回传落盘。协议见 pkg/figmabridge/bridge.go。
figma.showUI(__html__, { width: 320, height: 110, themeColors: true });

const SNAPSHOT_MAX_NODES = 4000;
const SNAPSHOT_MAX_DEPTH = 32;

const hexByte = v => Math.round(v * 255).toString(16).padStart(2, '0');

function snapshot(node, depth, budget) {
  budget.count++;
  const out = { id: node.id, name: node.name, type: node.type };
  if (node.visible === false) out.visible = false;
  if (typeof node.width === 'number') { out.width = Math.round(node.width); out.height = Math.round(node.height); }
  if (typeof node.x === 'number') { out.x = Math.round(node.x); out.y = Math.round(node.y); }
  if (node.type === 'TEXT') {
    out.characters = node.characters;
    if (node.fontName && node.fontName !== figma.mixed) out.fontName = node.fontName;
    if (typeof node.fontSize === 'number') out.fontSize = node.fontSize;
    if (typeof node.textAutoResize === 'string' && node.textAutoResize !== 'NONE') out.textAutoResize = node.textAutoResize;
  }
  // Figma 对节点不具备的属性直接读取会抛异常（而非 undefined），用 in 收窄再读。
  if ('fills' in node && Array.isArray(node.fills) && node.fills.length) {
    const fills = [];
    for (const p of node.fills.slice(0, 4)) {
      if (p.type === 'SOLID' && p.color) fills.push('#' + hexByte(p.color.r) + hexByte(p.color.g) + hexByte(p.color.b));
      else fills.push(p.type);
    }
    out.fills = fills;
  }
  // 仅 H/V 可安全读完整 auto-layout 状态;GRID 上轴/间距属性会抛(见文件头约定)。
  // sizing/align 是静默塌陷(FIXED→AUTO)的第一现场,缺失会误判写入成功。
  const isHV = 'layoutMode' in node && (node.layoutMode === 'HORIZONTAL' || node.layoutMode === 'VERTICAL');
  if ('layoutMode' in node && node.layoutMode && node.layoutMode !== 'NONE') {
    out.layout = { mode: node.layoutMode };
    if (isHV) {
      out.layout.gap = node.itemSpacing;
      out.layout.padding = [node.paddingTop, node.paddingRight, node.paddingBottom, node.paddingLeft];
      out.layout.sizing = [node.primaryAxisSizingMode, node.counterAxisSizingMode];
      out.layout.align = [node.primaryAxisAlignItems, node.counterAxisAlignItems];
    }
  }
  if ('cornerRadius' in node && typeof node.cornerRadius === 'number' && node.cornerRadius) out.cornerRadius = node.cornerRadius;
  if ('children' in node && node.children) {
    if (depth >= SNAPSHOT_MAX_DEPTH) {
      out.truncated = true;
    } else {
      out.children = [];
      for (const child of node.children) {
        if (budget.count >= SNAPSHOT_MAX_NODES) { out.truncated = true; break; }
        const cs = snapshot(child, depth + 1, budget);
        if (isHV) {
          if (child.layoutAlign && child.layoutAlign !== 'INHERIT') cs.layoutAlign = child.layoutAlign;
          if (child.layoutGrow) cs.layoutGrow = child.layoutGrow;
        }
        out.children.push(cs);
      }
      if (!out.truncated && out.children.length < node.children.length) out.truncated = true;
    }
  }
  return out;
}

const boundedSnapshot = node => snapshot(node, 0, { count: 0 });

async function locateTarget(job) {
  // 绑定来自 figma.com 链接，fileKey 必非空；本地打开的 .fig 无 fileKey（undefined），
  // 放行并靠绑定 nodeId 兜底识别（.fig 保留云端节点 ID，找不到即报「绑定节点不存在」）；
  // 云端文件仍严格匹配 fileKey，防串文件。
  if (figma.fileKey && figma.fileKey !== job.binding.fileKey)
    throw Error('当前文件与绑定不一致：' + figma.fileKey + ' ≠ ' + job.binding.fileKey + '；请在正确文件中运行插件');
  const root = await figma.getNodeByIdAsync(job.binding.nodeId);
  if (!root) throw Error('绑定节点不存在：' + job.binding.nodeId + '；请重新 init');
  if (!job.targetNodeId || job.targetNodeId === job.binding.nodeId) return root;
  const target = await figma.getNodeByIdAsync(job.targetNodeId);
  if (!target) throw Error('目标节点不存在：' + job.targetNodeId);
  let p = target;
  while (p && p.id !== job.binding.nodeId) p = p.parent;
  if (!p) throw Error('目标 ' + job.targetNodeId + ' 不在绑定子树内，已拒绝执行');
  return target;
}

async function execute(job) {
  let target, before, executionStarted = false;
  try {
    target = await locateTarget(job);
    before = boundedSnapshot(target);
    // 前后各划一个 undo 边界，让单次脚本可用 Ctrl+Z 回退；不可用时静默跳过。
    try { if (typeof figma.commitUndo === 'function') figma.commitUndo(); } catch {}
    executionStarted = true;
    const raw = await new Function('figma', 'target', 'return (async()=>{\n' + job.code + '\n})()')(figma, target);
    try { if (typeof figma.commitUndo === 'function') figma.commitUndo(); } catch {}
    const after = target.removed ? null : boundedSnapshot(target);
    // exportAsync 的 Uint8Array 一律走图像通道（落盘为 preview*.png），深检测同时
    // 捕获嵌套在对象/数组里的 Uint8Array（如 { geo, png: await exportAsync(...) }），
    // 输出中留 {__png,bytes} 占位引用，避免 JSON 序列化成数字数组刷屏。命名与
    // bridge.go handleComplete 的落盘规则一致：第 1 张 preview.png，第 N 张 preview-N.png。
    const pngs = [];
    class CycleError extends Error {}
    // 进入标记、离开撤销：无环的共享引用（{a:s,b:s}）放行，真环拒绝；数组一并
    // 纳入，避免先在递归里栈溢出。循环引用错误原样上抛，不落进笼统的序列化 catch。
    const inStack = new Set();
    const extract = v => {
      if (v instanceof Uint8Array) {
        pngs.push(v);
        return { __png: pngs.length === 1 ? 'preview.png' : 'preview-' + pngs.length + '.png', bytes: v.length };
      }
      if (Array.isArray(v)) {
        if (inStack.has(v)) throw new CycleError('返回值含循环引用；脚本可能已执行，请核实现状后重试');
        inStack.add(v);
        try { return v.map(extract); } finally { inStack.delete(v); }
      }
      if (v && typeof v === 'object' && Object.getPrototypeOf(v) === Object.prototype) {
        if (inStack.has(v)) throw new CycleError('返回值含循环引用；脚本可能已执行，请核实现状后重试');
        inStack.add(v);
        try { const o = {}; for (const k of Object.keys(v)) o[k] = extract(v[k]); return o; } finally { inStack.delete(v); }
      }
      return v;
    };
    let output;
    try { output = extract(raw); if (output !== undefined) output = JSON.parse(JSON.stringify(output)); }
    catch (e) { if (e instanceof CycleError) throw e; throw Error('返回值无法序列化；脚本可能已执行，请核实现状后重试'); }
    const payload = { type: 'result', id: job.id, ok: true, output: output === undefined ? null : output, before, after };
    if (pngs.length) payload.pngs = pngs;
    figma.ui.postMessage(payload);
  } catch (e) {
    let after;
    try { if (target && !target.removed) after = boundedSnapshot(target); } catch {}
    figma.ui.postMessage({ type: 'result', id: job.id, ok: false, error: String(e),
      executionStarted, ...(before ? { before } : {}), ...(after ? { after } : {}) });
  }
}

figma.ui.onmessage = message => {
  if (message && message.type === 'job') void execute(message);
};
