package docx

// This file is Part C ("页面页脚") of docs/superpowers/plans/
// 2026-08-12-docx-chinese-typography.md: every document WriteDocx produces
// gets a centered page-number footer, word/footer1.xml, referenced from the
// document's one section via <w:sectPr><w:footerReference> (write.go's
// documentXMLFooterXML). See write.go's renderCtx.addFooterRelID for how
// this part's relationship id is allocated -- from the exact same counter
// hyperlinks draw from, never a second independent one, which is what makes
// the two id spaces structurally unable to collide regardless of how many
// links a document has.

// footer1Part names the one footer part this package ever writes. WriteDocx
// adds it to every document unconditionally (unlike, say, docProps/core.xml,
// which is title-gated) -- see footer1XML.
const footer1Part = "word/footer1.xml"

// footer1XML is word/footer1.xml's complete content: one centered paragraph
// holding a PAGE field, 9pt (<w:sz w:val="18"/>, half-points) per the
// reference document's own footer (.superpowers/sdd/reference-values.md:
// "页脚 | 居中 PAGE 域，<w:sz w:val="18"/> = 9pt").
//
// <w:fldSimple w:instr=" PAGE "> is the simple form of a Word field: one
// element carrying the field code as an attribute (w:instr) plus a cached
// display result inside it (the literal "1" run below), rather than the
// three-part <w:fldChar w:fldCharType="begin/separate/end"> form real Word
// documents often use for the same field. Both forms are valid OOXML and
// both display a real, live page number; fldSimple is used here because it
// is one element instead of three runs sharing one field, which is simpler
// to generate correctly. Word recalculates a field's displayed value
// against the field code the moment the document is opened, printed, or the
// field is manually updated (F9) -- it does not trust the cached run except
// as what to show before that first recalculation -- so the literal "1"
// here is never a hardcoded page number a reader would actually see on
// page 2 of a multi-page document.
const footer1XML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:p><w:pPr><w:jc w:val="center"/></w:pPr>` +
	`<w:fldSimple w:instr=" PAGE ">` +
	`<w:r><w:rPr><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr><w:t>1</w:t></w:r>` +
	`</w:fldSimple>` +
	`</w:p>` +
	`</w:ftr>`
