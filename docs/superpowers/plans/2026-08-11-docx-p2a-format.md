# DOCX P2a: `docx_format` 排版 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补上排版能力。P1 落地后用户实测发现：让 agent 排版时它会退回 `bash` + `python-docx` 现写脚本 —— 文件本身不会坏（已实测），但**绕开了备份、保护清单、审计记录、修订闸门，且是非沙箱的任意代码执行**。根因是我们没给它工具。

**Architecture:** 沿用 P1 的分层。`pkg/docx/format.go` 在 `styles.xml` 与 `document.xml` 的**原始字节上做定点替换**（与正文编辑同一套 byte-splice 纪律）；`pkg/tools/builtin/docx.go` 加薄封装；一个 `docx-format` skill。

**Tech Stack:** Go 1.26.1 标准库 + 已有的 `pkg/docx`。

前置：P1 全部（`ef958d1`，已合并 main）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §4.3、§3.1、§3.1.1。

## Global Constraints

- **零外部 Go 依赖**：`go.mod` 不得改动。
- **绝不 DOM 重建**：`encoding/xml` 只作扫描器，所有写入都是字节区间替换。
- **不改正文文字**：`docx_format` 只动样式与版式。唯一例外是 `normalize`（合并连续空段），那是明确声明的正文操作。
- **P1 的全部保证必须继续成立**：`TestWriteTo_UntouchedPartsAreByteIdentical`、`TestFidelity_SingleWordEditKeepsEverythingElseIdentical`，以及未被本次触碰的 zip 条目逐字节不变。
- **验红必须最小分支**。P1 期间累计六次栽在验红过粗或"成对分支只测一半"上。
- **不自动提交**：提交由用户口头触发。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`（`pkg/agent/compact.go` 的 gofmt 报错是既有的，不要动）。

## 实测得到的 OOXML 事实（照此实现，勿凭想象）

在 `pkg/docx/testdata/structure.docx` 上实测：

1. **`Normal` 样式是空的** —— `<w:style w:styleId="Normal"><w:name/><w:qFormat/><w:rsid/></w:style>`，没有 `<w:rPr>` 也没有 `<w:pPr>`。正文默认值实际来自 `<w:docDefaults>`：
   ```xml
   <w:docDefaults>
     <w:rPrDefault><w:rPr><w:rFonts w:asciiTheme="minorHAnsi" .../><w:sz w:val="22"/><w:szCs w:val="22"/>...</w:rPr></w:rPrDefault>
     <w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault>
   </w:docDefaults>
   ```
   所以 `body_font` / `body_size_pt` / `line_spacing` 落在 `docDefaults`，**不是**改 `Normal`（那需要凭空插入元素，且优先级更高、更容易与文档既有设置打架）。

2. **标题用主题字体**：`<w:rFonts w:asciiTheme="majorHAnsi" w:eastAsiaTheme="majorEastAsia" w:hAnsiTheme="majorHAnsi" w:cstheme="majorBidi"/>`。**只加 `w:ascii="Georgia"` 是无效的** —— 主题属性仍在，Word 以主题为准。设置 `heading_font` 必须**同时移除 `*Theme` 属性**并写入字面 `w:ascii` / `w:hAnsi` / `w:eastAsia` / `w:cs`。这是本任务最容易做错的一处。

3. **单位互不相同，全部要换算**：
   | 参数 | XML | 单位 | 换算 |
   |---|---|---|---|
   | `body_size_pt` | `<w:sz w:val>` | 半磅 | `22` = 11pt，即 `val = pt*2` |
   | `line_spacing` | `<w:spacing w:line>` + `w:lineRule="auto"` | 240 分之一行 | `276` = 1.15 倍，即 `line = round(spacing*240)` |
   | `margins_mm` | `<w:pgMar w:top/right/bottom/left>` | twip | `1440` = 1 英寸 = 25.4mm，即 `twip = round(mm*1440/25.4)` |
   `<w:sz>` 变了要同步改 `<w:szCs>`（复杂文种字号），否则中日韩与阿拉伯文不跟随。

4. **页边距在 `document.xml` 的 `<w:sectPr><w:pgMar>`**，不在 `styles.xml`。

5. `styles.xml` 有 349 KB，含 `Heading1`-`Heading9`、`Title`、`Subtitle` 等。byte-splice 不受体积影响。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/format.go` | `(*Document).Format(FormatOptions) (FormatResult, error)` 与 styles/sectPr 的定点替换 |
| `pkg/docx/format_test.go` | |
| `pkg/tools/builtin/docx.go` | 修改：新增 `DocxFormatTool` / handler，`DocxTools()` 加入它 |
| `pkg/tools/builtin/docx_test.go` | 修改 |
| `pkg/agent/types_config.go` | 修改：`document-editor` 的 `DefaultTools` 加 `docx_format` |
| `.deepai/skills/docx-format/SKILL.md` | 新建 |

