# Hashline 编辑 — 设计稿（供评审）

> 状态：**Phase 1 已实施并合入（`747799c`）；§17 Phase 2 设计 v5 修订稿，待终审**。调研日期 2026-09-14。v2 响应 D1–D3 / F1；v3 响应 N1–N5；v5 响应 D4–D5 / N6–N7。见文末「修订记录」。
> 目标：读文件时给每行一个短 hash；模型用 **hash 范围 + 新内容** 编辑，不再复述旧文本，从而大幅节省 **输出 token**，并消灭「凭记忆重打 `old_string`」这一主失败模式。
> 不在本文范围：实现。本文只定语义、格式、兼容策略与分期，供评审否决或修订后再动代码。

---

## 0. 结论（推荐方案，一句话）

在 `read_file` 的编号输出上为每行附加 **6 位小写 hex 内容哈希**，前缀形如 `12:a3f2b1`。`edit_file` 新增 hash 模式：模型把 **整个 `N:hhhhhh` 前缀**抄进 `start_hash` / `end_hash`，只另传 `new_string`。对当前磁盘文件 **现场重算 hash、按闭区间替换**，**不建会话缓存**。行号是消歧提示，hash 是内容校验——多段候选时取距提示行号最近且 hash 仍匹配的那一段；对不上或仍不唯一则失败。`old_string` 路径完整保留。hash 是行内容的地址，不是行号的别名——插入上方行之后，未改动行的 hash 仍然有效。

不选「只靠行号替换」（静默写错是本工具的硬禁令）；不选「会话级行 ID 表」（工具层今天无状态，缓存失效与子代理隔离都会变成新事故）。不选「纯 hash 联合唯一性」（整函数替换的 end 锚点必然是共用的 `}`，纸面上就会结构性失败，见 §5.2）。

---

## 1. 问题：现在的 token 与失败都花在复述旧文本上

### 1.1 现状契约

`edit_file`（`pkg/tools/builtin/edit.go`）是 **精确子串替换**：

| 参数 | 作用 |
|---|---|
| `path` | 目标文件 |
| `old_string` | **必填**（schema `required` 与 handler 入口双重强制，`edit.go:29` / `edit.go:533`）。必须是文件里的原文，且默认全局唯一 |
| `new_string` | 替换文本 |
| `replace_all` | 允许多处相同 `old_string` |
| `start_line` / `end_line` | 可选窗口，把唯一性检查限制在行范围内 |

为了让这个契约在模型手上还能用，handler 已经堆了一层容错，而且每一层都来自真实失败，不是臆测：

- 反转义 `\n` / `\t`（模型把字面转义写进参数）
- 去掉 `read_file` 贴回来的 `12<TAB>` 前缀（`stripLineNumberPrefixes`）。grep 的 `file.go:12: `（`grep.go:110`，`%s:%d: `）**不会被剥**，只出现在 miss 错误文案里（`edit.go:165`）
- 字面匹配失败后再做空白容忍（tab/space、CRLF、连续空白）
- 窗口外命中时指出「其实在第 N 行」
- `nearestMissHint`：对齐后报告 **第一处分叉行** 的两边原文

`nearestMissHint` 的注释（`edit.go:541-549`）写的是：本仓库自己的会话史里，每一个能分析的 `edit_file` miss 都是同一形状——锚点对了、前几行也对了，中间一行被模型 **凭记忆重打**（不是行号前缀，也不是转义）。错误信息以前只谈这两类，模型看完找不到可改的，把同一串再发一遍。

`read_file` 为了给后续编辑提供坐标，range 模式默认输出：

```
  12<TAB>func Foo() {
  13<TAB>    return
```

于是出现了两个互相打架的目标：编号方便引用，编号又正好是 `old_string` 贴回失败的主因。`line_numbers=false` 是为此开的逃生口。

### 1.2 输出 token 花在哪里

一次「改 15 行函数、为唯一性再垫 10 行上下文」的典型调用：

| 字段 | 约字符 | 约 token | 谁付 |
|---|---|---|---|
| `old_string`（含垫上下文） | 900–2000 | 220–500 | **输出** |
| `new_string` | 400–1200 | 100–300 | 输出（改动本身，省不掉） |
| 失败后整段重发 × 1–2 | 同上再来一遍 | 再 ×1–2 | 输出 |

`docs/context-compression-design.md` 把 `edit_file` **结果**标成 p99 < 105B、可即时折叠——那是工具 **回包**，不是模型 **发出去的参数**。贵的是 `old_string`。

Hashline 把「定位」从「复述 25 行旧文」收成两个 `N:hhhhhh` 前缀（约 10 字符一对）。`new_string` 仍在。单次成功编辑大约砍掉 **旧文本那一半输出**；若再算上少一次「重打 old_string」的重试，实际节省经常大于 50%。

读侧成本：每行多 `:hhhhhh`（约 7 字节）。150 行 range ≈ +1 KB 输入。输出单价通常是输入的 3–5 倍，**一次成功的 hash 编辑就能覆盖这次读取的增量**。多处互不重叠的编辑可以共用同一次 read（见 §5.3），输入摊得更薄。

### 1.3 本设计要对准的失败

| # | 形态 | 今日对策 | hashline 之后 |
|---|---|---|---|
| E1 | 复述 `old_string` 耗输出 token | 无 | 定位改为 hash 范围 |
| E2 | 凭记忆重打旧文 → miss → 同串重试 | `nearestMissHint` | 不再要求旧文，这类 miss 从根上消失 |
| E3 | 把 `12<TAB>` 贴进 `old_string` | strip 前缀；失败则提示 | hash 模式根本不吃旧文；`old_string` 路径仍 strip（且必须认识新前缀，见 §6.1 / D3） |
| E4 | 短片段全文件重复，必须垫上下文或设窗口 | `start_line`/`end_line` / `replace_all` | `N:hhhhhh` 前缀：hash 定位 + 行号消歧（§5.2）。不再靠「end 锚点内容全球唯一」 |
| E5 | 先改文件上部，下部行号全部错位 | 只能再 read，或赌行号 | **内容 hash 不随行号移动**；提示行号对不上时退回「距提示最近且 hash 匹配」的 span |
| E6 | 文件已变仍按记忆覆盖 | `old_string` 校验被替换的每个字节，对不上则失败 | **只保证端点**：起止行内容变了则 hash 不符、失败，不会按过期行号写到另一段。区间**内部**被改而两端仍在，hash 模式会覆盖模型没见过的中间内容（`old_string` 同样情况下会失败）。明确接受，见 C13 / Q9 |

---

## 2. 三个「显然的方案」为什么不够

### 2.1 只加 `start_line`/`end_line` + `new_string`，丢掉 `old_string`

行号便宜、模型也熟悉，而且窗口参数已经存在。但行号 **不是内容能力（capability）**：

- 第一次插入/删除之后，后续行号全部平移。模型若按第一次 read 的行号再改，会写到 **另一段代码上**。本工具把「静默写错报成功」当作事故（`edit.go` 里「编号没剥干净就跳过 candidate」就是这条原则）。
- 文件在 read 与 edit 之间被 `gofmt` / 另一个工具改过，行号仍可能对得上，内容已经不是当时看到的。
- 模型数行经常 off-by-one。

行号可以当 **导航**（「再读 80–120 行」）和 **消歧提示**（§5.2），不能当 **无校验的写地址**。§5.2 用行号消歧并不违反这条：最终落笔的区间必须 hash 仍匹配，行号从不单独授权一次写入。

### 2.2 会话级行 ID 表（read 时发号，edit 时查表）

给每行发一个本次 read 内唯一的短 ID（`a3k9`），存在 `map[id]→(path, lineno, content)`。重复行也好办，实现也直观。代价是工具层首次有状态：

- 失效：同一 path 被 bash/`write_file`/另一个 agent 改掉，表里的 ID 还在，指向的内容已经不是文件。
- 子代理 / 并行 `task`：两份 read 谁的表生效？`SessionCarry` 今天只记成功写过的路径（`session_carry.go:68`），不给工具做索引。
- 内存：长会话反复读大文件，表要驱逐策略。
- 与现有 handler 形态不一致：`ReadFileHandler` / `EditFileHandler` 只收 `context` + `ToolCall`，不持有会话对象。

能做，但把「省 token」绑到一套新的生命周期上，评审成本高于收益。**v1 不建表**。§5.2 的行号消歧已经覆盖「end 锚点是共用 `}`」这个主场景，Q7 的 v2 触发条件（「`} ` 歧义占 miss 大头」）预期不会出现。会话表降为存档备选，不是待观察的上线项。

### 2.3 换成 apply_patch / V4A / `// ... existing code ...`

这些格式仍要复述旧上下文（或依赖第二个「fast apply」模型）。不是「hash 范围 + 新内容」，也解决不了 E1。本设计不引入第二种补丁语言。

