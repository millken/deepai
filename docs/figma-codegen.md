# Figma 确定性代码生成管线(design → code pipeline)

> 让「Figma 改一处 → 代码 diff 一行」成为可预测的工程流程:同输入必同输出(逐字节)、局部改动只动对应文件、生成物全部可进 git 审阅。
> 本文是架构说明与落地指南;**可运行模板在仓库内 [`codegen-template/`](../codegen-template/README.md)**(自包含:ir 样例 + assets + codegen,`node codegen.js` 双跑 `diff -r` 即验确定性);外部实证项目参考 `~/v2hub-v3/pipeline/`(V2HUB V3,45 屏全量)。

## 一、为什么需要确定性管线

LLM 直接生成 UI 代码的三个工程问题:

| 问题 | 表现 | 确定性管线的答案 |
|---|---|---|
| 产出不稳定 | 同一设计两次生成,结构/措辞/顺序都可能变 | 同输入必同输出,`diff -r` 逐字节一致 |
| 局部改动扩散 | 改一屏也要人肉重写,diff 不可读 | 一屏一文件,Figma 改一处 → 只有对应文件变 |
| 不可审 | 无法 review"AI 又改了什么" | 生成物进 git,变更 = diff = 可 review 可回滚 |

核心思路:**LLM 只写一次生成器,之后退出循环**。语义决策(这是按钮、这是列表、这屏叫 Topic-Hot)不发生在生成时,而是提前固化到 Figma 源头(图层命名/组件结构/变量 token),生成器只做机械映射。

## 二、架构(三段式)

```
Figma 文件(单一事实源)
  │  ① extract(桥上跑,语义化抽取)
  ▼
IR + 资产层(ir/design.json + assets/*.json,规范化中间表示)
  │  ② codegen(纯函数模板,零 LLM)
  ▼
dist/(逐文件输出:tokens.css / base.css / screens/*.html / index.html 画廊)
```

各层职责:

- **extract**:经 figmabridge `run` 执行;按图层名做语义化读取(尺寸/圆角/填充→token 引用/布局模式/文本),输出 IR。抽取内容以组件集为主(递归稳定),屏级按「直接子级分区 + 分区 findAll」扁平读取。
- **IR/资产层**:排序定格的 JSON(`sort_keys + indent=2 + 尾换行`)。桥读不稳或 findAll 覆盖不到的内容(静态默认文本、icon path 字典、token 双主题值)放 `assets/` 版本化——**资产层是桥能力的减震器**,桥异常不阻塞管线。
- **codegen**:Node 纯函数,零依赖零 LLM。类型化渲染器(feed/detail/notify/setting/brand/skeleton… 每类屏一个模板),从 IR+assets 渲染,输出固定属性序、固定缩进。

## 三、确定性四要素(照抄即得)

1. **稳定键**:图层名为主键(构建时规范命名是前提);不要用 node-id(复制粘贴后会变)。
2. **固定排序**:children 按文档序;JSON 键 `sort_keys`;输出文件按名排序。
3. **规范格式**:固定缩进、固定 CSS 属性序、尾换行;需要更强保证时产物过一遍 Prettier 定格。
4. **无挥发性字段**:IR/产物里绝不写时间戳、运行环境、绝对路径。

## 四、实测证据(首行为仓库内模板,其余为仓库外 V2HUB V3 45 屏参考)

| 验证项 | 方法 | 结果 |
|---|---|---|
| **模板自验证**(仓库内 `codegen-template/`,任何人可复现) | `node codegen.js` 双跑 + `diff -r dist` | 逐字节一致(demo-feed.html + 3 css) |
| extract 确定性(仓库外参考) | 同脚本双跑,规范化 JSON 对比 | IR 逐字节一致(31,728B);还原后第三跑与基线 diff 0(三重确认) |
| codegen 确定性(仓库外参考) | `node codegen.js` 双跑 + `diff -r dist` | 逐字节一致(45 屏 + 3 资源 + 画廊共 48 文件) |
| 增量性(仓库外参考) | Figma 仅改一处卡片标题 → 重跑全管线 → `git status` | 仅 `topic-hot.html` 1 行 + `ir/design.json` 1 行 diff;还原后 dist 与基线 diff 0 |
| 像素保真(仓库外参考) | Chrome 1x 截图 vs Figma 导出基准 | Light 9.6% / Dark 10.0% 差异(与手写 LLM 版同量级,剩余为跨渲染器文本栅格化) |

