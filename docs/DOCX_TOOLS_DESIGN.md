# DOCX Tools 设计：文档润色 / 排版 / 总结

> 目标：参考 GenOffice 的"外部独立实现"三层架构图，结合 deepai 自身的工具注册、子代理、技能、沙箱机制，设计一套处理 `.docx` 的工具层。
>
> 文中 `file.go:123` 形式的行号是**撰写时的快照**，代码变动后会漂移 —— 定位时以同时给出的符号名（函数/常量/字段）为准。

---

## 1. 三层架构映射

GenOffice 图把 AI 流程拆成三层，并标出"需要替换"的部分。deepai 已经天然解决了前两层，**只需补第三层的 docx 工具**：

| GenOffice 层 | GenOffice 实现 | deepai 对应 | 动作 |
|---|---|---|---|
| ① AgentLoop + AgentSkill | ReAct 循环 / 工具定义 / 历史管理 | `pkg/agent` ReAct + `pkg/skill` + `pkg/tools` registry | **直接复用** |
| ② Transport (Electron IPC) | `window.desktop` IPC、主进程路由、看门狗 | 无 IPC 层 —— 直连 LLM API（`pkg/llm` 多 provider） | **不需要**（deepai 本就是"外部独立"的） |
| ③ tools / search | TipTap/ProseMirror 文档编辑、Genspark CLI | `pkg/tools/builtin/*`（web 已有，**docx 缺失**） | **本次新建** |

关键结论：

- GenOffice 把文档编辑做成 **TipTap/ProseMirror**（浏览器富文本 SDK），因为它跑在 Electron 渲染进程里。
- deepai 跑在终端 / HTTP 网关，处理的是 **OOXML（.docx 是 zip+XML）**，不是 DOM。所以"文档编辑"层在 deepai 里 = **能解析、能改、能保格式写回 .docx 的工具**。
- GenOffice 图里的"Genspark 部分需替换"在 deepai 不存在 —— deepai 的 `web_search`/`web_fetch` 本就不依赖 Genspark。

### 1.1 润色原理：Ground Truth + 窄补丁（贯穿全设计）

参考元宝对"AI 润色 docx 原理"的拆解，docx 润色的核心不是"重生成文档"，而是**把原文件当唯一真值（Ground Truth），只对改动的窄块做补丁（Narrow Patch）**。这条原则约束下面每一层：

1. **解析 + 位置映射**：.docx 是 zip+XML。抽取文本块时，**保留每个块到原 `<w:p>` / `<w:t>` 元素的字节区间映射**（para_index / run_index ↔ `document.xml` 里的 `[start, end)` 偏移）。`docx_read` 的 `para_index` 不仅是行号，更是回写锚点。
2. **LLM 加工（三层提示）**：系统规则（不可变约束）/ 任务模式（润色/总结/翻译）/ **保护清单（术语、数字、正则 —— 一律不改）**。没有显式边界，模型会过度改写（展开缩写、改版本号、换术语）。
3. **补丁 + 重建**：只在原始字节流上替换被改动 `<w:t>` 的内容区间，**未改动部分逐字节不变**（zip 内未触碰条目 styles/headers/images 原样拷贝）。见 §3 对"为什么不能用 DOM 重建"的说明。
4. **可选 Track Changes**：生成 `w:ins` / `w:del`，产出 Word 原生修订标记，让用户在 Word 里逐条接受/拒绝 —— 这是润色场景的关键 UX（工程量不小，见 §4.2，排在 P2）。

> **四角色映射**：GenOffice 用 Polisher / Reviewer / Differ / BlockSelector 四个角色。deepai 里 Reviewer（写修订标记）和 Differ（快照/diff）是**确定性工具能力**（`docx_edit` 的 `protect` 校验与 P2 的 `track_changes` + `docx_read` 的范围参数），不需要额外 LLM —— 比 GenOffice 的多 LLM 角色更省、更稳。Polisher = 主 agent 跑 skill；BlockSelector = `heading`/`start_para`/`end_para`。详见 §4.2 与 §7。

---

## 2. 核心设计原则：tool / skill / profile 三分

这是把 GenOffice 的 AgentSkill 模型迁到 deepai 的关键。三者职责严格分开：

| 层 | 职责 | 智能来源 | 本设计产出 |
|---|---|---|---|
| **Tool** | OOXML 的机械能力：读二进制、保格式改、套样式、转换。确定性。 | 无 LLM | `docx_read` / `docx_edit` / `docx_format` / `docx_write` / `docx_convert` |
| **Skill** | 把工具串成"润色 / 总结 / 排版"工作流。 | LLM（复用 deepai 多 provider） | `.deepai/skills/docx-{polish,summarize,format}/SKILL.md`（扁平一层，见 §7） |
| **Profile** | 受限子代理：docx 工具白名单 + 文档专用系统提示。 | —— | `AgentTypeDocEditor = "document-editor"` |

**为什么润色 / 总结不是 tool？** 因为它们的核心是 LLM 文本能力，tool 只是给 LLM 提供"读 docx"和"保格式写回 docx"的手脚。把它们做成 skill，就能复用 deepai 已有的 `skill` 工具加载机制 —— 即 GenOffice 图里的"直接复用 AgentLoop"那条绿线。

> **注意 skill 层当前的真实能力边界**：`skill` 工具目前是**纯提示注入**——handler 只把渲染后的正文放进 `Data["system_prompt"]`（`pkg/skill/tool.go:71-79`），主 agent 收到后仅调 `AppendSystemPrompt`（`pkg/agent/react.go:851-862`）。frontmatter 的 `allowed-tools` / `agent` / `model` / `effort` 会被 `buildConfig` 填进 `AgentConfig`（`pkg/skill/executor.go:110-124`），但那条 config 只有 fork 路径消费，而 fork 路径**没有接线**（`SubagentRunner` 接口 `pkg/skill/subagent.go:16` 无生产实现；`chat.go:213` 的 `SkillToolWithRegistry` → `NewExecutor` 未装 runner，写 `context: fork` 会直接报 "no subagent runner configured"）。所以 Skill 与 Profile 的连接方式必须显式设计，见 §7.1。

---

## 3. docx 引擎：能力阶梯 + 优雅降级

deepai 处理 .docx 没有"唯一正确库"。纯 Go 的 OOXML 库成熟度参差，pandoc/LibreOffice 又是外部依赖。借鉴 deepai 自身的降级哲学（Landlock → bwrap → direct；`imageproc.ClipboardSupport()` 运行时探测能力），引擎做成**三级阶梯**：

| 级别 | 实现 | 能力 | 依赖 |
|---|---|---|---|
| **Tier 1（必选）** | 纯 Go 零依赖：`archive/zip` 取出 `word/document.xml` 原始字节，`encoding/xml` 只作**扫描器**（记录 token 字节偏移），改动以 byte-offset splice 写回 | 文本抽取、**段落/run ↔ 字节区间映射**、**保 run 格式的窄补丁替换**（未改部分逐字节原样）、基础样式应用、写回 | 零外部依赖 |
| **Tier 2（可选）** | `pandoc` 子进程 | 高保真 md ↔ docx 互转、复杂结构 round-trip | 启动期探测 `pandoc` |
| **Tier 3（可选）** | `soffice`（LibreOffice headless）子进程 | docx → pdf、整篇"格式归一化"、TOC 重建 | 启动期探测 `soffice` |

探测结果决定工具**如实**上报能力（缺 pandoc 时 `docx_convert` 降级为"仅纯 Go 文本转 md"，并在描述里说明），不假装具备。这与 `view_image` 的能力探测一脉相承。