---

## 3. 目标与非目标

**目标**

1. 模型在看过带 hash 的 `read_file` 之后，编辑参数里 **不必出现旧文件原文**。
2. 定位失败必须 **失败**，并指出是「hash 不存在 / 范围歧义 / 起止颠倒 / 行号提示无法消歧」，引导再 read，不猜测着写。
3. 一次 read 之后，对 **互不破坏彼此锚点** 的多处编辑可以连续 `edit_file`，不必每改一次就重读。**整函数替换**（签名行 + 收尾 `}`）必须是成功路径，不能结构性歧义。
4. 现有 `old_string` 路径、测试、TUI diff、near-miss 提示 **行为不变**。
5. 工具保持无状态：hash 算法对「当前文件字节」纯函数可复算。

**非目标（v1）**

- 不删 `old_string`，不强迫 grep-then-edit 的路径改用 hash。
- 不为 grep / `code_map` 符号表加 hash（§11 的 Phase 2）。
- 不做会话 ID 表、不做跨文件能力令牌。
- 不把 hash 当安全边界（短 hash 可碰撞；安全靠「对不上则拒绝」）。
- 不在 v1 做一次调用里的多 hunk 批处理（内容 hash 跨调用仍有效，见 §5.3；批处理是便利，不是前提）。

---

## 4. 总体形态

```
read_file(path, start_line, end_line)
        │
        ▼
  12:a3f2b1<TAB>func Foo() {
  13:9c01d4<TAB>    return x
  14:7e88aa<TAB>}

edit_file(path, start_hash="12:a3f2b1", end_hash="14:7e88aa",
          new_string="func Foo() {\n    return y\n}")
        │
        ▼
  解析 N:hhhhhh（行号=提示，hash=校验）
  对当前文件逐行重算 hash
  按 §5.2 解析闭区间 [start, end]
  用 new_string 替换该行区间（§7 的尾换行补齐）
  回包带上新区间起止 hash，供链式再改
```

判别模式（`EditFileHandler` 入口，**必须排在** 今日 `old_string is required` 检查之前，`edit.go:29`）：

```
path 空 → 错（与今日同）
new_string 键不存在 → 错          // 漏传 ≠ 空串。空串 = 删除（只在键存在时）
if start_hash 有非空值:
    hash 模式                     // old_string 若也传了，忽略（Q4）
                                  // replace_all 若也传了，忽略（N5；语义是「这一处」）
else if end_hash 有非空值:
    错：end_hash without start_hash; set start_hash (copy the N:hhhhhh prefix)
else:
    今日 old_string 模式          // old_string 空 → 错；其余一字不改
```

`end_hash` 缺省 = `start_hash`（单行替换）。`start_hash` / `end_hash` 接受 `hhhhhh` 或 `N:hhhhhh`（含 read_file 左侧填充空格，以及误抄上的尾 TAB）；推荐模型抄整个前缀。

Schema（`edit.go:533`）`required` 从 `[path, old_string, new_string]` 改为 `[path, new_string]`。严格 schema 校验的 provider 否则会在 hash 模式拒掉缺 `old_string` 的合法调用。handler 仍要求「`start_hash` 或 `old_string` 至少一个」。不做 JSON Schema `oneOf`（部分 provider 支持差）。

> Phase 2 起本节的分派被 §17.2 的完整版本**取代**（新增 `edits` / `after_hash` 分支，`required` 进一步收缩为 `[path]`，见 D5）。

---

## 5. Hash 语义（本设计最容易审错的一节）

### 5.1 哈希的是什么

对 **去掉行尾 `\n` / `\r\n` 之后的行字节** 做 FNV-1a 64 位（标准库 `hash/fnv`，仓库里 `pkg/agent/breaker.go:458` 已在用，不在 builtin 包，无新依赖），取低 24 bit，格式化为 **6 位小写 hex**。

- **不**把行号编进 hash。编进去就退化成行号别名，E5 全回来。
- **不**做空白归一。归一会让 `foo` 与 `foo  ` 撞到同一 hash，再写入时无法区分——这是静默写错的变种。匹配用精确内容；空白容忍只留在 `old_string` 路径。
- **不**引入 xxhash 等新依赖。6 hex 的分布足够；真相同内容（`}`、空行）加宽一位都救不了，消歧靠 §5.2 的行号提示，不靠 hash 宽度（Q1）。

空行、独立的 `}`、`return` 会 **自然共用同一个 hash**。这是内容寻址的定义，不是 bug。

### 5.2 定位算法（v2：行号消歧 + hash 校验）

v1 草案用「一对 hash 的联合唯一性」当唯一规则。这对 **整函数替换**——本设计自己的基准场景——是结构性失败，不是上线后才看得到的评测项：

- end 锚点只能是该函数列 0 的收尾 `}`。
- Go 文件里每个顶层函数的收尾行内容都是 `}`，hash 必然相同。
- 设 start 唯一（函数签名），`S={10}`，`E={20, 45, 80, …}`，则 `spans = {(10,20), (10,45), (10,80)}` → ≥2 → 拒绝。
- 只要目标函数后面还有任何函数，整函数替换必然歧义。§16.1 的「15 行函数改写」恰好是这个形状。
- 「换更有辨识度的 end 锚点」在这里无解：函数最后一行没有替代。退到函数体内最后一个独特行、让 `new_string` 不含尾 `}`，模型会大量写错，等于换一种失败。

所以定位必须同时吃 **行号提示** 和 **内容 hash**。模型直接抄整个 `N:hhhhhh` 前缀（比从中抽出 6 位 hex 更不易错，也顺带收掉 C2）。

**解析。** `parseHashRef(s) → (lineHint int, hash string)`：

| 输入 | 结果 |
|---|---|
| `a3f2b1` | hint=0（无提示），hash=`a3f2b1` |
| `12:a3f2b1` / `  12:a3f2b1` / `12:a3f2b1<TAB>` | hint=12，hash=`a3f2b1` |
| 仅 `12`、空、非 6 位 hex | 错。提示抄 `N:hhhhhh` 整个前缀，不要只抄行号或只发明 hex |

hash 模式 **不**复用 `start_line`/`end_line` 作提示（Q8）：这两个参数在 `old_string` 路径里是硬窗口，再给一套「软提示」语义会让同一字段两种意思。行号提示只从 `N:hhhhhh` 前缀来。若模型同时传了 `start_line`/`end_line`，hash 模式忽略它们。

对当前文件（与 `old_string` 路径同一套 `split`：最后一行若文件以换行结束则不造空行，与 `splitFileLines` / `lineStartOffsets` 对齐）算出每行 hash。

设 `H_s`/`H_e` 为起止 hash，`L_s`/`L_e` 为可选行号提示（0 表示没有）。`S` = 所有 `hash(line)==H_s` 的 1-based 行号，`E` 同理。

```
spans = { (s, e) | s ∈ S, e ∈ E, s ≤ e }
```

分派：

| 条件 | 行为 |
|---|---|
| `len(spans)==0` | 失败。区分「start 不存在 / end 不存在 / 都在但所有 start 行号 > 所有 end」（见 §8） |
| `len(spans)==1` | 用这一段。不需要行号提示。hash-only 的单处命中仍然合法 |
| `len(spans)≥2` 且 `L_s`、`L_e` 都 >0，且当前文件第 `L_s` 行 hash 为 `H_s`、第 `L_e` 行 hash 为 `H_e`、且 `L_s≤L_e` | **精确命中**（刚 read 完的主路径）。用 `(L_s, L_e)` |
| `len(spans)≥2` 且至少有一个行号提示 | 对每个 span 打分：有 `L_s` 则计入 `\|s-L_s\|`，有 `L_e` 则计入 `\|e-L_e\|`；**缺的那一侧不计入得分**（不得写成 `\|s-0\|`，那会系统性偏向文件开头的 span）。**唯一最近**则用它；并列最近则失败 |
| `len(spans)≥2` 且没有任何行号提示 | 失败。列出候选 span，要求抄整个 `N:hhhhhh` 前缀，不要只抄 hex |

精确命中与「最近 span」都已经要求两端 hash 匹配，所以提示行号即使漂了也不会授权一次无校验写入：漂了之后精确命中失败，退到最近 hash 匹配段——那是内容还在、只是行号变了。提示行上内容已被改掉、hash 对不上：该行不进入 `S`/`E`，不会被选中。

