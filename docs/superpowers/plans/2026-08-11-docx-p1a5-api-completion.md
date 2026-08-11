# DOCX P1a.5: API 缺口补齐 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐 P1a 全分支审查指出的 API 缺口，使 P1b（`docx_read` / `docx_edit`）能够实现设计文档 §4.1/§4.2 承诺的全部能力，而不必绕路。

**Architecture:** 不新增文件。全部改动落在既有的 `scan.go`（扫描期多采集几类信息）与 `splice.go`（新增 raw 补丁类型 + 一个读改写辅助）。扫描仍是单遍 token 扫描，绝不构建 DOM；补丁仍是原始字节流上的区间替换。

**Tech Stack:** Go 1.26.1 标准库。

前置：`docs/superpowers/plans/2026-08-11-docx-p1a-byte-splice-core.md`（已完成并验收）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §4.1、§4.2、§5.4。

## Global Constraints

- **零外部 Go 依赖**：不得向 `go.mod` 添加任何 require。只用标准库。
- **绝不 DOM 重建**：`encoding/xml` 只作 token 扫描器。
- **不得破坏既有保真保证**：`TestWriteTo_UntouchedPartsAreByteIdentical` 与 `TestFidelity_SingleWordEditKeepsEverythingElseIdentical` 必须继续通过，且 P1a 的全部 66 项断言必须继续通过。
- **每项改动都要有"移除该行为即失败"的测试**。验红时只移除**被测中的那一个最小分支**，不要删掉整块功能——P1a 阶段两次假绿都源于验红过粗。
- **不自动提交**：提交由用户在里程碑处口头触发。
- 命名空间判定复用既有的 `isWordElement`，容忍列表为 `{"", "w", WordprocessingML URI}`。
- **测试命令**：`go test ./pkg/docx/... -race -count=1`、`go vet ./pkg/docx/... && GOOS=windows go vet ./pkg/docx/...`、`go build ./...`、`gofmt -l pkg/docx`。

---

## 缺口来源对照

每项都对应设计文档的一条明确要求，不是臆想的通用化：

| # | 缺口 | 设计文档依据 | 不补的后果 |
|---|---|---|---|
| 1 | 无 raw XML 补丁 | §4.2 的 `insert_before`/`insert_after`/段落级 `delete` | **硬阻塞**：四个 op 有三个无法表达 |
| 2 | `Para` 无样式名 | §4.1 outline 的标题树、`heading` 参数 | 需对每段二次解析，等于全文再扫一遍 |
| 3 | `Para` 无单元格坐标 | §4.1「输出里标注所属单元格坐标」 | 承诺的输出字段做不出来 |
| 4 | `Run` 无 `<w:r>` 区间 | §4.2 run 级 `delete` | 只能清空 `<w:t>`，残留空 run |
| 5 | 无修订标记信号 | §4.1「检测到已含 `w:ins`/`w:del` 即拒绝编辑」 | 只能靠 `bytes.Contains` 猜 |
| 6 | 无读改写辅助 | 审查 I5（`Part` 返回陈旧别名） | 批次间静默丢更新 |
| 7 | `<w:br>`/`<w:tab>` 不可见 | §4.1 输出 markdown | 换行丢失，markdown 与原文不符 |
| 8 | 文本框段落被并入外层段落 | 审查 I6 | `find` 命中两次而拒绝编辑任何含文本框的段落 |

---

### Task 1: `scan.go` —— 扫描期补齐段落与 run 的元信息

**Files:**
- Modify: `pkg/docx/scan.go`
- Test: `pkg/docx/scan_test.go`

