# codegen-template — Figma 确定性代码生成管线模板

> 配套文档:[docs/figma-codegen.md](../docs/figma-codegen.md)(架构/确定性四要素/落地 checklist)。
> 本目录是**可直接运行的完整模板**:在 deepai 仓库内自包含,不依赖任何外部路径。

## 目录结构(三段式)

```
codegen-template/
├── extract.js              ① 桥上跑(figmabridge run 的 code):Figma → IR
├── ir/
│   └── design.json         IR 样例(含 demo 屏分区 + 组件段;跑 extract 后替换为真实数据)
├── assets/                 资产层(桥读不稳内容的减震器,git 版本化)
│   ├── tokens.json         主题 token(Light/Dark × 10,值=Figma 变量集合)
│   ├── icon-paths.json     贝塞尔图标字典(stroke 1.8 风格)
│   └── screen-defaults.json  屏静态默认(类型/文案/卡片数据)
├── codegen.js              ② 纯函数生成器:IR+assets → dist(node codegen.js)
└── dist/                   ③ 产物(生成,勿手改;git 审 diff)
    ├── tokens.css / base.css / components.css
    └── screens/demo-feed.html
```

## 快速开始

```sh
# ③ 生成(可独立运行,只需 ir/ + assets/)
node codegen.js
open dist/screens/demo-feed.html     # 改 html 的 data-theme="dark" 即深色

# 确定性自验证(必做)
cp -r dist /tmp/dist-run1 && node codegen.js && diff -r /tmp/dist-run1 dist
# → 期望:无输出(逐字节一致)

# 增量性验证
改 assets/screen-defaults.json 里一张卡的标题 → node codegen.js
git diff --stat → 仅 dist/screens/demo-feed.html 变化
```

## extract.js 的用法

`extract.js` 的内容是**提交给 figmabridge `run` 的 code**(不是 node 脚本):整段复制进
deepai 的 figma 工具 `run` 参数,返回值 `{ ir: {...} }` 落盘到 `ir/design.json`(排序定格:
`json.dump(..., indent=2, sort_keys=True) + '\n'`)。

要点(实测坑,详见 figma-codegen.md 第六节):
- 组件集内递归稳定;**屏级不要深度递归实例子树**(场景代理会抛 TypeError),
  用「直接子级分区 + 每分区 `findAll` 取文本」的扁平模式;
- 防御式读取 `safe()` 只用于**属性**——`typeof v === 'function'` 会把 `findAll`
  等方法判空,方法必须直调 + try/catch;
- 每分区/每组件集独立 try/catch,坏节点标记 `{ err: true }` 不炸整跑。

## 扩展到真实项目

1. `assets/tokens.json` ← Figma 变量集合值(变量 API 或 Tokens 展示板直读);
2. `assets/screen-defaults.json` ← 每屏一条(先跑 extract 拿真实数据回填,
   桥覆盖不到的文案/静态默认手工补,git 版本化);
3. `codegen.js` 的 `RENDERERS` ← 按屏类型加渲染函数(feed/detail/notify/setting/…,
   V2HUB 45 屏用了 13 类);
4. 每次改 Figma → 重跑 extract(物化 IR)→ `node codegen.js` → git diff 审变更。

## 团队公约

- **禁止手改 `dist/`**:手调写回 Figma(单一事实源)或走 assets;特殊覆写用 `*.override.css` 组合层,
  **放在 `dist/` 之外**(如 codegen-template 根目录)——`codegen.js` 开头 `fs.rmSync(DIST)` 会清空整个 dist/,override 放里面会被删;
- IR/产物不含时间戳/绝对路径等挥发性字段(确定性四要素之一);
- 生成器本身进 git:改模板 = 改 codegen.js,同样可 diff 可审。
