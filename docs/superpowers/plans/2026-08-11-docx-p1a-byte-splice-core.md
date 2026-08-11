# DOCX P1a: Byte-Splice 地基 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 `pkg/docx` 的字节级地基——安全打开 .docx、为段落/run 建立字节区间索引、在原始字节流上做定点替换并原子写回，且保证未改动部分逐字节不变。

**Architecture:** 三层纯 Go 零依赖模块。`zipio.go` 负责 zip 层（安全解压、条目白名单、原子写回，未改条目用 `CreateRaw` 原样拷贝压缩字节）；`scan.go` 用 `encoding/xml` 只作扫描器，为每个 `<w:p>`/`<w:t>` 记录 `[start, end)` 字节区间，绝不做 DOM 重建；`splice.go` 按降序应用补丁、负责 XML 转义与 `xml:space="preserve"`。上层的 read/edit 工具（P1b）和工具封装（P1c）不在本计划范围内。

**Tech Stack:** Go 1.26.1 标准库（`archive/zip`、`encoding/xml`、`os`）。测试夹具由 Python + python-docx 一次性生成后提交进仓库，测试本身不依赖 Python。

设计文档：`docs/DOCX_TOOLS_DESIGN.md`（§3.1、§3.1.1、§4.1、§8、§10 第一组验收）。

## Global Constraints

- **零外部 Go 依赖**：本计划不得向 `go.mod` 添加任何 require。只用标准库。
- **绝不 DOM 重建**：不得把 `document.xml` 反序列化成结构体再重新序列化。`encoding/xml` 只用作扫描器（读 token + `InputOffset()`），所有写操作都是原始字节流上的区间替换。见设计文档 §3.1。
- **`InputOffset()` 语义**：返回"刚返回的 token 的结束位置"，不是开始位置。取元素内容区间的 `start` 要在 `Token()` 返回 `StartElement` 之后立刻取；`end` 是下一次 `Token()` 返回对应 `EndElement` **之前**记录的值。实现时维护 `prevOffset` 滚动变量。见设计文档 §3.1.1 第 1 条。
- **写回必须 XML 转义**：新文本里的 `&`、`<`、`>` 一律过 `xml.EscapeText`。见设计文档 §3.1.1 第 2 条。
- **不做字符↔字节映射**：`find` 类替换一律整体重写目标 `<w:t>` 的完整内容区间，不得尝试只 splice 子串对应的那几个字节。见设计文档 §3.1.1 第 3 条。
- **安全上限**（设计文档 §8）：`maxDocxBytes = 25 << 20`（压缩后）、`maxDecompressedBytes = 200 << 20`（解压后总量）、`maxZipEntries = 2000`。
- **Go 代码风格**：跟随仓库既有风格——导出符号写 doc comment；错误用 `fmt.Errorf` 带上下文；表驱动测试用 `t.Run` 子测试。参考 `pkg/imageproc/optimize.go` 与 `pkg/tools/builtin/file.go`。
- **不自动提交**：每个 Task 结束时**不要** `git commit`。本项目的提交由用户口头指示触发，实施者只需把工作留在工作树里并在报告中说明改了哪些文件。
- **测试命令**：`go test ./pkg/docx/...`。全仓库回归用 `go build ./... && go vet ./pkg/docx/...`。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `pkg/docx/testdata/gen_fixtures.py` | 一次性夹具生成脚本（Python，不参与构建/测试） |
| `pkg/docx/testdata/structure.docx` | 生成并提交的结构夹具：表格、图片、页眉页脚、超链接、多 run 段落、XML 实体、`xml:space`、已有 `w:ins`/`w:del` |
| `pkg/docx/zipio.go` | zip 层：安全打开、条目校验、原子写回 |
| `pkg/docx/zipio_test.go` | zip 层测试（含恶意输入） |
| `pkg/docx/scan.go` | `document.xml` 字节区间索引 |
| `pkg/docx/scan_test.go` | 索引测试 |
| `pkg/docx/splice.go` | 补丁应用、XML 转义、`xml:space` |
| `pkg/docx/splice_test.go` | 补丁测试 |
| `pkg/docx/fidelity_test.go` | §10 第一组验收：逐字节保真 |

---

### Task 1: 测试夹具

**Files:**
- Create: `pkg/docx/testdata/gen_fixtures.py`
- Create: `pkg/docx/testdata/structure.docx`（由脚本生成后提交）

**Interfaces:**
- Consumes: 无（首个任务）
- Produces: `pkg/docx/testdata/structure.docx`，后续所有任务的测试都读它。其 `word/document.xml` 保证包含：
  - 至少 1 个多 run 段落（同段内含粗体片段），文本 `Hello bold world`。**注意实际产物**：python-docx 会自动给首尾带空格的 run 加上 `xml:space="preserve"`，所以该段三个 run 分别是 `<w:t xml:space="preserve">Hello </w:t>`、`<w:t>bold</w:t>`、`<w:t xml:space="preserve"> world</w:t>` —— 中间的 `bold` run 是本夹具里唯一一个"无 preserve 属性"的目标，Task 4 测试补属性路径时必须用它
  - 至少 1 个含 XML 实体的段落，解码后文本为 `Tom & Jerry <fast>`
  - 至少 1 个带 `xml:space="preserve"` 且首尾有空格的 `<w:t>`
  - 至少 1 个 `<w:hyperlink>` 包裹的 run
  - 至少 1 个表格（2×2），单元格内有段落
  - 至少 1 处已有修订：`<w:ins>` 包一个 run、`<w:del>` 包一个 `<w:delText>`
  - 页眉、页脚、1 张图片（使 zip 里存在 `word/header1.xml`、`word/footer1.xml`、`word/media/*`）

- [ ] **Step 1: 写生成脚本**

创建 `pkg/docx/testdata/gen_fixtures.py`：