---

### Task 1: `pkg/docx/format.go` —— 样式与版式的定点替换

**Files:** Create `pkg/docx/format.go`, `pkg/docx/format_test.go`

**Interfaces:**
```go
type FormatOptions struct {
    Template    string   // "corporate" | "academic" | "minimal"；先展开成下列字段再应用
    HeadingFont string
    BodyFont    string
    BodySizePt  float64
    LineSpacing float64
    Align       string   // "left" | "justify"
    MarginsMM   []float64 // 恰好 4 个：上 右 下 左；nil 表示不改
    Normalize   bool
}
type FormatResult struct {
    Applied []string // 人类可读的已改项，例如 "body font -> Georgia"
    Notes   []string
}
func (d *Document) Format(opts FormatOptions) (FormatResult, error)
```

**落点规则**（依据上文实测事实）：

| 参数 | 落在 | 元素 |
|---|---|---|
| `BodyFont` | `styles.xml` | `docDefaults/rPrDefault/rPr/rFonts`（移除 `*Theme` 属性） |
| `BodySizePt` | `styles.xml` | `docDefaults/rPrDefault/rPr/sz` + `szCs` |
| `LineSpacing` | `styles.xml` | `docDefaults/pPrDefault/pPr/spacing`（`w:line` + `w:lineRule="auto"`） |
| `Align` | `styles.xml` | `docDefaults/pPrDefault/pPr/jc` |
| `HeadingFont` | `styles.xml` | 每个 `Heading1`..`Heading9` 的 `rPr/rFonts`（移除 `*Theme`） |
| `MarginsMM` | `document.xml` | `sectPr/pgMar` 的 `w:top/right/bottom/left` |
| `Normalize` | `document.xml` | 合并连续空段落 |

**元素不存在时要插入**，因为实测显示 `Normal` 之类节点可能是空的。规则：若目标属性存在则改属性值；若元素存在但缺属性则加属性；若元素不存在则在其父元素内插入一个最小元素。三种情况各要有测试。

**`Normalize` 的定义收窄**：本任务只做**合并连续空段落**（保留一个）。"统一标点间距"涉及语言判断，留待后续，且要在 `Notes` 里说明本次未做。

**`Template` 预设**（展开成具体值，实现里写成一张表）：
- `corporate`：body Calibri 11pt / 行距 1.15 / 两端对齐 / 边距 25.4mm
- `academic`：body "Times New Roman" 12pt / 行距 2.0 / 左对齐 / 边距 25.4mm
- `minimal`：body 保持不动 / 行距 1.0 / 左对齐 / 边距 20mm

显式给出的字段**覆盖**模板值。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/format_test.go`。要覆盖：

```go
package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func formatDoc(t *testing.T) (*Document, string) {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "f.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	return d, p
}

func stylesXML(t *testing.T, d *Document) string {
	t.Helper()
	b, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("styles.xml missing")
	}
	return string(b)
}

