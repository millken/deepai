#!/usr/bin/env node
// codegen.js — 确定性生成模板(设计→代码管线第③段)
// 输入:ir/design.json(② extract 产物)+ assets/(tokens/icon-paths)
// 输出:dist/(tokens.css + screens/*.html),双跑逐字节一致。
// 零依赖零 LLM:同输入必同输出(键排序、固定属性序、固定缩进、尾换行)。
//
// 用法:node codegen.js   (产物在 dist/,git diff 即变更)
// 验证:跑两次 + diff -r dist_backup dist → 应逐字节一致
//
// 本模板含两类渲染器示范:
//   - 类型化渲染(feed:筛选胶囊流 + 卡片列表 + tabbar)——对应资产层驱动的屏
//   - IR 组件段 → CSS 类(topicList 变体样式)——对应 extract 抽取的组件规格
// 新项目:按屏幕类型扩展 RENDERERS,数据进 assets/screen-defaults.json。

const fs = require('fs');
const path = require('path');
const ROOT = __dirname;
const IR = JSON.parse(fs.readFileSync(path.join(ROOT, 'ir', 'design.json'), 'utf8'));
const TOKENS = JSON.parse(fs.readFileSync(path.join(ROOT, 'assets', 'tokens.json'), 'utf8'));
const ICONS = JSON.parse(fs.readFileSync(path.join(ROOT, 'assets', 'icon-paths.json'), 'utf8'));
const DEFAULTS = JSON.parse(fs.readFileSync(path.join(ROOT, 'assets', 'screen-defaults.json'), 'utf8'));
const DIST = path.join(ROOT, 'dist');

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const kebab = n => n.replace(/([A-Z])/g, '-$1').toLowerCase();
const slug = s => s.toLowerCase().replace(/[^a-z0-9]+/g, '-');
const px = v => Math.round(v * 10) / 10 + 'px';
const iconSvg = (name, size) => ICONS[name]
  ? `<svg viewBox="0 0 24 24" width="${size}" height="${size}" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="${ICONS[name]}"/></svg>`
  : '';

// ===== ① tokens.css(值来自资产层;键排序) =====
const tks = Object.keys(TOKENS).sort();
const tokensCss = [
  '/* codegen 确定性生成 — 勿手改;源:codegen-template/assets/tokens.json */',
  ':root {',
  ...tks.map(k => `  --${kebab(k)}: ${TOKENS[k].light};`),
  '}',
  'html[data-theme="dark"] {',
  ...tks.map(k => `  --${kebab(k)}: ${TOKENS[k].dark};`),
  '}',
  'html[data-theme="auto"] {',
  '}',
  '@media (prefers-color-scheme: dark) {',
  '  html[data-theme="auto"] {',
  ...tks.map(k => `    --${kebab(k)}: ${TOKENS[k].dark};`),
  '  }',
  '}',
  '',
].join('\n');

// ===== ② IR 组件段 → CSS(IR.comps 每变体一段;固定属性序) =====
const nodeCss = n => {
  const p = [];
  if (n.w) p.push(`width: ${px(n.w)}`);
  if (n.h) p.push(`height: ${px(n.h)}`);
  if (n.r) p.push(`border-radius: ${px(n.r)}`);
  if (n.fill) p.push(`background: ${n.fill}`);
  if (n.pad) p.push(`padding: ${n.pad.map(px).join(' ')}`);
  if (n.gap) p.push(`gap: ${px(n.gap)}`);
  if (n.dir) p.push('display: flex', `flex-direction: ${n.dir === 'h' ? 'row' : 'column'}`);
  return p;
};
let compCss = '/* codegen 确定性生成 — 勿手改;源:ir/design.json components */\n';
for (const key of Object.keys(IR.comps).sort()) {
  for (const variant of IR.comps[key]) {
    compCss += `\n.${key}--${slug(variant.v)} {\n`;
    for (const line of nodeCss(variant.n)) compCss += `  ${line};\n`;
    compCss += '}\n';
  }
}