```python
#!/usr/bin/env python3
"""One-shot fixture generator for pkg/docx tests.

Run once and commit the output:  python3 gen_fixtures.py
Requires: pip install python-docx

python-docx cannot express raw-XML edge cases (entities, xml:space,
w:ins/w:del), so we build the base document with it, then post-process
word/document.xml inside the zip to inject those cases verbatim.
"""
import os
import re
import shutil
import zipfile

from docx import Document
from docx.shared import Inches

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "structure.docx")

# A 1x1 transparent PNG, so the fixture has a word/media/ entry.
PNG_1X1 = bytes.fromhex(
    "89504e470d0a1a0a0000000d494844520000000100000001080600000"
    "01f15c4890000000a49444154789c63000100000500010d0a2db40000"
    "000049454e44ae426082"
)


def build_base(path):
    doc = Document()

    # Multi-run paragraph: "Hello bold world" split across 3 runs.
    p = doc.add_paragraph()
    p.add_run("Hello ")
    p.add_run("bold").bold = True
    p.add_run(" world")

    # Placeholder paragraphs; raw XML is injected later.
    doc.add_paragraph("ENTITY_PLACEHOLDER")
    doc.add_paragraph("PRESERVE_PLACEHOLDER")
    doc.add_paragraph("HYPERLINK_PLACEHOLDER")
    doc.add_paragraph("REVISION_PLACEHOLDER")

    # 2x2 table with text in every cell.
    table = doc.add_table(rows=2, cols=2)
    for r in range(2):
        for c in range(2):
            table.cell(r, c).text = f"cell {r}{c}"

    # Header and footer, so header1.xml / footer1.xml exist.
    section = doc.sections[0]
    section.header.paragraphs[0].text = "Fixture Header"
    section.footer.paragraphs[0].text = "Fixture Footer"

    # An image, so word/media/ exists.
    png = os.path.join(HERE, "_tmp_1x1.png")
    with open(png, "wb") as fh:
        fh.write(PNG_1X1)
    doc.add_picture(png, width=Inches(1))
    os.remove(png)

    doc.save(path)


def para(inner):
    return "<w:p>" + inner + "</w:p>"


REPLACEMENTS = {
    "ENTITY_PLACEHOLDER": para(
        "<w:r><w:t>Tom &amp; Jerry &lt;fast&gt;</w:t></w:r>"
    ),
    "PRESERVE_PLACEHOLDER": para(
        '<w:r><w:t xml:space="preserve"> padded text </w:t></w:r>'
    ),
    "HYPERLINK_PLACEHOLDER": para(
        '<w:hyperlink r:id="rId1"><w:r><w:t>link text</w:t></w:r></w:hyperlink>'
    ),
    "REVISION_PLACEHOLDER": para(
        '<w:ins w:id="101" w:author="fixture" w:date="2026-01-01T00:00:00Z">'
        "<w:r><w:t>inserted</w:t></w:r></w:ins>"
        '<w:del w:id="102" w:author="fixture" w:date="2026-01-01T00:00:00Z">'
        "<w:r><w:delText>deleted</w:delText></w:r></w:del>"
    ),
}


def inject_raw_xml(path):
    with zipfile.ZipFile(path) as zf:
        names = zf.namelist()
        parts = {n: zf.read(n) for n in names}

    xml = parts["word/document.xml"].decode("utf-8")
    for marker, replacement in REPLACEMENTS.items():
        # Replace the whole <w:p>...</w:p> that contains the marker.
        pattern = re.compile(r"<w:p\b[^>]*>(?:(?!</w:p>).)*" + marker + r".*?</w:p>", re.S)
        xml, n = pattern.subn(replacement, xml, count=1)
        if n != 1:
            raise SystemExit(f"failed to inject {marker}")
    parts["word/document.xml"] = xml.encode("utf-8")

    tmp = path + ".tmp"
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zf:
        for n in names:  # preserve original entry order
            zf.writestr(n, parts[n])
    shutil.move(tmp, path)


if __name__ == "__main__":
    build_base(OUT)
    inject_raw_xml(OUT)
    print("wrote", OUT)
```

- [ ] **Step 2: 生成夹具并人工确认**

Run:
```bash
cd pkg/docx/testdata && python3 gen_fixtures.py
```
Expected: 打印 `wrote .../structure.docx`，且文件存在。

再确认注入生效与条目齐全：
```bash
cd pkg/docx/testdata && python3 - <<'EOF'
import zipfile
z = zipfile.ZipFile("structure.docx")
names = z.namelist()
print("entries:", len(names))
for want in ("word/document.xml", "word/header1.xml", "word/footer1.xml", "[Content_Types].xml"):
    assert want in names, want
assert any(n.startswith("word/media/") for n in names), "no media"
x = z.read("word/document.xml").decode()
for want in ('Tom &amp; Jerry', 'xml:space="preserve"', '<w:hyperlink', '<w:ins ', '<w:delText>'):
    assert want in x, want
print("OK")
EOF
```
Expected: 打印 `entries: N`（N ≥ 10）然后 `OK`。若任一断言失败，修脚本重跑。

- [ ] **Step 3: 确认夹具体积可提交**

Run: `ls -l pkg/docx/testdata/structure.docx`
Expected: 体积 < 100 KB。若超出，缩小图片（PNG_1X1 已是 1×1，正常不会超）。

---

### Task 2: zipio.go —— 安全打开与原子写回

**Files:**
- Create: `pkg/docx/zipio.go`
- Test: `pkg/docx/zipio_test.go`

**Interfaces:**
- Consumes: Task 1 的 `pkg/docx/testdata/structure.docx`
- Produces:
  ```go
  type Package struct { /* unexported fields */ }
  func Open(name string) (*Package, error)
  func (p *Package) Part(name string) ([]byte, bool)
  func (p *Package) SetPart(name string, data []byte) error
  func (p *Package) Names() []string
  func (p *Package) WriteTo(path string) error
  const DocumentPart = "word/document.xml"
  ```
  `Open` 返回的 `*Package` 持有全部条目的解压内容与原始顺序。`SetPart` 只标记覆盖，不立即写盘；**条目名不存在时返回 error**（P1 不支持新增条目，静默忽略会让上层"改了没生效却报成功"）。`Part` 返回的切片**别名包内存储**：就地修改它不会置上 modified 标记，`WriteTo` 仍会原样拷贝原始压缩字节 —— 所有编辑必须走 `SetPart`。`WriteTo` 原子写回：未被 `SetPart` 覆盖的条目用 `CreateRaw` 原样拷贝压缩字节，被覆盖的用 `CreateHeader` 重新压缩；条目顺序与原文件一致，目标文件权限沿用被替换文件的模式。

> **本节代码块是实施起点，不是最终形态。** Task 2 的代码经审查后有一轮修复，最终以仓库中的 `pkg/docx/zipio.go` 为准。审查发现并已修复：`Open` 用路径两次 stat/read 导致非常规文件（FIFO、`/dev/zero`）绕过 25 MB 上限；`SetPart` 静默忽略未知条目名；`Part` 的别名语义未文档化；`WriteTo` 把目标文件权限收紧成 0600 且缺 `Sync`；解压预算在恰好等于上限时的 off-by-one；`checkEntryName` 放过 NUL 字节与 `C:` 盘符前缀；`Open` 的参数名遮蔽 `path` 包。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/zipio_test.go`：

```go
package docx

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const fixture = "testdata/structure.docx"

func TestOpen_ReadsKnownParts(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, ok := pkg.Part(DocumentPart)
	if !ok {
		t.Fatalf("Part(%q) missing", DocumentPart)
	}
	if !bytes.Contains(doc, []byte("<w:p")) {
		t.Errorf("document.xml has no paragraphs")
	}
	for _, want := range []string{"[Content_Types].xml", "word/header1.xml", "word/footer1.xml"} {
		if _, ok := pkg.Part(want); !ok {
			t.Errorf("Part(%q) missing", want)
		}
	}
}