### 3.1 为什么 Tier 1 不能用现成的纯 Go docx 库

判据是**库怎么持有文档**，不是"库"这个词本身。分两类：

- **就地操作 XML 树**（典型：Python 的 `python-docx`，建在 lxml 上）。它不认识的元素照样留在树里，写回时原样输出。**2026-08-11 实测**：把内容控件 `<w:sdt>`、域 `<w:fldSimple>` 注入一份 .docx，用 python-docx 打开再保存，`document.xml` **逐字节相同**，20 个 zip 条目内容零改动，`w:ins`/`w:delText`/`xml:space` 全部保留。所以"用了库就一定丢东西"是**错的**，本节早期版本这么写过，特此更正。
- **反序列化成结构体再重新序列化**（`unmarshal → struct → marshal`）。这类库只能写回它建模过的东西，未知元素/属性在往返中消失，命名空间前缀与空白也会被规范化。`fumiama/go-docx` 属于这一类（**采用前应实测确认**，不要照搬本文断言）。

Go 生态里成熟的纯 Go OOXML 库多为后一类，而 Tier 1 的要求是 §1.1 第 3 条"未改动部分逐字节不变"。所以：

- **Tier 1 的读写必须在原始字节流上做定点替换**：用 `xml.Decoder` + `InputOffset()` 扫一遍 `document.xml`，为每个 `<w:p>` / `<w:t>` 记录 `[start, end)` 区间，改动时只 splice 对应区间，其余字节（含整个 zip 里未触碰的 `styles.xml` / `headers` / `media`）原样拷贝。
- **结构体型的第三方库最多只用于 `docx_write` 新建文档**（§4.4）——那里没有"原文件"要保，重建是安全的。
- **就地树操作型的库不受此限**，但引入前仍要实测往返保真，且要权衡它带来的依赖与语言边界（走 Python 意味着子进程、非沙箱执行、以及绕开本设计的备份/保护清单/审计记录机制，见 §4.3 的注）。

#### 3.1.1 splice 的三个必踩坑（实现规范）

这三条不写进规范，实现者一定会踩，且症状都是"Word 提示文档已损坏"或"改错位置"：

1. **`InputOffset()` 的语义是"刚返回的 token 的结束位置"**，不是开始位置。要拿一个元素内容区间的 `start`，得在 `Token()` 返回 `StartElement` 之后立刻取 `InputOffset()`；`end` 则是下一次 `Token()` 返回对应 `EndElement` **之前**记录的值（即上一 token 结束处）。实现时维护 `prevOffset` 滚动变量。

2. **写回必须做 XML 转义**。新文本里的 `&`、`<`、`>` 直接 splice 进 `<w:t>` 会产出非法 XML。写回前一律过 `xml.EscapeText`（或等价实现）。**这是 §10 验收第 4 条最常见的失败原因。**

3. **`find` 不能在解码文本的偏移上直接换算回字节偏移**。`xml.Decoder` 给出的 `CharData` 是**解码后**的文本（`&amp;`→`&`、字符引用、CDATA 已合并），它的字符位置与原始字节位置**不是线性对应**的。

   因此 `find` 的实现规范是：在解码文本上定位并完成替换，然后**整体重写目标 `<w:t>` 的完整内容区间** —— 即"取旧解码文本 → 在其上做子串替换 → 对整个新文本做 XML 转义 → splice 掉整个内容区间"。这仍然满足"补丁边界落在单个 `<w:t>` 内"（§4.2 的保格式承诺不受影响），而且只需要一对 `[start, end)` 偏移，不需要任何字符↔字节映射表。**不要**尝试只 splice 子串对应的那几个字节。

### 3.2 子进程与沙箱（现状如实说明）

**目前 `bash` 工具并不在沙箱内运行**：它调用的是 `sandbox.ExecDirect`（`pkg/tools/builtin/bash.go:32`），该函数注释明写 "runs a command without sandbox restrictions (fallback)"。带 Landlock/bwrap 后端探测的 `Sandbox.Exec`（`pkg/sandbox/sandbox.go:164`）当前**没有任何调用方**。

因此本设计对 pandoc / soffice 的立场是：

- P3 落地时，子进程**默认与 `bash` 同级别**（`ExecDirect`，非沙箱），并在文档与工具描述中如实说明，不宣称沙箱保护。
- 若后续要收紧，路径是 `tools.SandboxFromContext(ctx)`（`pkg/tools/registry.go:194`）取到会话 sandbox 再 `.Exec()`；但要先解决 sandbox 自带的 `resolvePath` 目录约束与"pandoc 需要读写用户真实路径"的冲突——这是独立于 docx 工具的一项改造，不应作为本设计的隐含前提。

---

## 4. 工具定义

放在 `pkg/tools/builtin/docx.go`，组 `Groups: []string{"builtin", "document"}`。所有工具复用现有的 `resolveReadablePath` / `resolveWritablePath` / 虚拟路径（`/mnt/user-data`）机制。

### 4.1 `docx_read` —— 结构化读取

借鉴 `read_file` 的 T2a 大纲策略：大文档默认返回**结构大纲**而非全文，agent 再按段落范围钻取。

注意**不能复用** `ReadFileOutlineThreshold`：那是 500 **行**的阈值，且 `buildFileOutline` 只对 `extToLang()` 有值的代码文件生效（`pkg/tools/builtin/file.go:97-98`）。docx 需要自己的按**段落数**阈值常量（`DocxOutlineParaThreshold`，初值取 200）和自己的大纲构造函数。

```go
func DocxReadTool() models.Tool {
    return models.Tool{
        Name:         "docx_read",
        Groups:       []string{"builtin", "document"},
        ParallelSafe: true,
        Description: "Read a .docx as structured content. By default returns an outline " +
            "(heading tree + per-section paragraph count + word count). Pass heading or " +
            "start_para/end_para for a specific section/range; full=true for the whole body as markdown.",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "path":       map[string]any{"type": "string", "description": ".docx path"},
                "heading":    map[string]any{"type": "string", "description": "Restrict to a named heading section"},
                "start_para": map[string]any{"type": "number", "description": "1-based inclusive paragraph index"},
                "end_para":   map[string]any{"type": "number", "description": "1-based inclusive paragraph index"},
                "full":       map[string]any{"type": "boolean", "description": "Return entire body as markdown (rejected when the body exceeds the chunk budget — read section by section instead)"},
                "runs":       map[string]any{"type": "boolean", "description": "Include per-run breakdown (run_index + text) for format-preserving edits"},
                "max_chars":  map[string]any{"type": "number", "description": "Hard cap on returned body text; the range is cut at a paragraph boundary and next_start_para is returned for the next call"},
            },
            "required": []any{"path"},
        },
        Handler: DocxReadHandler,
    }
}
```

> `map[string]any` 的 value 类型是 `any`，复合字面量**不能省略类型名**——每个属性都要写全 `map[string]any{...}`（现有写法见 `pkg/tools/builtin/file.go:249`）。

**`para_index` / `run_index` 的枚举域（P1 必须定死，否则 skill 流程有盲区）**：

