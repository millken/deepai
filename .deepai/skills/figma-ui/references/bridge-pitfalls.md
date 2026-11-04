# 原生桥实战坑谱（2026-11 webwrap.app 24 屏项目全历程沉淀）

> 来源：一次从零到 24 屏 + 组件库的完整设计会话，每个坑都实际翻车过至少一次。
> 读法：动手前扫 §1 可用性矩阵与 §3 铁律；报错时按 §2/§4 对号；写脚本套 §5 模式。

## 1. API 可用性矩阵（原生桥实测，dynamic-page 模式）

| API | 状态 | 替代方案 |
|---|---|---|
| `appendChild` / `remove` / `findOne` / `findAll` | ✅ | — |
| `createFrame` / `createText` / `createInstance` / `resize` | ✅ | — |
| `setBoundVariableForPaint(field, idx, var)` | ✅（收 Variable 对象） | 绑定路径全塌时用 SolidPaint 真值色兜底（§5.4） |
| `figma.variables.getLocalVariableCollectionsAsync()` | ✅ 但 `collection.variables` 属性为 undefined | 只用它取 `id → name` 映射 |
| `collection.getVariablesAsync()` | ❌ not a function | `figma.variables.getLocalVariablesAsync()` 全量列表按 `(name, collectionId)` 匹配 |
| `figma.variables.getVariableByIdAsync(id)` | ❌ not a function | 同上，全量列表里 `find(v => v.id === id)` |
| **`parent.insertBefore(node, ref)`** | ❌ **not a function** | 「删尾 → appendChild → 补尾」保序法（§5.2） |
| `figma.loadAllPagesAsync()` | ✅（原生桥无此禁令；那是官方 MCP use_figma 的限制） | 每次脚本开头调用以访问其他页 |
| `figma.getLocalTextStyles()` | ❌ | `getLocalTextStylesAsync()`（dynamic-page 一律用 Async 版） |
| `node.exportAsync({format:'PNG', constraint})` | ✅ | run 返回 Uint8Array → 桥自动落盘 preview.png（比官方远程 URL+curl 省 token） |

## 2. 枚举值陷阱（报错信息不含正确答案，背下来）

| 属性 | ❌ 想当然值 | ✅ 合法值 |
|---|---|---|
| `counterAxisAlignItems` | `'END'` | `'MIN' \| 'MAX' \| 'CENTER' \| 'BASELINE'` |
| `primaryAxisAlignItems` | `'END'` | `'MIN' \| 'MAX' \| 'CENTER' \| 'SPACE_BETWEEN' \| 'SPACE_AROUND' \| 'SPACE_EVENLY'` |

## 3. 布局铁律（违反必报错或产出畸形）

1. **FILL/HUG 只能设在 auto-layout 父级的子节点上**；先 `appendChild/insert` 入树、后设 sizing。工厂函数内绝不预设 FILL。
2. **`resize()` 会把该轴锁回 FIXED**——resize 后必须重设 HUG（FAQ 列表、Timeline 两次翻车）。
3. **`createFrame()` 默认 100×100**——新建 frame/divider/spacer/chip 一律入树后显式 `layoutSizingVertical='HUG'`，否则残留 100px 高（OAuth 轮 OrDivider、LangChip 连环翻车）。
4. **`findAll()` 不含根节点自身**——全量重绑/改色时根节点底色单独处理（15 号画板黑底漏绑案例）。
5. **报错不回滚**——脚本中途崩溃，已执行的变更保留。重跑前先按名 find+remove 清理半成品（幂等，§5.1）。
6. `layoutPositioning='ABSOLUTE'` 在 `layoutMode='NONE'` 的画布上是多余赋值，直接报错；NONE 画布的子节点本就是绝对定位，设 x/y 即可。

## 4. 组件与变量陷阱

- **INSTANCE_SWAP 默认值必须是组件 `key`**；本地组件（未发布）key 为空串 → 直接损坏组件集（"has existing errors"，删集重建也救不回属性）。替代：**variant property 方案**——把可换内容做成变体（如 `label=功能模块, state=active`），不用 INSTANCE_SWAP。NavItem 两次损坏后以此方案重建成功。
- `componentPropertyReferences` / `addComponentProperty` 须在节点 **append 进组件集之后**再设。
- **paint 绑定后改 opacity 会丢绑定**——淡底一律用预混 tint 变量，禁止 `color + opacity`。
- **中文文本禁 JetBrains Mono**（无 CJK 字形）——含中文行一律 Noto Sans SC；代码块里中文注释行单独换字体。
- **变量绑定可能"绑上但绑错"**——`boundVariables` 有 id ≠ 解析出正确颜色（14 号画板印记绑到错误变量解析为纯黑）。验收时读 `fills[0].color` 的实际 hex，别只看 bound 标志。
- 组件集内改一个变体，同属性可能同步到全部变体（Badge 文案全变 "iOS" 事故）——批量改后逐变体回读 characters。

