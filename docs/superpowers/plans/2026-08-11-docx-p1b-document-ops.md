# DOCX P1b: 文档操作层（read / edit）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 P1a 的字节级地基之上建出文档操作层：打开/保存的生命周期、大纲、分块读取与游标、markdown 渲染、以及带 protect 校验的批量编辑。这一层是 P1c 工具封装将直接调用的 API，本身不含任何 LLM 逻辑。

**Architecture:** 新增 `document.go`（生命周期与策略闸门）、`read.go`（大纲 / 分块读 / markdown）、`edit.go`（定位 / 校验 / 补丁编排）。全部构建在既有 `Open`/`Scan`/`Apply`/`ApplyToPart` 之上，**不改动** `zipio.go` / `scan.go` / `splice.go`。

**Tech Stack:** Go 1.26.1 标准库。

前置：P1a + P1a.5（提交 `66365f3`）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §4.1、§4.2、§5。

## Global Constraints

- **零外部 Go 依赖**：不得向 `go.mod` 添加任何 require。
- **不改动 P1a 三个文件**：`zipio.go` / `scan.go` / `splice.go` 及其测试。若你认为必须改，**停下来报告**，不要自行修改——那三层刚过完多轮审查。
- **绝不 DOM 重建**；所有写操作最终都落到 `Apply` / `ApplyToPart`。
- **P1a 的 66 项断言与两条保真测试必须继续通过**：`TestWriteTo_UntouchedPartsAreByteIdentical`、`TestFidelity_SingleWordEditKeepsEverythingElseIdentical`。
- **验红必须最小分支**：只注释掉被测中的那一个分支，不要删掉整块功能。本项目已三次栽在验红过粗上。
- **不自动提交**：提交由用户在里程碑处口头触发。
- **实现风格**：本计划**只给全量测试代码 + 类型定义 + 算法步骤**，函数体由你写。这是刻意的——照抄逐行实现会把规划者的错误直接转写进代码。如果测试与说明冲突，**以测试为准并报告冲突**。
- **测试命令**：`go test ./pkg/docx/... -race -count=1`、`go vet ./pkg/docx/... && GOOS=windows go vet ./pkg/docx/...`、`go build ./...`、`gofmt -l pkg/docx`。

## 范围决定（已定，勿重议）

- **`style` 参数推迟到 P2**：改段落样式需要 `<w:pStyle>` 的字节区间（`scan.go` 目前只存 `Para.Style` 字符串不存位置），而改样式本属 §4.3 `docx_format` 的职责。`docx_edit` 只管文本。设计文档 §4.2 需同步加注。
- **已含修订标记的文档拒绝编辑**：§4.1 建议的 P1 策略。检测到任一段 `HasRevisions` 即拒绝整批编辑并提示"请先在 Word 里接受或拒绝现有修订"。这样完全回避"在已有修订上叠加改动"的语义问题。
- **页眉页脚 / 脚注 / 批注不读取**，但**必须在输出里显式声明存在且未包含**，不能让 agent 以为读到了全文。
- **文本框内容不读取**（P1a.5 已跳过），含文本框的段落在输出里标注。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/document.go` | `Document` 生命周期：打开、重扫、保存；编辑前的策略闸门 |
| `pkg/docx/document_test.go` | |
| `pkg/docx/read.go` | `Outline()` 与 `Read()`：大纲、范围/标题选取、`max_chars` 游标、markdown 渲染 |
| `pkg/docx/read_test.go` | |
| `pkg/docx/edit.go` | `Edit()`：定位、protect 校验、补丁编排、逐条结果 |
| `pkg/docx/edit_test.go` | |
| `pkg/docx/testdata/gen_fixtures.py` | 扩充：新增 `outline.docx` 夹具（多级标题 + 长正文，供大纲与分块测试） |

---

### Task 1: `document.go` —— 生命周期与策略闸门

**Files:**
- Create: `pkg/docx/document.go`, `pkg/docx/document_test.go`
- Modify: `pkg/docx/testdata/gen_fixtures.py`（新增 `outline.docx`）

**Interfaces:**
- Consumes: `Open` / `Part` / `DocumentPart` / `Scan` / `ApplyToPart` / `WriteTo` / `Para`
- Produces:
  ```go
  type Document struct{ /* unexported */ }

  func OpenDocument(path string) (*Document, error)
  func (d *Document) Paras() []Para          // 只读快照
  func (d *Document) TotalParas() int
  func (d *Document) Notes() []string        // 未包含的内容的显式声明
  func (d *Document) HasRevisions() bool
  func (d *Document) Save() error            // 写回原路径
  func (d *Document) SaveAs(path string) error
  ```
  `OpenDocument` 打开包、取出 `document.xml`、`Scan` 一次并缓存。`Notes()` 返回形如 `"headers/footers present but not included"` 的声明串，来源：`Names()` 里存在 `word/header*.xml` / `footer*.xml` / `footnotes.xml` / `endnotes.xml` / `comments.xml`，以及任一段 `SkippedTextBox`。

- [ ] **Step 1: 扩充夹具生成脚本**

在 `gen_fixtures.py` 末尾追加一个 `build_outline_fixture()`，生成 `outline.docx`：

- 三级标题结构：`Heading1` "Chapter One" → 两段正文；`Heading2` "Section 1.1" → 三段正文；`Heading1` "Chapter Two" → 两段正文；`Heading2` "Section 2.1" → 一段正文
- 每段正文用可预测的文本（例如 `"Body paragraph %d of section %s."`），便于断言字数
- **必须包含一个多 run 段落**：紧跟 `Heading2 "Section 1.1"` 之后插入一段，由三个 run 组成 —— `"Plain "` / `"bold"`（加粗）/ `" tail"`。没有它，"整段替换的多 run 警告"和"跨 run 的 find 拒绝"两条都无法测（`outline.docx` 其余段落都是单 run，断言会空过）。
- 额外追加 **60 段**填充正文（文本 `"Filler paragraph %d."`），使总段数超过分块测试所需规模
- 无表格、无图片、无页眉页脚——保持这份夹具**只测大纲与分块**，与 `structure.docx` 的职责区分开

在 `__main__` 里同时生成两份夹具。运行后提交 `outline.docx`。

- [ ] **Step 2: 跑生成脚本并确认**

Run:
```bash
cd pkg/docx/testdata && python3 gen_fixtures.py && python3 - <<'EOF'
import zipfile
x = zipfile.ZipFile("outline.docx").read("word/document.xml").decode()
assert x.count('w:val="Heading1"') == 2, x.count('w:val="Heading1"')
assert x.count('w:val="Heading2"') == 2
assert x.count("Filler paragraph") == 60
print("outline.docx OK")
EOF
```
Expected: `outline.docx OK`。`structure.docx` 必须保持不变——若它的字节变了，说明脚本改动影响了既有夹具，**停下来报告**（P1a 的全部测试都绑在那份夹具上）。

- [ ] **Step 3: 写失败的测试**

创建 `pkg/docx/document_test.go`：

```go
package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const outlineFixture = "testdata/outline.docx"

