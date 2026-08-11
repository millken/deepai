# DOCX P2b: `track_changes` Word 原生修订标记 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `docx_edit` 能以 Word 原生修订标记（`w:ins` / `w:del`）写入改动，用户在 Word 里逐条接受/拒绝。这是润色场景的关键 UX，也是设计 §1.1 第 4 条与 §10 P2 的内容。

**Architecture:** 全部新逻辑放进新文件 `pkg/docx/revision.go`；`edit.go` 只在四个 planner 里各加一个分支点，语义字段（before/after/warning/reason）完全不变 —— 修订模式只改**产出的 patch**。

**Tech Stack:** Go 1.26.1 标准库。

前置：P1 全部 + P2a（`6dc8b4e`，已在 main）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §1.1 第 4 条、§4.2 的 `track_changes` 小节。

## Global Constraints

- **零外部 Go 依赖**：`go.mod` 不得改动。
- **绝不 DOM 重建**：所有写入仍是字节区间替换。
- **P1/P2a 的全部保证必须继续成立**，含两条保真门与 `docx_format` 的"正文不改"。
- **未开启 `track_changes` 时行为必须逐字节不变** —— 有测试钉住：同一批编辑在关闭时产出的 `document.xml` 与本阶段之前完全相同。
- **验红必须最小分支**。P1/P2a 累计七次栽在验红过粗或"成对分支只测一半"上。
- **不自动提交**。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`。

---

## 规划阶段发现的设计冲突（必须先解决，否则实现到一半必炸）

`Edit()` 开头有一道闸门：

```go
if d.HasRevisions() {
    return EditResult{}, fmt.Errorf("docx: refusing to edit a document that already contains revision marks ...")
}
```

`HasRevisions()` 读的是**当前**段落缓存，而每次编辑后 `rescan()` 都会刷新它。于是开启 `track_changes` 后：

1. 第 1 块编辑 → 产生 `w:ins`/`w:del` → `rescan()` → `HasRevisions()` 变 true
2. 第 2 块编辑 → **被自己刚写下的标记挡住**

而分块润色正是 `track_changes` 的主要用例。**这道闸门原本的用意**是"不要往语义不明的、别人留下的修订上叠加"，不是"不许有任何修订"。

**解法**：`Document` 记住**打开时**是否已有修订，闸门改为只看这一位。

- `document.go` 增加不导出的 `hadRevisionsAtOpen bool`，在 `OpenDocument` 里由首次 `Scan` 的结果填充，**`rescan()` 不得更新它**。
- `Edit()` 的闸门改用它。
- `HasRevisions()` 的语义保持不变（当前状态），因为 `docx_read` 与 `format.go` 都在用它做别的判断 —— **不要改那个方法的含义**，只是不再用它做闸门。

**跨会话续跑不在本阶段范围**：重新打开一份已含自己修订的文档仍会被拒。错误信息必须说清这一点并给出可操作的出路（在 Word 里接受或拒绝现有修订后再继续），不要让用户以为是 bug。这条要在 `Notes`/错误文案里写明，并记进设计文档。

---

## OOXML 形状（照此实现）

夹具 `structure.docx` 里已有真实样例，且用户已确认 Word 打开无修复提示：

```xml
<w:ins w:id="101" w:author="fixture" w:date="2026-01-01T00:00:00Z"><w:r><w:t>inserted</w:t></w:r></w:ins>
<w:del w:id="102" w:author="fixture" w:date="2026-01-01T00:00:00Z"><w:r><w:delText>deleted</w:delText></w:r></w:del>
```

要点：

1. **`w:id` 必须全文唯一**。实现前先扫一遍 `document.xml` 里已有的 `w:id="N"`，从 `max+1` 开始递增。
2. **删除的文本必须从 `<w:t>` 改写成 `<w:delText>`**，原样保留会让 Word 把它当作仍然存在的正文。
3. **`w:date` 会让输出非确定**。`EditOptions` 要能注入时钟（`Now func() time.Time`，为 nil 时用 `time.Now`），否则测试无法断言字节。
4. **必须保留原 run 的格式**。构造 del/ins 时**克隆原 `<w:r>` 元素**再替换其文本节点，而不是新建一个裸 `<w:r>` —— 后者会丢掉 `<w:rPr>` 里的粗体、超链接样式等，用户接受修订后格式就变了。
5. **段落级插入/删除要标记段落标记本身**：在该段的 `<w:pPr><w:rPr>` 里放 `<w:ins .../>` 或 `<w:del .../>`。缺了它，Word 接受修订后段落会与相邻段落合并。

---

## 各 op 的修订形态

| op | 未开启时 | 开启 `track_changes` 后 |
|---|---|---|
| `replace`（run/find） | 改写 `<w:t>` 内容 | 原 run 克隆两份：一份包进 `<w:del>` 且 `<w:t>`→`<w:delText>` 装旧文本，一份包进 `<w:ins>` 装新文本 |
| `replace`（整段） | 逐个 `<w:t>` 改写 | 同上，对该段每个 run 处理 |
| `insert_before`/`insert_after` | 插入新 `<w:p>` | 新 `<w:p>` 的 run 包进 `<w:ins>`，且 `<w:pPr><w:rPr><w:ins/></w:rPr></w:pPr>` |
| `delete`（整段） | 删除 `<w:p>` | **不删**：runs 包进 `<w:del>` 并转 `delText`，段落标记打 `<w:del/>` |
| `delete`（run） | 删除 `<w:r>` 或清空 | 包进 `<w:del>` 并转 `delText` |

`find` 的情形要拆成四段：`<w:r>前缀</w:r>` + `<w:del>命中</w:del>` + `<w:ins>新文本</w:ins>` + `<w:r>后缀</w:r>`，前后缀 run 同样克隆自原 run 以保格式。前缀或后缀为空时不要产出空 run。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/revision.go` | 修订 XML 构造：id 分配、run 克隆、`<w:t>`→`<w:delText>`、段落标记 |
| `pkg/docx/revision_test.go` | |
| `pkg/docx/document.go` | 修改：`hadRevisionsAtOpen` |
| `pkg/docx/edit.go` | 修改：`EditOptions` 加 `TrackChanges`/`Author`/`Now`；闸门改判据；四个 planner 各加一个分支点 |
| `pkg/tools/builtin/docx.go` | 修改：`docx_edit` 增加 `track_changes` / `author` 参数 |
| `.deepai/skills/docx-polish/SKILL.md` | 修改：默认开启修订标记的工作流 |