func TestFormat_BodySizeLandsInDocDefaultsAndSyncsSzCs(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 14}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:sz w:val="28"/>`) {
		t.Errorf("docDefaults lacks sz=28 (14pt in half-points):\n%.400s", dd)
	}
	if !strings.Contains(dd, `<w:szCs w:val="28"/>`) {
		t.Error("szCs was not synced; CJK and complex scripts would keep the old size")
	}
}

// TestFormat_HeadingFontRemovesThemeAttributes is the trap this task exists
// to avoid: the fixture's heading rFonts carries w:asciiTheme="majorHAnsi",
// and a literal w:ascii added beside it is ignored by Word — the theme wins.
func TestFormat_HeadingFontRemovesThemeAttributes(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{HeadingFont: "Georgia"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	h1 := s[strings.Index(s, `w:styleId="Heading1"`):]
	h1 = h1[:strings.Index(h1, "</w:style>")]
	if !strings.Contains(h1, `w:ascii="Georgia"`) {
		t.Errorf("Heading1 lacks the literal font:\n%s", h1)
	}
	if strings.Contains(h1, "Theme=") {
		t.Errorf("Heading1 still carries theme font attributes, which override the literal one:\n%s", h1)
	}
}

func TestFormat_MarginsLandInSectPrAsTwips(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `w:top="1440"`) {
		t.Errorf("pgMar top is not 1440 twips (25.4mm)")
	}
}

func TestFormat_RejectsWrongMarginCount(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{MarginsMM: []float64{10, 20}}); err == nil {
		t.Fatal("a 2-element margins list was accepted; it must be exactly 4")
	}
}

// TestFormat_LeavesBodyTextUntouched is §4.3's core promise.
func TestFormat_LeavesBodyTextUntouched(t *testing.T) {
	d, _ := formatDoc(t)
	before := make([]string, 0, d.TotalParas())
	for _, p := range d.Paras() {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		before = append(before, b.String())
	}
	if _, err := d.Format(FormatOptions{BodyFont: "Georgia", BodySizePt: 13, LineSpacing: 1.5, Align: "justify"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if d.TotalParas() != len(before) {
		t.Fatalf("paragraph count changed: %d -> %d", len(before), d.TotalParas())
	}
	for i, p := range d.Paras() {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		if b.String() != before[i] {
			t.Errorf("paragraph %d text changed: %q -> %q", i+1, before[i], b.String())
		}
	}
}

// TestFormat_TouchesOnlyTheExpectedParts pins that formatting does not
// disturb entries it has no business in.
func TestFormat_TouchesOnlyTheExpectedParts(t *testing.T) {
	d, p := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 13, MarginsMM: []float64{20, 20, 20, 20}}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, fixture, p, map[string]bool{
		"word/styles.xml": true,
		DocumentPart:      true,
	})
}

func TestFormat_StylesOnlyChangeLeavesDocumentXMLAlone(t *testing.T) {
	d, p := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 13}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	assertEntriesEqual(t, fixture, p, map[string]bool{"word/styles.xml": true})
}

func TestFormat_NormalizeMergesConsecutiveEmptyParagraphs(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>` +
		`<w:p/><w:p/><w:p/>` +
		`<w:p><w:r><w:t>two</w:t></w:r></w:p>` +
		`</w:body>`)
	got, removed, err := normalizeEmptyParagraphs(doc)
	if err != nil {
		t.Fatalf("normalizeEmptyParagraphs: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (three empties collapse to one)", removed)
	}
	paras, err := Scan(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3 (one, one empty, two)", len(paras))
	}
}

func TestFormat_NormalizeLeavesASingleEmptyParagraphAlone(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>a</w:t></w:r></w:p><w:p/><w:p><w:r><w:t>b</w:t></w:r></w:p></w:body>`)
	_, removed, err := normalizeEmptyParagraphs(doc)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0; a lone empty paragraph is deliberate spacing", removed)
	}
}

func TestFormat_TemplateExpandsAndExplicitFieldsWin(t *testing.T) {
	d, _ := formatDoc(t)
	res, err := d.Format(FormatOptions{Template: "academic", BodySizePt: 13})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:sz w:val="26"/>`) {
		t.Errorf("explicit BodySizePt 13 did not override the academic template's 12pt")
	}
	if !strings.Contains(dd, "Times New Roman") {
		t.Errorf("the academic template's body font was not applied")
	}
	if len(res.Applied) == 0 {
		t.Error("Applied is empty; the caller cannot tell what changed")
	}
}

func TestFormat_UnknownTemplateErrors(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{Template: "fancy"}); err == nil {
		t.Fatal("an unknown template was accepted")
	}
}