**Interfaces:**
- Consumes: 既有 `Span` / `isWordElement` / `prevOffset` 扫描循环
- Produces（新增字段，既有字段一律不动）:
  ```go
  type Run struct {
      // ... 既有字段不变 ...
      Elem       Span // <w:r> 元素整体的字节区间（缺口 4）
      InInsertion bool // 该 run 位于 <w:ins> 内（缺口 5）
  }
  type Para struct {
      // ... 既有字段不变 ...
      Style    string   // <w:pPr><w:pStyle w:val="..."/> 的 val，无则空串（缺口 2）
      Cell     *CellRef // 非 nil 表示在表格单元格内（缺口 3）
      HasRevisions bool // 段内存在 <w:ins> 或 <w:del>（缺口 5）
      Breaks   []int    // 各 run 之后的换行位置，见下（缺口 7）
  }
  type CellRef struct {
      Table int // 1-based，文档内第几张表
      Row   int // 1-based
      Col   int // 1-based
  }
  ```

**关于缺口 7 的设计决定**：不要把 `<w:br/>` / `<w:tab/>` 塞进 `Run.Text`——那会破坏"`Text` 解码自 `Content` 区间"这条不变式（P1a 的全局验证正是靠它）。改为在 `Para` 上记录 `Breaks []int`：值为 run 索引（1-based），表示"该 run 之后存在一个 `<w:br/>`"。P1b 渲染 markdown 时据此插入换行。`<w:tab/>` 同理并入（用负数索引区分过于晦涩，故本任务只处理 `<w:br/>`，`<w:tab/>` 留待 P1b 若确有需要再议——在报告中说明这一取舍）。

**关于缺口 8 的策略决定**：`w:txbxContent`（文本框正文）子树**整体跳过**——其中的 `<w:p>` 不计入 `Para.Index`，其中的 run 不被索引。理由：(a) Word 的真实序列化把同一段文字同时写进 `<mc:Choice>` 和 `<mc:Fallback>`，索引进来会让同一文本出现两次，进而使 §4.2「`find` 命中多次时报错」拒绝编辑任何含文本框的段落；(b) 与 P1 明确不支持页眉页脚的立场一致。`Scan` 需在返回值里让调用方能知道"跳过了内容"，故 `Para` 增加：

```go
  // SkippedTextBox reports that a <w:txbxContent> subtree inside this
  // paragraph was skipped, so the paragraph's runs do not cover all text a
  // reader sees. docx_read must say so in its output rather than silently
  // presenting a partial paragraph.
  SkippedTextBox bool
```

- [ ] **Step 1: 写失败的测试**

在 `pkg/docx/scan_test.go` 末尾追加：