func TestOpenDocument_ScansOnce(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if d.TotalParas() != len(d.Paras()) {
		t.Errorf("TotalParas = %d, len(Paras) = %d", d.TotalParas(), len(d.Paras()))
	}
	if d.TotalParas() != 10 {
		t.Errorf("TotalParas = %d, want 10 (structure.docx)", d.TotalParas())
	}
}

// TestParas_IsASnapshot pins that callers cannot corrupt the document's
// internal index by mutating what Paras() handed them.
func TestParas_IsASnapshot(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	got := d.Paras()
	if len(got) == 0 {
		t.Fatal("no paragraphs")
	}
	got[0].Index = 9999
	if d.Paras()[0].Index == 9999 {
		t.Error("Paras() returned an aliasing slice; mutation leaked into the document")
	}
}

// TestNotes_DeclaresUnreadContent pins §4.1's requirement that the reader
// must never silently present a partial document.
func TestNotes_DeclaresUnreadContent(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	notes := strings.Join(d.Notes(), " | ")
	// structure.docx has header1.xml and footer1.xml.
	if !strings.Contains(notes, "header") {
		t.Errorf("Notes = %q, want it to mention headers", notes)
	}
	if !strings.Contains(notes, "footer") {
		t.Errorf("Notes = %q, want it to mention footers", notes)
	}
}

func TestNotes_EmptyWhenNothingIsOmitted(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if len(d.Notes()) != 0 {
		t.Errorf("Notes = %v, want none for a plain document", d.Notes())
	}
}

// TestHasRevisions_DetectsExistingMarks feeds §4.1's recommended P1 policy.
func TestHasRevisions_DetectsExistingMarks(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if !d.HasRevisions() {
		t.Error("HasRevisions = false, want true (structure.docx contains w:ins and w:del)")
	}
	plain, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if plain.HasRevisions() {
		t.Error("HasRevisions = true for outline.docx, want false")
	}
}

func TestSaveAs_ProducesAReadableCopy(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	out := filepath.Join(t.TempDir(), "copy.docx")
	if err := d.SaveAs(out); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reopened, err := OpenDocument(out)
	if err != nil {
		t.Fatalf("OpenDocument(copy): %v", err)
	}
	if reopened.TotalParas() != d.TotalParas() {
		t.Errorf("copy has %d paragraphs, original has %d", reopened.TotalParas(), d.TotalParas())
	}
	// An untouched save must preserve every entry byte for byte.
	assertEntriesEqual(t, fixture, out, nil)
}

func TestSave_WritesBackToTheOriginalPath(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work.docx")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(work, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(work)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, fixture, work, nil)
}