- **正文段落**：`w:body` 下的 `w:p`，计入。
- **表格内段落**：`w:tbl/w:tr/w:tc` 里的 `w:p` **计入同一条线性 `para_index`**（§10 的验收文档就含表格；若不计入，表格文字会成为润色盲区）。输出里标注所属单元格坐标，便于 agent 理解上下文。
- **run 扫描必须递归**：`w:r` 不总是 `w:p` 的直接子元素 —— `w:hyperlink`、`w:smartTag`、`w:ins` 等容器会把它包一层。只扫一层会漏掉超链接里的全部文字。
- **已有修订标记**：`w:del` 内的 `w:delText` 是**已被删除**的文本，**排除**在抽取结果外（否则用户在 Word 里看不到的文字会混进来，且 para/run 索引与所见不符）；`w:ins` 内的 `w:r` 是已插入文本，**纳入**。
- **P1 明确不支持**：页眉页脚（`word/header*.xml` / `footer*.xml`）、脚注尾注（`footnotes.xml` / `endnotes.xml`）、批注 —— 它们是独立的 zip 条目，P1 只处理 `word/document.xml`。`docx_read` 检测到文档含这些 part 时在输出里**显式声明"未包含"**，不要让 agent 以为读到了全文。

> 更保守的 P1 选项：检测到文档**已含修订标记**（存在 `w:ins`/`w:del`）时，`docx_edit` 直接拒绝并提示"请先接受或拒绝现有修订"。这样可以完全回避"在已有修订上叠加改动"的语义问题（§4.2 把它列为 P2 议题），代价是少数场景不可用。建议 P1 就这么做。

输出形态：

- **outline（默认，段落数 > 阈值时）**：标题树 + 每节段落数 / 字数。
- **range / heading**：所选段落的 markdown，每段带 `para_index`（供 `docx_edit` 引用）；`runs=true` 时额外给出每段的 run 列表与 `run_index`。
- **full**：整篇 markdown。**仅在正文小于分块预算时可用**——超出时直接报错并提示改用 outline + range，见 §5。

每次返回都附带游标字段：`next_start_para`（若本次因 `max_chars` 截断则给出下一段索引，否则为 null）与 `total_paras`。分块由**工具侧确定性地切**，不要让 LLM 自己估算长度。

### 4.2 `docx_edit` —— 保格式外科手术式编辑（Ground Truth + 窄补丁）

对应 `edit_file`，但结构感知：定位到段落 / run / 段内子串，**保留原 run 的样式**只换文本，支持批量。遵循 §1.1 的窄补丁模型 —— 只 splice 命中的字节区间，其余逐字节保留。

```go
InputSchema: {
  "path":  string,
  "edits": [{
    "para":    number,        // 1-based，来自 docx_read 的 para_index（= <w:p> 锚点）
    "run":     number?,       // 1-based run 索引；给出则只改这一个 <w:t>，段内其他 run 不动
    "find":    string?,       // 段内子串精确匹配；命中区间即补丁边界（与 run 二选一）
    "text":    string,        // 新文本
    "op":      "replace"|"insert_before"|"insert_after"|"delete",  // 默认 replace
    // 注意：早期设计此处还有 "style"（覆盖段落样式），**已推迟到 P2**，见下。
  }],
  "protect": [string]?,       // 保护清单：术语/正则（§1.1 边界层），语义见下
}
```

**编辑粒度（关键）**：一个 `<w:p>` 通常含多个 run（粗体、超链接、域），而润色多数时候只改一句里的几个词。

- 只给 `para` + `text` = 整段替换，**会把段内格式抹平成首个 run 的格式**，仅适用于纯文本段落。工具要在返回值里对多 run 段落发出显式警告。
- 给 `run` 或 `find` 是**推荐路径**：补丁边界落在单个 `<w:t>` 内部，段内其他 run 的格式完全不受影响。`find` 未命中或命中多次时报错而不是猜。
- skill 的润色流程默认走 `find`（LLM 已经知道原文），退化到整段替换只在改写幅度大到无法定位子串时。

**各 op 与 `run`/`find` 的组合语义（必须定死，§5.4 的索引分析依赖它）**：

| op | 允许配 `run`/`find` | 作用粒度 | 是否改变 `<w:p>` 个数 |
|---|---|---|---|
| `replace` | 是 | 有 `run`/`find` → run 内子串；否则整段 | 否 |
| `insert_before` / `insert_after` | **否** | **恒为段落级**：新建一个 `<w:p>` 插在目标段前/后 | **是** |
| `delete` | 是 | 有 `run`/`find` → 只删该 run / 子串；否则删整个 `<w:p>` | 仅整段删除时是 |

> **`style` 参数已推迟到 P2（2026-08-11 决定）。** 实现它需要 `<w:pStyle>` 元素的字节区间，而扫描层只记录 `Para.Style` 字符串、不记录位置 —— 补上意味着又一轮扫描层改动与审查。更重要的是职责划分：改段落样式属于 §4.3 `docx_format` 的范围，而 §7 的润色系统规则本就要求不改版式。**`docx_edit` 管文本，`docx_format` 管样式**，这条边界更干净。P1c 的工具 schema 不暴露该字段，收到时应显式报错而非静默忽略。

`insert_*` 不接受 `run`/`find`，是刻意的：如果允许"在段内某处插入文本"，它与 `replace` 就完全重叠了（`find` 命中处替换成"原文+新文本"即可），徒增歧义。需要段内插入就用 `replace`。

**protect 是校验而非跳过**：早期设计的"命中保护项则整段跳过"过粗——一段里有一个数字就整段不润色，会造成大量段落静默不改，而 LLM 只看到 "skipped"。改为：对每条 edit 比较 before/after，若某个保护项在 before 中出现而在 after 中**丢失或被改动**，则拒绝这一条并把"哪个保护项被破坏"回给 LLM 重试；保护项原样保留的改写正常放行。

**各 op 的 before/after 边界**（必须逐个定死，否则实现者只能猜）：

| op | before | after | 是否校验 protect |
|---|---|---|---|
| `replace` | 被替换区间（`run`/`find` 命中的子串，或整段）的原文本 | `text` | **是** |
| `insert_before` / `insert_after` | 空 | `text` | **是**（仅检查新插入的文本是否伪造/篡改了保护项的写法，如插入一个写错的版本号） |
| `delete` | 被删段落/run 的原文本 | 空 | **否** |

`delete` 不做保护校验，理由：删除是 skill 的显式决策（"这段整段冗余"），而保护清单要防的是"润色时把 v1.2.3 悄悄改成 v1.2.4"这类**改写**，不是删除。若对 `delete` 也做校验，任何含数字的段落都永远删不掉，等于把 op 废掉。作为补偿，`delete` 命中保护项时在返回值里给出 `warning: deleted text contained protected items [...]`，由 agent 决定是否向用户复述。

其余要点：

- 返回每条 edit 的 before/after 摘要（Differ 角色的最小形态），供 agent 向用户汇报。
- 批量提交（一次 tool call 多 edits）减少往返；非 ParallelSafe（写操作）。edits 按字节偏移**降序**应用，避免前一条改动使后一条的偏移失效。

#### `track_changes`：单列 P2，不放进 MVP

生成 Word 原生修订标记确实是确定性能力（工具同时握有旧文本和新文本，不需要第二个 LLM），但工程量比"加个 bool 开关"大得多：

- `w:ins` / `w:del` 的 `w:id`、`w:author` 是必填，`w:date` 可选但 Word 总会写、也应当写；`date` 会让输出非确定，**测试必须能注入时钟**。
- 删除的文本要从 `<w:t>` 改写成 `<w:delText>`，不能原样保留。
- 段落级插入/删除要在 `w:pPr/w:rPr` 里打对应标记，否则 Word 接受修订后段落结构错乱。
- 文档**已有**修订时的叠加语义需要单独设计。

