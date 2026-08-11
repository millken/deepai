# DOCX P1c: 工具层接线（tools / profile / skills）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `pkg/docx` 接到 deepai 上，使用户能真正让 agent 润色 / 总结 .docx。这是 P1 的最后一步 —— 完成后 `docx_read` / `docx_edit` 出现在工具列表里，`document-editor` 子代理可被委派，两个 skill 可被调用。

**Architecture:** 薄封装。`pkg/tools/builtin/docx.go` 只做参数解析、调用 `pkg/docx`、把结果序列化成 JSON，并强制 §5.1 的体积上限；一切文档语义都已在 `pkg/docx` 里。profile 与 skill 是配置，不含逻辑。

**Tech Stack:** Go 1.26.1 标准库 + 已有的 `pkg/docx`。

前置：P1a+P1a.5（`66365f3`）、P1b（`e7db0a9`）。设计依据：`docs/DOCX_TOOLS_DESIGN.md` §4.1、§4.2、§5.1、§6、§7、§7.1、§8。

## Global Constraints

- **零外部 Go 依赖**：`go.mod` 不得改动。
- **不得修改 `pkg/docx/*.go`**：该包已提交并经多轮审查。工具层若发现它缺能力，**停下来报告**，不要自行扩展。
- **薄封装**：`docx.go` 里不得出现文档语义判断（什么算标题、怎么分块、protect 怎么比对）。那些都在 `pkg/docx`。工具层只做：解析参数 → 调 API → 序列化 → 体积把关。
- **§5.1 的 24 KB 上限管的是序列化后的整个工具结果**，不是 markdown 长度。这是本阶段最容易做错的一条，见 Task 1。
- **验红必须最小分支**。本项目已四次栽在验红过粗或"成对分支只测一半"上。
- **不自动提交**：提交由用户在里程碑处口头触发。
- **测试命令**：`go test ./pkg/tools/... ./pkg/agent/... ./pkg/commands/... -race -count=1`、`go vet ./... && GOOS=windows go vet ./...`、`go build ./...`、`gofmt -l pkg`。

## 复审给出的 P1c 就绪清单（逐条落实，勿遗漏）

| # | 要求 | 落在 |
|---|---|---|
| 1 | 一次性备份由工具层负责（`Modified()` 不够） | Task 2 |
| 2 | 碰撞理由已在 P1b 修好 | 无需动作 |
| 3 | `style` 必须**显式报错**，不得静默忽略 | Task 2 |
| 4 | `reviewed_through_para` 由工具层原样回显 | Task 2 |
| 5 | markdown 用 `ReadResult.Markdown`，**不要**把 `ParaView.Text` 重新拼成散文 | Task 1 |
| 6 | 体积断言要针对**序列化后的 JSON**，不只是 `len(Markdown)` | Task 1 |
| 7 | `EditResult.ParaCountChanged` 要传给模型 | Task 2 |

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/tools/builtin/docx.go` | `DocxReadTool` / `DocxEditTool` / `DocxTools()` 与两个 handler |
| `pkg/tools/builtin/docx_test.go` | |
| `pkg/tools/builtin/descriptions_test.go` | 修改：`allBuiltinTools()` 加入 `DocxTools()` |
| `pkg/agent/types_config.go` | 修改：新增 `AgentTypeDocEditor` |
| `pkg/agent/aging.go` | 修改：`defaultToolBudgetsByTool` 加 `docx_read` |
| `pkg/commands/chat.go` | 修改：注册 `DocxTools()` |
| `.deepai/skills/docx-polish/SKILL.md` | |
| `.deepai/skills/docx-summarize/SKILL.md` | |

---

### Task 1: `docx_read` 工具

**Files:**
- Create: `pkg/tools/builtin/docx.go`, `pkg/tools/builtin/docx_test.go`

**Interfaces:**
```go
func DocxReadTool() models.Tool
func DocxReadHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error)

