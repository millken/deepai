# DOCX P2c: `docx_write` 从 markdown 新建文档 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 agent 能从 markdown 生成一份新的 .docx。这是 P2 的最后一块，完成后 `docx-summarize` 也能直接产出 .docx 而不只是 markdown。

**Architecture:** 纯 Go 自建最小 OOXML 骨架，用 `archive/zip` 直接写。**不走 `zipio.Package`** —— 那条路是为"打开既有文件、原样保留未触碰条目"设计的，新建没有原文件要保，两者的约束正相反。

**Tech Stack:** Go 1.26.1 标准库。

前置：P1 全部 + P2a/P2a.5/P2b（`807587a`，已在 main）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §4.4、§10。

---

## 范围决定（2026-08-11 用户反馈后修订）

**用户的要求是"很好地工作,不只是简单的 markdown 转换"**,依据是实测中模型已能自行产出完整的设计文档(docx)。这条要求比"多几个功能"更硬:

> **一个比回退方案更弱的工具,一定会输给回退方案。**

P2a.5 刚证明过这一点 —— agent 在工具做不到时会退回 bash + python,而且它的推理链完全正确。如果 `docx_write` 只能做标题/段落/粗斜体,而模型用 python 能写出带表格和列表的设计文档,那么**第一次有人要表格就会重演同样的回退**。所以本阶段的目标不是"能生成 docx",而是"生成的 docx 好到模型不想绕过它"。

### 仍然自建,不引库

设计 §4.4 允许在此处引第三方库,并写了"若将来要支持表格、图片、编号列表等复杂结构,自建成本会陡增,届时应重新评估"。该触发条件现已满足,故重新评估如下 —— **结论仍是自建**:

1. **输出质量需要精确控制。** 目标是"像样的设计文档":表格边框、列表缩进层级、代码块的等宽与底色。这些细节决定成品观感,自建才能逐项调;用库拿到的是它给什么样就什么样。
2. **库的实际能力未知且验证成本高。** `fumiama/go-docx` 对表格/编号的支持程度需要试过才知道,若不足则前功尽弃,还多了一个依赖。
3. **我们的验证手法只在自建时有意义。** 用本包自己的 `OpenDocument`/`Scan` 读回产物、断言段落与样式 —— 这验的是"我们的读者(进而 Word)如何理解产物"。用库的话变成在验别人的输出。
4. 全项目至今零新增依赖,构建对用户无网络要求。

**若最终成品质量达不到要求,应重新评估此决定** —— 这条留在这里,不要因为"已经自建了"就不再考虑。

## Global Constraints

