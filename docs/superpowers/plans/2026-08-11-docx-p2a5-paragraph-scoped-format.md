# DOCX P2a.5: 段落级排版（补能力缺口）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补上"改某一段的字号/字体"这个能力。用户实测发现它落在两个工具之间，于是 agent 只能退回 bash + python。

**Architecture:** `docx_format` 增加段落范围。有范围时改为**直接格式化**（run 的 `<w:rPr>`、段落的 `<w:pPr>`），无范围时仍改 `styles.xml`。这与 OOXML 的实际做法一致：整篇默认走样式表，个别段落走直接格式。

**Tech Stack:** Go 1.26.1 标准库。

前置：P1 全部 + P2a（`6dc8b4e`）+ P2b（已完成待提交）。

---

## 缺口是怎么产生的（写下来，因为它是一个规划错误而非实现错误）

用户实测时 agent 的推理链如下，**每一步都是对的**：

> `docx_edit` 只能修改文字内容，无法直接修改字体大小。我需要通过 `docx_format` 来设置，**但它是文档级格式**。让我先检查文档的 XML 结构，找到第 2 段的字体大小定义，然后精确修改。

然后它用 bash + python 改了。

根因链：

1. 设计 §4.2 原本给 `docx_edit` 留了 `style` 参数（覆盖段落样式）。
2. P1b 规划时把它**推迟到 P2**，理由写的是"改段落样式属于 §4.3 `docx_format` 的职责，`docx_edit` 管文本、`docx_format` 管样式，这条边界更干净"。
3. 但 §4.3 的 `docx_format` 从一开始就是**"对整篇应用一组排版规则"** —— 它没有、也从未打算有段落范围。
4. 于是"改某一段的格式"**掉在两个工具之间**：一个说不管、一个只做整篇。
5. `docx_edit` 的工具描述还写着 `Paragraph styling is out of scope here; use docx_format.` —— **把模型指向一个同样做不到的工具**。

教训：把一个能力从 A 推迟到 B 时，必须确认 B 真的覆盖它，而不是只确认"B 在概念上更该管这件事"。

---

## Global Constraints

- **零外部 Go 依赖**；`go.mod` 不得改动。
- **绝不 DOM 重建**；所有写入是字节区间替换。
- **P1 / P2a / P2b 的全部保证必须继续成立**，含两条保真门、`docx_format` 的"正文不改"、以及关闭 `track_changes` 时输出逐字节不变。
- **无范围时行为必须逐字节不变** —— 有测试钉住：同一组规则不带范围时产出的 `styles.xml` 与本阶段之前完全相同。
- **验红必须最小分支**。
- **不自动提交**。
- **测试命令**：`go test ./pkg/docx/... ./pkg/tools/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`gofmt -l pkg/docx pkg/tools`。

---

## 落点：整篇 vs 段落范围

OOXML 的两条路本来就不同，实现要分开：

| | 无范围（现状，不改） | 有范围（本次新增） |
|---|---|---|
| 落在 | `word/styles.xml` | `word/document.xml` |
| 字体 / 字号 | `docDefaults/rPrDefault/rPr` | 该段**每个 run** 的 `<w:rPr><w:rFonts>` / `<w:sz>`+`<w:szCs>` |
| 行距 / 对齐 | `docDefaults/pPrDefault/pPr` | 该段的 `<w:pPr><w:spacing>` / `<w:jc>` |
| 机制 | 改样式表默认值 | **直接格式化**（优先级高于样式，正是用户手选文字改字号的效果） |

**只对整篇有意义、给了范围就必须报错的规则**：`template`、`heading_font`、`margins_mm`、`normalize`、`page_numbers`、`rebuild_toc`。报错要说清为什么（模板与页边距是文档级概念；`heading_font` 改的是标题样式定义）。

**单位换算与整篇路径完全一致**（P2a 已实测确认）：`w:sz` 半磅、`w:line` 240 分之一行且需 `w:lineRule="auto"`、`w:sz` 改了必须同步 `w:szCs`。

**run 内已有 `<w:rPr>` 时要合并而非替换** —— 直接覆盖会抹掉粗体、颜色、超链接样式。三种情形各要有测试：run 无 `<w:rPr>`（插入）、有但缺目标属性（追加）、已有目标属性（改值）。同理段落的 `<w:pPr>`。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/format.go` | 修改：`FormatOptions` 加范围；分派整篇 / 段落两条路径 |
| `pkg/docx/format_direct.go` | 新建：直接格式化（run 的 rPr、段落的 pPr） |
| `pkg/docx/format_direct_test.go` | 新建 |
| `pkg/tools/builtin/docx.go` | 修改：`docx_format` 加 `start_para`/`end_para`；修正 `docx_edit` 那句误导性描述 |
| `.deepai/skills/docx-format/SKILL.md` | 修改：说明段落级用法 |
| `docs/DOCX_TOOLS_DESIGN.md` | 修改：§4.3 补段落范围；§4.2 更正 `style` 的推迟说明 |

---

### Task 1: `format_direct.go` —— 直接格式化

**Files:** Create `pkg/docx/format_direct.go`, `pkg/docx/format_direct_test.go`; modify `pkg/docx/format.go`

**Interfaces:**
```go
// FormatOptions 增加：
StartPara int // 1-based inclusive；0 表示整篇
EndPara   int // 1-based inclusive；0 且 StartPara>0 时表示只该一段

// format_direct.go：
func applyDirectRunFormat(documentXML []byte, paras []Para, from, to int, font string, sizePt float64) ([]byte, int, error)
func applyDirectParaFormat(documentXML []byte, paras []Para, from, to int, lineSpacing float64, align string) ([]byte, int, error)
```
返回值中的 `int` 是实际改动的段落数，供 `FormatResult.Applied` 如实汇报。