## 5. 脚本工程模式（可靠写法）

### 5.1 幂等脚本
每段变更前先按名清理：
```js
for (const nm of ['FormGroupPhone', 'ButtonLogin', ...]) {
  const n = card.findOne(x => x.name === nm); if (n) n.remove();
}
```
脚本崩溃重跑不产生重复节点。

### 5.2 保序插入（绕 insertBefore 不可用）
```js
// 目标顺序 [head..., NEW..., tail...]
const tailNodes = [reg, agree]; // 先记录尾部文案再 remove
tailNodes.forEach(n => n.remove());
newNodes.forEach(n => card.appendChild(n));   // append 可用
newNodes.forEach(n => n.layoutSizingHorizontal = 'FILL'); // 入树后才设
tailNodes.forEach(n => card.appendChild(n)); // 尾部补回
```

### 5.3 探针先行（连续同错时定位元凶）
不明 "not a function" 报错时，把可疑 API 逐个 try/catch：
```js
const t = {};
const tryIt = async (k, fn) => { try { await fn(); t[k] = 'ok'; } catch (e) { t[k] = e.message.slice(0, 60); } };
await tryIt('insertBefore', () => fr.parent.insertBefore(fr, fr.parent.children[0]));
return t; // 一次定位，不再盲试
```

### 5.4 SolidPaint 真值色兜底
变量绑定路径全部塌方时（§1 两条 ❌），直接写已知对比度达标的真值色：
```js
const solid = (r,g,b) => ({ type:'SOLID', color:{ r:r/255, g:g/255, b:b/255 } });
mark.fills = [solid(15,118,110)]; // #0F766E，值取自规格表
```
代价：脱离 token 传导；须在交付报告注明。

### 5.5 唯一可靠的变量查找路径
```js
const cols = await figma.variables.getLocalVariableCollectionsAsync();
const colName = {}; for (const c of cols) colName[c.id] = c.name;
const allVars = await figma.variables.getLocalVariablesAsync();
const v = allVars.find(x => x.name === 'color/accent/600' && colName[x.collectionId] === 'wvapp-light');
node.setBoundVariableForPaint('fills', 0, v); // Variable 对象直传
```

### 5.6 异步语法红线
`arr.map(async ...)` 内的 await 不会等待；`forEach` 回调非 async。循环内异步一律 `for...of`。

## 6. MCP 选路与成本经验

- **优先级：原生桥 > 官方远程**。`status` 返回 `connected:true` 才可用；面板 >60s 未开/停滞 >10min 会 auto-expire，先 inspect:true 再 ack，**绝不盲目重投同一变更**。
- **`create_new_file` 在 starter plan 上会被拒**（连续 3 次）——直接请用户在 Figma Desktop 手动建空文件+空画板，发链接后 `init` 绑定，比反复重试省一整轮。
- 官方远程 MCP（41 tools）适合：只读结构/截图/变量定义、无桥时的兜底。每次调用 token 开销大；原生桥一次 `run` 可打包整段构建+量测+导出。
- PNG 导出统一走 `return await target.exportAsync(...)`：evidence 目录自动落 preview.png，bash cp 归档——避免官方远程的临时 URL + curl 链路。

## 7. 验收要点（§8 量测能抓出的真问题）

量测脚本须覆盖：对比度（fg vs 最近实底祖先）、图标-文字垂直中心差 dy、按钮高度档、子级溢出父容器。本项目实测抓出过：
- resize 锁 HUG 导致的溢出（FAQ/Timeline）
- 命名误判：层名含 `button|search|field` 会被识别为控件 → 改名（HelpEntry/QueryWrap/HamburgerMenu 先例）
- 组件内嵌按钮绑错变量集（SiteNav ghost 亮底 1.4:1）
- 对比度压线（tint 上 4.5:1 恰好不达标 → 升 text/secondary）
- 移动端触控目标 <44px

**流程闭环**：改板 → 量测（零 error 零 warn）→ PNG 重导 → 归档 → 交付报告/规格同步。快速迭代后不回写规格 = 真相源腐化。