因此 `track_changes` 参数在 P1 不提供；P1 的可回退性由"改前备份原文件 + `docx_edit` 返回 before/after 摘要"承担。

### 4.3 `docx_format` —— 排版 / 套样

这是"排版"的核心。对整篇应用一组排版规则，**不改正文文字**：

```go
InputSchema: {
  "path": string,
  "rules": {
    "template":     "corporate"|"academic"|"minimal"?,  // 预设模板
    "heading_font": string?, "body_font": string?,
    "body_size_pt": number?,  "line_spacing": number?,
    "align":        "left"|"justify"?,  "margins_mm": [number;4]?,
    "normalize":    boolean?,  // 合并连续空段、统一标点间距
    "page_numbers": boolean?,  // Tier 3 / P3，见下
    "rebuild_toc":  boolean?,  // Tier 3 only，见下
  },
}
```

分层落点要分清楚：

- **字体 / 字号 / 行距 / 对齐 / 页边距**：改 `word/styles.xml` 与 `word/document.xml` 的 `<w:sectPr>`，Tier 1 纯 Go 可做。
- **`normalize`**：合并连续空段、统一标点间距是**正文操作**，改的是 `document.xml` 而不是 `styles.xml`。Tier 1 可做（仍走 byte-splice）；有 `soffice` 时可选走 Tier 3 做更彻底的整篇归一。
- **`page_numbers`**：**Tier 3 / P3**。加页码不是"改一个属性"，而是四处联动：新增 `word/footer1.xml` 条目 + `w:sectPr/w:footerReference` + `[Content_Types].xml` 声明 + `document.xml.rels` 关系项。这既超出 §3.1 的单文件 byte-splice 模型，也要求 zipio 支持**新增条目**（读取侧无冲突——§8 已明确全部条目原样读入保留；但写入侧需要额外设计）。P2 的 `docx_format` 不提供此参数。
- **`rebuild_toc`**：**Tier 3 only**。TOC 是域（field）+ 缓存的结果文本，重建需要重新分页，纯 Go 做不到；即便有 `soffice` 也要靠宏触发字段更新。无 `soffice` 时该参数直接报错说明不可用，不静默忽略。

### 4.4 `docx_write` —— 新建

从 markdown 或结构 JSON 生成新 .docx（有 pandoc 走 Tier 2；无则 Tier 1 基础段落 + 标题）。

**已决定（2026-08-11）：第三方纯 Go docx 库只用在这里，不用于任何编辑既有文档的路径。**

界线的依据是"有没有原文件要保"：

| 路径 | 有原文件 | 用库 |
|---|---|---|
| `docx_read` / `docx_edit`（P1a/P1b，编辑既有文档） | 有 | ❌ |
| `docx_write`（本节，从 markdown/JSON **新建**） | 没有 | ✅ |
| `docx_format`（P2，改既有文档样式） | 有 | ❌ |
| `docx_convert`（P3） | 有，但那是 pandoc/soffice 的活 | —— |

编辑路径不能用库的理由见 §3.1，且已被 P1a 的验收结果实证：byte-splice 在真实 .docx 上做到了"改一个词只动 4 个字节、20 个 zip 条目里 19 个逐字节不变"。任何 `unmarshal → struct → marshal` 的库都会丢弃它不认识的元素、重排命名空间前缀、规范化空白 —— 用户文档里的自定义 XML、嵌入对象、审阅历史会在润色后**静默消失**，而文件照样能打开、看着正常。这是最难被发现的一类损坏。

`docx_write` 没有这个约束：目标就是造一份新文件，DOM 重建即是正解。是否真的引入，取决于 Tier 1 自建的 markdown→docx 够不够用，在 P2 落地时决定；**不进 P1 依赖**（P1 的 `go.mod` 必须保持零新增）。

### 4.5 `docx_convert` —— 格式转换

`docx↔md`（pandoc 优先）、`docx→pdf`（soffice）。无外部依赖时降级并明示。

### 汇总注册

**`DocxTools()` 随阶段增长，不能一次性列全**：`mustRegisterTool` 在注册失败时 `panic`（`pkg/commands/chat.go:400-404`），而 `Registry.Register` 先调 `models.Tool.Validate()`，`Handler == nil` 即返回 error。所以在 P1 就把 `DocxFormatTool()` 等未实现的工具写进返回列表，程序会**在启动时直接 panic**。

P1 只返回已实现的两个，后续阶段各自 append 自己那行：

```go
// P1
func DocxTools() []models.Tool {
    return []models.Tool{DocxReadTool(), DocxEditTool()}
}

// P2 追加 DocxFormatTool(), DocxWriteTool()
// P3 追加 DocxConvertTool()
```

在 `pkg/commands/chat.go` 注册工具的位置（`mustRegisterTool` 那一段，`chat.go:334-353`，紧跟 `builtin.FileTools()` / `builtin.WebTools()` 的循环）追加：

```go
for _, tool := range builtin.DocxTools() {
    mustRegisterTool(registry, tool)
}
```

同时把 `DocxTools()` 加进 `pkg/tools/builtin/descriptions_test.go` 的 `allBuiltinTools()`，并让工具描述满足该测试的约束（例如不得出现 "via bash" 之类与系统提示重复的路由文案）。

---

## 5. 大文档：分章节流式处理

一份几十万字的 .docx 既装不进上下文，也装不进模型单次输出。这不是"优化项"，而是 P1 就必须成立的前提——否则大文档上的润色会**静默丢内容**而不是报错。

### 5.1 三条来自本仓库的硬约束

不是理论限制，是 deepai 现有机制会直接施加的。注意三者的**默认开关状态不同**，设计必须按"默认路径"来定，不能假设可选项已打开：

1. **24KB 自动 offload —— 默认开启**。任何工具结果超过 `offloadThresholdBytes = 24 * 1024`（`pkg/agent/compact.go:30`）就会被写入文件，上下文里只留下"文件路径 + 前 50 行 + 后 50 行"（`offloadIfNeeded` / `buildOffloadedContent`，`pkg/agent/toolexec.go:60-112`）。这条**无需任何配置即生效**：`cfg.OffloadDir` 为空时会兜底到 `~/.deepai/offload`（`pkg/agent/react.go:194-197`），所以 `offloadDir == ""` 那条短路分支在正常会话里走不到。

   两点精确化，免得实现者按错误的模型排查问题：
   - "前 50 行 + 后 50 行"只在结果**超过 100 行**时才裁剪；≤100 行时全文照留（`totalLines <= headLimit+tailLimit` 分支，`toolexec.go:98-103`）。所以一个**单行长 JSON** 即使 200KB 也不会被裁——但它会撞上下面第 (3) 层的 50KB 截断。
   - 替换文本**带显式标记**（`[offloaded: full output (N bytes, N lines) saved to ...]` 与 `... (N lines omitted) ...`），并非不可察觉。真实风险是**模型看到标记也照常往下干**，而不去读 offload 文件 —— 这在长任务里极其常见。

   后果是：`docx_read(full)` 在大文档上**不会报错**，中间几百段被替换成一行省略标记，模型多半会照常继续润色它"看得见"的那部分。这是本设计里最危险的静默失败面。

   → **`docx_read` 必须在构造上保证单次返回远低于 24KB**（建议正文预算 8KB，给 JSON 结构、run 明细、元数据留足余量）。`full=true` 在超预算时**直接报错**，不许降级返回。

   附带记住这三层护栏的**触发顺序**，调试"内容去哪了"时需要：**(1) offload（24KB，`toolexec.go:320`）→ (2) 存入消息时 50KB 硬截断（`maxToolContentBytes = 50_000`，`toolMessageContent`，`toolexec.go:44-53`）→ (3) 发请求时的 aging 视图（`react.go:498`）**。第 (2) 层是 offload 失效（取不到 home 目录）时的兜底，同样带 `[truncated: N bytes total]` 标记。这与 `aging.go:19-22` 的 Layering note 一致。

