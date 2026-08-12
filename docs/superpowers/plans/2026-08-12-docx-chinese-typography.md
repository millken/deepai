# docx_write 中文技术文档排版 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `docx_write` 产出的中文技术文档达到可以拿去评审的专业水准，而不是"把 markdown 机械转成 docx"。

**Architecture:** 排版决策绝大多数落进已有的命名样式集（`styles.go`），少数每实例数据落在正文。**核心不变量继续成立**：`document.xml` 不得出现内联 `<w:spacing>` / `<w:ind>` / `<w:shd>`。

**Tech Stack:** Go 1.26.1 标准库。

前置：`9096562`（样式架构）。

---

## 依据：解剖用户提供的参照文档（实测）

用户给了一份他认可的专业文档：`jp-small/docs/软件定制开发合同.docx`（Opus 5 生成），并指出我们的产出"是小学生的水平"。解剖它得到九条具体差距。

**值得注意的是：那份文档的 `styles.xml` 只有 4 KB、16 个样式，`document.xml` 却有 270 KB** —— 它的专业感**不来自样式架构**，而来自一整套中文排版决策。我们刚做完的样式架构没有白做（它让下面这些决策有地方安放、且可被 `docx_format` 重排），但它本身不解决专业度。

| 参照文档 | 我们现在 | 后果 |
|---|---|---|
| `<w:rFonts w:ascii="Calibri" w:eastAsia="微软雅黑"/>`（682 处） | 只设 `w:ascii` | 中文字体由 Word 自选；**代码块里中文回退成比例字体，ASCII 画的框线全部错位** |
| `<w:pgSz w:w="11906" w:h="16838"/>` = **A4** | `12240×15840` = **US Letter** | 中文文档用信纸尺寸，打印和评审都不对 |
| `<w:ind w:firstLine="420"/>`（首行缩进 2 字符） | 无 | 中文正文的基本约定缺失 |
| 正文 `w:line="360"`（1.5 倍）、表格 `260`（紧凑） | 单一默认 | 正文过密、表格过松 |
| 标题 `before="240" after="160"` | 未分化 | 章节层次不清 |
| 表头 `<w:shd w:fill="DDE5F0"/>` | 无 | 表头与数据行无区分 |
| `<w:tcMar>` 单元格内边距 60/100 twips | 无 | 文字贴着框线 |
| `<w:tblHeader/>` | 无 | 长表跨页后没有表头 |
| 页脚 `PAGE` 域（居中、9pt） | 无 | 没有页码 |
| `<w:docGrid w:linePitch="360"/>` | 无 | 中文版式网格缺失 |

---

## Global Constraints

- **零外部 Go 依赖**；`go.mod` 不得改动。不引入二进制模板。
- **核心不变量继续成立**：`document.xml` 无内联 `<w:spacing>` / `<w:ind>` / `<w:shd>`。本阶段新增的表头底纹**必须**走表格样式的条件格式（见下），不得写成单元格内联 `<w:shd>`。
- **不得修改** `zipio.go` / `scan.go` / `splice.go` / `read.go` / `edit.go` / `revision.go` / `document.go`。
- **组合测试必须继续通过**：生成的文档 `docx_format` 每条规则都能处理。
- **产出仍须字节确定**。
- 此前所有阶段的保证继续成立。
- **验红必须最小分支**。
- **不自动提交**。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`。

---

## 表头底纹要走条件格式，不要内联

Word 的表格样式支持**条件格式**，这正是内建表格样式做表头的方式：

```xml
<w:style w:type="table" w:styleId="TableGrid">
  …
  <w:tblStylePr w:type="firstRow">
    <w:rPr><w:b/></w:rPr>
    <w:tcPr><w:shd w:val="clear" w:fill="DDE5F0"/></w:tcPr>
  </w:tblStylePr>
</w:style>
```

表格侧只需 `<w:tblLook w:firstRow="1" w:lastRow="0" w:firstColumn="0" w:lastColumn="0" w:noHBand="0" w:noVBand="1"/>` 启用。

这样表头底纹与加粗都由样式提供，**不变量不破**，而且以后想换配色只改一处。单元格内边距同理走 `<w:tblPr><w:tblCellMar>`。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/styles.go` | 修改：加 `eastAsia` 字体、首行缩进、行距分化、标题间距、表格条件格式与内边距 |
| `pkg/docx/write.go` | 修改：A4 页面、`docGrid`、页脚部件与引用、表格 `tblLook`、表头行 `tblHeader` |
| `pkg/docx/footer.go` | 新建：页脚部件（`PAGE` 域） |
| 对应测试 | |

---

### Task 1: 字体与页面 —— 视觉冲击最大的一步

**Files:** Modify `pkg/docx/styles.go`、`pkg/docx/write.go` 及测试

- [ ] **Step 1: 写失败的测试**

```go
// 1. 每处字体声明都要有 eastAsia，否则中文由 Word 自选
func TestType_EveryFontHasEastAsia(t *testing.T) {
	s := string(buildStylesXML())
	for _, m := range regexp.MustCompile(`<w:rFonts[^/]*/>`).FindAllString(s, -1) {
		if !strings.Contains(m, "w:eastAsia=") {
			t.Errorf("rFonts without eastAsia: %s — Chinese text falls back to Word's pick", m)
		}
	}
}

// 2. 代码块的 eastAsia 必须是等宽中文字体，否则 ASCII 画的框线错位
func TestType_CodeUsesCJKCapableMonospace(t *testing.T)

// 3. 页面是 A4，不是 US Letter
func TestType_PageIsA4(t *testing.T) {
	x := generateDocumentXML(t, "# H\n")
	if !strings.Contains(x, `<w:pgSz w:w="11906" w:h="16838"`) {
		t.Errorf("page is not A4 (11906x16838 twips); a Chinese document on US Letter is wrong")
	}
}

// 4. docGrid 存在
func TestType_HasDocGrid(t *testing.T)

// 5. 表格列宽仍精确加总到新的 A4 可用宽度（回归：宽度是从 sectPr 算的，不是硬编码）
func TestType_TableWidthsFollowA4(t *testing.T)
```