**校验范围是两端，不是区间内部（N1 / Q9）。** `old_string` 模式核对将被替换的每一个字节；hash 模式只核对起止两行。read 与 edit 之间若只有函数体被改（另一个 agent、`gofmt` 只动内部、或模型自己上一次改过这个函数内部），端点 hash 仍匹配，本次调用会覆盖模型没见过的中间内容。不加「所选 span 行数 == `L_e-L_s+1`」启发式：它会误伤合法链式场景——先改函数内部使其变长，再用旧前缀整函数替换，这正是 §5.3 应当成功的路径。单代理串行是主态，与 C6 已接受的并行竞态同级；记入 C13，明确接受。

整函数替换的主路径：`start_hash="10:a3f2b1"`（签名，通常内容唯一），`end_hash="20:7e88aa"`（`}`）。精确命中 `(10,20)`，后面那些同样是 `}` 的 45、80 行根本轮不到。上方插入 10 行之后再发同一对前缀：精确命中失败（第 10、20 行已经不是那两行），`S={20}`，`E={30,55,90,…}`，距提示 `(10,20)` 最近的是 `(20,30)`，即平移后的 Foo。E5 仍成立。

单行：`end_hash` 缺省等于 `start_hash`。该 hash 只出现一次 → 直接用；出现多次则必须带行号提示才能消歧，**禁止**默默改第一处。

不提供 `replace_all` 给 hash 模式：hash 范围的语义是「这一处」，不是「所有相同内容」。全文件替换继续用 `old_string` + `replace_all`。hash 模式若收到 `replace_all`，**忽略**（与 Q4 忽略多余 `old_string` 同精神），不报错。

### 5.3 为什么一次 read 能撑多次 edit

内容 hash **不随上方插入而改变**；行号提示漂了走「最近匹配」兜底。

```
read 一次
  edit_file(start="10:hash(Foo)", end="20:hash(})")   // 在文件前部插入 10 行
  edit_file(start="40:hash(Bar)", end="55:hash(})")   // Bar 的 hash 还在；提示行号对不上就取最近匹配
```

会失效的情况（应当失效）：

- 第二次编辑的锚点行本身被第一次改掉了（内容变了，hash 消失）。
- 第一次编辑复制出另一段相同内容，且行号提示距两段一样近（并列最近 → 拒绝）。
- 需要改的是自己刚刚写进去的新行——上下文里还没有这些行的 hash，必须再 read，或对这处改用 `old_string`。

因此 v1 **不需要** 多 hunk 批处理也能覆盖「读一次、改三处独立函数」。批处理只减少往返，放到 Phase 2。

### 5.4 碰撞概率（不同内容撞成同一 6 hex）

24 bit 空间。对「内容各不相同」的行，500 行文件期望意外碰撞对数 ≈ C(500,2)/2²⁴ ≈ 0.007；2000 行 ≈ 0.12。意外碰撞与「真·相同内容」在算法里同一处理：多 span，走 §5.2 的行号消歧；没有提示或并列最近则拒绝。不在 v1 做自适应加长（4/6/8 位混用），避免模型搞不清「这次 hash 有几位」。

---

## 6. `read_file` 输出格式

### 6.1 编号模式（推荐默认）

现在：

```
  12<TAB>func Foo() {
```

改为：

```
  12:a3f2b1<TAB>func Foo() {
```

- 行号保留：研究者角色、outline、「再读 80–120」、TUI、人类对照都还在用行号；hash 模式的消歧提示也从这里抄。
- hash 紧贴行号、仍以 TAB 分隔正文。`splitLineNumberPrefix` 今日要求 `digits<TAB>`（`edit.go:323`），新前缀是 `digits:hex<TAB>`，**旧 strip 不会误剥**——这是换格式的安全前提，仍然成立。
- `old_string` 路径若有人把新前缀整段贴回去：必须让 **`splitLineNumberPrefix` 同时认识** `digits<TAB>` 与 `digits:hex<TAB>`。`stripLineNumberPrefixes` 与 `looksLineNumbered` 都走它（`edit.go:88-101` 的防线是一对，见 D3）：strip 失败后，`looksLineNumbered` 负责拦下「带前缀但因删行编号跳跃而剥不干净的 `new_string`」，跳过 candidate，防止把前缀写进文件。只扩展 strip、不扩展 `looksLineNumbered`，一个带 `12:a3f2b1<TAB>`、编号不连续的 `new_string` 会通过 `!looksLineNumbered`，被原样写入——正是要防的静默腐蚀。

### 6.2 谁带 hash

| `read_file` 路径 | 是否带 `N:hhhhhh<TAB>` |
|---|---|
| range 且未显式 `line_numbers=false` | 是（今日已默认编号，`file.go:110`） |
| `line_numbers=true` 的全文 | 是 |
| outline 的 head / tail | 是（`shaping.go:57,74`；模型常直接改文件头/尾） |
| outline 的 `--- symbols ---` | 否，仍 `L  42  func Foo`（`shaping.go:63`，不含 `:`，C7 成立） |
| `line_numbers=false` 的 raw span | 否（`file.go:96-105`）。这是今日「干净 old_string」逃生口，保持可粘贴 |
| `full=true` 且未开 `line_numbers` | 否，与今日一致（原文） |
| 二进制 / 空文件 | 不变 |

**不**做 `hashlines=` 开关。再加一个开关等于让模型猜「这次有没有 hash」。编号输出一律带 hash；要原文去 `line_numbers=false`。

### 6.3 输入增量（粗算）

相对今日已有的 `  12<TAB>`（约 4–6 字节前缀），新前缀多 `:hhhhhh`（7 字节）× 行数。500 行 outline 的 head(50)+tail(20) 只多约 500 字节，可忽略。真正加成本的是「带着 hash 读了 200 行准备改」——这正是即将产生输出节省的那次读。

---

## 7. 替换的字节区间与 `new_string` 尾换行

与现有 `lineWindow`（`edit.go:213-238`）对齐：闭区间 `[s, e]` 的字节 span 是 **第 s 行首字节到第 e 行的行尾**。

- 若 `e` 不是文件最后一行：span **含** 第 e 行末的换行符。
- 若 `e` 是最后一行：span 到 EOF；文件原来有无最终换行，跟 `splitFileLines` 的规则走，不发明一个换行。
- `new_string` 先按文件主体换行风格做 `conformLineEndings`（hash 模式也做；这是写回，不是匹配）。
- **尾换行补齐（D2）**：模型从编号输出里看不到换行符，`new_string="func Foo() {\n    return y\n}"`（最后一行是 `}`，**没有**再跟一个 `\n`）是最自然的形态——§4 的示例就是这样。若按「span 含 e 的换行」原样写入且 `new_string` 无尾换行，新内容最后一行会与原第 `e+1` 行 **合并成一行**。写死：
  - `new_string` 非空，且 `e` 不是文件最后一行，且补齐换行风格之后仍不以换行结尾 → **补一个**（LF 或 CRLF，与 `conformLineEndings` 一致）。
  - `e` 是最后一行 → 跟原文件的 EOF 换行状态走：原来有最终换行则结果也有，原来没有则不发明。
  - `new_string` 为空（删除）→ **不**补换行。span 已含 e 的换行，区间消失后上一行与下一行之间只留原来那一个换行。
- 插入新行：v1 **不**单独提供 `insert_before` / `insert_after`。在锚点行上做「替换为 锚点原文 + 新行」——这要求模型写出锚点原文，只在插入场景退回少量复述。`after_hash` 放到 Phase 2（Q2）。

整文件替换：起止分别是第一行与最后一行的 `N:hhhhhh`，或继续用 `write_file`。空文件没有行，hash 模式直接报错，指去 `write_file`。

---

## 8. 失败信息（比今日 near-miss 更短，因为没有旧文可对齐）

Hash 模式 **不走** `nearestMissHint`（没有 `old_string` 可对齐）。错误必须让模型一次知道该换锚点、补前缀还是该重读：

| 情况 | 信息要点 |
|---|---|
| start 不在文件里 | `start_hash a3f2b1 not in foo.go (412 lines). Re-read with read_file and copy the N:hhhhhh prefix from the transcript — do not invent hashes` |
| end 不在 | 同上，点名 `end_hash` |
| 都在但所有 start 行号 > 所有 end | `start_hash is after end_hash (start at line X, end at line Y); swap them` |
| 多段合法区间且无法消歧（无行号提示，或并列最近） | `hash range a3f2b1..7e88aa matches N spans at lines A-B, C-D, ...; copy the full N:hhhhhh prefix (line+hash), not just the hex` |
| 单行 hash 出现 N 次且无行号提示 | `hash a3f2b1 occurs N times (lines …); pass start_hash as N:hhhhhh, or use old_string` |
| 像写错了一位 | 若某个文件 hash 与给定值在 **6 个 hex 字符位上恰好差 1 个字符**（不是 bit Hamming）：额外一句 `closest hash is a3f2b0 at line 88`。只提示，不代写 |
| `new_string` 键缺失 | `new_string is required (omit nothing; pass an empty string to delete the range)` |
| 只传 `end_hash`、不传 `start_hash` | `end_hash without start_hash; set start_hash (copy the N:hhhhhh prefix)` |
| 空文件 | `foo.go is empty; use write_file` |