2. **上下文压缩（compaction）—— 默认开启**。只要 `contextWindow > 0`，估算占用超过 `defaultCompactionThreshold = 0.75`（`compact.go:14`）就触发 `compactMessages(runMessages, keepTail)`：只完整保留 system 消息、第一条用户消息和**最后 `defaultCompactionKeepTail = 6` 条消息**，更早的工具结果一律截到 `compactToolResultKeep = 300` 字节（`compact.go:15-16`、`compact.go:199-205`、`compact.go:344-345`）。

   → 这条给出了工作流的硬性节奏要求：**一次 `docx_read` 的结果，只能指望它在随后的 6 条消息内可用**。"先把全文读进来，再花二十轮慢慢改"必然丢原文。所以 §5.3 的"读→改→写紧邻"不是风格偏好，是被这个数字逼出来的。

3. **老化压缩（aging）—— 默认关闭，属于可选增强**。`buildPromptView` 按"年龄"压缩历史工具结果：不在 `defaultToolBudgetsByTool`（`pkg/agent/aging.go:23-31`）里的工具走 `default` 行 —— age 1 保 4096 字节、age 2 保 1024、age ≥3 只剩 300 字节。但 `cfg.Aging` **仅当 `DEEPAI_TOKEN_AGING` 为真值时才被构造**（`applyTokenEfficiencyDefaults`，`pkg/agent/config_env.go:52-68`，注释明写 "A no-op unless the env vars are truthy, so default behavior is unchanged"；也可由 config.yaml 的 `token_aging: true` 经 `setup.go:59-63` 桥接），且还要上下文压力超过 `defaultMinContextPressure = 0.4` 才生效。

   → 因此这条**不能作为设计前提**，但**开启时会显著加重**约束 2 的效果。应对动作：把 `docx_read` 加进 `defaultToolBudgetsByTool`，给 `read_file` 同级或更高的预算（`{1: 8192, 2: 2048, 3: 300}`），这样用户打开 aging 时分块流程不会退化。

> 三条叠加后的设计底线只有一句：**永远不要让任何一次 `docx_read` 的结果承担"跨多轮存活"的职责**。分块循环（§5.3）用完即弃，正是唯一同时满足这三条的形态。

此外还有一条模型侧约束：**润色的输出长度约等于输入长度**（1:1 改写），所以分块大小受**单次输出上限**约束，比受输入上限约束更紧。总结类任务是 N:1 压缩，不受此限。分块预算按最紧的场景（润色）定。

### 5.2 分块单位：章节优先，字数硬切兜底

```
docx_read(path)                      → outline：标题树 + 每节段落数/字数
  └─ 对每个标题节：
       节字数 ≤ 预算  → 整节为一块
       节字数 > 预算  → 按 max_chars 硬切成多块，切点必须落在段落边界
```

- 优先按标题切，因为润色/总结都需要语义完整的上下文，跨章节的硬切会让语气判断失据。
- 硬切点**永远落在段落边界**，绝不切开一个 `<w:p>`——`para_index` 是回写锚点，切开就没法引用了。
- 表格：一个 `w:tbl` 内的所有段落尽量不拆到两块（§4.1 已把表格段落纳入线性 `para_index`），拆开会让 LLM 看到半张表。

### 5.3 处理循环：读→改→写紧邻

```
outline = docx_read(path)                    # 一次，小
for chunk in chunks(outline):                # 见 §5.4 的顺序要求
    text  = docx_read(path, start_para=..., end_para=..., runs=true)
    edits = LLM 改写(text, 系统规则, 任务模式, 保护清单, 决策清单)
    docx_edit(path, edits)                   # 立即写回
```

三条纪律：

- **每块处理完立即写回**，不要攒到最后一次性提交。攒批会同时撞上输出上限和 offload 线。
- **读和写之间不插入其他工具调用**。`read → edit` 相邻只占 2 条消息，稳稳落在 compaction 的 `keepTail = 6` 保护窗口内（§5.1 约束 2）；每多插一次工具调用，就多消耗一格这个窗口。
- **块间只传"决策清单"（§5.5），不传原文**。注意这**减缓**但不**消除**上下文增长，原因见下。

> **不要以为"用完即弃"能让上下文保持常数 —— 单个 agent run 内做不到。** 每次 `docx_read` 的结果都会作为 RoleTool 消息**无条件追加进 `runMessages`**（`appendToolResultMessage`，`pkg/agent/view_image.go:90-103`），LLM 没有任何删除历史的手段。所以单 run 内的上下文是**按块线性增长**的（约 8KB/块），只有等占用摸到 75% 触发 compaction 才被截到 300 字节/条（§5.1 约束 2）。
>
> 换算一下：默认 `ContextWindow = 192000`（`pkg/commands/chat.go:138-139`），8KB/块 ≈ 2.4K token，压缩阈值前大约能跑 **50-60 块**——但 `MaxTurns: 30` 会先一步耗尽（§5.8），所以单 run 的实际瓶颈是轮数而非上下文。
>
> **真正的常数上下文只能靠 §5.8 的分批 subagent 委派实现**：每个 subagent 有独立的 `runMessages`，处理完若干块即销毁，只把决策清单回传给主 agent。这是本设计中大文档能力的**架构性依赖**，不是可选优化。

### 5.4 关键约束：写回会让索引失效

每次 `docx_edit` 成功写回后，之前 `docx_read` 得到的**字节偏移全部失效**（文件已重写）。`para_index` 则分两种情况：

- 纯 `replace`（不增删段落）：段落总数不变，**后续块的 `para_index` 仍然有效**。
- 一旦出现 `insert_before` / `insert_after` / `delete`：该位置之后的所有 `para_index` **整体平移**，后续块的索引全错。

三种可行策略，按场景选：

| 策略 | 适用 | 代价 |
|---|---|---|
| **正序 + 禁止增删**（推荐给润色） | 语气/术语判断需要前文顺序 | 放弃 `insert`/`delete` |
| **倒序处理**（末尾块先改） | 必须增删段落时 | 失去正序的语义连贯性 |
| **每次增删后重读 outline** | 两者都要 | 多一次 `docx_read` 往返 |

**润色默认走第一条**：正序处理（保住语气与术语的连贯判断），同时**禁止 `insert_*` / `delete`** —— 润色本就不该增删段落，§7 的系统规则里已有"不增删段"这一条。两者叠加使 `para_index` 全程稳定，既不用倒序也不用重读。真正需要增删的排版类操作走 `docx_format`，不与润色混在同一遍里。

倒序之所以有效：改动只影响"已处理完"的那部分索引，尚未处理的前面部分恒定不变。若哪天润色确实需要增删段落，切到倒序即可，无需引入索引重建。

**只有段落级增删才触发平移**（§4.2 已定死 `insert_*` 恒为段落级、`delete` 分段落级与 run 级两种）：run 级的 `delete` 不改变 `<w:p>` 个数，`para_index` 不受影响。