// ===== ③ 公共骨架 + 类型化渲染器(资产层驱动) =====
const BASE_CSS = `*{margin:0;padding:0;box-sizing:border-box}
body{font-family:"Noto Sans SC","PingFang SC",sans-serif;background:var(--bg);-webkit-font-smoothing:antialiased}
.screen{width:390px;height:844px;margin:0;display:flex;flex-direction:column;overflow:hidden;background:var(--bg)}
.status{height:44px;position:relative;flex-shrink:0}
.status .time{position:absolute;left:28px;top:12px;font-size:15px;font-weight:500;color:var(--title)}
.signal{position:absolute;left:300px;top:14px;display:flex;gap:2px;align-items:flex-end}
.signal i{width:3px;background:var(--title);border-radius:.5px;display:block}
.signal i:nth-child(1){height:4px}.signal i:nth-child(2){height:6px}.signal i:nth-child(3){height:8px}
.battery{position:absolute;left:322px;top:16px;width:22px;height:11px;border:1px solid var(--title);border-radius:3px;padding:2px}
.battery i{display:block;width:15px;height:6px;background:var(--title);border-radius:1px}
.big-header{height:60px;padding:6px 20px 0;display:flex;align-items:center;gap:8px;flex-shrink:0}
.big-header .titles{display:flex;flex-direction:column;gap:2px;flex:1}
.big-header h1{font-size:26px;font-weight:700;color:var(--title);line-height:32px}
.big-header .sub{font-size:11px;color:var(--caption)}
.filters{height:50px;padding:13px 20px 0;display:flex;gap:8px;flex-shrink:0}
.pill{height:32px;padding:4px 14px 3px;border-radius:100px;display:inline-flex;align-items:center;font-size:13px;background:var(--surface);color:var(--body);border:1px solid var(--border)}
.pill.active{background:var(--accent);color:var(--on-accent);font-weight:500;border-color:transparent}
.content{flex:1;padding:12px 16px;display:flex;flex-direction:column;gap:12px;overflow:hidden}
.card{background:var(--surface);border-radius:16px;padding:14px;display:flex;gap:10px}
.avatar{width:36px;height:36px;border-radius:18px;flex-shrink:0;color:var(--on-accent);font-size:14px;font-weight:500;display:flex;align-items:center;justify-content:center}
.card .cbody{width:284px;display:flex;flex-direction:column;gap:5px;min-width:0}
.byline{display:flex;align-items:baseline;gap:6px;height:16px}
.byline .author{font-size:13px;font-weight:500;color:var(--title);line-height:16px}
.byline .dot,.byline .time{font-size:11px;color:var(--caption);line-height:16px}
.card h2{font-size:16px;font-weight:500;color:var(--title);line-height:22px}
.meta{display:flex;align-items:center;gap:10px;height:20px}
.chip{height:20px;padding:3px 10px 2px;border-radius:100px;background:var(--accent-soft);color:var(--accent);font-size:11px;font-weight:500;line-height:15px;display:inline-flex;align-items:center;white-space:nowrap}
.meta .sp{flex:1}
.meta .replies{font-size:11px;color:var(--caption);line-height:13px}
.meta .likes{font-size:11px;color:var(--like);line-height:13px}
.tabbar{height:75px;background:var(--surface);padding:6px 12px 18px;display:flex;flex-direction:column;gap:6px;flex-shrink:0}
.tabs{display:flex;height:40px;align-items:center}
.tab-item{flex:1;display:flex;flex-direction:column;align-items:center;gap:3px}
.tab-item svg{width:24px;height:24px;stroke:var(--caption)}
.tab-item span{font-size:10px;color:var(--caption)}
.tab-item.active svg{stroke:var(--accent)}
.tab-item.active span{color:var(--accent);font-weight:500}
.home-indicator{width:134px;height:5px;border-radius:3px;background:var(--title);opacity:.9;margin:0 auto}
`;

const status = () => `<div class="status"><span class="time">9:41</span><span class="signal"><i></i><i></i><i></i></span><span class="battery"><i></i></span></div>`;
const tabbar = active => {
  const items = ['home', 'discover', 'publish', 'notify', 'my'];
  const labels = { home: '首页', discover: '发现', publish: '发布', notify: '通知', my: '我的' };
  return `<footer class="tabbar"><div class="tabs">
${items.map(i => `      <div class="tab-item${i === active ? ' active' : ''}">${iconSvg(i, 24)}<span>${labels[i]}</span></div>`).join('\n')}
    </div><div class="home-indicator"></div></footer>`;
};
const topicCard = ([author, time, title, node, replies, likes, letter, color]) => `    <article class="card">
      <div class="avatar" style="background:${color}">${esc(letter)}</div>
      <div class="cbody">
        <div class="byline"><span class="author">${esc(author)}</span><span class="dot">·</span><span class="time">${esc(time)}</span></div>
        <h2>${esc(title)}</h2>
        <div class="meta"><span class="chip">${esc(node)}</span><span class="sp"></span><span class="replies">${esc(replies)}</span><span class="likes">${esc(likes)}</span></div>
      </div>
    </article>`;

// 类型化渲染器注册表:新屏类型在此加一个函数即可
const RENDERERS = {
  feed: d => [
    status(),
    `<header class="big-header"><div class="titles"><h1>${esc(d.title)}</h1><div class="sub">${esc(d.sub)}</div></div></header>`,
    `<nav class="filters">
${d.filters.map((f, i) => `    <span class="pill${i === (d.activeFilter || 0) ? ' active' : ''}">${esc(f)}</span>`).join('\n')}
  </nav>`,
    `  <main class="content">
${d.cards.map(topicCard).join('\n')}
  </main>`,
    tabbar(d.tab),
  ],
};

// ===== ④ 输出(dist 逐文件;屏名排序) =====
fs.rmSync(DIST, { recursive: true, force: true });
fs.mkdirSync(path.join(DIST, 'screens'), { recursive: true });
fs.writeFileSync(path.join(DIST, 'tokens.css'), tokensCss);
fs.writeFileSync(path.join(DIST, 'components.css'), compCss);
fs.writeFileSync(path.join(DIST, 'base.css'), '/* codegen 确定性生成 — 勿手改 */\n' + BASE_CSS);

for (const name of Object.keys(DEFAULTS).sort()) {
  const d = DEFAULTS[name];
  const render = RENDERERS[d.type];
  const parts = render ? render(d) : [`<!-- 未注册类型: ${d.type} -->`];
  const html = `<!DOCTYPE html>
<html lang="zh-CN" data-theme="light">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>${esc(name)}</title>
<link rel="stylesheet" href="../tokens.css">
<link rel="stylesheet" href="../base.css">
<link rel="stylesheet" href="../components.css">
</head>
<body>
<main class="screen">
${parts.join('\n')}
</main>
</body>
</html>
`;
  fs.writeFileSync(path.join(DIST, 'screens', slug(name) + '.html'), html);
}
console.log('dist/ regenerated:', Object.keys(DEFAULTS).length, 'screens + 3 css files');