成功回包（仍保持短，兼容 aging 的 300B 档）：

```
Replaced lines 12-18 (a3f2b1..7e88aa) in foo.go with 4 lines (d1e2f3..c0ffee)
```

删除变体（`new_string` 为空，没有新行 hash）：

```
Deleted lines 12-18 (a3f2b1..7e88aa) in foo.go
```

`Data`：

| 键 | 用途 |
|---|---|
| `start_line` | 与今日相同，TUI gutter |
| `end_line` | 原区间末行（可选，TUI 暂不用） |
| `old_text` | **解析出来的旧区间原文**。TUI 今日用 `args["old_string"]` 做 diff；hash 模式下参数里没有旧文，必须从 Data 补，否则 TUI 空白（见 §9） |
| `start_hash` / `end_hash` | 回显解析后的 6 hex（不含行号），便于日志 |

`Data` 不进模型上下文：`toolexec.go:218-234` 的 `toolResultPreview` 只在 `Content` 为空时才序列化 `Data` 做预览；成功路径有 `Content`，offload 也只作用于 `Content`。所以 `old_text` 不花 token。

---

## 9. TUI 与其它调用方

`pkg/chat/tooldisplay.go` 的 `renderToolDiff("edit_file")` 现在是：

```go
oldStr, _ := args["old_string"].(string)
newStr, _ := args["new_string"].(string)
return m.diffBlock(path, lineDiff(...), startLine)
```

hash 模式下 `args["old_string"]` 为空，diff 会变成「纯新增」，**用户看见的和磁盘上的不一致**。v1 必须改成：`old_string` 空则用 `data["old_text"]`。签名已接收 `data`，不必改。这是功能正确性，不是美化。

`SessionCarry.editedFiles`、review 归因、breaker 里对 `edit_file` 的计数，都只看工具名与 path，不受模式影响。

---

## 10. 提示词与工具描述

路由规则在系统提示，不在每个工具描述里复述（`descriptions_test.go` 的 T3a）。

`fileOperationRulePrompt`（`pkg/agent/promptbuild.go`）末尾加一句，不新开段落：

> After read_file, prefer edit_file with start_hash/end_hash copied from the whole `N:hhhhhh` prefix (not just the hex, not just the line number) and only new_string; do not restate the old text. If a hash range misses, re-read and copy the prefixes; do not invent them. old_string remains valid when you did not get hashes (grep hits, raw spans).

`edit_file` Description 仍须含 `"Replace"`（现有 gist 测试）。扩成两段模式说明：hash 范围优先，值是整个 `N:hhhhhh` 前缀；`old_string` 作为无 hash 时的路径。不要在描述里再列 bash 禁令。Description 里今日那句「strip … and grep add」要改准：strip 只处理 `read_file` 的编号前缀（含新的 `N:hhhhhh<TAB>`），不处理 grep 的 `file:line: `。

`read_file` Description：写明编号输出是 `N:hhhhhh<TAB>content`，整个前缀抄进 `edit_file` 的 `start_hash`/`end_hash`；`line_numbers=false` 仍是 raw span。

---

## 11. 分期

**Phase 1 — 闭环（建议一次做完，否则模型会处于「看见 hash 却不能用」的状态）**

1. `pkg/tools/builtin/hashline.go`（新文件）：`lineHash` / `renderNumbered` / `parseHashRef` / `resolveHashRange`。`edit.go` 已 644 行，不要再堆。
2. `file.go` + `shaping.go`：编号与 outline head/tail 改用新前缀。
3. `edit.go`：
   - 入口分派 **先于** `old_string is required`（`edit.go:29`）。只传 `end_hash` 明确报错（N4）；`replace_all` 在 hash 模式忽略（N5）。
   - schema `required` 改为 `[path, new_string]`（`edit.go:533`）。
   - `new_string` 按键是否存在取值，缺失报错，空串才是删除。
   - hash 模式走 §5.2 / §7。`resolveHashRange` 评分：缺的一侧 hint **不计入**（N2）。
   - **扩展 `splitLineNumberPrefix`**（`edit.go:313`）同时认 `digits<TAB>` 与 `digits:hex<TAB>`——`stripLineNumberPrefixes` 与 `looksLineNumbered` 自动继承（D3）。
4. `tooldisplay.go`：diff 回退到 `Data["old_text"]`。
5. 提示词 + 两个 Tool Description。
6. 测试见 §12。

**Phase 2 — 分派、格式与字节语义一律以 §17 为准**

- grep 命中行带 hash（17.1）。
- `edits` 单调用多 hunk，按原始字节 span 单遍拼接（17.2）。
- `after_hash` 纯插入（17.3）。
- 成功回包带新区间逐行 hash（17.4）。

**Phase 3 — 只在 hash 模式成为实测主路径之后**

- 评估能否收缩 unescape / strip / whitespace 容错（**不能删**：raw span 仍走 `old_string`；grep 自 Phase 2 起已带 hash，不再是保留理由）。
- 不删除 `old_string`。

**触发判据（写死，免得到时凭感觉）。** 三个信号全部可从 `~/.deepai/sessions/*.json` 离线统计（会话持久化了 `tool_calls.arguments` 与 `tool_result.content/error`），零新增埋点：

1. **模式份额**：按 arguments 分类——有 `edits` / `after_hash` / `start_hash` 的算 hash 系，其余算 `old_string`。
2. **各模式失败率**：`old_string not found`（old 路径 miss）；`not in`（hash miss）；`matches N spans` / `occurs N times`（歧义拒绝）。
3. **容错层使用率**——Phase 3 **真正的开关**：三层容错每次实际救回编辑，成功消息都带注记 `escape-normalized` / `line-number prefixes stripped` / `whitespace-tolerant match`，直接数窗口内出现次数。

| 条件 | 阈值 | 不满足时 |
|---|---|---|
| 样本量 | 累计 ≥200 次 `edit_file` 且跨 ≥2 周真实使用 | 继续等；不足量的份额没有统计意义 |
| 主路径 | hash 系份额 ≥70% | 先查提示词/描述为何模型不用，不是开 Phase 3 |
| 质量不劣化 | hash 系一次成功率 ≥ `old_string`；歧义拒绝 <5% | 先回 §5.2 调参，不是开 Phase 3 |
| **收缩开关（逐层）** | 该层的注记在窗口内归零或接近零 | 哪层注记还在出现，哪层就不能收 |

份额达标不等于可以收缩：只要 `whitespace-tolerant match` 还在救 raw-span 场景的编辑，那层就是活的。最可能先死的是 strip 层——模型拿到 hash 后不再贴编号前缀。评估时写个十几行脚本扫 sessions 即可；观测窗口自 Phase 2 合入（2026-09-14，`e5f5177`）起算，本机此前的会话史里只有 2 次 `edit_file`（皆为 hashline 之前的 `old_string`），不计入。

---

## 12. 测试计划（Phase 1 必须有的，不是建议）

**回归门是 edit 侧行为不变，不是「这些文件零失败」。** `edit_test.go` / `edit_range_test.go` / `edit_nearmiss_test.go` 以及 `linenum_test.go` 里走 `old_string` 的用例（含手工拼的 `1\talpha` 旧前缀）应原样通过——旧 `digits<TAB>` 仍被 `splitLineNumberPrefix` 认识。

读侧编号格式断言必须更新，**不是回归失败**（§13）：

- `improvements_test.go`：`TestReadFileHandler_LineRange`（断言 `"2\tb\n3\tc\n4\td\n"`）、`TestReadFileHandler_LineNumbersOnly`、`TestReadFileHandler_ReversedOutOfRangeLineRange`
- `linenum_test.go`：`TestReadFile_RangeStillNumbersByDefault`（断言 `"2\tb\n3\tc\n"`）

哨兵：`linenum_test.go` 的 `TestEditFile_OldStringCopiedFromRangeRead` 把 **真实** `read_file` 输出原样贴进 `old_string`。格式改成 `N:hhhhhh<TAB>` 加上 strip 扩展之后，这条应 **原样通过**——它同时锁住「新前缀能剥」和「旧 edit 路径不被新输出弄坏」。

新增：