**注意 `para_index` 稳定 ≠ `run_index` 稳定。** `replace` 不改段落个数与顺序，所以序数型的 `para_index` 全程稳定（字节偏移的失效由工具每次编辑内部重扫吸收，不暴露给 LLM）。但 §4.2 说过整段替换会把段内格式**抹平成首个 run 的格式** —— 即 run 结构坍缩，**该段落的所有 `run_index` 立即失效**。若 LLM 拿上一次 `docx_read` 的 `run_index` 去对一个已被整段替换过的段落做二次编辑，会改到错误的位置。规范：**对同一段落的后续 run 级编辑，必须先用 `runs=true` 重读该段**。

### 5.5 跨块一致性：传递"决策清单"而非原文

分块处理最典型的质量问题是**风格漂移**：第 1 章把"用户"改成"用户方"，第 8 章又改回"用户"。解决办法是在块之间传一份体积固定的状态：

- **保护清单**（全局，不变）：数字、术语、专有名词、正则。
- **决策清单**（增量，上限几百字）：已确定的术语取舍、语气基调、称谓习惯。每块处理后由 LLM 追加新决策，超过上限时合并压缩。
- 这两份都随每块的提示一起下发，体积恒定，不随文档增长。

### 5.6 总结走 map-reduce

`docx-summarize` 天然是分块任务：

1. **map**：每块 → 一段限长摘要（每块摘要长度上限写死，例如 200 字），块间互不依赖。这是本设计里唯一**可以并行**的环节（只读，无写回冲突）。

   但"并行"必须落在**subagent 层，而不是工具层**。`docx_read` 确实声明了 `ParallelSafe: true`，agent 也确有并行批次（`allParallelSafe(toolCalls) && len(toolCalls) > 1`，`pkg/agent/react.go:710`），但那只在**模型恰好在一条 assistant 消息里发出多个 tool call** 时才发生，没有任何保证；更要命的是 map 阶段的主体是"LLM 为每块生成摘要"，同一个 agent 内这必然串行产出，且所有块的原文与摘要**全部累积进同一份 `runMessages`**（§5.3）。
   
   正确形态是**并行只读 subagent**：`task` 工具本身 `ParallelSafe: true`，子代理池 `MaxConcurrent: 4`（`pkg/commands/chat.go:345`），每个 subagent 领若干块、独立上下文、只返回摘要。这同时解决了并行度和上下文隔离两个问题。
2. **reduce**：把所有块摘要拼起来 → 汇总摘要。若块摘要总量仍超预算，则**递归再来一层**。
3. 每层输出都限长，保证下一层的输入可控。

### 5.7 中断可恢复

因为 `docx_edit` 是原地增量写回，中断后文档处于"部分章节已处理"的一致状态（不是半个损坏文件——§8 的原子写回保证单次写入的完整性）。

- **游标不能由 `docx_edit` 推断出来**：工具只看得到提交上来的 edits，而一个块里"审阅过但无需修改"的段落从不出现在任何 edit 里。极端情况是某块零修改，游标原地不动，续跑重复处理整块。
- 正确做法二选一：(a) 给 `docx_edit` 加显式入参 `reviewed_through_para`，由 skill 每块传入本块的 `end_para`，工具原样回显；(b) 干脆不放进工具，把游标定义为"最近一次完成的 `docx_read` 块的 `end_para`"，由 skill 自己在对话里维护。**推荐 (a)** —— 让游标和写回在同一次调用里落地，避免"改了但没记上"的窗口。
- 续跑时从该游标之后继续，不重复处理。这依赖**会话本身可恢复**（`-r` / `--continue` 恢复历史），游标是几十字节的状态、不受 §5.1 的压缩影响；但若用户开新会话，游标就丢了，此时只能重跑或由用户手工指定起始段落。
- **备份只在第一次编辑前做一次**（§8），后续块的写回不再覆盖备份，否则跑到一半的中间态会把原始版本冲掉，丧失回退能力。

### 5.8 对轮数与委派的影响

每块约需 2-3 轮（read → edit，可能加一轮重试），`document-editor` 的 `MaxTurns: 30`（§6）大约只够 **10-12 块**。对超出这个规模的文档：

- **不要**简单调高 `MaxTurns`——上下文会被历史工具结果撑爆（即便有 aging）。
- 正确做法是**按章节区间分批委派多个 subagent**，每个 subagent 处理若干块后返回决策清单，主 agent 把它传给下一个。
- 这些 subagent 必须**串行**：它们写同一个文件，并行会让字节偏移与 `para_index` 相互失效。唯一例外是 §5.6 的 map 阶段 —— 那里是**并行只读 subagent**，无写回冲突。
- 这条委派架构不只是为了轮数，更是**大文档保持常数上下文的唯一途径**（§5.3）：每个 subagent 有独立的 `runMessages`，处理完即销毁，主 agent 只累积体积固定的决策清单。

---

## 6. 子代理 profile

在 `pkg/agent/types_config.go` 增 `document-editor`，给一个文档专用的系统提示（润色时保留作者语气、总结时不杜撰、破坏性排版先确认）：

```go
AgentTypeDocEditor AgentType = "document-editor"

AgentTypeDocEditor: {
    Type:        AgentTypeDocEditor,
    Name:        "Document Editor",
    Description: "Profile for .docx polishing, typesetting, and summarization.",
    SystemPrompt: docEditorSystemPrompt, // 保留语气 / 不杜撰 / 破坏性操作先问
    DefaultTools: []string{
        "docx_read", "docx_edit", "docx_format", "docx_write", "docx_convert",
        "read_file", "write_file", "ask_clarification",
    },
    MaxTurns: 30, Temperature: 0.2,
},
```

**`MaxTurns` 不能写 0**：在 subagent 路径下，安全底线只看**数值**——`maxTurns <= 0` 一律兜成 **15**（`pkg/agent/subagent.go:133-140`）。`AgentTypeConfig.maxTurnsSet` 这个"显式零值"标志只被 YAML 的 `mergeConfig` 消费（`yaml_loader.go:146`），subagent 那条路径根本不看它，所以连 YAML 里显式写 `max_turns: 0` 也照样兜成 15。分块润色每块要 2-3 轮，15 轮只够四五块，所以显式给 30（≈10-12 块）。再大的文档不是靠调高这个数解决，而是按 §5.8 分批委派多个串行 subagent。

调用方：主 agent 通过 `task` 工具委派 —— 参数名是 **`agent_type`**（`subagent_type` 已标 Deprecated，见 `pkg/tools/subagent.go:55-56`），值 `document-editor`。subagent 自动只看到 docx 工具集（`selectSubagentTools` 按 group/name 过滤已经支持 `"document"` 这个 selector，见 `pkg/agent/subagent.go:454`）。

`DefaultTools` 可以一次性列全 5 个 docx 工具，不必随阶段增长 —— 与 `DocxTools()` 不同，`selectSubagentTools` 对**未注册的 selector 名字只是不匹配**，仅当一个都没匹配上时才报错（`subagent.go:454` 之后的 fail-hard 分支）。P1 时 `docx_format` 等三个名字静默无效，`docx_read`/`docx_edit` 正常命中。

落地时 `AgentTypeDocEditor` **只需加进 profile map**：e316284 已把 agent-type profile 定为唯一真源，`task` 工具描述里的可选类型清单是运行时从 agent catalog 动态拼出来的（`formatAgentOptions`，`pkg/tools/subagent.go:251-271`），加进 map 即自动出现在工具描述里，无需手改任何枚举。

---

## 7. 技能（Skill）

