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
  if ('layoutMode' in node && node.layoutMode && node.layoutMode !== 'NONE') {
    out.layout = { mode: node.layoutMode, gap: node.itemSpacing,
      padding: [node.paddingTop, node.paddingRight, node.paddingBottom, node.paddingLeft] };
  }
  if ('cornerRadius' in node && typeof node.cornerRadius === 'number' && node.cornerRadius) out.cornerRadius = node.cornerRadius;
  if ('children' in node && node.children) {
    if (depth >= SNAPSHOT_MAX_DEPTH) {
      out.truncated = true;
    } else {
      out.children = [];
      for (const child of node.children) {
        if (budget.count >= SNAPSHOT_MAX_NODES) { out.truncated = true; break; }
        out.children.push(snapshot(child, depth + 1, budget));
      }
      if (!out.truncated && out.children.length < node.children.length) out.truncated = true;
    }
  }
  return out;
}

const boundedSnapshot = node => snapshot(node, 0, { count: 0 });

async function locateTarget(job) {
  // 绑定来自 figma.com 链接，fileKey 必非空；当前文件无 fileKey（本地 .fig）即非绑定文件，拒绝。
  if (figma.fileKey !== job.binding.fileKey)
    throw Error('当前文件与绑定不一致：' + (figma.fileKey || '无 fileKey（本地文件）') + ' ≠ ' + job.binding.fileKey + '；请在绑定的云端文件中运行插件');
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
    // exportAsync 返回 Uint8Array 按预览图通道回传（ui 侧转 base64 压缩体积），仍携带 before/after 结构证据。
    if (raw instanceof Uint8Array) {
      figma.ui.postMessage({ type: 'result', id: job.id, ok: true, png: raw, before, after });
      return;
    }
    let output;
    try { output = raw === undefined ? null : JSON.parse(JSON.stringify(raw)); }
    catch { throw Error('返回值无法序列化；脚本可能已执行，请核实现状后重试'); }
    figma.ui.postMessage({ type: 'result', id: job.id, ok: true, output, before, after });
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
