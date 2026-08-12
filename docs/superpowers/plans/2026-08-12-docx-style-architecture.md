# docx_write 排版架构：样式集中定义 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `docx_write` 的排版从"每个视觉属性各自内联"改成"样式集中定义、正文只引用样式名"，这是主流实现保证排版的机制。

**Architecture:** 在 `styles.xml` 里定义一套互相协调的命名样式（`basedOn` 继承），`document.xml` 只写结构与 `<w:pStyle>` 引用。

**Tech Stack:** Go 1.26.1 标准库。

前置：`fb3e09e` + 当前正在进行的四缺陷修复（HTML 实体、表格宽度、代码块间距、标题重复）。

---

## 依据：解剖 python-docx 的 `default.docx`（实测，非推测）

真实 Word 文档的 `styles.xml` 是 **438 KB、164 个样式**；我们生成的是几百字节。但差别的实质不是大小，是**布局信息放在哪里**：

```xml
<!-- 表格样式自带"单元格段落零间距" -->
<w:style w:type="table" w:styleId="TableGrid">
  <w:pPr><w:spacing w:after="0" w:line="240" w:lineRule="auto"/></w:pPr>
  <w:tblPr><w:tblBorders>…</w:tblBorders></w:tblPr>
</w:style>

<!-- 列表样式自带缩进 + 同类项收紧 -->
<w:style w:styleId="ListParagraph">
  <w:pPr><w:ind w:left="720"/><w:contextualSpacing/></w:pPr>
</w:style>
```

而 `docDefaults` 是 `<w:spacing w:after="200" w:line="276" w:lineRule="auto"/>` —— **每个段落默认带 200 twips 段后距**。

这一条解释了用户看到的全部三个观感问题：

| 现象 | 真实原因 |
|---|---|
| 代码块渲染成一条条横纹 | 继承 `after=200`，且没有 `<w:contextualSpacing/>` |
| 表格行很高、单元格字字换行 | 单元格段落同样继承 `after=200`（真实 Word 用 `TableGrid` 把它清零） |
| 列表项之间有空隙 | 同上，缺 `contextualSpacing` |

**`<w:contextualSpacing/>` 是"同类相邻段落之间不留间距"的机制**，列表和代码块都靠它。

### 主流架构

> 参考文档提供样式，转换器只写结构与样式名。

pandoc 用 `reference.docx`、python-docx 用 `default.docx`，都是 Word 亲自撰写的真实文件。转换器不合成样式定义。本阶段采用同一**纪律**但自建样式集（用户决定），不引入二进制模板。

**额外收益**：引用命名样式的内容，`docx_format` 以后能整体重排；带内联直接格式的内容重排不了。这与我们已建好的工具直接相关。

---

## 核心不变量（本阶段的可测试纪律）

> **`document.xml` 中不得出现内联的 `<w:spacing>`、`<w:ind>`、`<w:shd>`。**

这三个元素是段落级视觉属性，必须住在 `styles.xml`。用一条测试钉死它，将来谁图省事内联一个间距，测试立刻变红。

**例外（要在测试里显式列出并说明理由）**：
- `<w:numPr>`：列表项挂编号定义，是结构不是样式。
- 表格的 `<w:tblW>` / `<w:gridCol>`：列宽随每张表的列数变化，无法写进共享样式。
- `<w:jc>`：GFM 表格的列对齐是**每张表自带的数据**，不是样式。

**不受此约束的是字符级强调**：markdown 的 `**粗体**` / `*斜体*` 产出的 `<w:b/>` / `<w:i/>` 留在内联 —— Word 自己对手动加粗也是这么写的，那是内容语义而非排版决策。

---

## Global Constraints

- **零外部 Go 依赖**；`go.mod` 不得改动。**不引入二进制模板文件。**
- **不得修改** `zipio.go` / `scan.go` / `splice.go` / `read.go` / `edit.go` / `revision.go` / `document.go`。
- **组合测试必须继续通过**：`docx_write` 产出的文档仍能被 `docx_format` 的每一条规则处理（`write_format_compose_test.go`）。
- **产出仍须字节确定**。
- 此前所有阶段的保证继续成立（两条保真门、关闭 `track_changes` 时逐字节不变）。
- **验红必须最小分支**。
- **不自动提交**。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`。

---

## 样式集（约 12 个，含 `basedOn` 继承）

| styleId | 类型 | 关键属性 | 用于 |
|---|---|---|---|
| `Normal` | paragraph | 基准，不设多余属性 | 正文段落 |
| `Heading1`..`Heading6` | paragraph | `basedOn=Normal`、`keepNext`、字号递减、段前距 | `#`..`######` |
| `SourceCode` | paragraph | `spacing before/after=0`、`contextualSpacing`、`shd fill=F5F5F5`、等宽字体 | ``` 代码块 |
| `VerbatimChar` | character | 等宽字体 | `` `行内代码` `` |
| `Quote` | paragraph | `basedOn=Normal`、左缩进、左边框、斜体 | `> ` 引用 |
| `ListParagraph` | paragraph | `basedOn=Normal`、`ind left`、`contextualSpacing` | 列表项 |
| `TableGrid` | **table** | `tblBorders`、`pPr spacing after=0 line=240` | 所有表格 |
| `Hyperlink` | character | 蓝色 + 下划线 | `[text](url)` |

**`Heading*` 要有 `keepNext`** —— 否则标题会被留在页底、正文翻到下一页。用户截图里"2.4 关键横切设计"前后的大片空白正是这类问题的表现之一。

**`TableGrid` 必须是 `w:type="table"` 的样式**，它能同时携带 `tblPr`（边框）与 `pPr`（单元格内段落的间距）。表格通过 `<w:tblPr><w:tblStyle w:val="TableGrid"/></w:tblPr>` 引用后，边框与单元格零间距一并生效 —— 这正是当前表格行过高的修法。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/styles.go` | 新建：样式集定义（唯一的视觉属性来源） |
| `pkg/docx/styles_test.go` | 新建 |
| `pkg/docx/write.go` | 修改：正文改为引用样式名，移除内联视觉属性 |
| `pkg/docx/write_test.go` | 修改 |