---

### Task 1: `revision.go` —— 修订 XML 构造

**Files:** Create `pkg/docx/revision.go`, `pkg/docx/revision_test.go`; modify `pkg/docx/document.go`

**Interfaces:**
```go
type revisionCtx struct {
    Author string
    Now    func() time.Time
    nextID int
}
func newRevisionCtx(documentXML []byte, author string, now func() time.Time) *revisionCtx
func (rc *revisionCtx) attrs() string          // ` w:id="N" w:author="A" w:date="..."`
func cloneRunWithText(runElem []byte, newText string, asDelText bool) ([]byte, error)
func (rc *revisionCtx) wrapDel(runElem []byte, oldText string) ([]byte, error)
func (rc *revisionCtx) wrapIns(runElem []byte, newText string) ([]byte, error)
func (rc *revisionCtx) markParagraph(paraElem []byte, inserted bool) ([]byte, error)
```

`newRevisionCtx` 扫描 `documentXML` 中所有 `w:id="N"`，取 `max+1` 作为起点，保证与既有修订不撞号。

- [ ] **Step 1: 写失败的测试**（要点，不是全部）

```go
func TestRevision_IDsStartAboveExistingMax(t *testing.T) {
	doc := []byte(`<w:body><w:ins w:id="7"/><w:del w:id="42"/></w:body>`)
	rc := newRevisionCtx(doc, "A", fixedClock)
	a := rc.attrs()
	if !strings.Contains(a, `w:id="43"`) {
		t.Errorf("attrs = %q, want the first id to be 43 (one past the existing max)", a)
	}
}

func TestRevision_IDsAreUniqueAcrossCalls(t *testing.T) { /* 连取三次，三个不同 id */ }

// 最容易做错的一条：克隆必须保住 <w:rPr>
func TestRevision_CloneKeepsRunProperties(t *testing.T) {
	run := []byte(`<w:r><w:rPr><w:b/><w:color w:val="FF0000"/></w:rPr><w:t>old</w:t></w:r>`)
	got, err := cloneRunWithText(run, "new", false)
	if err != nil { t.Fatal(err) }
	s := string(got)
	if !strings.Contains(s, "<w:b/>") || !strings.Contains(s, `w:val="FF0000"`) {
		t.Errorf("run properties were lost: %s", s)
	}
	if !strings.Contains(s, "<w:t>new</w:t>") { t.Errorf("text not replaced: %s", s) }
	if strings.Contains(s, "old") { t.Errorf("old text survived: %s", s) }
}

func TestRevision_DelTextConversion(t *testing.T) {
	run := []byte(`<w:r><w:t xml:space="preserve"> gone </w:t></w:r>`)
	got, err := cloneRunWithText(run, " gone ", true)
	if err != nil { t.Fatal(err) }
	s := string(got)
	if !strings.Contains(s, "<w:delText") { t.Errorf("w:t was not converted to w:delText: %s", s) }
	if strings.Contains(s, "<w:t ") || strings.Contains(s, "<w:t>") { t.Errorf("a w:t survived: %s", s) }
	if !strings.Contains(s, `xml:space="preserve"`) { t.Error("xml:space was dropped; Word would eat the spaces") }
}

func TestRevision_EscapesNewText(t *testing.T) { /* "A & B" -> "A &amp; B" */ }

func TestRevision_ClockIsInjectable(t *testing.T) { /* 两次构造用同一时钟 -> 字节相同 */ }
```

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