git 证据链(**仓库外**项目 `~/v2hub-v3/pipeline`,仅供参考、不可在 deepai 仓库内复现;仓库内可复现验证见上表「模板自验证」行):
```
b5fbfb7  codegen v3: 45 屏一次性生成(双跑逐字节一致)
41c85d9  codegen v2: 真实 UI 渲染器(Light 9.62% / Dark 10.00%)
26c53af  还原后与基线 diff 0(确定性三重复证)
956132d  增量性证据:改一处 → 单文件单行 diff
```

## 五、落地步骤(新项目 checklist)
> 模板起点:复制 `codegen-template/`(含 ir 样例/assets/可跑 codegen,README 有快速开始与验证命令),按下述步骤替换数据源与渲染器。
> 仓库内 `codegen-template/` 是唯一可复现的验证载体(双跑 `diff -r` 逐字节一致);上表(§四)外部项目的数字仅作规模参考。


1. **Figma 源规范化**(管线质量的上限):公共组件抽 Component Set + 变体;图层语义命名(`avatar/title/chip/username`…);颜色绑变量集合(Light/Dark 同名 token);一屏一画板、分区命名(`status/header/filters/list/tabbar`)。
2. **写 extract**:对组件集递归、对屏按分区扁平读取(见已知坑①②);每分区/每集独立 try/catch,坏节点标记不炸整跑;返回值只带摘要,PNG 走 `exportAsync` 字节直返。
3. **建资产层**:桥读不稳的内容(token 值/静态默认/icon path)进 `assets/*.json`,git 版本化。
4. **写 codegen**:类型化渲染器,每类屏一个模板;公共骨架(状态栏/tabbar/卡片)抽共享段;输出逐文件。
5. **验证闭环**(必须做,别跳):双跑 `diff -r`;改一处验单文件 diff;Chrome 截图 vs Figma 基准像素比对。
6. **团队公约**:禁止手改 `dist/`——手调写回 Figma(单一事实源);特殊覆写用 `*.override.css` 组合层。

## 六、已知坑(实测,防重踩)

1. **`safe()` 防御包装会误杀方法**:`typeof v === 'function' ? undefined` 把 `findAll` 等方法也判空 → 分区文本恒空。防御式读取只用于**属性**;方法必须直调 + try/catch。
2. **屏级实例树递归存在无法定位的场景代理抛错**(`TypeError: not a function` 穿透所有捕获层,面包屑为空):组件集内递归稳定可用;**屏级不要深度递归实例子树**,改「直接子级分区 + 每分区 findAll(文本)」扁平模式。
3. **findAll 不进实例子树**:屏级实例覆写文本要靠**父级** findAll 间接触达(分区层 findAll 能拿到,深递归反而炸)。
4. **manifest/auth 断连不影响已物化的产物**:extract 结果落 `runs/<id>/result.json` 即可离线物化;管线重跑只需读盘,不需要桥在线。

## 七、与 skill / 现有文档的关系

- 本文档是**工程说明**;操作配方(桥 run 代码片段)不在此重复,按需从本文第五节 checklist 对照 `figma-ui/references/bridge-recipes.md` 取用。
- 前置能力链:公共组件抽取(§6 recipes)/ 变量 TOKEN(figma_mcp 变量 + bridge gotchas)/ §8 验收 lint(生成前保证 Figma 源质量)。
- 适用边界:适合「设计已规范化 → 需要长期维护的工程化交付」;一次性原型/探索期仍可直接 LLM 手写(app/ 目录那类),不必上管线。
