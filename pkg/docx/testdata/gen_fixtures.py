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
OUTLINE_OUT = os.path.join(HERE, "outline.docx")

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


# FIXED_DATE_TIME pins every zip entry's timestamp so the fixtures are
# byte-reproducible.
#
# Without it, python-docx stamps each entry with the current time (DOS zip
# timestamps have 2-second granularity), so regenerating produced a file whose
# entry CONTENT was byte-identical but whose zip shell differed -- enough to
# dirty a committed binary fixture in git and to make the generator
# unauditable. Note the cause is the zip metadata, not the XML: docProps and
# settings.xml come out identical.
FIXED_DATE_TIME = (2026, 1, 1, 0, 0, 0)


def normalize_zip(path):
    """Rewrites path's zip with fixed entry timestamps, preserving entry order
    and decompressed content exactly. Makes the fixture reproducible so anyone
    can regenerate it and get the committed bytes back."""
    with zipfile.ZipFile(path) as zf:
        names = zf.namelist()
        parts = {n: zf.read(n) for n in names}
    _write_zip(path, names, parts)


def _write_zip(path, names, parts):
    tmp = path + ".tmp"
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zf:
        for n in names:  # preserve original entry order
            info = zipfile.ZipInfo(n, date_time=FIXED_DATE_TIME)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o600 << 16
            zf.writestr(info, parts[n])
    shutil.move(tmp, path)


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
    _write_zip(path, names, parts)


def build_outline_fixture(path):
    """Generates outline.docx: a heading/body-only fixture for P1b's outline
    and chunking tests. Deliberately has no tables, images, headers, or
    footers — structure.docx already covers those, and mixing concerns here
    would make it unclear which fixture a given test failure implicates.
    """
    doc = Document()

    def body(section_label, n):
        for i in range(1, n + 1):
            doc.add_paragraph(f"Body paragraph {i} of section {section_label}.")

    doc.add_heading("Chapter One", level=1)
    body("Chapter One", 2)

    doc.add_heading("Section 1.1", level=2)
    # The one multi-run paragraph in this fixture. It must sit immediately
    # after this heading: Task 4's tests locate it dynamically and assert
    # both whole-paragraph multi-run behavior and cross-run find rejection.
    # Every other paragraph here is single-run, so those two assertions
    # would pass vacuously without this one.
    p = doc.add_paragraph()
    p.add_run("Plain ")
    p.add_run("bold").bold = True
    p.add_run(" tail")
    body("Section 1.1", 3)

    doc.add_heading("Chapter Two", level=1)
    body("Chapter Two", 2)

    doc.add_heading("Section 2.1", level=2)
    body("Section 2.1", 1)

    # Filler paragraphs, purely to push the paragraph count past what the
    # chunking tests need.
    for i in range(1, 61):
        doc.add_paragraph(f"Filler paragraph {i}.")

    doc.save(path)
    normalize_zip(path)


if __name__ == "__main__":
    # Both fixtures are committed and every test binds to them, so generation
    # must be reproducible: re-running this script on an unchanged checkout
    # must leave `git status` clean. normalize_zip pins the entry timestamps
    # that would otherwise make each run differ.
    build_base(OUT)
    inject_raw_xml(OUT)
    print("wrote", OUT)

    build_outline_fixture(OUTLINE_OUT)
    print("wrote", OUTLINE_OUT)