// TestWriteTo_UntouchedPartsAreByteIdentical is the core Ground Truth guard:
// rewriting without any SetPart must reproduce every entry's decompressed
// bytes exactly, and keep the entry set and order intact.
func TestWriteTo_UntouchedPartsAreByteIdentical(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	assertEntriesEqual(t, fixture, out, nil)
}

// assertEntriesEqual compares two .docx by decompressed entry content.
// Entries named in changed are expected to differ; all others must match
// byte for byte. Entry names and their order must match exactly.
func assertEntriesEqual(t *testing.T, oldPath, newPath string, changed map[string]bool) {
	t.Helper()
	oldZ, err := zip.OpenReader(oldPath)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	defer oldZ.Close()
	newZ, err := zip.OpenReader(newPath)
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	defer newZ.Close()

	if len(oldZ.File) != len(newZ.File) {
		t.Fatalf("entry count: old %d, new %d", len(oldZ.File), len(newZ.File))
	}
	for i := range oldZ.File {
		oldName, newName := oldZ.File[i].Name, newZ.File[i].Name
		if oldName != newName {
			t.Fatalf("entry %d: name old %q, new %q", i, oldName, newName)
		}
		oldData := readZipEntry(t, oldZ.File[i])
		newData := readZipEntry(t, newZ.File[i])
		if changed[oldName] {
			if bytes.Equal(oldData, newData) {
				t.Errorf("%s: expected to change but is identical", oldName)
			}
			continue
		}
		if !bytes.Equal(oldData, newData) {
			t.Errorf("%s: not byte-identical (old %d bytes, new %d bytes)", oldName, len(oldData), len(newData))
		}
	}
}

func readZipEntry(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("open entry %s: %v", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entry %s: %v", f.Name, err)
	}
	return data
}

func TestWriteTo_ModifiedPartOnly(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	if err := pkg.SetPart(DocumentPart, bytes.Replace(doc, []byte("Hello "), []byte("Howdy "), 1)); err != nil {
		t.Fatalf("SetPart: %v", err)
	}

	out := filepath.Join(t.TempDir(), "out.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	assertEntriesEqual(t, fixture, out, map[string]bool{DocumentPart: true})
}

func TestOpen_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			name:    "encrypted CFB container",
			data:    append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...),
			wantErr: "encrypted",
		},
		{
			name:    "not a zip",
			data:    []byte("this is plain text, not a docx at all"),
			wantErr: "not a valid .docx",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, "bad.docx")
			if err := os.WriteFile(p, tt.data, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Open(p)
			if err == nil {
				t.Fatalf("Open succeeded, want error containing %q", tt.wantErr)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tt.wantErr)) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpen_RejectsDuplicateEntryNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dup.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "word/document.xml"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("<x/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("Open succeeded on duplicate entry names, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("duplicate")) {
		t.Errorf("error = %q, want it to mention duplicate", err)
	}
}

func TestOpen_RejectsPathTraversal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trav.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "../../evil.sh"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("Open succeeded on traversal entry, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("unsafe entry name")) {
		t.Errorf("error = %q, want it to mention unsafe entry name", err)
	}
}

