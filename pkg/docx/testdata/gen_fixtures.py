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