func TestFormat_NoOptionsIsANoOp(t *testing.T) {
	d, p := formatDoc(t)
	res, err := d.Format(FormatOptions{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("Applied = %v, want empty for an empty request", res.Applied)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	assertEntriesEqual(t, fixture, p, nil)
}
```

- [ ] **Step 2: 跑测试确认失败**（`undefined: FormatOptions` 等）

- [ ] **Step 3: 实现**

要点：

1. 复用既有的 byte-splice 纪律：用 `xml.Decoder` + `InputOffset()` 定位目标元素/属性的字节区间，再 splice。**不要**把 styles.xml 反序列化。
2. 属性替换需要三种能力：改已有属性值、给已有元素加属性、在父元素内插入新元素。抽成小函数，各自有测试。
3. 移除 `*Theme` 属性时要连同 `w:asciiTheme` / `w:eastAsiaTheme` / `w:hAnsiTheme` / `w:cstheme` 一起移除。
4. 所有写入通过 `SetPart` + 一次 `rescan()`（若动了 `document.xml`）。
5. `page_numbers` / `rebuild_toc` **不在 `FormatOptions` 里** —— 它们是 P3，工具层负责报错（Task 2）。

- [ ] **Step 4-5: 绿 + 验红**

| 移除的最小分支 | 应变红 |
|---|---|
| `szCs` 同步 | `TestFormat_BodySizeLandsInDocDefaultsAndSyncsSzCs` |
| `*Theme` 属性移除 | `TestFormat_HeadingFontRemovesThemeAttributes` |
| mm→twip 换算（改成直接用 mm） | `TestFormat_MarginsLandInSectPrAsTwips` |
| "连续 ≥2 个才合并"的判断 | `TestFormat_NormalizeLeavesASingleEmptyParagraphAlone` |
| 显式字段覆盖模板的逻辑 | `TestFormat_TemplateExpandsAndExplicitFieldsWin` |

---

### Task 2: `docx_format` 工具 + profile + skill

**Files:** 修改 `pkg/tools/builtin/docx.go`、`docx_test.go`、`pkg/agent/types_config.go`；新建 `.deepai/skills/docx-format/SKILL.md`

- [ ] **Step 1: 工具**

`DocxFormatTool()`：`ParallelSafe: false`（写操作），组 `{"builtin","document"}`。schema 为 `path` + `rules` 对象，字段同 `FormatOptions` 的 JSON 形式。

工具层专属职责（与 `docx_edit` 一致）：
- **备份一次**：`<path>.bak` 不存在才创建，返回 `backup_path` 与 `backup_created`。
- **`page_numbers` / `rebuild_toc` 显式报错**：说明属 P3、需要 LibreOffice，**不要静默忽略**（§4.3 明确要求）。
- **类型检查**：字符串/布尔/数字都要校验，不得静默强制转换（P1c 的教训：`text` 写成裸数字曾静默删内容）。
- 返回 `applied` 列表与 `notes`，让模型能向用户复述改了什么。

- [ ] **Step 2: profile**

`pkg/agent/types_config.go`：`document-editor` 的 `DefaultTools` 加 `"docx_format"`。系统提示补一句：排版用 `docx_format`，**不要用 bash/python 直接改 .docx** —— 那样会绕开备份与审计。

- [ ] **Step 3: skill**

`.deepai/skills/docx-format/SKILL.md`，扁平一层。frontmatter 的 `description` 要含排版类触发词（排版、字体、行距、页边距、套模板）。正文按 §7.1 的 A 方案：委派 `agent_type: document-editor`。

必须写明：
- 破坏性操作前用 `ask_clarification` 与用户确认（§4.3 与 §7）
- 先 `docx_read` 看文档现状再决定规则
- `page_numbers` / `rebuild_toc` 尚不支持，会报错，**不要转而用 python 绕过**
- 完成后复述 `applied` 列表与备份路径

- [ ] **Step 4: 测试与回归**

工具层测试：备份一次、`page_numbers` 报错、类型检查、`applied` 非空。全量回归确认 P1 的两条保真门仍通过。

---

## 完成标准

1. `go test ./pkg/docx/... ./pkg/tools/... -race` 全绿；P1 两条保真门继续通过。
2. 每个新行为都有"移除最小分支即失败"的验红证据。
3. **纯样式改动只动 `word/styles.xml`**，其余 zip 条目逐字节不变（有测试钉住）。
4. **正文文字一字未改**（有测试逐段比对）。
5. `go vet`（含 `GOOS=windows`）/ `go build` / `gofmt` 全清，`go.mod` 未改。
6. `page_numbers` / `rebuild_toc` 报错而非静默忽略。
7. **人工验收**：排版后的文件用 Word 打开无修复提示，且字体/字号/行距/边距确实生效。本机无 Word，需用户执行。

P2b（`track_changes`）与 P2c（`docx_write`）在 P2a 通过后另行规划。