```go
// TestScan_ParagraphStyle pins that <w:pStyle w:val="..."/> is captured, which
// §4.1's heading outline depends on.
func TestScan_ParagraphStyle(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Title</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>body</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(paras))
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want %q", paras[0].Style, "Heading1")
	}
	if paras[1].Style != "" {
		t.Errorf("paras[1].Style = %q, want empty", paras[1].Style)
	}
}

// TestScan_CellCoordinates pins §4.1's "标注所属单元格坐标" requirement.
func TestScan_CellCoordinates(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>before</w:t></w:r></w:p>` +
		`<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>r1c1</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>r1c2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>r2c1</w:t></w:r></w:p></w:tc></w:tr>` +
		`</w:tbl>` +
		`<w:p><w:r><w:t>after</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []struct {
		text string
		cell *CellRef
	}{
		{"before", nil},
		{"r1c1", &CellRef{Table: 1, Row: 1, Col: 1}},
		{"r1c2", &CellRef{Table: 1, Row: 1, Col: 2}},
		{"r2c1", &CellRef{Table: 1, Row: 2, Col: 1}},
		{"after", nil},
	}
	if len(paras) != len(want) {
		t.Fatalf("got %d paragraphs, want %d", len(paras), len(want))
	}
	for i, w := range want {
		got := paraText(paras[i])
		if got != w.text {
			t.Fatalf("paras[%d] text = %q, want %q", i, got, w.text)
		}
		switch {
		case w.cell == nil && paras[i].Cell != nil:
			t.Errorf("paras[%d] (%q): Cell = %+v, want nil", i, w.text, *paras[i].Cell)
		case w.cell != nil && paras[i].Cell == nil:
			t.Errorf("paras[%d] (%q): Cell = nil, want %+v", i, w.text, *w.cell)
		case w.cell != nil && *paras[i].Cell != *w.cell:
			t.Errorf("paras[%d] (%q): Cell = %+v, want %+v", i, w.text, *paras[i].Cell, *w.cell)
		}
	}
}

// TestScan_RunElemSpanCoversWholeRun pins that Run.Elem delimits the entire
// <w:r> element, which §4.2's run-level delete needs in order to remove the
// run instead of leaving an empty one behind.
func TestScan_RunElemSpanCoversWholeRun(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 || len(paras[0].Runs) != 1 {
		t.Fatalf("got %d paragraphs / %d runs, want 1/1", len(paras), len(paras[0].Runs))
	}
	got := string(doc[paras[0].Runs[0].Elem.Start:paras[0].Runs[0].Elem.End])
	want := `<w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r>`
	if got != want {
		t.Errorf("Elem span = %q, want %q", got, want)
	}
}

// TestScan_RevisionSignals pins §4.1's recommended P1 policy input: callers
// must be able to detect existing revision marks without scanning bytes
// themselves.
func TestScan_RevisionSignals(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>plain</w:t></w:r></w:p>` +
		`<w:p><w:ins w:id="1"><w:r><w:t>added</w:t></w:r></w:ins></w:p>` +
		`<w:p><w:del w:id="2"><w:r><w:delText>gone</w:delText></w:r></w:del><w:r><w:t>kept</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
	if paras[0].HasRevisions {
		t.Error("paras[0] (plain): HasRevisions = true, want false")
	}
	if !paras[1].HasRevisions {
		t.Error("paras[1] (w:ins): HasRevisions = false, want true")
	}
	if !paras[2].HasRevisions {
		t.Error("paras[2] (w:del): HasRevisions = false, want true")
	}
	if len(paras[1].Runs) != 1 || !paras[1].Runs[0].InInsertion {
		t.Errorf("paras[1] run InInsertion = false, want true")
	}
	if len(paras[2].Runs) != 1 || paras[2].Runs[0].InInsertion {
		t.Errorf("paras[2] kept-run InInsertion = true, want false")
	}
}

// TestScan_BreaksRecordRunPositions pins §4.1's markdown output need: a <w:br/>
// between runs must be recoverable, without polluting Run.Text (which must stay
// exactly the decoding of Run.Content).
func TestScan_BreaksRecordRunPositions(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>line1</w:t></w:r><w:r><w:br/></w:r><w:r><w:t>line2</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if len(paras[0].Breaks) != 1 || paras[0].Breaks[0] != 1 {
		t.Errorf("Breaks = %v, want [1] (a break after run 1)", paras[0].Breaks)
	}
	// Run.Text must remain the pure decoding of Run.Content.
	for _, r := range paras[0].Runs {
		if strings.ContainsAny(r.Text, "\n\r") {
			t.Errorf("run %d Text = %q: break leaked into Text", r.Index, r.Text)
		}
	}
}

// TestScan_TextBoxContentIsSkipped pins the P1 policy decision: <w:txbxContent>
// subtrees are not indexed, and the containing paragraph says so.
func TestScan_TextBoxContentIsSkipped(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>before</w:t></w:r>` +
		`<w:r><w:drawing><wps:txbx><w:txbxContent>` +
		`<w:p><w:r><w:t>inside box</w:t></w:r></w:p>` +
		`</w:txbxContent></wps:txbx></w:drawing></w:r>` +
		`<w:r><w:t>after</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>next</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 2 {
		var got []string
		for _, p := range paras {
			got = append(got, paraText(p))
		}
		t.Fatalf("got %d paragraphs %q, want 2 (text-box paragraph must not be indexed)", len(paras), got)
	}
	if got := paraText(paras[0]); got != "beforeafter" {
		t.Errorf("paras[0] text = %q, want %q (box text excluded, siblings kept)", got, "beforeafter")
	}
	if !paras[0].SkippedTextBox {
		t.Error("paras[0].SkippedTextBox = false, want true")
	}
	if got := paraText(paras[1]); got != "next" {
		t.Errorf("paras[1] text = %q, want %q", got, "next")
	}
	if paras[1].SkippedTextBox {
		t.Error("paras[1].SkippedTextBox = true, want false")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run 'TestScan_(ParagraphStyle|CellCoordinates|RunElemSpan|RevisionSignals|Breaks|TextBox)' -v`