| 用例 | 断言 |
|---|---|
| 整函数替换：签名唯一、收尾 `}` 在文件中出现多次，带 `N:hhhhhh` 前缀 | 命中目标函数，不动后面那些 `}`（D1 主路径 / §16.1） |
| 同上但只传 hex、不带行号 | 歧义拒绝，文件原样 |
| 上方插入 10 行后再用 **同一对旧前缀** 改下方函数 | **不**重读，走最近匹配，第二次命中平移后的函数（E5） |
| 提示行号处的内容已被改掉 | hash miss，文件原样 |
| `end_hash` 缺省 | 单行替换；该 hash 多处出现且无行号提示 → 歧义，不改第一处 |
| start/end 对调 | 明确 swap 错误 |
| 6 个 hex 字符位 Hamming=1 的笔误 | 提示 closest，不代写 |
| `new_string` 无尾换行 + `e` 非末行 | 补换行，**不**与下一行合并（D2） |
| `new_string` 无尾换行 + `e` 是末行 | 跟原文件 EOF 换行状态走 |
| 删除区间 / 替换为更多行 / 无最终换行的文件 | 与 `lineWindow` 字节约定一致；删除成功回包走 Deleted 变体 |
| `new_string` 键缺失 | 报错，不删除 |
| `new_string` 显式 `""` | 删除该区间 |
| CRLF 文件 | 写回仍是 CRLF；补的尾换行也是 CRLF |
| 把 `12:a3f2b1<TAB>` 贴进 `old_string` | strip 后按正文匹配，**不**把前缀写入文件 |
| `new_string` 带新前缀但编号因删行而跳跃（剥不干净） | `looksLineNumbered` 认新前缀，跳过 candidate，**不**把前缀写入文件（D3） |
| `line_numbers=false` | 输出无 hash，仍可当 `old_string` |
| TUI | 无 `old_string` 时用 `old_text` 渲染出 `-` 行 |
| schema / 入口 | 无 `old_string`、有 `start_hash` 的调用能进 hash 模式；无二者则错 |
| 只传 `end_hash` | 明确报错补 `start_hash`，不落进 `old_string is required` |
| hash 模式带 `replace_all` | 忽略，仍按这一处替换 |
| 描述测试 | `Replace` gist 仍在；T3a 不回归 |

---

## 13. 涉及文件（实施时）

| 文件 | 改动 |
|---|---|
| `pkg/tools/builtin/hashline.go` | 新：hash / 渲染 / `parseHashRef` / 解析 |
| `pkg/tools/builtin/hashline_test.go` | 新 |
| `pkg/tools/builtin/file.go` | 编号输出 |
| `pkg/tools/builtin/shaping.go` | outline head/tail |
| `pkg/tools/builtin/edit.go` | 分派顺序、schema `required`、`new_string` 键检查、`splitLineNumberPrefix` |
| `pkg/chat/tooldisplay.go` | diff 数据源 |
| `pkg/agent/promptbuild.go` | `fileOperationRulePrompt` 一句 |
| `pkg/tools/builtin/improvements_test.go` | **预期改动**：三处读侧格式金值改为 `N:hhhhhh<TAB>`（`LineRange` / `LineNumbersOnly` / `ReversedOutOfRangeLineRange`） |
| `pkg/tools/builtin/linenum_test.go` | **预期改动**：`TestReadFile_RangeStillNumbersByDefault` 金值更新。`TestEditFile_OldStringCopiedFromRangeRead` 不改断言，作哨兵 |
| 上述其余测试文件 | edit 侧回归 + 新用例 |

不改 `write_file`、不改 sandbox、不改 MCP 工具包装（它们不走这条编号格式）。

---

## 14. 要评审拍板的问题

**Q1. Hash 宽度：6 hex 还是 8 hex？**  
**已采纳：6。** 真正的歧义来源是内容真相同（`}`、空行），加宽救不了，由 §5.2 的行号提示解决。

**Q2. v1 是否做 `after_hash` 纯插入？**  
**已采纳：不做。**

**Q3. 编号输出是否允许关 hash？**  
**已采纳：不允许。** 要原文走已有的 `line_numbers=false`。

**Q4. `old_string` 与 hash 同时传入？**  
**已采纳：hash 赢、忽略 `old_string`。** 分派必须在 `old_string is required` 之前（§4 / §11.3）。

**Q5. grep 是否进 Phase 1？**  
**已采纳：否。** grep 仍是 `file.go:12:`。从 grep 直接 edit 继续走 `old_string`。

**Q6. 成功回包是否逐行列出新 hash？**  
**已采纳：v1 只报新区间起止两个**（删除走 Deleted 变体，§8）。

**Q7. 要不要会话 ID 表作为并列方案？**  
**已采纳：否决，且降为存档备选。** §5.2 的行号提示覆盖了「`} ` 歧义」这个原设想的 v2 触发条件，不需要上线后再看评测占比。

**Q8. 行号提示从哪来：整个 `N:hhhhhh` 前缀，还是复用 `start_line`/`end_line`？**  
**已采纳：抄整个前缀进 `start_hash`/`end_hash`。** 不给 `start_line`/`end_line` 加第二套「软提示」语义。字段名保持 `start_hash`/`end_hash`（schema 加法，旧文档仍能对上），值可以是 `hhhhhh` 或 `N:hhhhhh`。

**Q9. 区间内部不校验，要不要加「span 行数 == `L_e-L_s+1`」启发式？**  
**已采纳：不加，明确接受。** 启发式会误伤「先改函数内部使其变长、再用旧前缀整函数替换」（§5.3 应当成功）。端点校验挡住「写到另一段」；内部漂移与 C6 同级，记入 C13。

---

## 15. 自审（定稿级，不是实现后的）

| # | 风险 | 处置 |
|---|---|---|
| C1 | 模型编造 hash | miss +「不要 invent」；hex 字符 Hamming=1 只提示 |
| C2 | 模型把 hash 当行号用，或只抄 6 hex | 教抄整个 `N:hhhhhh`；只传 `12` 直接拒；只传 hex 在多 span 时拒并要求补前缀 |
| C3 | 新前缀被贴进 `old_string` / `new_string` 写成文件 | 扩展 `splitLineNumberPrefix`，strip 与 `looksLineNumbered` 一起继承（D3） |
| C4 | TUI 把替换显示成纯新增 | `old_text` 是 Phase 1 硬门；Data 不进模型上下文 |
| C5 | 重复行无法定位 | 行号消歧 + hash 校验；无提示或并列最近才拒绝。整函数 `}` 是成功路径 |
| C6 | 并行两个 `edit_file` 打同一文件 | 今日也有这个竞态；hash 不使之更糟。同一 turn 并行写同一 path 仍应避免 |
| C7 | outline 只有 head/tail 有 hash，模型用符号行号去当 hash | 符号行仍是 `L  42`，不含 `:` hex；误用会 miss |
| C8 | 提示词变长 vs T3a | 只改系统提示一句 + 两段 Description，不把路由表抄回工具 |
| C9 | 6 hex 意外碰撞 | 当多 span，走行号消歧；Q1 维持 6 |
| C10 | 删除 `old_string` 的冲动 | 明确非目标；grep / raw span / 没读过的文件还靠它 |
| C11 | 漏传 `new_string` 被当成删除 | 检查键是否存在（§4） |
| C12 | `new_string` 无尾换行导致与下一行合并 | §7 补齐规则；§12 有用例 |
| C13 | 只校验端点：区间内部被改仍会覆盖未见内容 | **接受**。单代理串行是主态，与 C6 同级。不加长度启发式（Q9 / §5.2） |

---

## 16. 建议的验收标准（实现之后用，现在不当作已完成）

1. 同一段 15 行函数改写（签名 + 收尾 `}`，文件后面还有其他函数）：hash 模式发出的工具参数字符数相对 `old_string` 模式下降 ≥ 40%（`new_string` 等长的前提下），且 **一次调用成功**，不因 end hash 共用而歧义。
2. 用本仓库历史里「中间一行凭记忆打错」的那类样本：hash 模式 **不会** 再以「old_string not found」失败（因为根本不发 old_string）。
3. 先改文件前 20 行（插入），再改文件后部一个未触及函数（仍用第一次 read 抄下的 `N:hhhhhh`）：第二次调用 **零** 中间 `read_file`。
4. edit 侧现有测试保持绿色（含 `TestEditFile_OldStringCopiedFromRangeRead` 哨兵）。读侧编号格式金值按 §13 更新后保持绿色，不把「旧金值失败」当成回归。

---

## 17. Phase 2 设计（v4 草案，待评审）

Phase 1 的四个后续项各自的语义、格式与失败面。基线是已合入的 Phase 1（`747799c`）。共同原则不变：hash 是内容校验，行号是提示；定位失败必须失败；工具无状态。

### 17.1 P2-A：grep 命中行带 hash

**现状**（调研于 v4）：grep 有三处渲染点，全部是 `%s:%d: %s`——平铺（`grep.go:110`）、context 模式的读文件失败回退（`grep.go:253`）、context 行本体（`grep.go:280`）。`readFileLines`（`grep.go:301`）用 `strings.Split(data, "\n")`，与 `splitFileLines` 不同：文件以 `\n` 结尾时会多出一个幻影空行（可被 `x*` 这类可匹配空串的 pattern 命中，报出 read_file 认为不存在的行号），且行内保留 `\r`（`lineHash` 剥 `\r`，所以 hash 天然与 read_file 一致）。`Data["matches"]` 是结构化边车（`grepMatch{File, Line, Content}`）。