---

### Task 1: `styles.go` —— 样式集

**Files:** Create `pkg/docx/styles.go`, `pkg/docx/styles_test.go`

- [ ] **Step 1: 写失败的测试**

```go
// 1. 每个样式都真的被定义（缺定义时 Word 按 Normal 渲染 —— 看起来正常实则失败）
func TestStyles_AllReferencedStylesAreDefined(t *testing.T)

// 2. basedOn 链有效：引用的父样式必须存在，否则 Word 悄悄回退
func TestStyles_BasedOnTargetsExist(t *testing.T)

// 3. 代码块样式带 contextualSpacing 与零间距 —— 条纹问题的根治点
func TestStyles_SourceCodeCollapsesSpacing(t *testing.T)

// 4. 列表样式带 contextualSpacing
func TestStyles_ListParagraphCollapsesSpacing(t *testing.T)

// 5. TableGrid 是 table 类型，且把单元格段落间距清零
func TestStyles_TableGridZeroesCellSpacing(t *testing.T) {
	s := string(buildStylesXML())
	m := regexp.MustCompile(`<w:style w:type="table" w:styleId="TableGrid">.*?</w:style>`).FindString(s)
	if m == "" {
		t.Fatal("TableGrid is not defined as a table-type style")
	}
	if !strings.Contains(m, `w:after="0"`) {
		t.Error("TableGrid does not zero cell paragraph spacing; table rows will be tall")
	}
}

// 6. 标题带 keepNext，避免标题孤立在页底
func TestStyles_HeadingsKeepWithNext(t *testing.T)

// 7. docDefaults 链完整（docx_format 依赖它，且 fb3e09e 刚修过）
func TestStyles_DocDefaultsChainIsComplete(t *testing.T)

// 8. schema 顺序：docDefaults 是 w:styles 的第一个子元素
func TestStyles_DocDefaultsIsFirstChild(t *testing.T)
```

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

---

### Task 2: 正文改为引用样式

**Files:** Modify `pkg/docx/write.go`、`write_test.go`

- [ ] **Step 1: 写失败的测试 —— 核心不变量**

```go
func TestWrite_NoInlineVisualPropertiesInDocumentXML(t *testing.T) {
	md := "# H\n\nBody.\n\n- a\n    - b\n\n> quote\n\n```\ncode\n```\n\n| x | y |\n|---|---|\n| 1 | 2 |\n"
	x := generateAndReadDocumentXML(t, md)
	for _, banned := range []string{"<w:spacing", "<w:ind ", "<w:shd"} {
		if strings.Contains(x, banned) {
			t.Errorf("%s appears inline in document.xml; paragraph-level visual "+
				"properties belong in styles.xml", banned)
		}
	}
}
```

- [ ] **Step 2: 每类块引用对应样式**

代码块 → `SourceCode`；行内代码 → `VerbatimChar`；引用 → `Quote`；列表项 → `ListParagraph` + `numPr`；表格 → `<w:tblStyle w:val="TableGrid"/>`；链接 → `Hyperlink`。

- [ ] **Step 3: 观感回归测试**

用一份真实设计文档 markdown 生成，断言：
- 每个代码行段落都引用 `SourceCode`
- 每个列表项都引用 `ListParagraph` 且带 `numPr`
- 表格引用 `TableGrid` 且列宽仍精确加总到可用宽度
- 文档仍能 `OpenDocument` 读回、`Format` 每条规则仍可用（组合测试）

- [ ] **Step 4: 逐分支验红与全量回归。**

---

## 完成标准

1. **`document.xml` 里没有内联的 `<w:spacing>` / `<w:ind>` / `<w:shd>`**（例外已在不变量一节列明并有测试说明）。
2. 每个被引用的样式都真的有定义，`basedOn` 目标都存在。
3. 代码块与列表通过 `contextualSpacing` 收紧；表格通过 `TableGrid` 获得零单元格间距。
4. 组合测试继续通过：生成的文档 `docx_format` 每条规则都能处理。
5. 产出仍字节确定；此前所有阶段的保证成立。
6. **人工验收（用户执行）**：生成同一份设计文档，在 Word 中确认代码块是连续色块而非横纹、表格行高正常、列表缩进层次分明、标题不孤立在页底。