func TestOpen_RejectsMissingContentTypes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "noct.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<x/>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// NOTE: plain `=`, not `:=` — err is already in scope from
	// zw.Create above, so `_, err := Open(p)` would fail to compile with
	// "no new variables on left side of :=".
	_, err = Open(p)
	if err == nil {
		t.Fatal("Open succeeded without [Content_Types].xml, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("not a valid .docx")) {
		t.Errorf("error = %q, want it to mention not a valid .docx", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run 'TestOpen|TestWriteTo' -v`
Expected: 编译失败，报 `undefined: Open`、`undefined: Package`、`undefined: DocumentPart`。

- [ ] **Step 3: 实现 zipio.go**

创建 `pkg/docx/zipio.go`：

```go
// Package docx provides byte-faithful reading and editing of .docx (OOXML)
// files. The design principle is Ground Truth + narrow patch: the original
// file is the single source of truth, and edits replace only the byte ranges
// that actually changed. Nothing is ever round-tripped through a DOM.
package docx

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DocumentPart is the zip entry holding the main document body.
const DocumentPart = "word/document.xml"

// contentTypesPart must be present in every valid OOXML package.
const contentTypesPart = "[Content_Types].xml"

const (
	// maxDocxBytes caps the on-disk (compressed) size.
	maxDocxBytes = 25 << 20
	// maxDecompressedBytes caps the total decompressed size, guarding
	// against zip bombs that are small on disk but huge in memory.
	maxDecompressedBytes = 200 << 20
	// maxZipEntries caps the entry count, guarding against packages with
	// an absurd number of tiny entries.
	maxZipEntries = 2000
)

// cfbMagic identifies an OLE2/Compound File Binary container. Password
// encrypted OOXML files are CFB, not zip: they hold EncryptionInfo and
// EncryptedPackage streams. Detecting this up front lets us report
// "encrypted" instead of leaking a confusing zip or XML parse error.
var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// Package is an opened .docx: every entry's decompressed content plus the
// original entry order. Modifications are staged with SetPart and only hit
// disk in WriteTo.
type Package struct {
	// names preserves the original entry order; WriteTo replays it.
	names []string
	// parts maps entry name to decompressed content.
	parts map[string][]byte
	// raw holds the original compressed bytes for each entry, so untouched
	// entries can be copied verbatim instead of recompressed.
	raw map[string][]byte
	// headers holds each entry's original zip metadata.
	headers map[string]*zip.FileHeader
	// modified marks entries replaced via SetPart.
	modified map[string]bool
}

// Open reads a .docx, validating it against the zip-layer guards before any
// content is parsed. The whole package is held in memory; callers should
// treat Open as the size-bounded entry point.
func Open(path string) (*Package, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat docx: %w", err)
	}
	if info.Size() > maxDocxBytes {
		return nil, fmt.Errorf("docx exceeds %d MB limit", maxDocxBytes>>20)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read docx: %w", err)
	}
	if bytes.HasPrefix(data, cfbMagic) {
		return nil, errors.New("encrypted or password-protected .docx is not supported")
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a valid .docx (cannot read as zip): %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("docx has %d entries, limit is %d", len(zr.File), maxZipEntries)
	}

	pkg := &Package{
		names:    make([]string, 0, len(zr.File)),
		parts:    make(map[string][]byte, len(zr.File)),
		raw:      make(map[string][]byte, len(zr.File)),
		headers:  make(map[string]*zip.FileHeader, len(zr.File)),
		modified: make(map[string]bool),
	}

	var total int64
	for _, f := range zr.File {
		if err := checkEntryName(f.Name); err != nil {
			return nil, err
		}
		if _, dup := pkg.parts[f.Name]; dup {
			// Duplicate names make the package ambiguous: Word reads the
			// entry the central directory points at, while a different
			// reader may pick the other one. Refuse rather than guess.
			return nil, fmt.Errorf("docx has duplicate entry name %q", f.Name)
		}

		content, err := readEntry(f, &total)
		if err != nil {
			return nil, err
		}
		rawBytes, err := readEntryRaw(f)
		if err != nil {
			return nil, err
		}

		header := f.FileHeader
		pkg.names = append(pkg.names, f.Name)
		pkg.parts[f.Name] = content
		pkg.raw[f.Name] = rawBytes
		pkg.headers[f.Name] = &header
	}

	if _, ok := pkg.parts[contentTypesPart]; !ok {
		return nil, fmt.Errorf("not a valid .docx (missing %s)", contentTypesPart)
	}
	if _, ok := pkg.parts[DocumentPart]; !ok {
		return nil, fmt.Errorf("not a valid .docx (missing %s)", DocumentPart)
	}
	return pkg, nil
}

// checkEntryName rejects absolute paths and traversal segments, which could
// otherwise escape a target directory if an entry were ever written out.
func checkEntryName(name string) error {
	if name == "" {
		return errors.New("docx has an unsafe entry name (empty)")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("docx has an unsafe entry name %q", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("docx has an unsafe entry name %q", name)
	}
	return nil
}

// readEntry decompresses one entry while accumulating the running total, so
// a zip bomb is stopped mid-stream rather than after it has been fully
// expanded into memory.
func readEntry(f *zip.File, total *int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open entry %s: %w", f.Name, err)
	}
	defer rc.Close()

	remaining := maxDecompressedBytes - *total
	if remaining <= 0 {
		return nil, fmt.Errorf("docx decompresses to more than %d MB", int64(maxDecompressedBytes)>>20)
	}
	// Read one byte past the budget so an over-limit entry is detectable.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(rc, remaining+1))
	if err != nil {
		return nil, fmt.Errorf("read entry %s: %w", f.Name, err)
	}
	if n > remaining {
		return nil, fmt.Errorf("docx decompresses to more than %d MB", int64(maxDecompressedBytes)>>20)
	}
	*total += n
	return buf.Bytes(), nil
}

// readEntryRaw captures the entry's compressed bytes so WriteTo can copy
// untouched entries verbatim.
func readEntryRaw(f *zip.File) ([]byte, error) {
	rc, err := f.OpenRaw()
	if err != nil {
		return nil, fmt.Errorf("open raw entry %s: %w", f.Name, err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read raw entry %s: %w", f.Name, err)
	}
	return data, nil
}

// Part returns an entry's decompressed content.
func (p *Package) Part(name string) ([]byte, bool) {
	data, ok := p.parts[name]
	return data, ok
}

// SetPart stages replacement content for an existing entry. Adding new
// entries is out of scope for P1, so an unknown name is an error rather
// than a silent no-op: a caller that mistypes a part name would otherwise
// write the untouched original and be told it succeeded.
func (p *Package) SetPart(name string, data []byte) error {
	if _, ok := p.parts[name]; !ok {
		return fmt.Errorf("docx has no entry %q (adding entries is not supported)", name)
	}
	p.parts[name] = data
	p.modified[name] = true
	return nil
}

// Names returns the entry names in their original order.
func (p *Package) Names() []string {
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// WriteTo writes the package to path atomically (temp file + rename), so an
// interrupted write can never leave a truncated .docx behind. Untouched
// entries are copied as raw compressed bytes, which both preserves their
// decompressed content exactly and avoids recompressing megabytes of media.
func (p *Package) WriteTo(dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".docx-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	zw := zip.NewWriter(tmp)
	for _, name := range p.names {
		header := *p.headers[name]
		if p.modified[name] {
			header.Method = zip.Deflate
			w, err := zw.CreateHeader(&header)
			if err != nil {
				return fmt.Errorf("write entry %s: %w", name, err)
			}
			if _, err := w.Write(p.parts[name]); err != nil {
				return fmt.Errorf("write entry %s: %w", name, err)
			}
			continue
		}
		w, err := zw.CreateRaw(&header)
		if err != nil {
			return fmt.Errorf("copy entry %s: %w", name, err)
		}
		if _, err := w.Write(p.raw[name]); err != nil {
			return fmt.Errorf("copy entry %s: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finalize zip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/docx/ -run 'TestOpen|TestWriteTo' -v`
Expected: 全部 PASS。

若 `TestWriteTo_UntouchedPartsAreByteIdentical` 失败并提示条目内容不一致，检查 `CreateRaw` 是否连同原 `FileHeader` 一起用（`CreateRaw` 要求 header 里的 `CRC32`、`CompressedSize64`、`UncompressedSize64` 与 raw 数据匹配——这三个字段来自 `f.FileHeader`，不要清零）。

- [ ] **Step 5: vet 与全仓库构建**

Run: `go vet ./pkg/docx/... && go build ./...`
Expected: 无输出，退出码 0。

---

### Task 3: scan.go —— 段落/run 字节区间索引

**Files:**
- Create: `pkg/docx/scan.go`
- Test: `pkg/docx/scan_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Open` / `Part(DocumentPart)`
- Produces:
  ```go
  type Span struct{ Start, End int }
  type Run struct {
      Index       int
      Text        string
      Content     Span // byte range of the <w:t> text content
      Start       Span // byte range of the <w:t> start tag itself
      HasPreserve bool
  }
  type Para struct {
      Index   int
      Runs    []Run
      Span    Span // byte range of the whole <w:p> element
      InTable bool
  }
  func Scan(documentXML []byte) ([]Para, error)
  ```
  `Index` 均为 1-based。`Scan` 只索引正文可见文本：`<w:delText>`（已删除的修订文本）被跳过，`<w:ins>` 内的 run 被纳入。run 扫描递归进 `<w:hyperlink>` / `<w:smartTag>` / `<w:ins>` 等容器。表格内 `<w:p>` 计入同一条线性 `Para.Index` 并置 `InTable = true`。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/scan_test.go`：

```go
package docx

import (
	"strings"
	"testing"
)

// scanFixture opens the shared fixture and scans its document body.
func scanFixture(t *testing.T) ([]byte, []Para) {
	t.Helper()
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return doc, paras
}

// paraText joins a paragraph's run text, which is the visible text of the
// paragraph.
func paraText(p Para) string {
	var b strings.Builder
	for _, r := range p.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func findPara(paras []Para, want string) (Para, bool) {
	for _, p := range paras {
		if paraText(p) == want {
			return p, true
		}
	}
	return Para{}, false
}

func TestScan_MultiRunParagraph(t *testing.T) {
	_, paras := scanFixture(t)
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatalf("multi-run paragraph not found; got %d paragraphs", len(paras))
	}
	if len(p.Runs) != 3 {
		t.Errorf("Runs = %d, want 3", len(p.Runs))
	}
	wantRuns := []string{"Hello ", "bold", " world"}
	for i, want := range wantRuns {
		if i >= len(p.Runs) {
			break
		}
		if p.Runs[i].Text != want {
			t.Errorf("Runs[%d].Text = %q, want %q", i, p.Runs[i].Text, want)
		}
		if p.Runs[i].Index != i+1 {
			t.Errorf("Runs[%d].Index = %d, want %d", i, p.Runs[i].Index, i+1)
		}
	}
}

// TestScan_DecodesEntities pins that Text is the DECODED string. Callers
// must never assume Text's character offsets map linearly onto Content's
// byte offsets.
func TestScan_DecodesEntities(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "Tom & Jerry <fast>"); !ok {
		var got []string
		for _, p := range paras {
			got = append(got, paraText(p))
		}
		t.Fatalf("entity paragraph not found; paragraphs: %q", got)
	}
}

// TestScan_ContentSpanIsRawBytes pins that Content delimits the RAW
// (still-escaped) bytes inside <w:t>, which is what splice replaces.
func TestScan_ContentSpanIsRawBytes(t *testing.T) {
	doc, paras := scanFixture(t)
	p, ok := findPara(paras, "Tom & Jerry <fast>")
	if !ok {
		t.Fatal("entity paragraph not found")
	}
	if len(p.Runs) != 1 {
		t.Fatalf("Runs = %d, want 1", len(p.Runs))
	}
	raw := string(doc[p.Runs[0].Content.Start:p.Runs[0].Content.End])
	if raw != "Tom &amp; Jerry &lt;fast&gt;" {
		t.Errorf("raw content = %q, want the escaped form", raw)
	}
}

func TestScan_DetectsXMLSpacePreserve(t *testing.T) {
	_, paras := scanFixture(t)
	p, ok := findPara(paras, " padded text ")
	if !ok {
		t.Fatal("preserve paragraph not found")
	}
	if len(p.Runs) != 1 {
		t.Fatalf("Runs = %d, want 1", len(p.Runs))
	}
	if !p.Runs[0].HasPreserve {
		t.Error("HasPreserve = false, want true")
	}
}

// TestScan_RecursesIntoHyperlink guards the container-nesting rule: <w:r>
// is not always a direct child of <w:p>.
func TestScan_RecursesIntoHyperlink(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "link text"); !ok {
		t.Error("hyperlink run was not indexed")
	}
}

// TestScan_RevisionMarks pins the two halves of the revision rule:
// <w:ins> content is visible text and is indexed; <w:delText> is deleted
// text and must be excluded.
func TestScan_RevisionMarks(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "inserted"); !ok {
		t.Error("w:ins run was not indexed, want it included")
	}
	for _, p := range paras {
		if strings.Contains(paraText(p), "deleted") {
			t.Errorf("w:delText leaked into paragraph %d: %q", p.Index, paraText(p))
		}
	}
}

func TestScan_TableParagraphsAreIndexedInline(t *testing.T) {
	_, paras := scanFixture(t)
	var found int
	for _, p := range paras {
		if strings.HasPrefix(paraText(p), "cell ") {
			found++
			if !p.InTable {
				t.Errorf("paragraph %d %q: InTable = false, want true", p.Index, paraText(p))
			}
		}
	}
	if found != 4 {
		t.Errorf("found %d table paragraphs, want 4", found)
	}
}

func TestScan_IndicesAreSequential(t *testing.T) {
	_, paras := scanFixture(t)
	if len(paras) == 0 {
		t.Fatal("no paragraphs")
	}
	for i, p := range paras {
		if p.Index != i+1 {
			t.Fatalf("paras[%d].Index = %d, want %d", i, p.Index, i+1)
		}
	}
}

// TestScan_ParaSpanCoversElement pins that Para.Span delimits the whole
// <w:p> element, which later tasks rely on for paragraph-level operations.
func TestScan_ParaSpanCoversElement(t *testing.T) {
	doc, paras := scanFixture(t)
	for _, p := range paras {
		got := string(doc[p.Span.Start:p.Span.End])
		if !strings.HasPrefix(got, "<w:p") {
			t.Fatalf("paragraph %d span does not start at <w:p: %.40q", p.Index, got)
		}
		if !strings.HasSuffix(got, "</w:p>") {
			t.Fatalf("paragraph %d span does not end at </w:p>: %.40q", p.Index, got[max(0, len(got)-40):])
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run TestScan -v`
Expected: 编译失败，报 `undefined: Scan`、`undefined: Para`、`undefined: Span`、`undefined: Run`。

- [ ] **Step 3: 实现 scan.go**

创建 `pkg/docx/scan.go`：

```go
package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Span is a half-open byte range [Start, End) into the raw document.xml.
type Span struct {
	Start int
	End   int
}

// Run is one <w:t> text node together with the byte ranges needed to patch
// it in place.
type Run struct {
	// Index is the 1-based position of this run within its paragraph.
	Index int
	// Text is the DECODED text. Its character offsets do NOT map linearly
	// onto Content's byte offsets, because entities and character
	// references decode to a different length. Never derive byte offsets
	// from Text — replace the whole Content span instead.
	Text string
	// Content is the raw byte range between <w:t> and </w:t>.
	Content Span
	// Start is the byte range of the <w:t> start tag itself, so callers can
	// rewrite it to add xml:space="preserve".
	Start Span
	// HasPreserve reports whether the start tag already carries
	// xml:space="preserve".
	HasPreserve bool
}

// Para is one <w:p> element and the visible-text runs inside it.
type Para struct {
	// Index is the 1-based position of this paragraph in the linear
	// document order, table paragraphs included.
	Index int
	Runs  []Run
	// Span is the byte range of the whole <w:p> element, start tag through
	// end tag.
	Span Span
	// InTable reports whether this paragraph lives inside a <w:tbl>.
	InTable bool
}

// Scan indexes document.xml without building a DOM. It walks tokens with
// encoding/xml purely as a scanner, recording byte offsets from
// Decoder.InputOffset so edits can splice the original bytes directly.
//
// Visible-text rules: <w:delText> (already-deleted revision text) is
// skipped, while runs inside <w:ins> are included, matching what a reader
// sees in Word. Runs are found recursively, because <w:r> is not always a
// direct child of <w:p> — <w:hyperlink>, <w:smartTag> and <w:ins> all nest
// it one level deeper.
func Scan(documentXML []byte) ([]Para, error) {
	dec := xml.NewDecoder(bytes.NewReader(documentXML))

	var (
		paras []Para
		// prevOffset trails InputOffset by one token. InputOffset reports
		// the END of the token just returned, so the start of an element's
		// content is the offset taken right after its StartElement, and the
		// end of that content is the offset recorded BEFORE its EndElement
		// was consumed — that is, prevOffset.
		prevOffset int
		tableDepth int

		inPara      bool
		paraStart   int
		paraRuns    []Run
		paraInTable bool

		inText      bool
		textStart   int
		textTagSpan Span
		textPreserve bool
		textBuf     strings.Builder
	)

	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF is the only clean termination; anything else is a real
			// parse failure.
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("scan document.xml: %w", err)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			switch localName(t.Name) {
			case "tbl":
				tableDepth++
			case "p":
				// Word does not nest <w:p> inside <w:p>, so a flat flag is
				// enough; guard anyway so a malformed file cannot corrupt
				// the index.
				if !inPara {
					inPara = true
					// prevOffset is the end of the previous token, which is
					// exactly where this <w:p> start tag begins.
					paraStart = prevOffset
					paraRuns = nil
					paraInTable = tableDepth > 0
				}
			case "t":
				if inPara {
					inText = true
					textStart = offset
					textTagSpan = Span{Start: prevOffset, End: offset}
					textPreserve = hasPreserveAttr(t)
					textBuf.Reset()
				}
			case "delText":
				// Deleted revision text is not visible; skip its content by
				// simply not entering text-capture mode.
			}

		case xml.CharData:
			if inText {
				textBuf.Write(t)
			}

		case xml.EndElement:
			switch localName(t.Name) {
			case "tbl":
				if tableDepth > 0 {
					tableDepth--
				}
			case "t":
				if inText {
					paraRuns = append(paraRuns, Run{
						Index:       len(paraRuns) + 1,
						Text:        textBuf.String(),
						Content:     Span{Start: textStart, End: prevOffset},
						Start:       textTagSpan,
						HasPreserve: textPreserve,
					})
					inText = false
				}
			case "p":
				if inPara {
					paras = append(paras, Para{
						Index:   len(paras) + 1,
						Runs:    paraRuns,
						Span:    Span{Start: paraStart, End: offset},
						InTable: paraInTable,
					})
					inPara = false
					paraRuns = nil
				}
			}
		}
		prevOffset = offset
	}
	return paras, nil
}

// localName strips the namespace so w:p and p compare equal. The scanner
// deliberately ignores namespace URIs: WordprocessingML documents in the
// wild use the w: prefix, and matching on local name keeps the scanner
// resilient to prefix remapping.
func localName(n xml.Name) string {
	return n.Local
}

// hasPreserveAttr reports whether a <w:t> start tag carries
// xml:space="preserve", which tells Word to keep leading and trailing
// whitespace.
func hasPreserveAttr(t xml.StartElement) bool {
	for _, a := range t.Attr {
		if a.Name.Local == "space" && a.Value == "preserve" {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/docx/ -run TestScan -v`
Expected: 全部 PASS。

若 `TestScan_ContentSpanIsRawBytes` 失败（取到的原始字节含多余的 `<` 或缺字符），说明 `prevOffset` 的滚动时机不对——确认 `prevOffset = offset` 是在 `switch` 之后、每次循环末尾执行的，且 `textStart` 用的是 `StartElement` 分支里的 `offset`（即 start tag 的结束位置）。

- [ ] **Step 5: vet**

Run: `go vet ./pkg/docx/...`
Expected: 无输出。

---

### Task 4: splice.go —— 补丁应用与逐字节保真验收

**Files:**
- Create: `pkg/docx/splice.go`
- Test: `pkg/docx/splice_test.go`
- Test: `pkg/docx/fidelity_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Package`、Task 3 的 `Span` / `Para` / `Run`
- Produces:
  ```go
  type Patch struct {
      Content     Span
      TagSpan     Span
      NewText     string
      HasPreserve bool
      Old         []byte // raw bytes originally at Content; Apply verifies before splicing
  }
  func Apply(documentXML []byte, patches []Patch) ([]byte, error)
  func PatchRun(documentXML []byte, r Run, newText string) Patch
  ```

> **本节代码块是实施起点，不是最终形态**（同 Task 2）。最终以仓库中的 `pkg/docx/splice.go` 为准。全分支审查后的修订：`PatchRun` 增加 `documentXML` 首参以填充 `Old`（diff 式上下文校验，把陈旧/跨文档/错位 span 变成显式错误）；`Apply` 校验 `TagSpan` 边界（原先越界直接 panic → 进程崩溃）；拒绝 `Content.Start` 相同的补丁（零长度 span 曾绕过重叠检查）；拒绝针对自闭合 `<w:t/>` 的补丁（原先会产出 Word 无法打开的 XML）；改为单遍构建输出（原先每个补丁重建整个文档，2000 补丁 38.5 MB → 611 KB）。
  `Apply` 按 `Content.Start` 降序应用补丁，使前一次替换不会让后续补丁的偏移失效；补丁区间重叠时返回错误。新文本一律经 `xml.EscapeText` 转义。当新文本首尾有空白且原 `<w:t>` 没有 `xml:space="preserve"` 时，`Apply` 重写该 start tag 补上该属性。

- [ ] **Step 1: 写失败的测试**

创建 `pkg/docx/splice_test.go`：

```go
package docx

import (
	"strings"
	"testing"
)

// applyToFixtureRun scans the fixture, patches run runIdx (0-based) of the
// paragraph whose visible text is wantPara, and returns the original and
// patched XML.
func applyToFixtureRun(t *testing.T, wantPara string, runIdx int, newText string) ([]byte, []byte, Run) {
	t.Helper()
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, wantPara)
	if !ok {
		t.Fatalf("paragraph %q not found", wantPara)
	}
	if runIdx >= len(p.Runs) {
		t.Fatalf("paragraph %q has %d runs, want index %d", wantPara, len(p.Runs), runIdx)
	}
	target := p.Runs[runIdx]
	out, err := Apply(doc, []Patch{PatchRun(target, newText)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return doc, out, target
}

// applyToFixture patches the first run of the named paragraph.
func applyToFixture(t *testing.T, wantPara, newText string) ([]byte, []byte) {
	t.Helper()
	doc, out, _ := applyToFixtureRun(t, wantPara, 0, newText)
	return doc, out
}

func TestApply_ReplacesOnlyTargetRun(t *testing.T) {
	doc, out := applyToFixture(t, "Hello bold world", "Howdy ")
	if !strings.Contains(string(out), "<w:t>Howdy </w:t>") &&
		!strings.Contains(string(out), `<w:t xml:space="preserve">Howdy </w:t>`) {
		t.Errorf("replacement not found in output")
	}
	// The other two runs of the same paragraph must survive untouched.
	for _, want := range []string{"bold", " world"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("sibling run %q was lost", want)
		}
	}
	if len(out) == len(doc) && string(out) == string(doc) {
		t.Error("output is identical to input, patch did not apply")
	}
}

// TestApply_EscapesNewText is the guard for the most common corruption
// cause: unescaped & or < produces XML Word refuses to open.
func TestApply_EscapesNewText(t *testing.T) {
	_, out := applyToFixture(t, "Hello bold world", "A & B < C")
	s := string(out)
	if !strings.Contains(s, "A &amp; B &lt; C") {
		t.Errorf("new text was not XML-escaped; output lacks the escaped form")
	}
	if strings.Contains(s, "A & B < C") {
		t.Errorf("raw unescaped text leaked into the document")
	}
}

// TestApply_RewritesEntityRunFromDecodedText pins the find rule: the caller
// works on decoded text and the whole content span is rewritten, so no
// character-to-byte mapping is ever needed.
func TestApply_RewritesEntityRunFromDecodedText(t *testing.T) {
	_, out := applyToFixture(t, "Tom & Jerry <fast>", "Tom & Jerry <slow>")
	s := string(out)
	if !strings.Contains(s, "Tom &amp; Jerry &lt;slow&gt;") {
		t.Errorf("rewritten entity run not found in escaped form")
	}
	if strings.Contains(s, "&lt;fast&gt;") {
		t.Errorf("old content survived the patch")
	}
}

// TestApply_AddsPreserveWhenNewTextHasEdgeWhitespace targets the "bold" run
// specifically: python-docx already emits xml:space="preserve" on the runs
// whose text has edge whitespace ("Hello " and " world"), so patching those
// would pass without ever exercising the attribute-adding path. "bold" has
// no such attribute, which is exactly what makes it the right target.
func TestApply_AddsPreserveWhenNewTextHasEdgeWhitespace(t *testing.T) {
	_, out, target := applyToFixtureRun(t, "Hello bold world", 1, "  spaced out  ")
	if target.Text != "bold" {
		t.Fatalf("target run text = %q, want %q — fixture layout changed", target.Text, "bold")
	}
	if target.HasPreserve {
		t.Fatal("target run already has xml:space=preserve; this test would pass vacuously")
	}
	if !strings.Contains(string(out), `<w:t xml:space="preserve">  spaced out  </w:t>`) {
		t.Error("xml:space=preserve was not added to the patched <w:t>")
	}
}

// TestApply_LeavesTagAloneWhenNoEdgeWhitespace is the negative half: a run
// without the attribute must not gain one when it does not need it.
func TestApply_LeavesTagAloneWhenNoEdgeWhitespace(t *testing.T) {
	_, out, target := applyToFixtureRun(t, "Hello bold world", 1, "italic")
	if target.HasPreserve {
		t.Fatal("target run already has xml:space=preserve; this test would pass vacuously")
	}
	if !strings.Contains(string(out), "<w:t>italic</w:t>") {
		t.Error("patched tag should stay bare when the text has no edge whitespace")
	}
}

func TestApply_KeepsExistingPreserveWithoutDuplicating(t *testing.T) {
	_, out := applyToFixture(t, " padded text ", " still padded ")
	s := string(out)
	if strings.Contains(s, `xml:space="preserve" xml:space="preserve"`) {
		t.Error("xml:space=preserve was duplicated")
	}
	if !strings.Contains(s, " still padded ") {
		t.Error("replacement text not found")
	}
}

func TestApply_DescendingOrderKeepsOffsetsValid(t *testing.T) {
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
		t.Fatal("multi-run paragraph not found")
	}
	// Patch runs 1 and 3, deliberately passed in ascending order so the
	// implementation must sort them itself.
	out, err := Apply(doc, []Patch{
		PatchRun(p.Runs[0], "AAAA"),
		PatchRun(p.Runs[2], "ZZZZ"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "AAAA") || !strings.Contains(s, "ZZZZ") {
		t.Errorf("both patches should be present")
	}
	if !strings.Contains(s, "bold") {
		t.Errorf("untouched middle run was damaged")
	}
}

func TestApply_RejectsOverlappingPatches(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := paras[0].Runs[0]
	_, err = Apply(doc, []Patch{PatchRun(r, "a"), PatchRun(r, "b")})
	if err == nil {
		t.Fatal("Apply accepted overlapping patches, want error")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error = %q, want it to mention overlap", err)
	}
}

func TestApply_NoPatchesReturnsInputUnchanged(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
	out, err := Apply(doc, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(out) != string(doc) {
		t.Errorf("output = %q, want the input unchanged", out)
	}
}
```

创建 `pkg/docx/fidelity_test.go`：

```go
package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

// assertWellFormed walks every token, which fails on any malformed XML
// (unbalanced tags, bad escapes) without caring about the schema.
func assertWellFormed(t *testing.T, data []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("patched document.xml is not well-formed: %v", err)
		}
	}
}

// TestFidelity_SingleWordEditKeepsEverythingElseIdentical is the P1
// acceptance gate from the design doc §10. Changing one word in one
// paragraph must leave every other zip entry byte-identical, and must leave
// document.xml itself untouched outside the target <w:t>.
//
// This is the test that fails loudly if the implementation ever regresses
// to DOM rebuilding.
func TestFidelity_SingleWordEditKeepsEverythingElseIdentical(t *testing.T) {
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
	target := p.Runs[0]

	patched, err := Apply(doc, []Patch{PatchRun(target, "Howdy ")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. Everything before the patched run's start tag is untouched.
	if string(doc[:target.Start.Start]) != string(patched[:target.Start.Start]) {
		t.Error("bytes before the target <w:t> changed")
	}
	// 2. Everything after the patched run's content is untouched.
	oldTail := string(doc[target.Content.End:])
	newTail := string(patched[len(patched)-len(oldTail):])
	if oldTail != newTail {
		t.Error("bytes after the target <w:t> changed")
	}
	// 3. The patched document is still well-formed XML. Walking every token
	// is the direct check; Unmarshal would conflate schema mismatch with
	// malformedness.
	assertWellFormed(t, patched)

	// 4. Write the package back out and compare entry by entry.
	if err := pkg.SetPart(DocumentPart, patched); err != nil {
		t.Fatalf("SetPart: %v", err)
	}
	out := filepath.Join(t.TempDir(), "patched.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	assertEntriesEqual(t, fixture, out, map[string]bool{DocumentPart: true})
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/docx/ -run 'TestApply|TestFidelity' -v`
Expected: 编译失败，报 `undefined: Apply`、`undefined: Patch`、`undefined: PatchRun`。

- [ ] **Step 3: 实现 splice.go**

创建 `pkg/docx/splice.go`：

```go
package docx

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Patch replaces one <w:t> element's text content with NewText.
//
// The whole content span is rewritten rather than a sub-range of it. That
// is deliberate: the decoded text a caller matches against does not map
// linearly onto the raw bytes (entities and character references decode to
// a different length), so sub-range splicing would corrupt any run holding
// an escape. Rewriting the full span needs only one offset pair and no
// character-to-byte mapping at all.
type Patch struct {
	// Content is the raw byte range between <w:t> and </w:t>.
	Content Span
	// TagSpan is the byte range of the <w:t> start tag, rewritten only when
	// xml:space="preserve" has to be added.
	TagSpan Span
	// NewText is the replacement text in DECODED form; Apply escapes it.
	NewText string
	// HasPreserve reports whether the start tag already carries
	// xml:space="preserve".
	HasPreserve bool
}

// PatchRun builds a Patch that replaces r's text with newText.
func PatchRun(r Run, newText string) Patch {
	return Patch{
		Content:     r.Content,
		TagSpan:     r.Start,
		NewText:     newText,
		HasPreserve: r.HasPreserve,
	}
}

// Apply rewrites documentXML with the given patches and returns the new
// bytes. The input is never modified.
//
// Patches are applied in descending offset order so that each splice leaves
// the offsets of the not-yet-applied patches valid. Overlapping patches are
// rejected rather than silently resolved, because the caller's intent is
// ambiguous and a wrong guess corrupts the document.
func Apply(documentXML []byte, patches []Patch) ([]byte, error) {
	if len(patches) == 0 {
		out := make([]byte, len(documentXML))
		copy(out, documentXML)
		return out, nil
	}

	ordered := make([]Patch, len(patches))
	copy(ordered, patches)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Content.Start > ordered[j].Content.Start
	})

	for i := range ordered {
		p := ordered[i]
		if p.Content.Start < 0 || p.Content.End > len(documentXML) || p.Content.Start > p.Content.End {
			return nil, fmt.Errorf("patch span [%d,%d) is out of range for a %d byte document",
				p.Content.Start, p.Content.End, len(documentXML))
		}
		if i > 0 {
			// ordered is descending, so the previous patch starts later.
			// Its start must not fall inside this patch's span.
			if ordered[i-1].Content.Start < p.Content.End {
				return nil, fmt.Errorf("patches overlap at byte %d", p.Content.End)
			}
		}
	}

	out := make([]byte, len(documentXML))
	copy(out, documentXML)

	for _, p := range ordered {
		escaped, err := escapeXMLText(p.NewText)
		if err != nil {
			return nil, err
		}
		// Replace the content span first; the start tag sits before it, so
		// rewriting the tag afterwards does not disturb this offset.
		out = spliceBytes(out, p.Content.Start, p.Content.End, escaped)

		if needsPreserve(p.NewText) && !p.HasPreserve {
			tag := out[p.TagSpan.Start:p.TagSpan.End]
			newTag, err := withPreserveAttr(tag)
			if err != nil {
				return nil, err
			}
			out = spliceBytes(out, p.TagSpan.Start, p.TagSpan.End, newTag)
		}
	}
	return out, nil
}

// spliceBytes returns b with [start,end) replaced by repl.
func spliceBytes(b []byte, start, end int, repl []byte) []byte {
	out := make([]byte, 0, len(b)-(end-start)+len(repl))
	out = append(out, b[:start]...)
	out = append(out, repl...)
	out = append(out, b[end:]...)
	return out
}

// escapeXMLText escapes text for inclusion in element content. Unescaped
// & or < is the single most common cause of "Word found unreadable
// content" on a patched document.
func escapeXMLText(s string) ([]byte, error) {
	var buf bytes.Buffer
	if err := xmlEscapeText(&buf, []byte(s)); err != nil {
		return nil, fmt.Errorf("escape replacement text: %w", err)
	}
	return buf.Bytes(), nil
}

// needsPreserve reports whether text has leading or trailing whitespace,
// which Word collapses unless the <w:t> carries xml:space="preserve".
func needsPreserve(s string) bool {
	return s != strings.TrimSpace(s)
}

// withPreserveAttr inserts xml:space="preserve" into a <w:t...> start tag.
// The attribute goes right after the element name, which is always followed
// by either a space, a '/', or the closing '>'.
func withPreserveAttr(tag []byte) ([]byte, error) {
	end := bytes.IndexAny(tag, " \t\r\n/>")
	if end <= 0 {
		return nil, fmt.Errorf("cannot parse <w:t> start tag %q", tag)
	}
	out := make([]byte, 0, len(tag)+len(` xml:space="preserve"`))
	out = append(out, tag[:end]...)
	out = append(out, []byte(` xml:space="preserve"`)...)
	out = append(out, tag[end:]...)
	return out, nil
}
```

再创建 escape 的薄包装（放在 `splice.go` 末尾，避免直接依赖 `encoding/xml` 的换行转义差异）：

```go
// xmlEscapeText escapes the five XML metacharacters. encoding/xml's
// EscapeText additionally rewrites newlines and tabs to character
// references, which would needlessly churn bytes in a document where the
// caller's text legitimately contains them.
func xmlEscapeText(w *bytes.Buffer, s []byte) error {
	for _, b := range s {
		switch b {
		case '&':
			w.WriteString("&amp;")
		case '<':
			w.WriteString("&lt;")
		case '>':
			w.WriteString("&gt;")
		case '"':
			w.WriteString("&quot;")
		case '\'':
			w.WriteString("&apos;")
		default:
			w.WriteByte(b)
		}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/docx/ -run 'TestApply|TestFidelity' -v`
Expected: 全部 PASS。

若 `TestFidelity_...` 的第 2 项（"bytes after the target <w:t> changed"）失败，检查 `Apply` 是否在替换内容区间**之后**才改 start tag——顺序反了会让 `TagSpan` 的偏移失效。

- [ ] **Step 5: 全量测试与 vet**

Run: `go test ./pkg/docx/... -v && go vet ./pkg/docx/... && go build ./...`
Expected: 全部 PASS，vet 无输出，build 成功。

- [ ] **Step 6: 人工验证（P1a 唯一无法自动化的一项）**

本机没有 `soffice` / Word，设计文档 §10 第 4 条（"Word 与 LibreOffice 打开无修复提示"）无法自动验证。生成一份产物供人工确认：

```bash
cd /Users/millken/github.com/millken/deepai && cat > /tmp/gen_patched.go <<'EOF'
//go:build ignore

package main

import (
	"fmt"
	"os"

	"github.com/millken/deepai/pkg/docx"
)

func main() {
	pkg, err := docx.Open("pkg/docx/testdata/structure.docx")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	doc, _ := pkg.Part(docx.DocumentPart)
	paras, err := docx.Scan(doc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := docx.Apply(doc, []docx.Patch{docx.PatchRun(doc, paras[0].Runs[0], "Howdy ")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := pkg.SetPart(docx.DocumentPart, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := pkg.WriteTo("/tmp/patched.docx"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote /tmp/patched.docx")
}
EOF
go run /tmp/gen_patched.go
```
Expected: 打印 `wrote /tmp/patched.docx`。在报告中注明该文件已生成、等待人工用 Word 打开确认无修复提示。

---

## 完成标准

P1a 完成的判据（对应设计文档 §10 第一组验收）：

1. `go test ./pkg/docx/...` 全绿。
2. `TestWriteTo_UntouchedPartsAreByteIdentical` 与 `TestFidelity_SingleWordEditKeepsEverythingElseIdentical` 通过——这两条共同证明 byte-splice 模型在真实 docx 上成立。
3. `go vet ./pkg/docx/...` 无输出，`go build ./...` 成功。
4. `go.mod` 未新增任何依赖。
5. `/tmp/patched.docx` 已生成，等待人工用 Word 确认。

P1b（`read.go` / `edit.go`：分块、游标、protect 校验、find 定位）与 P1c（工具封装、profile、skills）在 P1a 通过后另行规划。