- **零外部 Go 依赖**：`go.mod` 不得改动。这条在本阶段是**决定的结果**，不是惯性。
- **不得修改 `zipio.go` / `scan.go` / `splice.go` / `read.go` / `edit.go` / `format*.go` / `revision.go` / `document.go`**。新建是独立能力，应当自成一路。若你认为必须改，**停下来报告**。
- 此前所有阶段的保证必须继续成立（两条保真门、`docx_format` 正文不改、关闭 `track_changes` 时逐字节不变）。
- **验红必须最小分支**。
- **不自动提交**。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`。

---

## 支持的 markdown 子集(扩展后)

| 语法 | 产出 | 阶段 |
|---|---|---|
| `# `..`###### ` | `Heading1`..`Heading6` 样式 | Task 1 |
| 空行分隔的文本块 | `Normal` 段落 | Task 1 |
| `**粗体**` / `*斜体*` | run 带 `<w:b/>` / `<w:i/>` | Task 1 |
| `` `行内代码` `` | run 带等宽字体 | Task 3 |
| `- ` / `* ` 无序列表(含嵌套) | `<w:numPr>` + `numbering.xml` 项目符号 | Task 2 |
| `1. ` 有序列表(含嵌套) | `<w:numPr>` + `numbering.xml` 编号 | Task 2 |
| 表格(GFM 管道语法,含对齐行) | `<w:tbl>` 带边框与表头加粗 | Task 2 |
| ```` ``` ```` 代码块 | 等宽段落 + 浅底色 | Task 3 |
| `[文字](url)` | 超链接(`document.xml.rels` 关系 + `<w:hyperlink>`) | Task 3 |
| `> ` 引用 | 缩进段落样式 | Task 3 |
| `---` 分隔线 | 段落下边框 | Task 3 |
| 图片 | **不支持**,如实声明 | —— |

**为什么不静默降级**:用户给了含列表的 markdown,若悄悄写成普通段落而不说,他打开文档才发现列表没了。凡未按语义渲染的,必须在 `notes` 里逐类声明。

图片需要把二进制嵌进 zip 并加关系项,且要解决图片来源(本地路径?URL?)—— 留待后续,本阶段如实声明不支持。

## 最小 OOXML 骨架（五个部件）

新建的 .docx 至少要有：

| 部件 | 作用 | 阶段 |
|---|---|---|
| `[Content_Types].xml` | 声明各部件的 MIME 类型；缺了 Word 直接报损坏 | Task 1 |
| `_rels/.rels` | 根关系，指向 `word/document.xml` | Task 1 |
| `word/document.xml` | 正文 | Task 1 |
| `word/_rels/document.xml.rels` | 文档级关系；Task 3 的超链接也挂在这里 | Task 1 |
| `word/styles.xml` | `Normal`、`Heading1`..`Heading6`，以及代码/引用样式 | Task 1 / 3 |
| `word/numbering.xml` | 项目符号与编号的定义；列表必需 | Task 2 |

**zip 条目时间戳必须钉死**（与夹具生成器同一理由）：否则同样输入两次产出的文件字节不同，测试无法断言、用户也无法比对。用固定的 `time.Time`，不要用 `time.Now`。

**样式定义要真的存在**：`Heading1` 等必须在 `styles.xml` 里定义，否则 Word 会把 `w:pStyle w:val="Heading1"` 当成未知样式、按 `Normal` 渲染 —— 标题看起来就是普通文字。这是本阶段最容易"看起来成功实则失败"的一处。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/write.go` | markdown 解析 + OOXML 骨架生成 |
| `pkg/docx/write_test.go` | |
| `pkg/tools/builtin/docx.go` | 修改：新增 `DocxWriteTool` / handler，`DocxTools()` 加入 |
| `pkg/tools/builtin/docx_test.go` | 修改 |
| `pkg/agent/types_config.go` | 修改：`document-editor` 的 `DefaultTools` 加 `docx_write` |
| `.deepai/skills/docx-summarize/SKILL.md` | 修改：可选产出 .docx |
| `docs/DOCX_TOOLS_DESIGN.md` | 修改：§4.4 记录"自建不引库"的决定与 markdown 子集边界 |

---

### Task 1: `write.go` —— markdown → .docx

**Files:** Create `pkg/docx/write.go`, `pkg/docx/write_test.go`

**Interfaces:**
```go
type WriteOptions struct {
    Markdown string
    Title    string // 可选，写进 docProps 或作为首个 Heading1；实现者选定并说明
}
type WriteResult struct {
    Paras int
    Notes []string // 未支持语法的声明
}
func WriteDocx(path string, opts WriteOptions) (WriteResult, error)
```

- [ ] **Step 1: 写失败的测试**

**核心验证手法：用我们自己的读取器验产物。** 这比断言 XML 字符串强得多 —— 它验的是"我们的扫描器（进而 Word）会怎么理解这份文件"。