**格式**：三处渲染统一改为

```
file.go:12:a3f2b1: content
```

- **过抄容错（D4）——「edit 侧零改动」不成立，撤回。** Phase 1 已证明模型贴的是「看见的整段前缀」，不是精确抽取 6+2 位。grep 的分隔符是 `:` 不是 TAB，I2 的 TAB 截断帮不上：`12:a3f2b1: content` 会把 `a3f2b1: content` 当 hexPart 拒掉，`file.go:12:a3f2b1: content` 第一段按 `:` 切出 `file.go` 也拒。若不修，P2-A 的「grep 前缀直接编辑、零 read」主路径会按 Phase 1 见过的过抄方式系统性 miss。修法：`parseHashRef` 增加与 I2 同精神的定位规则——取串中**第一个**满足「前邻是串首或 `:`、后邻是 `:` / TAB / 串尾」的 `\d+:[0-9a-f]{6}`。`file.go:12:a3f2b1: see 99:deadbe: x` 解析出 `12:a3f2b1`（`99:deadbe` 前邻是空格，不入选）；bare `hhhhhh` 与 read_file 的 `N:hhhhhh<TAB>` 形态不变。miss 文案从「copy from read_file」改为「copy the N:hhhhhh prefix from read_file or grep」。
- context 行（`grep.go:280`）**同样带 hash**（Q11）：match ± context 就是现成的 start/end 锚点对，小范围编辑可以完全不 read。
- context 模式的读文件失败回退（`grep.go:253`）手上仍有 `m.Content`，hash 从它现算——回退行同一格式，不降级成无 hash。
- `grepMatch` 增加 `Hash` 字段进 `Data["matches"]`。`displayGrepMatches`（`grep.go:292-296`）是逐字段复制，**必须补抄 `Hash`**，否则文本输出有、结构化边车丢（17.6 有门）。
- `readFileLines` 改用 `splitFileLines`（Q13）：消灭幻影空行，让 grep / read_file / edit_file 对「文件有几行」口径一致。这是行为收紧：以前能匹配幻影空行的 pattern 少报一行，属修正不属回归。
- `edit.go:165` 的 miss 错误文案里 grep 前缀示例改为 `file.go:12:a3f2b1: `；grep Description 的 `file:line:content` 措辞同步。`old_string` 路径**仍不剥** grep 前缀（与今日一致；模型该抄的是 `N:hhhhhh`，不是整行贴回）。

**提示词**：`fileOperationRulePrompt` 尾句的「old_string remains valid when you did not get hashes (grep hits, raw spans)」中 grep 不再成立，改为「(raw spans)」并加半句「grep hits carry N:hhhhhh too」。prompt 金值随之更新（同 I1 惯例，记录字节增量）。

### 17.2 P2-B：`edits` 多 hunk（含插入 hunk，Q12）

新参数 `edits`：对象数组，每项是 `{start_hash, end_hash?, new_string}`（替换/删除）或 `{after_hash, new_string}`（插入）。**全部对照同一份原始文件解析**——hash 表只算一次，这正是「读一次、发 N 个 hunk」的意义。

**完整分派（D5，取代 §4 的 Phase 1 版本）。** schema `required` 收缩为 `[path]`：顶层 `new_string` 不再由 schema 强制——多 hunk 时每项自带 `new_string`，顶层再 required 会让严格 schema 的 provider 拒掉合法调用，与 Phase 1 小 2 同一个坑。顶层 `new_string` 的键检查移入非 `edits` 分支，由 handler 强制：

```
path 空 → 错
if edits 键存在:
    非数组或空数组 → 错
    多 hunk 模式：顶层 start_hash / end_hash / after_hash / old_string /
                  replace_all / new_string 一律忽略（Q4 精神，C18）
    每个 hunk 自检，错误带下标，与顶层规则对称：
      new_string 键不存在        → 错
      after_hash 与 start_hash 都非空 → 错
      after_hash 非空且 new_string 为空 → 错
      只有 end_hash              → 错（N4 同款）
      after_hash / start_hash 都空 → 错
else:
    顶层 new_string 键不存在 → 错（漏传 ≠ 空串）
    if after_hash 与 start_hash 都非空 → 错（17.3）
    if after_hash 非空 → 插入（new_string 为空 → 错）
    if start_hash 非空 → hash 替换（replace_all 忽略，N5）
    if end_hash 非空   → 错（N4）
    else               → old_string 模式（old_string 空 → 错；其余一字不改）
```

**解析与校验（写盘之前全部完成）**：

1. 每个 hunk 独立按 §5.2（替换）或单行规则（插入锚点）解析。解析错误**全部收集后一次报出**（Q14），每条带 hunk 下标——模型一次重试就能全修。
2. 重叠检查**只按行号**（N6）：替换闭区间 `[s,e]` 两两不相交；插入锚点行 ∉ 任何替换闭区间；两个插入锚点行不得相等（应用顺序无定义）。违反则拒绝整个调用，点名 hunk 下标与行号。相邻合法：hunk 到第 5 行、下一个从第 6 行起；after 第 5 行的插入 + 替换 6–10 也合法——插入的零宽字节点与替换 span 起点是**同一个字节**（第 5 行换行之后 = 第 6 行行首），所以重叠判定不能用字节闭区间，否则会把这条合法相邻误杀。
3. 任何一步失败 → **一个字节都不写**（原子性，C15）。

**应用**：字节 span 一律**半开区间** `[a,b)`，插入是零宽 `[p,p)`（N6）。按原始文件偏移升序单遍拼接（`orig[0:a₁] + r₁ + orig[b₁:a₂] + r₂ + …`）；同一偏移上零宽插入排在替换**之前**（`orig[0:p] + insert + replacement + orig[q:]`）。每个 hunk 的尾换行补齐按 §7；「e 是末行」的 EOF 规则只作用于覆盖末行的那个 hunk。模型发送顺序无关，内部按位置排序，回包按文件顺序列出。

**回包**（仍求短；细节**最多 3 条**，其余折叠——C21 罩的不只是逐行列表，8 个 hunk 的摘要本身就能先破 300B 档）：

```
Applied 3 edits in foo.go: inserted 2 lines after line 5, deleted lines 20-22, replaced lines 40-45 with 4 lines
Applied 8 edits in foo.go: inserted 2 lines after line 5, deleted lines 20-22, replaced lines 40-45 with 4 lines, +5 more
```

`Data["hunks"]`：`[{start_line, end_line, old_text, new_string}, …]`（TUI 用，见 17.5）。行号为原始文件坐标（与单 hunk 的 `start_line` 语义一致）。

### 17.3 P2-C：`after_hash` 纯插入（顶层）

顶层参数 `after_hash`：在锚点行**之后**插入 `new_string`，不复述锚点。与 `start_hash` 同时传 → 报错（两种定位语义，不猜）。`new_string` 为空 → 报错（插入空内容必是笔误，不当 no-op 吞掉）。

字节语义（对齐 §7）：

- 插入点 = 锚点行 span 的末尾（含其换行符之后）。
- 锚点不是末行，或是末行且文件有最终换行：插入文本补齐为以换行结尾。
- 锚点是末行且文件**无**最终换行：先补一个分隔换行（否则与锚点行合并）再接插入文本；工具**不发明**尾换行，但模型显式写在 `new_string` 末尾的 `\n` **保留**（N7，与 §7 / I4 同一口径——那是内容变更，文件因此带上最终换行，不算工具发明）。
- 空文件没有锚点行：报错指去 `write_file`（与 hash 替换的空文件行为一致）。
- **限制**：无法在第 1 行之前插入（没有第 0 行可锚）。prepend 场景（许可证头等）走 `old_string`（「替换第 1 行为 新内容+原第 1 行」，复述一行）或 `write_file`，Description 写明。不为此开 `before_hash`。

回包：`Inserted 3 lines after line 12 (a3f2b1) in foo.go`；`Data`：`start_line` = 锚点行号 + 1，`old_text` = ""（TUI 渲染为纯新增——这次是真的纯新增）。

### 17.4 P2-D：成功回包带新区间逐行 hash（可链式）

替换/插入后，新区间 ≤ **8 行**（Q10）时，回包把每行的**可直接粘贴的 `N:hhhhhh` 前缀**列出来，行号是**编辑后**的真实行号：

```
Replaced lines 12-18 (a3f2b1..7e88aa) in foo.go with 4 lines: 12:d1e2f3 13:0aa1b2 14:5c6d7e 15:c0ffee
```