func TestOpenDocument_PropagatesOpenErrors(t *testing.T) {
	if _, err := OpenDocument(filepath.Join(t.TempDir(), "missing.docx")); err == nil {
		t.Fatal("OpenDocument on a missing file returned nil error")
	}
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run 'TestOpenDocument|TestParas|TestNotes|TestHasRevisions|TestSave' -v`
Expected: 编译失败，`undefined: OpenDocument`。

- [ ] **Step 5: 实现 `document.go`**

结构：

```go
type Document struct {
	pkg   *Package
	path  string
	doc   []byte  // 当前 document.xml
	paras []Para
	notes []string
}
```

要点：

1. `OpenDocument`：`Open` → `Part(DocumentPart)` → `Scan` → 缓存。扫描失败要包装成带路径的错误。
2. `Paras()` 返回**深拷贝**（`Para` 内含 `Runs []Run` 与 `Breaks []int` 切片，浅拷贝仍会别名——测试专门验这个）。
3. `Notes()`：遍历 `pkg.Names()` 检测 header/footer/footnotes/endnotes/comments；再扫 `paras` 看有无 `SkippedTextBox`。每类给一条人类可读的声明。
4. `HasRevisions()`：任一段 `HasRevisions` 为真即真。
5. `Save()` = `SaveAs(d.path)`。`SaveAs` 直接 `pkg.WriteTo(path)`——未经编辑时必须逐字节保真，测试用 `assertEntriesEqual` 验。
6. 内部再留一个 `rescan()`（不导出）：`Part` → `Scan` → 更新 `doc`/`paras`，供 Task 4 在编辑后调用。

- [ ] **Step 6: 跑测试 + 全量回归**

Run: `go test ./pkg/docx/... -race -count=1`
Expected: 新增 8 个测试 PASS，P1a 既有断言全部 PASS。

- [ ] **Step 7: 验红**

| 移除的最小分支 | 应变红 |
|---|---|
| `Paras()` 的深拷贝（改为直接返回切片） | `TestParas_IsASnapshot` |
| header/footer 的 Notes 检测 | `TestNotes_DeclaresUnreadContent` |
| `HasRevisions` 的聚合 | `TestHasRevisions_DetectsExistingMarks` |

逐条给出实际失败输出。

---

### Task 2: `read.go` —— 大纲

**Files:**
- Create: `pkg/docx/read.go`, `pkg/docx/read_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Document`、`Para.Style`
- Produces:
  ```go
  const DocxOutlineParaThreshold = 200

  type Section struct {
      Heading   string // 标题文本；文档开头无标题的部分为 ""
      Style     string // 例如 "Heading1"
      Level     int    // 由 Style 尾部数字得出；非标题段落所属节为 0
      StartPara int    // 1-based，含标题段本身
      EndPara   int    // 1-based inclusive
      Paras     int
      Words     int
  }
  type Outline struct {
      TotalParas int
      Words      int
      Sections   []Section
      Notes      []string
  }
  func (d *Document) Outline() Outline
  ```

**标题判定**：`Para.Style` 以 `"Heading"` 开头（不区分大小写）且其后为数字，`Level` 取该数字。其余样式一律不算标题——不要试图猜 `"Title"`、`"Subtitle"` 等，猜错会让大纲结构错乱，宁可它们落进正文。

**字数口径**：`strings.Fields` 切分后的词数，对中文会退化为"按空白切"，这在 P1 可接受；在 `Outline` 的 doc comment 里写明该口径，避免上层误以为是字符数。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/read_test.go`：

```go
package docx

import (
	"strings"
	"testing"
)

func TestOutline_BuildsHeadingTree(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	if o.TotalParas != d.TotalParas() {
		t.Errorf("TotalParas = %d, want %d", o.TotalParas, d.TotalParas())
	}

	var headings []string
	for _, s := range o.Sections {
		if s.Heading != "" {
			headings = append(headings, s.Heading)
		}
	}
	want := []string{"Chapter One", "Section 1.1", "Chapter Two", "Section 2.1"}
	if len(headings) != len(want) {
		t.Fatalf("got %d headings %v, want %d %v", len(headings), headings, len(want), want)
	}
	for i := range want {
		if headings[i] != want[i] {
			t.Errorf("heading[%d] = %q, want %q", i, headings[i], want[i])
		}
	}
}

func TestOutline_LevelsComeFromStyle(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	want := map[string]int{
		"Chapter One": 1,
		"Section 1.1": 2,
		"Chapter Two": 1,
		"Section 2.1": 2,
	}
	for _, s := range d.Outline().Sections {
		if s.Heading == "" {
			continue
		}
		if got := want[s.Heading]; got != s.Level {
			t.Errorf("section %q: Level = %d, want %d", s.Heading, s.Level, got)
		}
	}
}

// TestOutline_SectionRangesTileTheDocument pins that every paragraph belongs
// to exactly one section — a gap or an overlap would make `heading` selection
// silently skip or duplicate content.
func TestOutline_SectionRangesTileTheDocument(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	covered := make([]int, o.TotalParas+1)
	for _, s := range o.Sections {
		if s.StartPara < 1 || s.EndPara > o.TotalParas || s.StartPara > s.EndPara {
			t.Fatalf("section %q has invalid range [%d,%d] (total %d)", s.Heading, s.StartPara, s.EndPara, o.TotalParas)
		}
		for i := s.StartPara; i <= s.EndPara; i++ {
			covered[i]++
		}
		if s.Paras != s.EndPara-s.StartPara+1 {
			t.Errorf("section %q: Paras = %d, but range [%d,%d] spans %d",
				s.Heading, s.Paras, s.StartPara, s.EndPara, s.EndPara-s.StartPara+1)
		}
	}
	for i := 1; i <= o.TotalParas; i++ {
		if covered[i] != 1 {
			t.Fatalf("paragraph %d is covered by %d sections, want exactly 1", i, covered[i])
		}
	}
}

func TestOutline_WordCountsSumToTotal(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	sum := 0
	for _, s := range o.Sections {
		sum += s.Words
	}
	if sum != o.Words {
		t.Errorf("section words sum to %d, Outline.Words = %d", sum, o.Words)
	}
	if o.Words == 0 {
		t.Error("Words = 0, want a positive count")
	}
}

// TestOutline_LeadingBodyBecomesAnUnnamedSection pins that content before the
// first heading is not dropped.
func TestOutline_LeadingBodyBecomesAnUnnamedSection(t *testing.T) {
	d, err := OpenDocument(fixture) // structure.docx has no headings at all
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	if len(o.Sections) != 1 {
		t.Fatalf("got %d sections, want 1 for a heading-less document", len(o.Sections))
	}
	s := o.Sections[0]
	if s.Heading != "" || s.Level != 0 {
		t.Errorf("section = %+v, want an unnamed level-0 section", s)
	}
	if s.StartPara != 1 || s.EndPara != o.TotalParas {
		t.Errorf("section range = [%d,%d], want [1,%d]", s.StartPara, s.EndPara, o.TotalParas)
	}
}

func TestOutline_CarriesNotes(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if len(d.Outline().Notes) == 0 {
		t.Error("Outline.Notes is empty, want the header/footer declaration carried through")
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `undefined: Outline` 等。

- [ ] **Step 3: 实现**

算法：线性扫一遍 `paras`；遇到标题段就结束上一节、开新节；文档开头若在第一个标题前有内容，先开一个 `Heading: ""` / `Level: 0` 的节。字数按节累加。`Notes` 直接取 `d.Notes()`。

**边界**：标题段本身计入它所引导的那一节（`StartPara` 指向标题段）。无任何标题的文档产出恰好一个未命名节覆盖全篇。

- [ ] **Step 4-5: 绿 + 验红**

验红项：把"以 Heading 开头且后接数字"的判定改成恒 false → `TestOutline_BuildsHeadingTree` 变红；去掉"开头未命名节"的处理 → `TestOutline_LeadingBodyBecomesAnUnnamedSection` 与 `TestOutline_SectionRangesTileTheDocument` 变红。

---

### Task 3: `read.go` —— 分块读取、游标与 markdown 渲染

**Files:**
- Modify: `pkg/docx/read.go`, `pkg/docx/read_test.go`

**Interfaces:**
```go
type ReadOptions struct {
    StartPara int    // 1-based inclusive；0 = 从头
    EndPara   int    // 1-based inclusive；0 = 到尾
    Heading   string // 非空则限定到该标题节，与 Start/EndPara 互斥
    Runs      bool   // 附带每段的 run 明细
    MaxChars  int    // 正文字符上限；0 = 不限
    Full      bool   // 整篇；超预算时报错
}
type RunView struct {
    Index int
    Text  string
}
type ParaView struct {
    Index    int
    Text     string
    Style    string
    Cell     *CellRef
    Runs     []RunView // 仅 Runs=true 时填充
    Note     string    // 例如 "contains a text box whose content is not shown"
}
type ReadResult struct {
    Markdown      string
    Paras         []ParaView
    NextStartPara int // 0 = 已到末尾
    TotalParas    int
    Notes         []string
}
func (d *Document) Read(opts ReadOptions) (ReadResult, error)
```

**分块规则**（§5.2）：按 `MaxChars` 累计**渲染后**的正文字符数；一旦加入下一段会超限就停，`NextStartPara` 指向下一段。**切点永远落在段落边界**，绝不切开一段。若单独一段就超过 `MaxChars`，仍然完整返回该段（否则永远推进不了），并在 `Notes` 里说明该段超出预算。

**`Full` 的预算**：`Full=true` 且渲染后正文超过 `MaxChars`（未给则用默认 8192）时**返回错误**，提示改用 outline + range。不要降级返回部分内容——§5.1 的静默丢内容正是本设计要消灭的东西。

**markdown 渲染**：
- 标题段按 `Level` 渲染成 `#` 前缀
- 普通段落原样输出，段间空行
- `Para.Breaks` 里记录的 run 位置后插入换行
- 表格：**P1 不还原表格结构**，每个单元格段落单独成段并在 `ParaView.Cell` 上带坐标；在 `Notes` 里声明"表格按单元格逐段呈现，未还原为 markdown 表格"
- 每段前缀 `[para N]` 标记，供 LLM 引用 `para_index`

- [ ] **Step 1: 写失败的测试**

追加到 `read_test.go`：

```go
func TestRead_RangeSelectsInclusiveBounds(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 3, EndPara: 5})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(r.Paras))
	}
	for i, want := range []int{3, 4, 5} {
		if r.Paras[i].Index != want {
			t.Errorf("Paras[%d].Index = %d, want %d", i, r.Paras[i].Index, want)
		}
	}
}

func TestRead_HeadingSelectsWholeSection(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	var want Section
	for _, s := range d.Outline().Sections {
		if s.Heading == "Section 1.1" {
			want = s
		}
	}
	if want.Heading == "" {
		t.Fatal("fixture lacks the Section 1.1 heading")
	}
	r, err := d.Read(ReadOptions{Heading: "Section 1.1"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != want.Paras {
		t.Fatalf("got %d paragraphs, want %d", len(r.Paras), want.Paras)
	}
	if r.Paras[0].Index != want.StartPara {
		t.Errorf("first index = %d, want %d", r.Paras[0].Index, want.StartPara)
	}
}

func TestRead_UnknownHeadingErrors(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{Heading: "No Such Heading"}); err == nil {
		t.Fatal("Read with an unknown heading returned nil error")
	}
}

// TestRead_MaxCharsCutsAtParagraphBoundary is the core chunking guarantee
// (design §5.2): a chunk never splits a <w:p>, and the cursor points at the
// next unread paragraph.
func TestRead_MaxCharsCutsAtParagraphBoundary(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{MaxChars: 200})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.NextStartPara == 0 {
		t.Fatal("NextStartPara = 0, want a cursor (the fixture is far larger than 200 chars)")
	}
	last := r.Paras[len(r.Paras)-1].Index
	if r.NextStartPara != last+1 {
		t.Errorf("NextStartPara = %d, want %d (one past the last returned paragraph)", r.NextStartPara, last+1)
	}
	// Every returned paragraph must be whole: its text equals the document's.
	all := d.Paras()
	for _, pv := range r.Paras {
		var b strings.Builder
		for _, run := range all[pv.Index-1].Runs {
			b.WriteString(run.Text)
		}
		if pv.Text != b.String() {
			t.Errorf("paragraph %d was truncated: %q vs %q", pv.Index, pv.Text, b.String())
		}
	}
}

// TestRead_CursorWalksTheWholeDocumentExactlyOnce is the coverage guarantee
// behind §10's second acceptance criterion.
func TestRead_CursorWalksTheWholeDocumentExactlyOnce(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	seen := make([]int, d.TotalParas()+1)
	next := 1
	for guard := 0; next != 0; guard++ {
		if guard > 1000 {
			t.Fatal("cursor did not terminate")
		}
		r, err := d.Read(ReadOptions{StartPara: next, MaxChars: 150})
		if err != nil {
			t.Fatalf("Read from %d: %v", next, err)
		}
		if len(r.Paras) == 0 {
			t.Fatalf("Read from %d returned no paragraphs but cursor is %d", next, r.NextStartPara)
		}
		for _, pv := range r.Paras {
			seen[pv.Index]++
		}
		next = r.NextStartPara
	}
	for i := 1; i <= d.TotalParas(); i++ {
		if seen[i] != 1 {
			t.Fatalf("paragraph %d was read %d times, want exactly 1", i, seen[i])
		}
	}
}

// TestRead_OversizedParagraphStillAdvances guards against an infinite loop
// when one paragraph alone exceeds the budget.
func TestRead_OversizedParagraphStillAdvances(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{MaxChars: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 1 {
		t.Fatalf("got %d paragraphs, want exactly 1 (the oversized one)", len(r.Paras))
	}
	if r.NextStartPara != 2 {
		t.Errorf("NextStartPara = %d, want 2", r.NextStartPara)
	}
	if len(r.Notes) == 0 {
		t.Error("Notes is empty, want a note that the paragraph exceeds the budget")
	}
}

func TestRead_FullRefusesWhenOverBudget(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	_, err = d.Read(ReadOptions{Full: true, MaxChars: 100})
	if err == nil {
		t.Fatal("Full read over budget returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "outline") {
		t.Errorf("error = %q, want it to point the caller at outline + range", err)
	}
}

func TestRead_RunsIncludesPerRunBreakdown(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 1, EndPara: 1, Runs: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(r.Paras))
	}
	got := r.Paras[0].Runs
	if len(got) != 3 {
		t.Fatalf("got %d runs, want 3", len(got))
	}
	for i, want := range []string{"Hello ", "bold", " world"} {
		if got[i].Text != want {
			t.Errorf("Runs[%d].Text = %q, want %q", i, got[i].Text, want)
		}
		if got[i].Index != i+1 {
			t.Errorf("Runs[%d].Index = %d, want %d", i, got[i].Index, i+1)
		}
	}
}

func TestRead_OmitsRunsUnlessRequested(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 1, EndPara: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras[0].Runs) != 0 {
		t.Errorf("Runs were populated without Runs=true")
	}
}

func TestRead_MarkdownRendersHeadingsAndParaMarkers(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{Heading: "Chapter One"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(r.Markdown, "# Chapter One") {
		t.Errorf("markdown lacks the level-1 heading:\n%s", r.Markdown)
	}
	if !strings.Contains(r.Markdown, "[para ") {
		t.Errorf("markdown lacks para_index markers:\n%s", r.Markdown)
	}
}

func TestRead_TableParagraphsCarryCellCoordinates(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var withCell int
	for _, pv := range r.Paras {
		if pv.Cell != nil {
			withCell++
			if pv.Cell.Table != 1 || pv.Cell.Row < 1 || pv.Cell.Col < 1 {
				t.Errorf("paragraph %d has implausible cell %+v", pv.Index, *pv.Cell)
			}
		}
	}
	if withCell != 4 {
		t.Errorf("%d paragraphs carry cell coordinates, want 4", withCell)
	}
}

func TestRead_HeadingAndRangeAreMutuallyExclusive(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{Heading: "Chapter One", StartPara: 2}); err == nil {
		t.Fatal("Read accepted both Heading and StartPara, want an error")
	}
}
```

- [ ] **Step 2-5**：确认红 → 实现 → 绿 → 验红。

验红项：把"加入下一段会超限就停"改成不停 → `TestRead_MaxCharsCutsAtParagraphBoundary`；去掉"单段超预算仍返回该段"的兜底 → `TestRead_OversizedParagraphStillAdvances`（并会让游标测试死循环，注意 guard）；去掉 `Full` 的超预算拒绝 → `TestRead_FullRefusesWhenOverBudget`。

---

### Task 4: `edit.go` —— 定位、protect 校验与批量编辑

**Files:**
- Create: `pkg/docx/edit.go`, `pkg/docx/edit_test.go`

**Interfaces:**
```go
type Edit struct {
    Para int
    Run  int    // 0 = 未指定
    Find string // "" = 未指定
    Text string
    Op   string // "replace"(默认) | "insert_before" | "insert_after" | "delete"
}
type EditOptions struct {
    Protect []string // 字面量或正则，见下
}
type EditOutcome struct {
    Edit    Edit
    Applied bool
    Before  string
    After   string
    Reason  string // 未应用时的原因
    Warning string // 已应用但有需要提醒的情况
}
type EditResult struct {
    Outcomes []EditOutcome
    Applied  int
}
func (d *Document) Edit(edits []Edit, opts EditOptions) (EditResult, error)
```

**语义（全部来自 §4.2，逐条实现）**：

1. **修订闸门**：`d.HasRevisions()` 为真 → 整批拒绝，返回 error（不是逐条 Reason）。
2. **op × run/find 组合**：`insert_before`/`insert_after` 给了 `run` 或 `find` → 该条 `Applied=false`，`Reason` 说明恒为段落级。`replace`/`delete` 两者皆可。
3. **`run` 与 `find` 互斥**：同时给 → 该条拒绝。
4. **`find` 未命中或命中多次** → 该条拒绝，`Reason` 写明命中次数。**不要猜**。
5. **整段替换的警告**：`replace` 且未给 `run`/`find` 且该段 run 数 > 1 → 仍然应用，但 `Warning` 说明段内格式会被抹平成首个 run 的格式。
6. **protect 校验**（按 op 表）：`replace` 与 `insert_*` 校验，`delete` 不校验但命中时给 `Warning`。规则：若某保护项在 `Before` 中出现而在 `After` 中丢失或被改动，则该条拒绝并在 `Reason` 里指名是哪一项。protect 项先尝试编译为正则；编译失败则按字面量处理（在 doc comment 里写明这一降级）。
7. **before/after 边界**：严格按 §4.2 的表 —— `replace` 的 before 是被替换区间原文；`insert_*` 的 before 为空；`delete` 的 after 为空。
8. **自闭合 run**：目标 run 的 `SelfClosing` 为真 → 拒绝该条（`Apply` 也会拒，但在这里给出可读的原因更好）。
9. **批量**：所有补丁基于**同一次扫描**计算，一次性交给 `Apply`（它内部降序应用）。任一条被拒绝只是不生成补丁，不影响其他条。
10. **编辑后重扫**：`Apply` 成功后 `SetPart` + `rescan()`，使 `d.Paras()` 反映新状态。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/edit_test.go`。测试需自建无修订标记的临时文档（`structure.docx` 含 `w:ins`/`w:del`，会被闸门拒绝），因此提供一个 helper：

```go
package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editableDoc copies outline.docx (which has no revision marks) into a temp
// dir and opens it, so edit tests can mutate freely.
func editableDoc(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "edit.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return d
}

func paraTextAt(t *testing.T, d *Document, idx int) string {
	t.Helper()
	var b strings.Builder
	for _, r := range d.Paras()[idx-1].Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func TestEdit_RefusesDocumentWithExistingRevisions(t *testing.T) {
	d, err := OpenDocument(fixture) // structure.docx has w:ins / w:del
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	_, err = d.Edit([]Edit{{Para: 1, Text: "x"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit on a document with revision marks returned nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "revision") {
		t.Errorf("error = %q, want it to mention revisions", err)
	}
}

func TestEdit_FindReplacesOnlyTheMatch(t *testing.T) {
	d := editableDoc(t)
	before := paraTextAt(t, d, 2)
	if !strings.Contains(before, "Body") {
		t.Fatalf("fixture paragraph 2 = %q, expected it to contain %q", before, "Body")
	}
	res, err := d.Edit([]Edit{{Para: 2, Find: "Body", Text: "BODY"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1; outcome = %+v", res.Applied, res.Outcomes[0])
	}
	after := paraTextAt(t, d, 2)
	if !strings.Contains(after, "BODY") {
		t.Errorf("paragraph 2 = %q, want it to contain BODY", after)
	}
	if strings.Replace(before, "Body", "BODY", 1) != after {
		t.Errorf("more than the match changed:\n before %q\n after  %q", before, after)
	}
}

func TestEdit_FindNotFoundIsRefusedNotGuessed(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Find: "no such text", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 || res.Outcomes[0].Applied {
		t.Fatal("a non-matching find was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "0") && !strings.Contains(res.Outcomes[0].Reason, "not found") {
		t.Errorf("Reason = %q, want it to state the match count", res.Outcomes[0].Reason)
	}
}

func TestEdit_FindMatchingTwiceIsRefused(t *testing.T) {
	d := editableDoc(t)
	// Build a deterministic two-match case.
	if _, err := d.Edit([]Edit{{Para: 2, Text: "dup dup"}}, EditOptions{}); err != nil {
		t.Fatalf("setup Edit: %v", err)
	}
	res, err := d.Edit([]Edit{{Para: 2, Find: "dup", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("an ambiguous find was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "2") {
		t.Errorf("Reason = %q, want it to state that the find matched twice", res.Outcomes[0].Reason)
	}
}

func TestEdit_WholeParagraphReplaceWarnsOnMultiRun(t *testing.T) {
	d := editableDoc(t)
	// The fixture's "Plain bold tail" paragraph is the only multi-run one.
	// Locate it rather than hardcoding an index, so the test fails loudly if
	// the fixture layout changes instead of silently testing a single-run
	// paragraph (which would make the warning assertion vacuous).
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) > 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no multi-run paragraph; this test would be vacuous")
	}

	res, err := d.Edit([]Edit{{Para: target, Text: "flattened"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("whole-paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("multi-run whole-paragraph replace produced no warning about flattened formatting")
	}
}

// TestEdit_SingleRunReplaceDoesNotWarn is the negative half: the warning must
// be specific to multi-run paragraphs, not emitted for every whole-paragraph
// replace.
func TestEdit_SingleRunReplaceDoesNotWarn(t *testing.T) {
	d := editableDoc(t)
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) == 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no single-run paragraph")
	}
	res, err := d.Edit([]Edit{{Para: target, Text: "replaced"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Warning != "" {
		t.Errorf("single-run replace warned unnecessarily: %q", res.Outcomes[0].Warning)
	}
}

// TestEdit_FindSpanningRunsIsRefused pins the P1 limitation: a match crossing
// run boundaries needs coordinated multi-patch editing, which is P2 work.
func TestEdit_FindSpanningRunsIsRefused(t *testing.T) {
	d := editableDoc(t)
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) > 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no multi-run paragraph")
	}
	// "Plain bold tail" — this substring straddles run 1 and run 2.
	res, err := d.Edit([]Edit{{Para: target, Find: "Plain bold", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("a find spanning two runs was applied, want refusal")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "run") {
		t.Errorf("Reason = %q, want it to explain the cross-run limitation", res.Outcomes[0].Reason)
	}
}

func TestEdit_InsertRejectsRunAndFind(t *testing.T) {
	d := editableDoc(t)
	for _, e := range []Edit{
		{Para: 2, Op: "insert_after", Run: 1, Text: "x"},
		{Para: 2, Op: "insert_after", Find: "Body", Text: "x"},
	} {
		res, err := d.Edit([]Edit{e}, EditOptions{})
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if res.Outcomes[0].Applied {
			t.Errorf("%+v was applied, want refusal", e)
		}
	}
}

func TestEdit_InsertAfterAddsTheParagraphBelow(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	original2 := paraTextAt(t, d, 2)

	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_after", Text: "inserted"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total+1 {
		t.Fatalf("TotalParas = %d, want %d", d.TotalParas(), total+1)
	}
	if got := paraTextAt(t, d, 2); got != original2 {
		t.Errorf("paragraph 2 = %q, want the original %q to stay put", got, original2)
	}
	if got := paraTextAt(t, d, 3); got != "inserted" {
		t.Errorf("paragraph 3 = %q, want %q", got, "inserted")
	}
}

// TestEdit_InsertBeforeAddsTheParagraphAbove is a separate test on purpose:
// insert_before anchors at Para.Span.Start while insert_after anchors at
// Para.Span.End, so they are distinct code paths. Testing only one would let
// a mis-wired anchor through unnoticed.
func TestEdit_InsertBeforeAddsTheParagraphAbove(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	original2 := paraTextAt(t, d, 2)

	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_before", Text: "inserted"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total+1 {
		t.Fatalf("TotalParas = %d, want %d", d.TotalParas(), total+1)
	}
	if got := paraTextAt(t, d, 2); got != "inserted" {
		t.Errorf("paragraph 2 = %q, want %q", got, "inserted")
	}
	// The paragraph that was at 2 must have shifted down to 3. Without this
	// assertion an insert_before wired to Span.End would still look right if
	// the caller only checked the new text's presence.
	if got := paraTextAt(t, d, 3); got != original2 {
		t.Errorf("paragraph 3 = %q, want the displaced original %q", got, original2)
	}
}

func TestEdit_DeleteParagraphRemovesIt(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	target := paraTextAt(t, d, 2)
	res, err := d.Edit([]Edit{{Para: 2, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total-1 {
		t.Errorf("TotalParas = %d, want %d", d.TotalParas(), total-1)
	}
	if paraTextAt(t, d, 2) == target {
		t.Error("the deleted paragraph is still present")
	}
}

// TestEdit_ProtectRefusesWhenAProtectedItemIsAltered is §4.2's core boundary.
func TestEdit_ProtectRefusesWhenAProtectedItemIsAltered(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: "v1.2.3", Text: "v1.2.4"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("an edit that altered a protected item was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "v1.2.3") {
		t.Errorf("Reason = %q, want it to name the broken protected item", res.Outcomes[0].Reason)
	}
}

func TestEdit_ProtectAllowsEditsThatPreserveTheItem(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: "shipped", Text: "released"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("a protection-preserving edit was refused: %s", res.Outcomes[0].Reason)
	}
	if !strings.Contains(paraTextAt(t, d, 2), "v1.2.3") {
		t.Error("the protected item did not survive")
	}
}

// TestEdit_DeleteSkipsProtectValidationButWarns is §4.2's explicit carve-out.
func TestEdit_DeleteSkipsProtectValidationButWarns(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Op: "delete"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("delete was refused by protect validation: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("deleting protected content produced no warning")
	}
}

func TestEdit_LiteralProtectItemWhenRegexFails(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "build (beta of the tool"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// "(beta" has an unclosed group, so regexp.Compile fails and the item
	// must fall back to a literal match. Note "[unknown]" would NOT work here:
	// it is a VALID regex (a character class), so it would never exercise the
	// fallback path.
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: "(beta", Text: "stable"}},
		EditOptions{Protect: []string{"(beta"}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Error("an edit destroying a literal protected item was applied")
	}
}

func TestEdit_BatchAppliesAllIndependentEdits(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{
		{Para: 2, Find: "Body", Text: "AAA"},
		{Para: 3, Find: "Body", Text: "BBB"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("Applied = %d, want 2 (%+v)", res.Applied, res.Outcomes)
	}
	if !strings.Contains(paraTextAt(t, d, 2), "AAA") || !strings.Contains(paraTextAt(t, d, 3), "BBB") {
		t.Error("not all batched edits landed")
	}
}

func TestEdit_OneRefusalDoesNotBlockOthers(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{
		{Para: 2, Find: "no such text", Text: "x"},
		{Para: 3, Find: "Body", Text: "OK"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1", res.Applied)
	}
	if res.Outcomes[0].Applied || !res.Outcomes[1].Applied {
		t.Errorf("wrong outcome pattern: %+v", res.Outcomes)
	}
}

func TestEdit_OutOfRangeParaIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 99999, Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 || res.Outcomes[0].Reason == "" {
		t.Error("an out-of-range paragraph index was not cleanly refused")
	}
}

func TestEdit_RunAndFindTogetherIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Run: 1, Find: "Body", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Error("run and find given together were accepted")
	}
}

// TestEdit_SavePersistsAndPreservesUntouchedEntries ties the layer back to the
// byte-fidelity guarantee.
func TestEdit_SavePersistsAndPreservesUntouchedEntries(t *testing.T) {
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "edit.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Edit([]Edit{{Para: 2, Find: "Body", Text: "EDITED"}}, EditOptions{}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, outlineFixture, p, map[string]bool{DocumentPart: true})

	reopened, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !strings.Contains(paraTextAt(t, reopened, 2), "EDITED") {
		t.Error("the edit did not survive save/reopen")
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `undefined: Edit` 等。

- [ ] **Step 3: 实现**

流程：

1. 闸门：`HasRevisions()` → 返回 error。
2. 对每条 edit 依次做**参数校验**（范围、互斥、op 合法性），失败的记 `Outcome{Applied:false, Reason:...}` 且不生成补丁。
3. **定位**：`run` → 取该 run；`find` → 在段落全文（各 run `Text` 拼接）上找子串，统计命中次数，恰好 1 次时**映射回所在 run 与该 run 内的子串**（注意：`find` 可能跨 run，此时 **P1 拒绝该条**并说明"匹配跨越多个 run，请改用 run 参数或缩小匹配"——跨 run 替换需要多补丁协同，留给 P2）。
4. **构造 before/after** 并按 §4.2 的表做 protect 校验。
5. **生成补丁**：`replace` 走 `PatchRun`（run/find）或整段 raw 替换；`insert_*` 与段落 `delete` 走 `PatchRawSpan`。段落级插入需要构造合法的 `<w:p>` —— 用最小形态 `<w:p><w:r><w:t>ESCAPED</w:t></w:r></w:p>`，文本必须转义（注意 raw 补丁不会替你转义，**这里要自己转义正文，只让标签是 raw 的**）。
6. 一次 `ApplyToPart` 提交所有补丁，成功后 `rescan()`。
7. 返回逐条 `Outcome`。

**跨 run 的 find 必须拒绝而不是猜** —— 这条要写进 doc comment 与 `Reason`。

- [ ] **Step 4-5: 绿 + 验红**

验红项（逐条最小分支）：

| 移除 | 应变红 |
|---|---|
| 修订闸门 | `TestEdit_RefusesDocumentWithExistingRevisions` |
| find 命中数 != 1 的拒绝 | `TestEdit_FindNotFoundIsRefusedNotGuessed`、`TestEdit_FindMatchingTwiceIsRefused` |
| protect 的 after 校验 | `TestEdit_ProtectRefusesWhenAProtectedItemIsAltered` |
| delete 跳过 protect 的分支 | `TestEdit_DeleteSkipsProtectValidationButWarns` |
| 正则编译失败降级为字面量 | `TestEdit_LiteralProtectItemWhenRegexFails` |
| `insert_*` 拒绝 run/find | `TestEdit_InsertRejectsRunAndFind` |

- [ ] **Step 6: 全量回归 + 基准**

Run: `go test ./pkg/docx/... -race -count=1 && go test ./pkg/docx/... -bench=. -benchmem -run='^$'`
Expected: 全绿；`BenchmarkApply` 仍在 ~200µs / ~2005 allocs 量级（本任务不应触及它）。

---

## 完成标准

1. `go test ./pkg/docx/... -race` 全绿，含 P1a 的全部断言与本阶段新增测试。
2. 每个新行为都有"移除该最小分支即失败"的验红证据。
3. 两条保真测试继续通过；编辑后保存仍只有 `word/document.xml` 变化。
4. `go vet`（含 `GOOS=windows`）/ `go build ./...` / `gofmt` 全清，`go.mod` 未新增依赖。
5. §4.1 的三种输出形态（outline / range+heading / full）与 §4.2 的四个 op 全部可通过 `Document` 的 API 达成，且 §4.2 的 before/after 与 protect 语义表逐条落实。

P1c（`pkg/tools/builtin/docx.go` 工具封装、`document-editor` profile、三个 SKILL.md）在 P1b 通过后另行规划。