```go
func writeAndReopen(t *testing.T, md string) (*Document, WriteResult, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := WriteDocx(p, WriteOptions{Markdown: md})
	if err != nil { t.Fatalf("WriteDocx: %v", err) }
	d, err := OpenDocument(p)
	if err != nil { t.Fatalf("the generated file cannot be reopened: %v", err) }
	return d, res, p
}

func TestWrite_HeadingsCarryHeadingStyles(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# Chapter\n\nbody text\n\n## Section\n\nmore text\n")
	paras := d.Paras()
	if len(paras) != 4 { t.Fatalf("got %d paragraphs, want 4", len(paras)) }
	if paras[0].Style != "Heading1" { t.Errorf("paras[0].Style = %q, want Heading1", paras[0].Style) }
	if paras[2].Style != "Heading2" { t.Errorf("paras[2].Style = %q, want Heading2", paras[2].Style) }
	if paras[1].Style != "" && paras[1].Style != "Normal" {
		t.Errorf("body paragraph has style %q", paras[1].Style)
	}
}

// The style definitions must actually exist, or Word renders headings as body
// text -- the failure mode that looks like success.
func TestWrite_HeadingStylesAreDefinedInStylesXML(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# H\n")
	s, ok := d.Part("word/styles.xml")
	if !ok { t.Fatal("styles.xml missing") }
	for _, id := range []string{"Normal", "Heading1", "Heading2", "Heading3"} {
		if !strings.Contains(string(s), `w:styleId="`+id+`"`) {
			t.Errorf("styles.xml does not define %s; Word would render it as plain text", id)
		}
	}
}

func TestWrite_BoldAndItalicBecomeRuns(t *testing.T) {
	d, _, _ := writeAndReopen(t, "plain **bold** and *italic* end\n")
	if len(d.Paras()) != 1 { t.Fatalf("got %d paragraphs, want 1", len(d.Paras())) }
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs { text.WriteString(r.Text) }
	if text.String() != "plain bold and italic end" {
		t.Errorf("visible text = %q, want the markers stripped", text.String())
	}
	if len(d.Paras()[0].Runs) < 3 {
		t.Errorf("got %d runs; bold and italic must be their own runs", len(d.Paras()[0].Runs))
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:b/>") { t.Error("no bold run property") }
	if !strings.Contains(string(doc), "<w:i/>") { t.Error("no italic run property") }
}

func TestWrite_XMLMetacharactersAreEscaped(t *testing.T) {
	d, _, _ := writeAndReopen(t, "A & B < C > D\n")
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs { text.WriteString(r.Text) }
	if text.String() != "A & B < C > D" {
		t.Errorf("round trip corrupted the text: %q", text.String())
	}
}

// Unsupported syntax must be declared, not silently flattened.
func TestWrite_UnsupportedSyntaxIsDeclared(t *testing.T) {
	_, res, _ := writeAndReopen(t, "- item one\n- item two\n\n| a | b |\n|---|---|\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "list") {
		t.Errorf("notes do not mention lists: %q", joined)
	}
	if !strings.Contains(joined, "table") {
		t.Errorf("notes do not mention tables: %q", joined)
	}
}

func TestWrite_SupportedOnlyInputProducesNoNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "# H\n\nbody **bold**\n")
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none for fully supported input", res.Notes)
	}
}

// Same input twice must produce the same bytes.
func TestWrite_IsDeterministic(t *testing.T) {
	md := "# H\n\nbody\n"
	a := filepath.Join(t.TempDir(), "a.docx")
	b := filepath.Join(t.TempDir(), "b.docx")
	if _, err := WriteDocx(a, WriteOptions{Markdown: md}); err != nil { t.Fatal(err) }
	time.Sleep(1100 * time.Millisecond) // cross a DOS timestamp bucket
	if _, err := WriteDocx(b, WriteOptions{Markdown: md}); err != nil { t.Fatal(err) }
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ab, bb) {
		t.Error("two runs produced different bytes; zip timestamps are not pinned")
	}
}

func TestWrite_EmptyMarkdownProducesAValidEmptyDocument(t *testing.T) {
	d, _, _ := writeAndReopen(t, "")
	if d.TotalParas() > 1 { t.Errorf("empty input produced %d paragraphs", d.TotalParas()) }
}

func TestWrite_RefusesToOverwriteAnExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.docx")
	if err := os.WriteFile(p, []byte("existing"), 0o644); err != nil { t.Fatal(err) }
	if _, err := WriteDocx(p, WriteOptions{Markdown: "# H\n"}); err == nil {
		t.Fatal("WriteDocx overwrote an existing file; creating must not destroy")
	}
}
```

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

验红项：标题级别映射、样式定义写入、粗体/斜体的 run 拆分、XML 转义、未支持语法的声明、时间戳钉死、拒绝覆盖既有文件。