Expected: 编译失败，报 `paras[0].Style undefined`、`undefined: CellRef`、`r.Elem undefined` 等。

- [ ] **Step 3: 实现**

在 `scan.go` 中：

1. 给 `Run` 增加 `Elem Span` 与 `InInsertion bool`，给 `Para` 增加 `Style string`、`Cell *CellRef`、`HasRevisions bool`、`Breaks []int`、`SkippedTextBox bool`，并新增 `CellRef` 类型。每个新字段都要写 doc comment 说明用途与来源。
2. 扫描循环中新增状态（全部与既有 `prevOffset` 滚动机制一致）：
   - `tblIndex`、`rowIndex`、`colIndex`：进入 `tbl` 时 `tblIndex++` 并重置行列；进入 `tr` 时 `rowIndex++`、`colIndex = 0`；进入 `tc` 时 `colIndex++`。段落开启时若 `tableDepth > 0` 则记 `Cell`。
   - `runStart`：进入 `r` 时用 `prevOffset` 记开始，离开 `r` 时用 `offset` 记结束，填入该 run 的 `Elem`。注意一个 `<w:r>` 可能含 0 个或多个 `<w:t>`——`Elem` 应挂到该 `<w:r>` 内产生的每个 run 上。
   - `insDepth`：进入 `ins` 时 `++`、离开时 `--`；run 生成时 `InInsertion = insDepth > 0`。段落内出现 `ins` 或 `del` 即置 `HasRevisions`。
   - `pStyle`：在 `pPr` 内遇到 `pStyle` 时读取 `w:val` 属性（属性名判定同样走命名空间容忍）。
   - `br`：遇到 `br` 时把当前段落已产生的 run 数追加进 `Breaks`。
   - `txbxDepth`：进入 `txbxContent` 时 `++`、离开时 `--`。**`txbxDepth > 0` 期间，`p` / `t` / `r` 的开启与关闭一律不改变扫描器状态**（既不新建段落也不索引 run），并把当前段落标记 `SkippedTextBox = true`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/docx/... -race -count=1 -v`
Expected: 新增 6 个测试全部 PASS，P1a 既有 66 项断言继续 PASS。

若 `TestScan_TextBoxContentIsSkipped` 报段落数为 3，说明 `txbxDepth` 只挡住了 run 而没挡住 `p` 的开启/关闭；若报 `paras[0]` 文本是 `"before"`，说明 `</w:p>` 的关闭在 txbx 内被误触发。

- [ ] **Step 5: 验红（逐项）**

对下列每一项，只注释掉那一个最小分支，确认对应测试变红，然后恢复：

| 移除的分支 | 应变红的测试 |
|---|---|
| `pStyle` 的 `w:val` 读取 | `TestScan_ParagraphStyle` |
| `colIndex++` | `TestScan_CellCoordinates` |
| `Elem` 的赋值 | `TestScan_RunElemSpanCoversWholeRun` |
| `insDepth` 的自增 | `TestScan_RevisionSignals` |
| `Breaks` 的追加 | `TestScan_BreaksRecordRunPositions` |
| `txbxDepth > 0` 的守卫 | `TestScan_TextBoxContentIsSkipped` |

在报告中逐行给出实际的失败输出。**不要用"删掉整个功能"的粗粒度验红**。

- [ ] **Step 6: vet / build / fmt**

Run: `go vet ./pkg/docx/... && GOOS=windows go vet ./pkg/docx/... && go build ./... && gofmt -l pkg/docx`
Expected: 全部无输出。

---

### Task 2: `splice.go` —— raw 补丁与读改写辅助

**Files:**
- Modify: `pkg/docx/splice.go`
- Test: `pkg/docx/splice_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Para.Span`、`Run.Elem`
- Produces:
  ```go
  // Patch 增加：
  //   Raw bool  —— true 时 NewText 按原样 splice，不做 XML 转义
  func PatchRawSpan(documentXML []byte, s Span, rawXML string) Patch
  func (p *Package) ApplyToPart(name string, patches []Patch) error
  ```

**关于 `Raw` 的安全约束**：raw 补丁绕过转义，是唯一能写出畸形 XML 的入口，所以 `Apply` 必须在应用后校验结果良构。P1b 只用它插入/删除整段 `<w:p>`，不用于任意文本。

- [ ] **Step 1: 写失败的测试**

在 `pkg/docx/splice_test.go` 末尾追加：

```go
// TestApply_RawPatchIsNotEscaped pins §4.2's paragraph-level insert: a whole
// <w:p> subtree must land verbatim, not as &lt;w:p&gt;.
func TestApply_RawPatchIsNotEscaped(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	newPara := `<w:p><w:r><w:t>two</w:t></w:r></w:p>`
	at := Span{Start: paras[0].Span.End, End: paras[0].Span.End}
	out, err := Apply(doc, []Patch{PatchRawSpan(doc, at, newPara)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(out), "&lt;w:p&gt;") {
		t.Fatalf("raw XML was escaped: %s", out)
	}
	got, err := Scan(out)
	if err != nil {
		t.Fatalf("Scan(out): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d paragraphs after insert, want 2", len(got))
	}
	if paraText(got[1]) != "two" {
		t.Errorf("inserted paragraph text = %q, want %q", paraText(got[1]), "two")
	}
}

// TestApply_RawPatchCanDeleteAParagraph pins §4.2's paragraph-level delete.
func TestApply_RawPatchCanDeleteAParagraph(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	out, err := Apply(doc, []Patch{PatchRawSpan(doc, paras[0].Span, "")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := Scan(out)
	if err != nil {
		t.Fatalf("Scan(out): %v", err)
	}
	if len(got) != 1 || paraText(got[0]) != "two" {
		t.Fatalf("after delete got %d paragraphs, first = %q; want 1 / %q", len(got), paraText(got[0]), "two")
	}
}

// TestApply_RawPatchRejectsMalformedResult guards the one escaping bypass in
// the package: a raw patch that produces invalid XML must be refused, not
// written out for Word to choke on.
func TestApply_RawPatchRejectsMalformedResult(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	at := Span{Start: paras[0].Span.End, End: paras[0].Span.End}
	_, err = Apply(doc, []Patch{PatchRawSpan(doc, at, `<w:p><w:r>`)})
	if err == nil {
		t.Fatal("Apply accepted a raw patch producing malformed XML, want error")
	}
	if !strings.Contains(err.Error(), "well-formed") {
		t.Errorf("error = %q, want it to mention well-formedness", err)
	}
}

// TestApplyToPart_ReadModifyWriteAvoidsLostUpdate pins the fix for the stale
// alias hazard: two sequential batches must both survive.
func TestApplyToPart_ReadModifyWriteAvoidsLostUpdate(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatal("target paragraph not found")
	}
	if err := pkg.ApplyToPart(DocumentPart, []Patch{PatchRun(doc, p.Runs[0], "Howdy ")}); err != nil {
		t.Fatalf("ApplyToPart batch 1: %v", err)
	}

	// Re-read and re-scan, as P1b must after every write-back (design §5.4).
	doc2, _ := pkg.Part(DocumentPart)
	paras2, err := Scan(doc2)
	if err != nil {
		t.Fatalf("Scan after batch 1: %v", err)
	}
	p2, ok := findPara(paras2, "Howdy bold world")
	if !ok {
		t.Fatalf("batch 1 was lost; paragraph text is %q", paraText(paras2[p.Index-1]))
	}
	if err := pkg.ApplyToPart(DocumentPart, []Patch{PatchRun(doc2, p2.Runs[2], " planet")}); err != nil {
		t.Fatalf("ApplyToPart batch 2: %v", err)
	}

	doc3, _ := pkg.Part(DocumentPart)
	paras3, err := Scan(doc3)
	if err != nil {
		t.Fatalf("Scan after batch 2: %v", err)
	}
	if _, ok := findPara(paras3, "Howdy bold planet"); !ok {
		t.Errorf("both batches should survive; got %q", paraText(paras3[p.Index-1]))
	}
}

// TestApplyToPart_UnknownPartErrors keeps ApplyToPart honest about the same
// contract SetPart enforces.
func TestApplyToPart_UnknownPartErrors(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := pkg.ApplyToPart("word/nope.xml", nil); err == nil {
		t.Fatal("ApplyToPart on an unknown part returned nil, want error")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run 'TestApply_Raw|TestApplyToPart' -v`
Expected: 编译失败，报 `undefined: PatchRawSpan`、`pkg.ApplyToPart undefined`。

- [ ] **Step 3: 实现**

在 `splice.go` 中：

1. `Patch` 增加 `Raw bool` 字段并写 doc comment，说明它是包内唯一绕过转义的入口、仅供段落级结构操作使用。
2. `PatchRawSpan(documentXML []byte, s Span, rawXML string) Patch` 构造 `Raw: true` 的补丁，`Content` 设为 `s`，`Old` 按既有约定填入 `documentXML[s.Start:s.End]`，`TagSpan` 留零值。
3. `Apply` 中：`Raw` 为真时跳过 `escapeXMLText`，直接用原文；同时**跳过 preserve 属性重写**（`TagSpan` 为零值，重写无意义且会越界）。
4. `Apply` 末尾：若本批含任何 `Raw` 补丁，对最终结果做一次良构校验（走一遍 `xml.Decoder` 的 token 循环，遇错则返回带 `"well-formed"` 字样的错误）。非 raw 批次不做这次校验，以免给常规路径增加一次全文扫描。
5. `ApplyToPart(name string, patches []Patch) error`：取出该部件、调用 `Apply`、再 `SetPart` 写回，任一步出错即返回。doc comment 要写明这是**推荐的调用方式**，因为 `Part` 返回的切片在 `SetPart` 之后会变陈旧。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/docx/... -race -count=1 -v`
Expected: 新增 5 个测试 PASS，既有全部 PASS。

- [ ] **Step 5: 验红**

| 移除的分支 | 应变红的测试 |
|---|---|
| `Raw` 时跳过转义的判断 | `TestApply_RawPatchIsNotEscaped` |
| raw 批次的良构校验 | `TestApply_RawPatchRejectsMalformedResult` |
| `ApplyToPart` 里的 `SetPart` 调用 | `TestApplyToPart_ReadModifyWriteAvoidsLostUpdate` |

逐条给出实际失败输出。

- [ ] **Step 6: 保真回归 + vet / build / fmt**

Run: `go test ./pkg/docx/... -race -count=1 && go vet ./pkg/docx/... && GOOS=windows go vet ./pkg/docx/... && go build ./... && gofmt -l pkg/docx`
Expected: 全绿；特别确认 `TestWriteTo_UntouchedPartsAreByteIdentical` 与 `TestFidelity_SingleWordEditKeepsEverythingElseIdentical` 仍通过。

Run: `go test ./pkg/docx/... -bench=. -benchmem -run='^$'`
Expected: `BenchmarkApply` 与 P1a 收尾时的量级相当（约 200μs / 611KB / 2005 allocs）。若显著劣化，说明良构校验被错误地加到了非 raw 路径上。

---

## 完成标准

1. `go test ./pkg/docx/... -race` 全绿，含 P1a 的 66 项断言与本阶段新增的 11 个测试。
2. 每个新行为都有一条"移除该最小分支即失败"的验红证据。
3. 两条保真测试继续通过，`BenchmarkApply` 无显著劣化。
4. `go vet`（含 `GOOS=windows`）/ `go build ./...` / `gofmt` 全清，`go.mod` 未新增依赖。
5. 设计文档 §4.2 的四个 op **全部可表达**：`replace`（run/find 级，已有）、`insert_before` / `insert_after` / `delete`（段落级，经 `PatchRawSpan` + `Para.Span`）、run 级 `delete`（经 `Run.Elem`）。