- 这直接解决 §5.3 的第三条失效（「改自己刚写的行必须再 read」）：模型从回包抄前缀即可链式再改，**零 read**。
- \> 8 行退回 v1 的起止两个 hash（回包保持短，兼容 aging 300B 档；8 行 ≈ +80 字节）。
- 多 hunk 模式：编辑后行号的公式写死——替换/删除 hunk 的行数增量 `Δ = k − (e−s+1)`（k 为新行数，删除 k=0），插入 `Δ = +k`；某 hunk 的编辑后起始行 = 原始起始行（插入为锚点行号 + 1）+ 文件序上其**前方**所有 hunk 的 Δ 之和，应用时顺手累计。多 hunk 回包里逐行列表只在**总新行数** ≤ 8 时给出，否则全部退回省略（一个 hunk 列、另一个不列反而教坏模型）。

### 17.5 TUI 与调用方

- 单 hunk 各模式不变（Phase 1 已处理）。
- 多 hunk：`renderToolDiff` 检测 `args["edits"]` 存在时，改从 `Data["hunks"]` 迭代，逐 hunk 用现有 `diffBlock` 渲染并纵向堆叠（每块自带 `start_line` gutter）。无 `Data["hunks"]`（老会话回放）则退化为不渲染 diff，走通用 preview。
- `SessionCarry` / breaker / review 归因不变（仍只看工具名与 path）。

### 17.6 测试计划（Phase 2 硬门）

**grep 金值预期改动**（同 N3 惯例）：`grep_test.go` 的格式断言（如 `grep_test.go:341` 的 `a.txt:3: target line`）更新为 `file:line:hash: `。注意 `a.txt:1:` 这类**子串**断言在新格式（`a.txt:1:hhhhhh: ` 含 `a.txt:1:`）下仍然命中——它们测的是「有没有命中该行」，可以留着，但**不能当格式回归门**；格式门一律断言完整的 `file:line:hash: ` 前缀。

| 用例 | 断言 |
|---|---|
| grep 平铺 / context / context-回退 三处输出 | 均为 `file:line:hash: content`；`Data["matches"]` 带 `hash`，`displayGrepMatches` 不丢 |
| grep 前缀直接喂 `start_hash`（不 read） | 单行 hash 编辑成功（P2-A 闭环） |
| `file.go:12:a3f2b1: content` 与 `12:a3f2b1: content` 整段贴进 `start_hash` | 都解析出 `12:a3f2b1`，编辑成功（D4） |
| 整行过抄且正文含假 hash（`…a3f2b1: see 99:deadbe: x`） | 取第一个合规前缀，不吃正文里的 `99:deadbe` |
| CRLF 文件 | grep 报的 hash == read_file 报的 hash |
| 以 `\n` 结尾的文件 + 可匹配空串的 pattern | 无幻影末行命中（Q13） |
| 多 hunk：乱序发送 3 个不相交 hunk | 结果 == 依次单 hunk 编辑；回包按文件顺序 |
| 多 hunk：相邻（e=5 与 s=6）、含插入与替换混合 | 合法且正确 |
| 多 hunk：after 第 5 行 + 替换 6–10（同一字节偏移） | 合法，insert 排在替换之前（N6） |
| hunk 级分派：只传 `end_hash` / `after_hash`+`start_hash` 同传 / 插入 hunk 空 `new_string` | 报错带下标，零写入（D5） |
| 顶层无 `old_string`、无 `start_hash`、无 `edits`（schema 只 require `path`） | handler 报 `old_string is required`，分派兜底不静默 |
| 8 个 hunk 的摘要 | 3 条细节 + `+5 more`（C21 封顶） |
| 多 hunk：span 重叠 / 插入锚点在替换区内 / 两插入同锚点 | 拒绝，点名 hunk 下标，文件原样 |
| 多 hunk：一个 hunk hash 错、其余合法 | 一次报出全部解析错误，零写入（C15/Q14） |
| 多 hunk：hunk 缺 `new_string` 键 / `edits` 为空数组 | 报错 |
| `after_hash`：中部插入 / 末行两种 EOF 状态 | 字节精确（17.3 规则） |
| `after_hash`：末行、文件无最终换行、`new_string="x\n"` | 结果 `…\nx\n`——分隔换行补上，模型的尾 `\n` 保留（N7） |
| `after_hash`：空文件 | 报错指向 `write_file` |
| `after_hash`：锚点 hash 多处出现且无行号提示 | 歧义拒绝；带提示则命中 |
| `after_hash` + `start_hash` 同时传 / `new_string` 为空 | 报错 |
| 链式：编辑 → 从回包抄 `N:hhhhhh` → 直接改刚写入的行 | 第二次调用零 read 成功（P2-D 闭环） |
| 新区间 9 行 | 回包只有起止 hash，无逐行列表 |
| 多 hunk 编辑后行号 | 回包前缀在编辑后的文件上逐一命中 |
| TUI 多 hunk | 逐 hunk 渲染 `-`/`+` 行；无 `Data["hunks"]` 不崩 |
| prompt 金值 / T3a / `Replace` gist | 按惯例更新并记录；不回归 |

### 17.7 自审补充（承接 §15 编号）

| # | 风险 | 处置 |
|---|---|---|
| C14 | grep 视图过期后拿旧 hash 来编辑 | 与 read 后编辑同一保护：端点 hash 现场重算，对不上即 miss |
| C15 | 多 hunk 部分成功、部分失败，文件进入中间态 | 解析、校验全部通过才写盘；任何失败零写入（17.2） |
| C16 | 两个 hunk 解析到重叠区间（模型复制了锚点） | 行区间相交即拒绝，点名下标 |
| C17 | 插入锚点行被另一 hunk 替换掉 | 锚点在替换区内即拒绝（锚点已被消费） |
| C18 | `edits` 与顶层 `start_hash`/`old_string` 同时传 | `edits` 赢，其余忽略（Q4 精神） |
| C19 | 自底向上应用的偏移错位 bug 类 | 不做增量偏移：按原始字节 span 单遍拼接；乱序输入测试压住 |
| C20 | grep 幻影末行的 hash 指向不存在的行 | Q13 对齐 `splitFileLines`，从源头消灭 |
| C21 | 逐行 hash 列表**或多 hunk 摘要**撑爆短回包 | 列表 ≤8 行才给（Q10）；摘要细节封顶 3 条 + `+N more`（17.2）——先破 300B 的是摘要，不是列表 |
| C22 | TUI 多 hunk 渲染缺数据（老会话回放无 `Data["hunks"]`） | 缺则退化为通用 preview，不崩（17.5） |

### 17.8 Phase 2 拍板问题

**Q10. 逐行 hash 的行数上限？**  
**已采纳：8**，且摘要本身封顶 3 条细节 + `+N more`（C21）——300B 档先被摘要破，不是列表。

**Q11. grep 的 context 行也带 hash？**  
**已采纳：带**。match ± context 就是 start/end 锚点对；只给 match 行带，等于只支持单行编辑。

**Q12. `edits` 数组里允许插入 hunk？**  
**已采纳：允许**。规则在 17.2 定死（锚点不得在替换区内、同锚点双插入拒绝）。

**Q13. grep 的 `readFileLines` 改用 `splitFileLines`？**  
**已采纳：改**。幻影空行的行号 read_file 与 edit_file 都不承认，报出来只会制造 miss。

**Q14. 多 hunk 解析错误：报第一个还是全部？**  
**已采纳：全部一次报出**（带下标）。逐个报会把一次重试拖成 N 次往返，违背多 hunk 省往返的初衷。

---

## 修订记录

### v2（2026-09-14）— 响应评审 D1 / D2 / D3 / F1 及小问题

| 编号 | 性质 | 修订 |
|---|---|---|
| D1 | 设计缺陷 | §5.2 从「联合唯一性」改为「行号消歧 + hash 校验」。模型抄整个 `N:hhhhhh` 前缀。整函数替换成为主路径。§0 / E4 / C2 / C5 / §16.1 / Q7 同步。新增 Q8。 |
| D2 | 设计缺陷 | §7 写死：非空 `new_string` 且 `e` 非末行、无尾换行时补一个；末行跟 EOF 状态；删除不补。§4 示例保持无尾 `\n`，作为此规则的输入。§12 加用例。 |
| D3 | 正确性缺口 | 扩展点从「只改 strip」改为扩展共用的 `splitLineNumberPrefix`，`looksLineNumbered` 自动继承。§6.1 / §11.3 / C3 / §12 加「编号跳跃的 new_string 不得写入」用例。 |
| F1 | 事实错误 | 删掉「49 次 miss」。改引 `edit.go:541-549` 原文：能分析的 miss 都是同一形状，无计数。 |
| 小 1 | 表述 | §1.1：strip 只处理 `read_file` 的 `digits<TAB>`；grep 的 `file.go:12: ` 只在错误文案里。§10 Description 同步改准。 |
| 小 2 | 实现缺口 | §4 / §11.3：schema `required` 去掉 `old_string`；分派在 `edit.go:29` 之前。 |
| 小 3 | 正确性 | §4 / §12：`new_string` 按键存在取值；缺失报错，空串才是删除。 |
| 小 4 | 回包 | §8：删除变体 `Deleted lines …`。 |
| 小 5 | 口径 | §8：Hamming=1 指 6 个 hex **字符位**差 1 个字符，不是 bit。 |
| 补 | 前提 | §8：`Data["old_text"]` 不进模型上下文（`toolexec.go:218-234` 仅 Content 为空才序列化 Data）。 |
| Q1–Q7 | 拍板 | 按评审意见落成「已采纳」。Q7 的会话表从「上线后看评测」降为存档备选。 |