第 5 条是回归保护：页面尺寸一变，列宽计算必须跟着变。`19fc297` 的实现就是从 `sectPr` 推导的，这条测试确认它没被写死。

**字体选择**：`w:ascii` 用无衬线拉丁字体，`w:eastAsia` 用一个 Windows 与 macOS 都常见的中文字体。代码块的 `w:eastAsia` 必须选**等宽中文字体**，否则中英混排的框线图仍会错位 —— 这是图 3 那个问题的直接修法。选定的字体名与理由写进注释。

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

---

### Task 2: 段落排版 —— 首行缩进与行距

**Files:** Modify `pkg/docx/styles.go` 及测试

- [ ] **Step 1: 写失败的测试**

```go
// 1. 正文首行缩进 2 字符（420 twips）
func TestType_BodyHasFirstLineIndent(t *testing.T)

// 2. 首行缩进不得污染列表项、代码、引用、表格单元格
func TestType_FirstLineIndentDoesNotLeakIntoOtherBlocks(t *testing.T) {
	s := string(buildStylesXML())
	for _, id := range []string{"SourceCode", "ListParagraph", "Quote"} {
		st := styleByID(s, id)
		if strings.Contains(st, "w:firstLine=") {
			t.Errorf("%s inherits a first-line indent; only body paragraphs take one", id)
		}
	}
}

// 3. 正文 1.5 倍行距，表格单元格紧凑
func TestType_LineSpacingDiffersBetweenBodyAndTableCells(t *testing.T)

// 4. 标题的段前/段后间距分化且随层级递减
func TestType_HeadingSpacingIsDifferentiated(t *testing.T)
```

**第 2 条是这个任务最容易做错的地方**：首行缩进如果放进 `Normal`，所有 `basedOn=Normal` 的样式都会继承它 —— 列表项、代码行、引用、单元格全部莫名其妙缩进两字。要么显式在这些样式里清零（`w:firstLine="0"`），要么不放进 `Normal` 而放进一个专用的正文样式。选哪种都行，但必须有测试钉住。

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

---

### Task 3: 表格与页脚

**Files:** Modify `pkg/docx/styles.go`、`pkg/docx/write.go`；新建 `pkg/docx/footer.go` 及测试

- [ ] **Step 1: 表格**

- `TableGrid` 加 `<w:tblStylePr w:type="firstRow">`：表头加粗 + 底纹（**走条件格式，不变量不破**）
- `TableGrid` 加 `<w:tblPr><w:tblCellMar>`：单元格内边距（参照文档是上下 60、左右 100 twips）
- 表格侧加 `<w:tblLook w:firstRow="1" …/>` 启用条件格式
- 表头行加 `<w:trPr><w:tblHeader/></w:trPr>`：**跨页重复表头**

- [ ] **Step 2: 页脚**

新增 `word/footer1.xml`，内容是居中的 `PAGE` 域（参照文档用 9pt）；在 `[Content_Types].xml`、`word/_rels/document.xml.rels` 注册，并在 `sectPr` 里 `<w:footerReference w:type="default" r:id="…"/>`。

**关系 id 必须与超链接的 id 空间不冲突** —— 超链接已经在用 `document.xml.rels`，页脚再加一个关系时 id 分配要统一，不能各算各的。

- [ ] **Step 3: 测试**

```go
func TestType_TableHeaderIsShadedViaStyle(t *testing.T)     // 底纹在样式里，不在单元格
func TestType_HeaderRowRepeatsAcrossPages(t *testing.T)     // tblHeader
func TestType_CellsHaveInnerMargins(t *testing.T)
func TestType_FooterHasPageNumberField(t *testing.T)
func TestType_FooterIsRegisteredEverywhere(t *testing.T)    // Content_Types + rels + sectPr 三处
func TestType_HyperlinkAndFooterRelIDsDoNotCollide(t *testing.T)
func TestWrite_NoInlineVisualPropertiesInDocumentXML(t *testing.T) // 仍然通过
```

倒数第二条是真会出错的：两处独立分配关系 id 会撞车，而撞车后 Word 会把页脚指向超链接目标，表现为"文件损坏"或页脚内容诡异。

- [ ] **Step 4: 逐分支验红与全量回归。**

---

## 完成标准

1. 每处 `rFonts` 都带 `w:eastAsia`；代码块用等宽中文字体，中英混排的框线图对齐。
2. 页面为 A4；表格列宽仍从 `sectPr` 推导并精确加总。
3. 正文首行缩进 2 字符，且**未泄漏**到列表/代码/引用/单元格。
4. 正文 1.5 倍行距、表格紧凑、标题间距分化。
5. 表头有底纹与加粗（**经样式条件格式**）、单元格有内边距、长表跨页重复表头。
6. 页脚有居中页码，三处注册齐全，关系 id 不冲突。
7. **核心不变量仍成立**；组合测试与字节确定性测试仍通过。
8. **人工验收（用户执行）**：用同一份设计文档 markdown 生成，与 `jp-small/docs/软件定制开发合同.docx` 并排对比 —— 字体、首行缩进、行距、表头底纹、页码、跨页表头。差距应显著缩小；若仍有明显差异，把差异点反馈回来。