**目录必须是扁平一层**：`Registry.LoadFromDir`（`pkg/skill/registry.go:220-247`）只扫 `.deepai/skills/<entry>/SKILL.md` 这一层——`<entry>` 下没有 `SKILL.md` 就直接 `continue`，不会递归。所以 `.deepai/skills/document/polish/SKILL.md` **永远不会被发现**。正确布局是 `.deepai/skills/docx-polish/SKILL.md`。

另外 skill 名是**全局唯一 key**（`r.skills[name]`，跨 global / project / plugin 三个来源），所以用 `docx-` 前缀而不是 `polish` / `format` 这种通用名，避免撞名覆盖。

复用现有 frontmatter（`name` / `description` / `allowed-tools` / `agent` / `model` / `effort`，见 `pkg/skill/types.go:23-39`）。每个 skill 就是一段把 docx 工具串起来的工作流说明，LLM 读到 `description` 自动启用（GenOffice 图里"直接复用 AgentSkill"那条线）。

**`docx-polish/SKILL.md`**：润色工作流，严格按 §1.1 三层提示 + 窄补丁：

- 流程：`docx_read()` 取 outline → **按 §5 的分块循环**逐块处理：`docx_read(start_para/end_para, runs=true)` → 润色 → `docx_edit(edits[], protect=[...])` 立即写回 → 丢弃本块原文、只留决策清单。**优先用 `find` 定位段内子串**，保住段内其余 run 的格式（§4.2）。
- **禁止 `insert_*` / `delete`**：润色不增删段落，这样 `para_index` 全程稳定，正序分块无需重读 outline（§5.4）。
- 提示三层：**系统规则**（不增删段、不改保护项、保留作者语气与术语）/ **任务模式**（grammar | fluency | formal-tone，按入参选）/ **保护清单**（数字、专有名词、术语、正则 —— 通过 `ask_clarification` 让用户补，默认保护所有数字与全大写缩写）。
- 可回退性：P1 靠"改前备份原文件 + `docx_edit` 的 before/after 摘要"；`track_changes` 是 P2 能力（§4.2），到位后再默认开启。
- frontmatter：`allowed-tools: [docx_read, docx_edit, ask_clarification]`，`agent: document-editor`。

**四角色落点**（§1.1）：Polisher = 本 skill 的 LLM；Reviewer = `docx_edit` 的 `protect` 校验（P2 起 + `track_changes`）；Differ = `docx_edit` 返回的 before/after 摘要；BlockSelector = `docx_read` 的 `heading`/范围参数。deepai 用工具确定性能力吃掉 Reviewer/Differ，比 GenOffice 的多 LLM 角色省 token、更稳定。

**`docx-summarize/SKILL.md`**：按 §5.6 的 map-reduce —— 分块摘要（每块限长，可并行）→ 递归汇总 → 输出 markdown（P1，`write_file`）或 `docx_write` 生成摘要稿（P2 起）。frontmatter：P1 用 `allowed-tools: [docx_read, write_file]`（P2 起加 `docx_write`），`agent: document-editor`。

**`docx-format/SKILL.md`**（P2）：选模板 → `docx_format(rules)`，破坏性归一前 `ask_clarification`。frontmatter：`allowed-tools: [docx_read, docx_format, ask_clarification]`，`agent: document-editor`。

### 7.1 Skill 如何真正连上 `document-editor` profile

**不能靠 frontmatter 的 `agent:` 字段**。如 §2 的注所述，`agent:` 和 `allowed-tools:` 在当前唯一接线的路径上是惰性的：skill 只会把正文追加进**主 agent** 的系统提示，工具集仍是主 agent 的全量集合。照字面理解写 `agent: document-editor` 并期待"自动跑在受限 profile 下"，会得到一个静默失效的白名单——比没有白名单更危险。

采用 **A 方案（今天就通）**：skill 正文显式指示主 agent 委派。

- SKILL.md 的正文写成一段**给主 agent 的操作指令**：「用 `task` 工具委派 `agent_type: document-editor`，把文档路径与润色要求传过去；不要自己直接调 `docx_edit`」。真正的受限执行发生在 subagent 里，profile 的 `DefaultTools` 白名单在那条路径上是**真实生效**的（`selectSubagentTools`，§6）。
- frontmatter 仍写 `agent: document-editor` 与 `allowed-tools:`，但**只作为意图声明**（面向未来接线 + 给读者看），不能当作运行时保障。文档和注释里要写明这一点，避免后人误判。

**B 方案（可选前置工程）**：实现 `skill.SubagentRunner` 并在 `chat.go` 用 `NewExecutor(reg).WithSubagentRunner(...)` 装配，让 `context: fork` 真正可用，届时 `agent:` / `allowed-tools:` / `model` / `effort` 一次性全部生效。这是**独立于 docx 的通用能力改造**，收益覆盖所有 skill；如果要做，应作为显式的工程项排期，而不是被本设计隐式假定为已有。

三个 skill 都声明 `agent: document-editor`，配合 A 方案的正文指令，使实际执行落在同一个受限 profile 下（只见 docx 工具集 + 文档专用系统提示）。

---

## 8. 安全

- **`.docm` 宏文档**：edit/write 拒绝；read 仅做文本抽取并附警告，绝不执行宏。
- **加密文档**：**打开时第一件事就是查文件头**。明文 .docx 以 zip magic `PK\x03\x04` 开头；密码加密的 OOXML 根本不是 zip，而是 OLE2/CFB 容器（magic `D0 CF 11 E0 A1 B1 1A E1`，内含 `EncryptionInfo` / `EncryptedPackage` 流）。命中 CFB magic 时直接返回 `encrypted or password-protected .docx is not supported`，**不要让 `archive/zip` 或 `xml.Decoder` 的底层错误冒到用户面前**（那会是一串莫名其妙的 "not a valid zip file" / XML syntax error）。同理，zip 能打开但 `[Content_Types].xml` 缺失时，也报"不是有效的 .docx"而非透传 XML 错误。
- **路径**：复用 `resolveReadablePath`/`resolveWritablePath` + 虚拟路径，与 `view_image`、`read_file` 一致。
- **zip 层防护**（.docx 即 zip，必须在解压前就设防）：
  - **路径穿越**：拒绝任何含 `..`、绝对路径、控制字节或 `X:` 盘符前缀的条目名。
  - **不要做条目白名单**（本条是 P1a 实施后的修正）：早期设计写的是"只按白名单读取 `word/`、`_rels/` 等已知条目"，这是**错的**，会直接摧毁 §10 验收第 2 条。白名单意味着未识别的条目不被读入，写回时也就无法原样重建，而 .docx 里合法但我们不认识的条目（自定义 XML、`customXml/`、`docProps/`、嵌入对象）比想象中多。**正确做法是读入并保留全部条目**，只对条目名做安全校验、对总量做上限约束。P1a 的实现就是这样做的，与本条一致。
  - **解压后总量与条目数上限**：`maxDecompressedBytes`（200 MB）、`maxZipEntries`（2000），边解压边累计，超限即中止。
  - **原始压缩字节也必须计量**（P1a 实施中发现的真实漏洞）：`OpenRaw()` 返回的 reader 大小取自中央目录里的 `CompressedSize64`，那是**攻击者可控**且从不与真实条目交叉校验的值。多个条目名指向同一个 local header 并虚报压缩尺寸，可以在"文件 < 25 MB、解压总量 < 200 MB、条目数 < 2000"三道防线全部通过的情况下放大约 2000 倍。所以除了解压预算，还要对原始字节读取单独设预算（合法上限就是文件自身大小）。
  - **重复条目名**：`archive/zip` 允许同名条目并存，而 Word 读中央目录里的那一个、你的代码可能读到另一个（zip 歧义攻击）。白名单和体积上限都挡不住这类，需单独检查：条目名重复即拒绝整个文件。
  - **zip 炸弹**：`maxDocxBytes`（压缩后，仿 `maxViewImageBytes`，取 25 MB）**不够**——同时限制解压后总字节数（如 200 MB）和条目数（如 2000），边解压边累计，超限即中止。
  - **XML 实体**：`encoding/xml` 默认不展开外部实体，但仍要拒绝含 DOCTYPE 的 `document.xml`，并忽略 `attachedTemplate` 之类的远程模板引用。