验红项：id 起点计算、`rPr` 克隆、`delText` 转换、`xml:space` 保留、转义、时钟注入。

- [ ] **Step 6: `hadRevisionsAtOpen`**

`document.go` 加不导出字段，`OpenDocument` 填充，`rescan()` **不动它**。加测试：打开一份干净文档 → 编辑产生修订 → `hadRevisionsAtOpen` 仍为 false，而 `HasRevisions()` 为 true。这两者必须能分开，否则闸门无法工作。

---

### Task 2: 接进 `edit.go`

**Files:** Modify `pkg/docx/edit.go`, `pkg/docx/edit_test.go`

- `EditOptions` 增加 `TrackChanges bool`、`Author string`、`Now func() time.Time`。`Author` 为空时用 `"deepai"`。
- 闸门改判 `hadRevisionsAtOpen`，错误文案说明跨会话续跑需先在 Word 里处理现有修订。
- 四个 planner 各加一个分支：`TrackChanges` 为真时构造修订形态的 `PatchRawSpan`，否则走原路径。

**必须有的测试**：

1. **关闭时逐字节不变**：同一批编辑在 `TrackChanges: false` 下产出的 `document.xml`，与不带该字段时完全相同。这条保证新功能没有污染既有路径。
2. 每个 op 开启后产出合法的 `w:ins`/`w:del`，且 `Scan` 后**可见文本符合预期**（`w:delText` 被排除、`w:ins` 内容被计入）—— 这直接复用 P1a 已有的扫描规则，是最好的自洽性检验。
3. **段落删除后段落数不减**（改为标记而非移除），且 `ParaCountChanged` 为 false。
4. 连续两次分块编辑都成功 —— 即闸门冲突确已解决。**这条是本阶段存在的理由，必须有。**
5. 保留原格式：对一个带 `<w:b/>` 的 run 做 replace，del 与 ins 两侧都还带着 `<w:b/>`。

---

### Task 3: 工具层与 skill

**Files:** Modify `pkg/tools/builtin/docx.go`、`docx_test.go`、`.deepai/skills/docx-polish/SKILL.md`

- `docx_edit` schema 增加 `track_changes`（boolean）与 `author`（string）。类型必须校验，不得静默强制转换。
- 结果里回显 `track_changes` 是否生效，让模型能如实告诉用户"改动以修订标记写入，请在 Word 中审阅"。
- `docx-polish` skill：**默认开启** `track_changes`（§4.2 说这是润色首选），并说明用户可要求关闭；完成后提示用户在 Word 里逐条接受/拒绝。

---

## 完成标准

1. `go test ./pkg/docx/... ./pkg/tools/... -race` 全绿；P1/P2a 全部保证继续成立。
2. **关闭 `track_changes` 时输出逐字节不变**（有测试钉住）。
3. 每个新行为有"移除最小分支即失败"的验红证据。
4. 分块场景下连续多次编辑不再被自己写下的修订挡住。
5. `go vet`（含 `GOOS=windows`）/ `go build` / `gofmt` 全清，`go.mod` 未改。
6. **人工验收（用户执行）**：带修订标记的产物在 Word 中打开，修订面板能看到逐条改动，接受后正文正确、格式保留，拒绝后回到原文。

P2c（`docx_write` 新建文档）在 P2b 通过后另行规划。