未改动、评审已核验通过的底座（v2 未重写）：`edit.go` 644 行与容错链、`lineWindow` / `conformLineEndings` / `splitFileLines` 字节语义、§6.2 的 read_file 各路径、`tooldisplay.go` 已收 `data`、碰撞概率数字、compression 105B/300B、T3a 与 `"Replace"` gist、`SessionCarry` 只记成功路径。

### v3（2026-09-14）— 响应评审 N1–N5

| 编号 | 性质 | 修订 |
|---|---|---|
| N1 / Q9 | 设计取舍 | E6 改准为「只保证端点」。§5.2 写明不校验区间内部、不加长度启发式（会误伤 §5.3 链式整函数替换）。§15 新增 C13：明确接受。 |
| N2 | 措辞 bug | §5.2 评分：缺的一侧 **不计入得分**，删掉「hint 当 0」（按字面实现会偏向文件开头）。 |
| N3 | 事实 / 测试门 | §12 回归门收窄为 edit 侧行为不变。读侧格式金值（`improvements_test.go` 三处 + `linenum_test.go` 的 `RangeStillNumbersByDefault`）列为 §13 预期改动。点名 `TestEditFile_OldStringCopiedFromRangeRead` 为哨兵。§16.4 同步。 |
| N4 | 分派缺口 | §4 / §8 / §12：只传 `end_hash` 明确报错补 `start_hash`，不落进 `old_string is required`。 |
| N5 | 未定义行为 | §5.2：hash 模式收到 `replace_all` 则忽略。§12 加用例。 |

### Phase 1 实施记录（2026-09-14）— 与设计的偏差

| # | 偏差 | 说明 |
|---|---|---|
| I1 | §13 漏列 `pkg/agent/promptbuild_golden_test.go` | §10 的提示词那句使系统提示 +344 字节，prompt 金值测试（len+sha256）按其自身惯例更新并记录变更原因；bash-only case 不含文件规则、不变。 |
| I2 | `parseHashRef` 对尾 TAB 更宽容 | §4 说容忍「误抄上的尾 TAB」；实现是**从第一个 TAB 起截断**，因此把整行 `12:a3f2b1<TAB>正文` 贴进 `start_hash` 也能解析。严格超集，不影响任何拒绝路径。 |
| I3 | 命名 | 渲染函数叫 `writeHashNumberedLine`（设计稿写 `renderNumbered`）；hash 模式的执行体 `editByHashRange` 也放在 `hashline.go`（`edit.go` 只留分派），符合 §11.1「不再堆 edit.go」的意图。 |
| I4 | 末行 EOF 换行的单向语义 | 「原来没有则不发明」实现为：原文件无最终换行时**不追加**，但模型显式发来的尾换行不剥（那是内容变更，不是工具发明）。原文件有最终换行、new_string 缺尾换行时照常补齐。 |
| I5 | `new_string` 键检查对两种模式生效 | §4 的伪代码即如此；对 `old_string` 路径这是一个边缘行为收紧（以前漏传 = 空串 = 删除匹配文本，现在报错）。schema 里 `new_string` 本就 required，现有测试无一依赖旧行为。 |

### v4（2026-09-14）— Phase 2 设计草案

新增 §17：P2-A grep 带 hash（三处渲染点统一 `file:line:hash: content`，context 行也带，`readFileLines` 对齐 `splitFileLines`）；P2-B `edits` 多 hunk（同一原始文件解析、重叠拒绝、原子写入、错误全量一次报出）；P2-C 顶层 `after_hash` 纯插入（EOF 两态字节语义、不支持 prepend 第 1 行）；P2-D 回包逐行 `N:hhhhhh`（≤8 行，编辑后行号，可直接链式）。新增拍板问题 Q10–Q14。§11 的 Phase 2 列表改为指向 §17。

### v5（2026-09-14）— 响应 Phase 2 评审 D4 / D5 / N6 / N7 及小问题

| 编号 | 性质 | 修订 |
|---|---|---|
| D4 | 设计缺陷 | 17.1：撤回「edit 侧零改动」——grep 分隔符是 `:`，I2 的 TAB 截断救不了整段过抄。`parseHashRef` 增加定位规则：取第一个「前邻串首或 `:`、后邻 `:`/TAB/串尾」的 `\d+:[0-9a-f]{6}`；miss 文案加 or grep；17.6 加两条过抄用例 + 假 hash 用例。 |
| D5 | 正确性 | 17.2 开头给出完整分派（`edits` → `after_hash` → `start_hash` → `end_hash` → `old_string`），hunk 级与顶层对称报错带下标；schema `required` 收缩为 `[path]`，顶层 `new_string` 键检查移入非 `edits` 分支；§4 加取代指针；17.6 加分派兜底用例。 |
| N6 | 口径 | 17.2：重叠**只按行号**判定（字节闭区间会误杀「after 第 5 行 + 替换 6–10」的合法相邻）；应用用半开字节区间 `[a,b)`，插入 `[p,p)`，同偏移时 insert 在前。 |
| N7 | 口径 | 17.3：末行无最终换行时，工具不发明尾换行、模型显式尾 `\n` 保留（与 §7/I4 同口径）；17.6 加 `new_string="x\n"` 用例。 |
| 小 | 各处 | 「против」笔误改「对照」；§11 子弹删「自底向上」改指 §17；C21 扩到摘要并封顶 3 条；grep 金值列为预期改动、子串断言不当格式门（17.6 开头）；`displayGrepMatches` 必须补抄 `Hash`；context 回退行同格式带 hash；17.4 行号公式写死（Δ 与前缀和）；空文件 + `after_hash` 报错指 `write_file`；prepend 维持限制、不开 `before_hash`。 |
| Q10–Q14 | 拍板 | 全部落成「已采纳」（Q10 含摘要封顶）。 |

### Phase 2 实施记录（2026-09-14）— 与设计的偏差

| # | 偏差 | 说明 |
|---|---|---|
| I6 | D4 的定位规则归并了 I2 | `parseHashRef` 先做 TAB 截断，再跑 `locateHashRef` 单一扫描（前邻串首或 `:`、后邻 `:`/TAB/串尾）；read_file 与 grep 两种过抄同一条路径处理，严格超集。 |
| I7 | prompt 金值再次移动 | §17.1 的提示词扩句 +64 字节，`promptbuild_golden_test.go` 按 I1 惯例更新并记录。 |
| I8 | `after_hash` 的防御性守卫 | 单 ref 经 §5.2 解析，评分数学上单行 span 必不劣于跨段 span，理论上拿不到 `s≠e`；实现仍显式拒绝 `s≠e`，当不变量守卫而非可达分支。 |
| I9 | 多 hunk 回包 Data 多一个键 | 除设计定义的 `hunks` 数组外，顶层再带 `start_line`（= 文件序首个 hunk 起始行），与单 hunk 的 Data 形状兼容。 |
| I10 | 测试文件组织 | Phase 2 用例集中在新文件 `hashline_phase2_test.go`；grep 金值按 17.6 更新（`grep_test.go` 的 `a.txt:3:7127c2: target line` 与 `:1:72e62b: ` 两处），`TestEditFile_OldStringCopiedFromRangeRead` 哨兵未动、保持绿色。 |

### v6（2026-09-14）— Phase 3 触发判据

§11 Phase 3 的「hash 模式成为实测主路径」从一句话落成可执行判据：三个信号（模式份额 / 各模式失败率 / 容错层注记使用率）全部离线统计自 `sessions/*.json`，零新增埋点；四条阈值（样本量 ≥200 次且跨 ≥2 周、hash 系份额 ≥70%、质量不劣化、**逐层**注记归零才收对应层）。同时修正过时表述：grep 自 Phase 2 起已带 hash，不再是保留 `old_string` 容错的理由（raw span 仍是）。观测窗口自 `e5f5177` 起算。