// 返回给模型的 JSON 形状：
type docxReadOutput struct {
    Markdown      string          `json:"markdown"`
    Paras         []docxParaIndex `json:"paragraphs,omitempty"`
    Outline       *docxOutline    `json:"outline,omitempty"`
    NextStartPara int             `json:"next_start_para"`
    RangeStart    int             `json:"range_start,omitempty"`
    RangeEnd      int             `json:"range_end,omitempty"`
    TotalParas    int             `json:"total_paras"`
    Notes         []string        `json:"notes,omitempty"`
}
type docxParaIndex struct {
    Index int            `json:"index"`
    Style string         `json:"style,omitempty"`
    Cell  *docx.CellRef  `json:"cell,omitempty"`
    Runs  []docxRunIndex `json:"runs,omitempty"` // 仅 runs=true
}
type docxRunIndex struct {
    Index int    `json:"index"`
    Text  string `json:"text"`
}
```

**关键决定 1 —— 不要重复正文。** `Markdown` 里每段已带 `[para N]` 标记且经过标记中和处理；`paragraphs` 只带**索引与元信息**，不带 `text`。这既把结果体积砍掉近一半，也直接消除复审第 5 条的隐患（把 `ParaView.Text` 重新拼成散文会撤销标记中和）。`runs=true` 时才带 run 文本，因为按 run 编辑必须知道每个 run 的确切内容。

**关键决定 2 —— 体积把关针对序列化结果。** 设计 §5.1 的 24 KB 管的是整个工具结果。`DefaultReadBudget` 只约束 markdown 正文字符，而 `runs=true` 会让同一段文本重复出现、JSON 转义与字段名还有额外开销，**实测可能翻数倍**。

规则：
1. 用调用方给的 `max_chars`（缺省则 `docx.DefaultReadBudget`）调一次 `Read`。
2. `json.Marshal` 结果，量 `len(payload)`。
3. 若超过 `maxDocxResultBytes`（= 20 KB，留 4 KB 余量给工具结果的外层封装），**把 body 预算减半重试**，最多 4 次。
4. 仍超限则返回错误，提示改用更小的 `max_chars` 或 `heading` 缩小范围。**绝不截断后返回** —— 截断正是 §5.1 要消灭的静默丢内容。
5. 每次缩减都要在 `notes` 里留一条说明，让模型知道它拿到的比它要的少。

**输出形态**（§4.1）：
- 未给 `heading`/`start_para`/`end_para`/`full` 且总段数 > `docx.DocxOutlineParaThreshold` → 返回 `outline`
- 否则返回 `markdown` + `paragraphs` + 游标

- [ ] **Step 1: 写工具定义与失败的测试**

`docx.go` 的 schema（**注意 `map[string]any` 的复合字面量不能省略类型名**，仓库现有写法见 `file.go:249`）：

```go
func DocxReadTool() models.Tool {
	return models.Tool{
		Name:         "docx_read",
		Groups:       []string{"builtin", "document"},
		ParallelSafe: true,
		Description: "Read a .docx as structured content. Returns a heading outline by default for " +
			"large documents; pass heading or start_para/end_para for a section or range, or full=true " +
			"for the whole body. Large ranges are chunked: next_start_para is the cursor for the next " +
			"call, and 0 means the range is exhausted. Headers, footers, footnotes and text boxes are " +
			"not included and are declared in notes.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "Path to the .docx file"},
				"heading":    map[string]any{"type": "string", "description": "Restrict to the section under this heading; mutually exclusive with start_para/end_para"},
				"start_para": map[string]any{"type": "number", "description": "1-based inclusive first paragraph"},
				"end_para":   map[string]any{"type": "number", "description": "1-based inclusive last paragraph"},
				"full":       map[string]any{"type": "boolean", "description": "Return the whole body; errors instead of chunking when it exceeds the budget"},
				"runs":       map[string]any{"type": "boolean", "description": "Include each paragraph's runs, needed to edit by run index"},
				"max_chars":  map[string]any{"type": "number", "description": "Body character budget for this chunk"},
			},
			"required": []any{"path"},
		},
		Handler: DocxReadHandler,
	}
}
```

`docx_test.go` 起手测试：

```go
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/docx"
	"github.com/millken/deepai/pkg/models"
)