**空段落**（无 run）在 run 级格式化中跳过，但段落级（行距/对齐）仍适用 —— 在 `Notes` 里说明跳过了几个空段。

- [ ] **Step 1: 写失败的测试**

要覆盖（每条都要有，且成对分支都要测）：

```go
// 1. 只改指定范围，范围外一字不动
func TestDirect_OnlyTheRangeIsTouched(t *testing.T)

// 2. run 已有 rPr 时合并，不抹掉既有格式 —— 最容易做错的一条
func TestDirect_MergesIntoExistingRunProperties(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/><w:color w:val="FF0000"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", 14)
	if err != nil { t.Fatal(err) }
	if n != 1 { t.Errorf("changed %d paragraphs, want 1", n) }
	s := string(got)
	if !strings.Contains(s, "<w:b/>") || !strings.Contains(s, `w:val="FF0000"`) {
		t.Errorf("existing run properties were wiped: %s", s)
	}
	if !strings.Contains(s, `<w:sz w:val="28"/>`) || !strings.Contains(s, `<w:szCs w:val="28"/>`) {
		t.Errorf("size not applied or szCs not synced: %s", s)
	}
}

// 3. run 没有 rPr 时插入一个
func TestDirect_InsertsRunPropertiesWhenAbsent(t *testing.T)

// 4. 已有目标属性时改值而非重复插入（幂等的前提）
func TestDirect_UpdatesAnExistingSizeInsteadOfDuplicating(t *testing.T)

// 5. 幂等：同样参数应用两次，字节相同
func TestDirect_IsIdempotent(t *testing.T)

// 6. 空段落在 run 级被跳过并计入 notes，但段落级仍适用
func TestDirect_EmptyParagraphSkippedForRunFormatButNotForParagraphFormat(t *testing.T)

// 7. 正文文字一字不改
func TestDirect_LeavesTextUntouched(t *testing.T)

// 8. 段落级：pPr 的合并同样三种情形
func TestDirect_MergesIntoExistingParagraphProperties(t *testing.T)
```

- [ ] **Step 2-5**：红 → 实现 → 绿 → 逐分支验红。

验红项：范围边界（改成全篇）、`rPr` 合并（改成替换）、`szCs` 同步、"已有属性改值"（改成总是插入 → 幂等测试变红）、空段落跳过。

- [ ] **Step 6: 在 `format.go` 里分派**

`Format()` 开头：若 `StartPara > 0` 则走直接格式化路径，且**校验只对整篇有意义的规则未被同时给出**，否则报错。若 `StartPara == 0` 则完全走现有路径。

**必须有的测试**：无范围时 `styles.xml` 产出与本阶段之前逐字节相同（证明没污染既有路径）。

---

### Task 2: 工具层、skill 与设计文档

**Files:** Modify `pkg/tools/builtin/docx.go`、`docx_test.go`、`.deepai/skills/docx-format/SKILL.md`、`docs/DOCX_TOOLS_DESIGN.md`

- [ ] **Step 1: `docx_format` 加范围参数**

`start_para` / `end_para`（number）。类型必须校验。schema 描述要讲清两种模式的区别：**不给范围 = 改文档默认值；给范围 = 对这些段落做直接格式化，优先级高于文档默认值**。模型需要这个区别才能选对。

- [ ] **Step 2: 修正 `docx_edit` 的误导性描述**

现在写的是 `Paragraph styling is out of scope here; use docx_format.` —— 后半句指向一个（在有范围支持前）做不到的工具。改成明确说明：段落与 run 的格式用 `docx_format` 并**带上 `start_para`/`end_para`**。

- [ ] **Step 3: skill**

`docx-format/SKILL.md` 补一节"只改某几段"：先 `docx_read` 拿到 `para_index`，再用 `docx_format` 带范围。并说明**不要**为此改用 python —— 这正是本阶段要消除的回退。

- [ ] **Step 4: 设计文档**

- §4.3 的 schema 加 `start_para`/`end_para`，并写明整篇 vs 范围的落点差异。
- §4.2 关于 `style` 推迟的那段要**更正**：原理由"改样式属于 `docx_format` 的职责"是不完整的 —— `docx_format` 当时只做整篇，能力掉在两个工具之间，用户实测撞上了。现在由带范围的 `docx_format` 覆盖；`style`（按样式名套用，如 `Heading2`）仍未实现，需要 `<w:pStyle>` 的字节区间，留待后续。

- [ ] **Step 5: 测试与回归**

工具层：范围参数生效、只对整篇有意义的规则给了范围就报错、类型检查、`applied` 如实反映改了几段。全量回归确认前面所有阶段的保证仍成立。

---

## 完成标准

1. `go test ./pkg/docx/... ./pkg/tools/... -race` 全绿；此前所有阶段的保证继续成立。
2. **不带范围时输出逐字节不变**（有测试钉住）。
3. 每个新行为有"移除最小分支即失败"的验红证据。
4. 带范围时：只有指定段落被改、既有 run/段落格式被合并而非抹掉、正文文字不变、幂等。
5. `docx_edit` 不再把模型指向做不到的工具。
6. **人工验收（用户执行）**：让 agent 改某一段的字号，确认它**用 `docx_format` 带范围**而不是 python，且 Word 里该段字号确实变了、其他段落没变、该段原有的粗体等格式还在。