**拒绝覆盖**是刻意的：`docx_write` 是"新建"，覆盖既有文件属于破坏性操作，应由调用方显式先删或改路径。

---

---

### Task 2: 列表与表格

**Files:** Modify `pkg/docx/write.go`, `pkg/docx/write_test.go`

这是"能不能写设计文档"的分水岭 —— 一份设计文档几乎必然有列表和表格。

**列表**需要新增 `word/numbering.xml` 部件,并在 `[Content_Types].xml` 与 `word/_rels/document.xml.rels` 里登记。段落通过 `<w:pPr><w:numPr><w:ilvl w:val="N"/><w:numId w:val="M"/></w:numPr></w:pPr>` 引用编号定义。

- 无序与有序各需一个 `w:num`(指向各自的 `w:abstractNum`)
- **嵌套靠 `w:ilvl`**(0 起),`abstractNum` 里要为每一级定义好项目符号/编号格式与缩进
- markdown 侧:按行首空格数判定层级,**每 2 或 4 空格算一级要定死并写进注释**,否则混排缩进的输入会产出错乱层级

**表格**用 `<w:tbl>`:`<w:tblPr>`(含边框 `<w:tblBorders>`)、`<w:tblGrid>`(每列一个 `<w:gridCol>`)、逐行 `<w:tr>` 逐格 `<w:tc>`。要点:

- **每个 `<w:tc>` 内必须至少有一个 `<w:p>`** —— 空单元格写空段落。缺了 Word 报文档损坏(P1b 的 I3 已经踩过同类问题)
- 表头行(GFM 分隔行之前那行)加粗
- 支持 GFM 对齐行(`:---`/`:---:`/`---:`)→ 单元格段落的 `<w:jc>`
- 列数以表头行为准;数据行多出的单元格丢弃、少的补空,并在 `notes` 里声明

- [ ] **Step 1: 写失败的测试**

沿用"用自己的读取器验产物"的手法。要点测试:

```go
func TestWrite_UnorderedListProducesNumPr(t *testing.T)          // 段落带 numPr,visible text 不含 "- "
func TestWrite_NestedListLevelsUseIlvl(t *testing.T)             // 两级嵌套 -> ilvl 0 与 1
func TestWrite_OrderedAndUnorderedUseDifferentNumIds(t *testing.T) // 否则有序列表会显示成项目符号
func TestWrite_NumberingXMLIsDeclaredInContentTypes(t *testing.T)  // 漏登记 -> Word 报损坏
func TestWrite_TableCellsAreScannedAsParagraphs(t *testing.T)      // Scan 后 Para.Cell 非 nil,坐标正确
func TestWrite_EmptyTableCellStillHasAParagraph(t *testing.T)      // 空单元格必须有 <w:p>
func TestWrite_TableHeaderRowIsBold(t *testing.T)
func TestWrite_TableAlignmentRowSetsJc(t *testing.T)
func TestWrite_RaggedTableRowIsPaddedAndDeclared(t *testing.T)     // 列数不齐 -> 补空 + notes
```

**`TestWrite_TableCellsAreScannedAsParagraphs` 是最强的一条**:它用 P1a.5 实现的 `Para.Cell` 坐标去验我们刚生成的表格 —— 两套独立写成的代码互相印证。

- [ ] **Step 2-5**:红 → 实现 → 绿 → 逐分支验红。

- [ ] **Step 6: 更新 Task 1 的"未支持"测试**

Task 1 的 `TestWrite_UnsupportedSyntaxIsDeclared` 断言列表与表格被声明为不支持 —— 本任务实现后**该断言必须改**,改成断言它们**不再**出现在 notes 里,并把仍不支持的项(图片)留在测试中。**不要只是删掉那条测试**,那会丢掉"未支持项必须声明"这条保证。

---

### Task 3: 代码块、行内代码、链接、引用、分隔线

**Files:** Modify `pkg/docx/write.go`, `pkg/docx/write_test.go`