// docxFixture copies the pkg/docx outline fixture into a temp dir so tests
// can edit it freely. The path is relative because pkg/tools/builtin sits
// beside pkg/docx in the module.
func docxFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "docx", "testdata", name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func callDocxRead(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxReadHandler(context.Background(), models.ToolCall{
		ID: "c1", Name: "docx_read", Arguments: args,
	})
}

func decodeRead(t *testing.T, res models.ToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Content)
	}
	return out
}

func TestDocxRead_RequiresPath(t *testing.T) {
	if _, err := callDocxRead(t, map[string]any{}); err == nil {
		t.Fatal("missing path returned nil error")
	}
}

func TestDocxRead_RangeReturnsMarkdownAndParagraphIndex(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(3)})
	if err != nil {
		t.Fatalf("DocxReadHandler: %v", err)
	}
	out := decodeRead(t, res)
	md, _ := out["markdown"].(string)
	if !strings.Contains(md, "[para 1]") {
		t.Errorf("markdown lacks para markers:\n%s", md)
	}
	paras, _ := out["paragraphs"].([]any)
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
}

// TestDocxRead_ParagraphsCarryNoBodyText pins the "do not duplicate the body"
// decision: the markdown already holds the text (marker-neutralized), and
// re-emitting it per paragraph both doubles the payload and reintroduces the
// raw "[para N]" spoofing hazard the neutralization exists to prevent.
func TestDocxRead_ParagraphsCarryNoBodyText(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	paras, _ := out["paragraphs"].([]any)
	for i, raw := range paras {
		p, _ := raw.(map[string]any)
		if _, ok := p["text"]; ok {
			t.Errorf("paragraphs[%d] carries a text field; body text belongs only in markdown", i)
		}
		if _, ok := p["index"]; !ok {
			t.Errorf("paragraphs[%d] has no index", i)
		}
	}
}

func TestDocxRead_RunsIncludedOnlyWhenRequested(t *testing.T) {
	p := docxFixture(t, "structure.docx")
	without, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := decodeRead(t, without)["paragraphs"].([]any)
	if pm, _ := first[0].(map[string]any); pm["runs"] != nil {
		t.Error("runs were included without runs=true")
	}

	with, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1), "runs": true})
	if err != nil {
		t.Fatal(err)
	}
	pm, _ := decodeRead(t, with)["paragraphs"].([]any)[0].(map[string]any)
	runs, _ := pm["runs"].([]any)
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	r0, _ := runs[0].(map[string]any)
	if r0["text"] != "Hello " {
		t.Errorf("runs[0].text = %v, want %q", r0["text"], "Hello ")
	}
}

// TestDocxRead_FitResultShrinksUntilItFits pins the budget loop itself.
//
// It uses a stub instead of a fixture on purpose: outline.docx serializes to
// roughly 6-8 KB even with runs=true, so an integration test against it would
// pass with the entire shrink loop deleted. Testing the loop directly is the
// only way to pin it.
func TestDocxRead_FitResultShrinksUntilItFits(t *testing.T) {
	var budgets []int
	// Each call returns a payload proportional to the budget, so the loop has
	// to halve twice before it fits.
	read := func(budget int) (docxReadOutput, error) {
		budgets = append(budgets, budget)
		return docxReadOutput{Markdown: strings.Repeat("x", budget*6)}, nil
	}
	payload, err := fitDocxReadResult(read, 8192)
	if err != nil {
		t.Fatalf("fitDocxReadResult: %v", err)
	}
	if len(payload) > maxDocxResultBytes {
		t.Fatalf("payload is %d bytes, over the %d cap", len(payload), maxDocxResultBytes)
	}
	if len(budgets) < 2 {
		t.Fatalf("budgets tried = %v, want the loop to shrink at least once", budgets)
	}
	for i := 1; i < len(budgets); i++ {
		if budgets[i] >= budgets[i-1] {
			t.Errorf("budget did not shrink: %v", budgets)
		}
	}
	// The caller must be told it got less than it asked for.
	var out docxReadOutput
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) == 0 {
		t.Error("the result was shrunk but notes says nothing about it")
	}
}