- **子进程**：pandoc / soffice 默认与 `bash` 同级别（`ExecDirect`，**非沙箱**），如实说明，见 §3.2。
- **写入原子性**：docx 写回走"临时文件 + rename"，避免中途失败留下损坏的 .docx；覆盖前保留一份备份供回退（P1 的可回退性依赖它）。

---

## 9. 文件布局

```
pkg/docx/                     # 能力包（镜像 pkg/imageproc）
  zipio.go                    # 安全解压 / 条目名校验 / 炸弹防护（解压+原始双预算）/ 原子写回
  scan.go                     # xml.Decoder + InputOffset() → 段落/run 字节区间索引
  read.go   edit.go   format.go   write.go   convert.go
  detect.go                   # 启动期探测 pandoc/soffice
  *_test.go
pkg/tools/builtin/docx.go     # 薄工具封装（镜像 view_image.go）
pkg/tools/builtin/descriptions_test.go  # allBuiltinTools() += DocxTools()
pkg/agent/types_config.go     # + AgentTypeDocEditor
pkg/agent/aging.go            # defaultToolBudgetsByTool += docx_read（§5.1 约束 3，可选增强）
pkg/commands/chat.go          # 注册 DocxTools()
.deepai/skills/              # 必须扁平一层，见 §7
  docx-polish/SKILL.md
  docx-summarize/SKILL.md
  docx-format/SKILL.md
docs/DOCX_TOOLS_DESIGN.md     # 本文档
```

---

## 10. 分阶段落地

| 阶段 | 内容 | 可用能力 |
|---|---|---|
| **P1（MVP）** | `pkg/docx` 的 `zipio` + `scan`（字节区间索引、安全解压、原子写回）→ Tier 1 read/edit + `docx_read`/`docx_edit`（run/find 粒度 + protect 校验 + `max_chars`/游标分块）+ `document-editor` profile + `docx-polish`/`docx-summarize` skill（含 §5 分块循环） | 润色（备份 + before/after 回退）、总结（map-reduce），**大文档可用** |
| **P2** | `track_changes`（w:ins/w:del，可注入时钟）+ `docx_format` + `docx_write`（Tier 1 样式 / 新建）+ `docx-format` skill | 排版、生成新稿、Word 原生修订标记 |
| **P3** | pandoc / soffice 探测接入 + `docx_convert`（md↔docx、PDF）+ `rebuild_toc` | 高保真转换 |
| **P4** | skills 扩展（translate 等）+ 视需要引入第三方库增强 `docx_write` | 开箱即用工作流 |

P1 即可覆盖用户提出的"润色 / 排版 / 总结"三项中的**润色与总结**；排版在 P2 完整。每阶段都不阻塞已有功能（纯增量注册）。

**P1 明确接受的 tradeoff：只有词法级质量闸门，没有语义级。** `protect` 校验和 before/after 摘要能挡住"数字/术语被悄悄改掉"这类可字符串比对的越界，但挡不住"语义上过度改写"——LLM 把一句话改得语气全变、含义偏移，词法检查一律放行。语义级的把关手段是 P2 的 `track_changes`：让人在 Word 里逐条看、逐条接受/拒绝。

这是有意的范围切分（P1 先把 Ground Truth 字节模型跑通），但有个连带约束：**如果 `track_changes` 滑出 P2，P1 就处于零语义安全网状态**。届时要么优先补 `track_changes`，要么在 `docx-polish` skill 里强制"改完先把 before/after 全量呈给用户确认再写回"作为临时替代。不要让它无声地一直停在 P1。

**P1 的验收标准**（决定 Ground Truth 模型是否真的成立）：取一份含表格、图片、页眉页脚、超链接、多 run 段落的真实 .docx，只改一个段落里的一个词，写回后：

1. **条目集合相同**：新旧 zip 的条目名列表完全一致，无增删、顺序不变。
2. **除 `word/document.xml` 外，每个条目解压后的字节流与原文逐字节相同**（styles / headers / media / rels 全部原样）。
3. **`word/document.xml` 解压后，除目标 `<w:t>` 元素（含其起始标签）外逐字节相同**。放宽到"元素"而非"内容区间"是有意的：新文本若以空白开头/结尾（润色后很常见），必须给该 `<w:t>` 补上 `xml:space="preserve"`，否则 Word 会吞掉首尾空白 —— 这需要改起始标签的字节。除这一个属性的增补外，起始标签不得有其他变化。
4. Word 与 LibreOffice 打开均无"文档已损坏，是否修复"提示。

> 注意验收比的是**解压后的条目内容**，不是 .docx 文件本身的字节。改动 `document.xml` 必然改变该条目的 CRC32 与压缩后长度，进而推移其后所有 local header 的偏移、中央目录和 EOCD —— zip 外壳一定会变，这是正常的，不构成失败。

这四条不过，Tier 1 的实现方式就是错的（多半是退回了 DOM 重建，见 §3.1）。

**P1 的第二条验收标准（大文档不丢内容）**：取一份**远超上下文**的 .docx（建议 ≥ 500 页 / ≥ 20 万字），跑完整的 `docx-polish`：

1. `docx_read` 的**每一次**返回都 < 24KB，即从未触发 offload。**测量方式**：断言 metrics 里每条 `ToolResultMetric` 的 `Offloaded == false` 且 `ResultBytes < 24576`（`RecordToolResult`，`pkg/agent/toolexec.go:328-336`）。**不要**用"检查 offload 目录为空"——默认目录 `~/.deepai/offload` 是**全局共享**的（`react.go:194-197`），别的会话留下的文件会造成误判；测试里应显式把 `AgentConfig.OffloadDir` 指到临时目录。
2. **每一个段落都被实际处理过**：处理前后逐段比对，不存在"既没被改写、也没出现在任何一块处理范围内"的段落。**测量方式**：metrics 只存 `ArgsHash` 不够，需要在测试模式下把每次 `docx_read` 的 `start_para`/`end_para` 写进 sidecar 文件，最后断言这些区间的并集覆盖 `[1, total_paras]`。这一条专门抓 §5.1 那个静默失败面 —— 它不会报错，只能靠覆盖率断言抓。
3. 中途 kill 进程，文档仍能被 Word 正常打开（原子写回），且用 `-r` 恢复会话后续跑不重复处理已完成的章节（§5.7）。
4. **分批 subagent 架构下**，单个 subagent 的峰值上下文与文档总大小**无关**（只与每批块数有关）。注意这条**不适用于单 agent run** —— 那里上下文必然按块线性增长（§5.3），这是 deepai 执行模型决定的，不是实现缺陷。**测量方式**：`DEEPAI_TOKEN_METRICS` 输出的 JSONL 逐轮记录。

第 2 条是这组里最关键的：Ground Truth 模型保证"改的地方对"，这条保证"该改的地方没被跳过"。