- **代码块**(``` 围栏):等宽字体段落,加浅色底纹(`<w:shd w:fill="F5F5F5"/>`),**每行一个段落**并保留缩进(需 `xml:space="preserve"`)。围栏后的语言标注忽略但不报错。
- **行内代码**(反引号):run 带等宽字体。
- **链接** `[文字](url)`:在 `word/_rels/document.xml.rels` 里加 `hyperlink` 关系(`TargetMode="External"`),正文用 `<w:hyperlink r:id="rIdN">` 包住 run,run 带 `Hyperlink` 字符样式(蓝色+下划线,需在 styles.xml 定义)。**关系 id 必须唯一且与已用的不冲突**。
- **引用** `> `:段落带左缩进 + 左边框。
- **分隔线** `---`(独占一行):空段落带下边框。

注意 `---` 与 setext 标题、以及表格分隔行的歧义:**只有在前后都不构成表格、且该行只有连字符时**才当分隔线。这条要写进注释,否则会把表格分隔行吃掉。

- [ ] 测试要点:链接的关系 id 不与其他关系冲突;代码块保留前导空格;行内代码不吃掉相邻文本;`---` 不误伤表格。

- [ ] **验收**:用一段**真实的设计文档 markdown**(含标题、段落、粗体、嵌套列表、表格、代码块、链接)生成 .docx,用 `OpenDocument`/`Scan` 读回并断言结构完整,再交用户在 Word 中人工确认观感。


### Task 4: 工具层、profile、skill 与设计文档

**Files:** Modify `pkg/tools/builtin/docx.go`、`docx_test.go`、`pkg/agent/types_config.go`、`.deepai/skills/docx-summarize/SKILL.md`、`docs/DOCX_TOOLS_DESIGN.md`

- [ ] **Step 1: `docx_write` 工具**

`path`（必需）、`markdown`（必需）、`title`（可选）。`ParallelSafe: false`。类型必须校验。

结果里回显 `paras` 与 `notes` —— 模型要能告诉用户"列表未按列表渲染"。

**路径走 `resolveWritablePath`**。**不做备份**（新建不覆盖既有文件，没有可备份的对象），但**必须拒绝覆盖**，错误信息说清。

- [ ] **Step 2: profile 与 skill**

`document-editor` 的 `DefaultTools` 加 `docx_write`。`docx-summarize` skill 增加一节：用户要 .docx 产物时，先按现有 map-reduce 得到 markdown，再 `docx_write`；并说明列表/表格不会被渲染成对应结构。

- [ ] **Step 3: 设计文档**

§4.4 记录：本阶段决定**自建不引库**及其三条理由；支持的 markdown 子集与明确不支持的部分；以及"若将来要支持表格/图片/编号列表应重新评估引库"。§10 的分期表把 P2 标记为完成。

- [ ] **Step 4: 回归**

全量测试，确认此前所有阶段的保证仍成立。

---

## 完成标准

1. `go test ./pkg/docx/... ./pkg/tools/...` 全绿；此前所有保证继续成立。
2. 生成的文件能被**我们自己的 `OpenDocument`/`Scan`** 正确读回，标题带正确的样式名。
3. `styles.xml` 里真的定义了 `Heading1`..`Heading6`。
4. 同样输入两次产出**字节相同**。
5. 未支持的 markdown 语法**如实声明**，不静默降级。
6. 拒绝覆盖既有文件。
7. `go vet`（含 `GOOS=windows`）/ `go build` / `gofmt` 全清，`go.mod` 未改。
8. 列表(含嵌套)、表格(含表头加粗与对齐)、代码块、链接均按语义渲染,不是普通段落。
9. 生成的表格能被本包的 `Scan` 读出正确的 `Para.Cell` 坐标 —— 两套独立代码互证。
10. **人工验收(用户执行)**:用一份真实的设计文档 markdown 生成 .docx,在 Word 中打开无修复提示,且:标题是标题样式、列表有正确的项目符号与缩进层级、表格有边框且表头加粗、代码块等宽带底色、链接可点击。**"标题不像标题"和"列表变成普通段落"是本阶段最典型的"看起来成功实则失败"**。