// TestDocxRead_FitResultGivesUpRatherThanTruncating pins that an
// irreducible result is an error, never a silently truncated success --
// silent truncation is the exact failure the chunking design exists to
// prevent.
func TestDocxRead_FitResultGivesUpRatherThanTruncating(t *testing.T) {
	read := func(budget int) (docxReadOutput, error) {
		return docxReadOutput{Markdown: strings.Repeat("x", maxDocxResultBytes*2)}, nil
	}
	if _, err := fitDocxReadResult(read, 8192); err == nil {
		t.Fatal("an irreducible result returned nil error; it must refuse, not truncate")
	}
}

// TestDocxRead_RealFixtureStaysUnderTheCap is the weaker integration guard
// that complements the two loop tests above.
func TestDocxRead_RealFixtureStaysUnderTheCap(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	for _, args := range []map[string]any{
		{"path": p},
		{"path": p, "runs": true},
		{"path": p, "start_para": float64(1), "end_para": float64(73), "runs": true},
		{"path": p, "max_chars": float64(1 << 20), "runs": true},
	} {
		res, err := callDocxRead(t, args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if len(res.Content) > maxDocxResultBytes {
			t.Errorf("args %v: result is %d bytes, over the %d cap", args, len(res.Content), maxDocxResultBytes)
		}
	}
}

// TestDocxRead_OutlineDecisionAtTheThreshold pins the outline-by-default
// rule at its boundary. It calls the decision directly because both fixtures
// are far below DocxOutlineParaThreshold (73 vs 200), so no fixture-based
// test could distinguish the branch.
func TestDocxRead_OutlineDecisionAtTheThreshold(t *testing.T) {
	tests := []struct {
		name  string
		total int
		opts  docxReadArgs
		want  bool
	}{
		{"below threshold", docx.DocxOutlineParaThreshold - 1, docxReadArgs{}, false},
		{"at threshold", docx.DocxOutlineParaThreshold, docxReadArgs{}, false},
		{"above threshold", docx.DocxOutlineParaThreshold + 1, docxReadArgs{}, true},
		{"above threshold but a range was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{StartPara: 3}, false},
		{"above threshold but a heading was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{Heading: "Intro"}, false},
		{"above threshold but full was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{Full: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReturnOutline(tt.total, tt.opts); got != tt.want {
				t.Errorf("shouldReturnOutline(%d, %+v) = %v, want %v", tt.total, tt.opts, got, tt.want)
			}
		})
	}
}

func TestDocxRead_DeclaresOmittedParts(t *testing.T) {
	p := docxFixture(t, "structure.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := decodeRead(t, res)["notes"].([]any)
	joined := ""
	for _, n := range notes {
		joined += n.(string) + " | "
	}
	if !strings.Contains(joined, "header") || !strings.Contains(joined, "footer") {
		t.Errorf("notes = %q, want the header/footer declaration", joined)
	}
}

func TestDocxRead_UnknownHeadingErrors(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxRead(t, map[string]any{"path": p, "heading": "No Such Heading"}); err == nil {
		t.Fatal("unknown heading returned nil error")
	}
}

func TestDocxRead_RejectsNonDocx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(p, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callDocxRead(t, map[string]any{"path": p}); err == nil {
		t.Fatal("a non-docx file returned nil error")
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `undefined: DocxReadTool` 等。

- [ ] **Step 3: 实现**

要点：
1. 路径走 `resolveReadablePath(ctx, path)`，与 `read_file`/`view_image` 一致。
2. `docx.OpenDocument` 的错误直接包装返回（它已给出"加密""不是有效 .docx"等可读消息）。
3. 参数缺省与互斥由 `pkg/docx` 判定，工具层只负责把 JSON number 转成 int（注意 JSON 解出来是 `float64`）。
4. 体积循环见上文规则。`maxDocxResultBytes = 20 << 10`，与 `DefaultReadBudget` 并列声明并写明为何是 20 而不是 24。
5. `Content` 放序列化后的 JSON 字符串。

- [ ] **Step 4-5: 绿 + 验红**

| 移除的最小分支 | 应变红 |
|---|---|
| 序列化后的体积检查与缩减循环 | `TestDocxRead_ResultStaysUnderTheSerializedCap` |
| `runs` 参数的条件填充 | `TestDocxRead_RunsIncludedOnlyWhenRequested` |
| paragraphs 不带 text 的决定（改成带上 text） | `TestDocxRead_ParagraphsCarryNoBodyText` |

---

### Task 2: `docx_edit` 工具

**Files:**
- Modify: `pkg/tools/builtin/docx.go`, `pkg/tools/builtin/docx_test.go`

**Interfaces:**
```go
func DocxEditTool() models.Tool
func DocxEditHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error)
func DocxTools() []models.Tool // = {DocxReadTool(), DocxEditTool()}
```

**四项工具层专属职责**（`pkg/docx` 不做，也不该做）：

1. **`style` 显式报错**（设计 §4.2 已注明推迟到 P2）。schema 不声明该字段，且 handler 检测到 `edits[].style` 存在时**返回错误说明它推迟到 P2、请用 docx_format**。静默忽略会让模型以为改了样式。
2. **一次性备份**（§8）。写回前，若 `<path>.bak` **不存在**则创建它。用"文件是否存在"而不是进程内的 map 来记录 —— 无全局状态、跨重启有效、天然只备份一次。备份路径在结果里回显。
3. **`reviewed_through_para` 原样回显**（§5.7）。工具不解释它，只把入参放回结果，让游标与写回落在同一次调用里。
4. **`para_count_changed` 传给模型**（§5.4）。来自 `EditResult.ParaCountChanged`；为真时在结果里附一句"段落索引已变化，请重新读取大纲或范围后再继续"。

**输出形状：**
```go
type docxEditOutput struct {
    Applied             int               `json:"applied"`
    Outcomes            []docxEditOutcome `json:"outcomes"`
    TotalParas          int               `json:"total_paras"`
    ParaCountChanged    bool              `json:"para_count_changed"`
    IndexAdvice         string            `json:"index_advice,omitempty"`
    BackupPath          string            `json:"backup_path,omitempty"`
    ReviewedThroughPara int               `json:"reviewed_through_para,omitempty"`
}
type docxEditOutcome struct {
    Para    int    `json:"para"`
    Applied bool   `json:"applied"`
    Before  string `json:"before,omitempty"`
    After   string `json:"after,omitempty"`
    Reason  string `json:"reason,omitempty"`
    Warning string `json:"warning,omitempty"`
}
```

- [ ] **Step 1: 写失败的测试**（追加到 `docx_test.go`）

```go
func callDocxEdit(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxEditHandler(context.Background(), models.ToolCall{
		ID: "e1", Name: "docx_edit", Arguments: args,
	})
}

func TestDocxEdit_AppliesAFindReplace(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler: %v", err)
	}
	out := decodeRead(t, res)
	if out["applied"] != float64(1) {
		t.Fatalf("applied = %v, want 1; content=%s", out["applied"], res.Content)
	}
	after, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(2), "end_para": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decodeRead(t, after)["markdown"].(string), "BODY") {
		t.Error("the edit did not reach the file")
	}
}

// TestDocxEdit_StyleIsRejectedNotIgnored pins design §4.2's note: style is
// deferred to P2, and silently dropping it would let the model believe it
// restyled a paragraph.
func TestDocxEdit_StyleIsRejectedNotIgnored(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	_, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{map[string]any{"para": float64(2), "text": "x", "style": "Heading2"}},
	})
	if err == nil {
		t.Fatal("style was accepted; want an explicit error")
	}
	if !strings.Contains(err.Error(), "docx_format") {
		t.Errorf("error = %q, want it to point at docx_format", err)
	}
}

// TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite pins §8. The second edit
// must NOT refresh the backup, or a half-finished polish run would overwrite
// the pristine original.
func TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(text string) {
		t.Helper()
		if _, err := callDocxEdit(t, map[string]any{
			"path":  p,
			"edits": []any{map[string]any{"para": float64(2), "text": text}},
		}); err != nil {
			t.Fatalf("edit %q: %v", text, err)
		}
	}
	edit("first")
	backup := p + ".bak"
	first, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no backup after the first edit: %v", err)
	}
	if !bytes.Equal(first, original) {
		t.Error("the backup is not the pristine original")
	}

	edit("second")
	second, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, original) {
		t.Error("the second edit refreshed the backup; it must be written once")
	}
}

func TestDocxEdit_ReportsBeforeAndAfterPerEdit(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, _ := decodeRead(t, res)["outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(outcomes))
	}
	o, _ := outcomes[0].(map[string]any)
	if o["before"] != "Body" || o["after"] != "BODY" {
		t.Errorf("before/after = %v/%v, want Body/BODY", o["before"], o["after"])
	}
}

// TestDocxEdit_SignalsParagraphIndexShift pins §5.4: after an insert or
// delete the caller's indices are stale, and nothing else tells it so.
func TestDocxEdit_SignalsParagraphIndexShift(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{map[string]any{"para": float64(2), "op": "insert_after", "text": "NEW"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if out["para_count_changed"] != true {
		t.Error("para_count_changed = false after an insert")
	}
	if out["index_advice"] == nil || out["index_advice"] == "" {
		t.Error("no advice telling the caller its paragraph indices are stale")
	}
}

// TestDocxEdit_NoIndexAdviceWhenParaCountUnchanged is the other half of
// §5.4, and the half that is easy to forget: a hardcoded ParaCountChanged
// of true would satisfy the test above. It also guards a real cost — if
// every ordinary replace told the model its indices were stale, each polish
// batch would spend a wasted docx_read re-reading the outline.
func TestDocxEdit_NoIndexAdviceWhenParaCountUnchanged(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if out["para_count_changed"] == true {
		t.Fatal("a plain replace changed para_count_changed; this test needs an edit that does not")
	}
	if advice, ok := out["index_advice"]; ok && advice != "" {
		t.Errorf("index_advice = %v, want none when the paragraph count did not change", advice)
	}
}

func TestDocxEdit_EchoesReviewedThroughPara(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":                  p,
		"reviewed_through_para": float64(12),
		"edits":                 []any{map[string]any{"para": float64(2), "find": "Body", "text": "B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decodeRead(t, res)["reviewed_through_para"] != float64(12) {
		t.Error("reviewed_through_para was not echoed back")
	}
}

func TestDocxEdit_RefusesDocumentWithExistingRevisions(t *testing.T) {
	p := docxFixture(t, "structure.docx") // contains w:ins / w:del
	_, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(1), "text": "x"}},
	})
	if err == nil {
		t.Fatal("editing a document with revision marks returned nil error")
	}
}

func TestDocxEdit_PerEditRefusalDoesNotFailTheCall(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{
			map[string]any{"para": float64(2), "find": "no such text", "text": "x"},
			map[string]any{"para": float64(3), "find": "Body", "text": "OK"},
		},
	})
	if err != nil {
		t.Fatalf("a per-edit refusal must not fail the whole call: %v", err)
	}
	out := decodeRead(t, res)
	if out["applied"] != float64(1) {
		t.Errorf("applied = %v, want 1", out["applied"])
	}
}
```

- [ ] **Step 2-5**：确认红 → 实现 → 绿 → 验红。

验红项：移除 `style` 检测 → `TestDocxEdit_StyleIsRejectedNotIgnored`；移除"备份已存在则跳过"的判断 → `TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite` 的第二半；移除 `index_advice` 填充 → `TestDocxEdit_SignalsParagraphIndexShift`。

`DocxEditTool` 的 schema：`path`（必需）、`edits`（必需，数组，元素含 `para`/`run`/`find`/`text`/`op`）、`protect`（字符串数组）、`reviewed_through_para`（数字）。**`ParallelSafe` 必须为 false**（写操作）。

---

### Task 3: 注册、profile 与老化预算

**Files:**
- Modify: `pkg/commands/chat.go`、`pkg/agent/types_config.go`、`pkg/agent/aging.go`、`pkg/tools/builtin/descriptions_test.go`

- [ ] **Step 1: 注册工具**

在 `chat.go` 的 `builtin.WebTools()` 循环之后追加：

```go
	for _, tool := range builtin.DocxTools() {
		mustRegisterTool(registry, tool)
	}
```

`mustRegisterTool` 在注册失败时 **panic**，所以 `DocxTools()` 只能返回已实现 handler 的工具。

- [ ] **Step 2: 加 profile**

`types_config.go`：新增常量 `AgentTypeDocEditor AgentType = "document-editor"`，并在 `BuiltinAgentTypes` 里加一项：

```go
	AgentTypeDocEditor: {
		Type:         AgentTypeDocEditor,
		Name:         "Document Editor",
		Description:  "Profile for .docx polishing and summarization: structured read, format-preserving edit, and protected-term validation.",
		SystemPrompt: docEditorSystemPrompt,
		DefaultTools: []string{"docx_read", "docx_edit", "read_file", "write_file", "ask_clarification"},
		MaxTurns:     30,
		Temperature:  0.2,
	},
```

`docEditorSystemPrompt` 要写清四条（依据 §7 与 §5）：保留作者语气与术语、**不增删段落**（这样 `para_index` 全程稳定）、优先用 `find` 做窄替换而非整段替换、以及分块处理时读→改→写必须紧邻。

**`MaxTurns: 30` 不能写 0** —— 0 会被 `subagent.go` 的安全底线兜成 15，而分块润色每块 2-3 轮，15 轮只够四五块。

- [ ] **Step 3: 加老化预算**

`aging.go` 的 `defaultToolBudgetsByTool` 加：

```go
	"docx_read": {1: 8192, 2: 2048, 3: 300}, // same as read_file: re-reading a chunk is expensive
```

这条只在用户开启 `DEEPAI_TOKEN_AGING` 时生效，属可选增强。

- [ ] **Step 4: 更新描述测试**

`descriptions_test.go` 的 `allBuiltinTools()` 加上 `DocxTools()...`。确认两个工具的描述不触犯既有断言（不得出现 "via bash" 等与系统提示重复的路由文案）。

- [ ] **Step 5: 写测试**

新增到 `pkg/agent` 与 `pkg/commands` 的既有测试文件（不新建文件，跟随现有组织）：

- `document-editor` profile 存在、`MaxTurns` 为 30、`DefaultTools` 含两个 docx 工具
- 用 `selectSubagentTools` 验证：以 `"document"` 组为 selector 时能选出两个 docx 工具且**不含** `bash`/`write_file` 之外的无关工具
- 注册后 registry 里能查到 `docx_read` / `docx_edit`，且 `docx_edit.ParallelSafe == false`

- [ ] **Step 6: 全量回归**

Run: `go test ./... 2>&1 | grep -E "^(ok|FAIL|---)"`
Expected: 除既有的 `pkg/tools` 8 个 `TestGitAutoCommit_*`（macOS `/private` 符号链接问题）与 `pkg/mcp` 2 个 mock 超时之外全绿。**这两处是已知的既有失败**，在基线提交 `b25edcf` 上同样复现过 —— 不要试图"修好"它们。

---

### Task 4: 技能

**Files:**
- Create: `.deepai/skills/docx-polish/SKILL.md`、`.deepai/skills/docx-summarize/SKILL.md`

**目录必须扁平一层**：`Registry.LoadFromDir` 只扫 `.deepai/skills/<name>/SKILL.md`，不递归。skill 名是全局唯一 key，所以用 `docx-` 前缀。

**接线走 §7.1 的 A 方案**：`agent:` 与 `allowed-tools:` 这两个 frontmatter 字段在当前唯一接线的路径上是**惰性的** —— skill 只会把正文追加进主 agent 的系统提示。所以正文必须写成**给主 agent 的操作指令**：让它用 `task` 工具委派 `agent_type: document-editor`。frontmatter 照写，但只作为意图声明。

- [ ] **Step 1: 写 `docx-polish/SKILL.md`**

frontmatter：`name: docx-polish`、`description`（要能让模型据此自动启用：提到 .docx、润色、保留格式）、`allowed-tools: [docx_read, docx_edit, task, ask_clarification]`、`agent: document-editor`。

正文要覆盖：

1. **先委派**：用 `task` 委派 `agent_type: document-editor`，把文件路径与润色要求传过去；不要在主 agent 里直接调 `docx_edit`。
2. **三层提示**（§1.1）：系统规则（不增删段、保留作者语气与术语）/ 任务模式（grammar | fluency | formal-tone）/ 保护清单（默认保护所有数字与全大写缩写，可用 `ask_clarification` 让用户补充）。
3. **分块循环**（§5.3）：`docx_read` 取大纲 → 逐块 `docx_read(start_para/end_para, runs=true)` → 润色 → `docx_edit` 立即写回 → 用返回的 `next_start_para` 继续。**读和写之间不要插入其他工具调用**。
4. **优先 `find`**：窄替换保住段内其他 run 的格式；整段替换只在改动幅度大到无法定位子串时用，且工具会警告格式将被抹平。
5. **禁止 `insert_*` 与 `delete`**：润色不增删段落，这样 `para_index` 全程稳定、不必重读大纲。
6. **看懂返回**：`applied` 少于提交数时逐条读 `outcomes[].reason` 并据此重试；`reason` 是可执行的（会指出是命中 0 次、跨 run、还是与第几条冲突）。
7. **收尾**：向用户汇报改了哪些段、每段的 before/after 摘要，以及备份文件路径。

- [ ] **Step 2: 写 `docx-summarize/SKILL.md`**

frontmatter 同构，`allowed-tools: [docx_read, write_file, task, ask_clarification]`。

正文覆盖 §5.6 的 map-reduce：逐块读 → 每块产出限长摘要（例如 200 字）→ 递归汇总 → 输出 markdown（P1 不生成新 .docx，那是 P2 的 `docx_write`）。强调**只读**，不调用 `docx_edit`。

- [ ] **Step 3: 验证 skill 能被加载**

Run:
```bash
ls .deepai/skills/ && go test ./pkg/skill/... -count=1
```
Expected: 两个目录各含一个 `SKILL.md`；skill 包测试通过。

再手工确认 frontmatter 能被解析（`pkg/skill/types.go` 的字段名：`name` / `description` / `allowed-tools` / `agent`）。

---

## 完成标准

1. `go test ./... ` 除两处**既有**失败外全绿。
2. `go vet ./...`（含 `GOOS=windows`）、`go build ./...`、`gofmt -l pkg` 全清，`go.mod` 未改。
3. `docx_read` 的**序列化结果**在所有参数组合下都在 20 KB 以内，且缩减时如实在 `notes` 里声明。
4. `docx_edit` 拒绝 `style`、备份只做一次、回显 `reviewed_through_para`、传出 `para_count_changed`。
5. `document-editor` profile 可被 `task` 委派，且只看到 docx 工具集。
6. 两个 skill 可被加载。
7. **人工端到端验收**：在真实 deepai 会话里让 agent 润色一份 .docx，确认（a）它先委派给 `document-editor`，（b）改动落到文件上，（c）备份文件存在，（d）Word 打开无修复提示。这一条本机无法自动化，需用户执行。
