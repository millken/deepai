package docx

// This file is task 12 of the docx-capability-review-format fix list
// (.superpowers/sdd/task-12-brief.md): FormatOptions.PageNumbers's whole
// implementation. The tool layer used to refuse page_numbers:true outright
// (format capability review, Important 11) on the theory that adding a
// footer to an arbitrary document needed a multi-part write pkg/docx could
// not do at all -- but every piece of that multi-part write already existed
// somewhere in this package: footer.go's footer1XML is the exact footer
// content docx_write puts in every document it creates, and write.go's own
// buildContentTypesXML/buildDocRelsXML/documentXMLFooterXML show the same
// three landing points (a Content_Types Override, a rels Relationship, and
// a sectPr footerReference) this file wires up -- just for a document
// write.go is building from scratch, where these three land in a package
// Open never saw before. The one genuinely missing piece was zipio.go's
// AddPart (task 12 brief, item 1): SetPart can only ever replace bytes at
// an existing entry, and adding a footer to a document that has none needs
// a brand-new zip entry.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// footerRelType is the relationship Type every word/footerN.xml part is
// registered under -- copied from write.go's buildDocRelsXML, which uses
// the identical literal for the footer1.xml relationship every docx_write
// document gets.
const footerRelType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer"

// footerContentType is the Content_Types Override ContentType for a footer
// part -- copied from write.go's buildContentTypesXML.
const footerContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"

// footerPartNameRe matches an existing word/footerN.xml entry name, letting
// nextFooterPartName find the smallest N not already in use.
var footerPartNameRe = regexp.MustCompile(`^word/footer([0-9]+)\.xml$`)

// relationshipIDRe matches a numeric "rIdN"-shaped relationship id, letting
// nextRelationshipID pick a fresh one above the highest already in use. Not
// every producer necessarily names ids this way, which is why
// nextRelationshipID does not stop at "highest matching N plus one" alone
// -- see its own doc comment for the collision-scan fallback that makes
// this regex a good default rather than a hard requirement.
var relationshipIDRe = regexp.MustCompile(`^rId([0-9]+)$`)

// nextFooterPartName picks word/footerN.xml, N the smallest positive
// integer not already used by any word/footerN.xml entry in names. A
// document that already has a footer (word/footer1.xml, almost always) has
// already been ruled out from reaching this function at all -- see
// addPageNumberFooter's "already has a footerReference somewhere" bail-out
// -- but a document CAN have an unreferenced word/footer1.xml sitting in
// the package with nothing pointing at it (an orphaned part Word itself
// left behind, or one this same call is retried against after a prior
// partial write some other tool made), so this always scans rather than
// assuming footer1 is free.
func nextFooterPartName(names []string) string {
	used := map[int]bool{}
	for _, n := range names {
		m := footerPartNameRe.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		num, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		used[num] = true
	}
	for i := 1; ; i++ {
		if !used[i] {
			return fmt.Sprintf("word/footer%d.xml", i)
		}
	}
}

// scanRelationshipIDs returns every Relationship element's own Id attribute
// value in relsXML, in document order. It is namespace-aware XML decoding
// (encoding/xml), not a regex over the raw bytes, because a Relationship's
// attributes can appear in any order and Id carries no namespace prefix to
// anchor a simpler string search against.
func scanRelationshipIDs(relsXML []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(relsXML))
	var ids []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "Id" {
				ids = append(ids, a.Value)
			}
		}
	}
	return ids, nil
}

// nextRelationshipID picks a relationship id for the new footer part that
// cannot collide with any id already in relsXML: it prefers the
// conventional "rId" + (highest existing rIdN + 1) shape every producer
// this package has seen (python-docx, Word itself, write.go's own
// buildDocRelsXML) actually uses, but does not simply trust that shape is
// safe on its own -- an id that happens not to match the rIdN pattern at
// all (a hand-edited document, say) could still collide with a naively
// constructed "highest N found plus one" guess if that guess coincides with
// one of the non-conforming ids. The loop below re-checks the full existing
// id set on every candidate, so the returned id is guaranteed unique
// regardless of what naming scheme (if any) the document's existing
// relationships follow.
func nextRelationshipID(relsXML []byte) (string, error) {
	ids, err := scanRelationshipIDs(relsXML)
	if err != nil {
		return "", fmt.Errorf("scan relationship ids: %w", err)
	}
	existing := make(map[string]bool, len(ids))
	maxNum := 0
	for _, id := range ids {
		existing[id] = true
		if m := relationshipIDRe.FindStringSubmatch(id); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}
	for n := maxNum + 1; ; n++ {
		candidate := fmt.Sprintf("rId%d", n)
		if !existing[candidate] {
			return candidate, nil
		}
	}
}

// findRootCloseTagStart returns the byte offset where data's single root
// element's own closing tag begins (e.g. the "</Types>" of
// [Content_Types].xml, or the "</Relationships>" of a .rels part) --
// found by namespace-aware XML decoding rather than a literal string
// search, so it works regardless of whatever prefix or whitespace the
// part's producer used for its root element. Both parts this file patches
// ([Content_Types].xml, word/_rels/document.xml.rels) have exactly one
// element at depth 0, so "depth returns to 0" unambiguously means "the root
// element's own end tag".
func findRootCloseTagStart(data []byte) (int, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var prevOffset, depth int
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		offset := int(dec.InputOffset())
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 {
				return prevOffset, nil
			}
		}
		prevOffset = offset
	}
	return 0, errors.New("no root element end tag found")
}

// insertBeforeRootClose is the narrow-patch primitive both
// [Content_Types].xml and word/_rels/document.xml.rels use: insertXML lands
// immediately before the part's root closing tag, so it becomes the LAST
// child of whatever root element the part already has (Types or
// Relationships) without touching a single byte of the part's existing
// content -- the "窄补丁" (narrow patch) task 12 brief item 2 requires for
// both files.
func insertBeforeRootClose(data []byte, insertXML string) ([]byte, error) {
	closeStart, err := findRootCloseTagStart(data)
	if err != nil {
		return nil, err
	}
	return Apply(data, []Patch{PatchRawSpan(data, Span{closeStart, closeStart}, insertXML)})
}

// planFooterReferenceInsertPatch returns the single patch that adds
// <w:footerReference w:type="default" r:id="footerRelID"/> to one
// already-scanned <w:sectPr> (sect/children, scanSectPrs' own output) that
// addPageNumberFooter has already confirmed carries no footerReference of
// its own anywhere in the document.
//
// This is deliberately NOT built through applyLeafOps/buildTag the way
// planMarginPatches builds a missing <w:pgMar>: buildTag hardcodes a "w:"
// prefix on every attribute it renders, but footerReference's r:id lives in
// the RELATIONSHIPS namespace (xmlns:r), not WordprocessingML's -- the one
// element sectPrChildOrder's whole leafOp machinery has ever needed to
// carry a second namespace prefix on. Reusing sectSectPr's scan output and
// sectPrChildOrder (the same schema-order anchor set planMarginPatches
// relies on) while hand-building just this one mixed-prefix tag keeps the
// insertion point logic shared without needing buildTag to grow a
// namespace parameter no other caller would ever use.
func planFooterReferenceInsertPatch(documentXML []byte, sect elemInfo, children map[string]elemInfo, footerRelID string) Patch {
	tag := fmt.Sprintf(`<w:footerReference w:type="default" r:id="%s"/>`, footerRelID)

	if sect.selfClosing {
		// <w:sectPr/> with no properties at all: expand it to hold the new
		// footerReference, exactly like planMarginPatches does for a
		// self-closing sectPr missing pgMar.
		return PatchRawSpan(documentXML, sect.tagSpan,
			buildTag("sectPr", sect.attrs, false)+tag+"</w:sectPr>")
	}

	// Insert immediately before whichever schema-later sibling (footnotePr,
	// endnotePr, type, pgSz, ...) is the FIRST one this section actually
	// has. sectPrChildOrder's own order guarantees this landing spot is
	// always AFTER any existing headerReference (which sorts earlier in the
	// same order, is left completely untouched here, and -- in any
	// well-formed document -- therefore already sits earlier in the actual
	// bytes too) and always BEFORE pgSz, exactly the schema constraint task
	// 12 brief item 2 calls out.
	insertAt := sect.closeStart
	afterFooterRefSlot := false
	for _, name := range sectPrChildOrder {
		if name == "footerReference" {
			afterFooterRefSlot = true
			continue
		}
		if !afterFooterRefSlot {
			continue
		}
		if info, ok := children[name]; ok {
			insertAt = info.tagSpan.Start
			break
		}
	}
	return PatchRawSpan(documentXML, Span{insertAt, insertAt}, tag)
}

// addPageNumberFooter implements FormatOptions.PageNumbers: see that
// field's doc comment (format.go) for the full contract. It either
// leaves the package completely untouched (every existing <w:sectPr>
// already has its own footerReference -- applied is "" and note explains
// why) or touches exactly four parts: a brand-new word/footerN.xml
// (Package.AddPart), one narrow Override in [Content_Types].xml, one
// narrow Relationship in word/_rels/document.xml.rels, and one narrow
// <w:footerReference> insertion per <w:sectPr> that lacked one.
func (d *Document) addPageNumberFooter() (applied string, note string, err error) {
	doc, ok := d.Part(DocumentPart)
	if !ok {
		return "", "", fmt.Errorf("docx: package has no %s part", DocumentPart)
	}
	sects, childrenList, err := scanSectPrs(doc)
	if err != nil {
		return "", "", fmt.Errorf("docx: scan document.xml for sectPr: %w", err)
	}
	if len(sects) == 0 {
		return "", "", errors.New("docx: document.xml has no <w:sectPr> element; cannot add page numbers")
	}
	for _, children := range childrenList {
		if _, ok := children["footerReference"]; ok {
			// At least one section already has its own footer: leave
			// everything alone rather than guess whether a second footer
			// part (or a second footerReference on top of an existing one)
			// is what the caller actually wanted (task 12 brief, item 2).
			return "", "document already has a footer; not modified", nil
		}
	}

	const relsName = "word/_rels/document.xml.rels"
	relsXML, ok := d.Part(relsName)
	if !ok {
		return "", "", fmt.Errorf("docx: package has no %s part; cannot add a footer relationship", relsName)
	}
	ctXML, ok := d.Part(contentTypesPart)
	if !ok {
		return "", "", fmt.Errorf("docx: package has no %s part", contentTypesPart)
	}

	footerPart := nextFooterPartName(d.pkg.Names())
	footerRelID, err := nextRelationshipID(relsXML)
	if err != nil {
		return "", "", fmt.Errorf("docx: allocate a relationship id in %s: %w", relsName, err)
	}

	// 1. The new footer part itself -- word/footerN.xml, reusing the exact
	// centered-PAGE-field XML docx_write's own footer1.xml uses (footer.go),
	// so a page number added here renders identically to one docx_write
	// produces from scratch.
	if err := d.AddPart(footerPart, []byte(footer1XML)); err != nil {
		return "", "", fmt.Errorf("docx: add %s: %w", footerPart, err)
	}

	// 2. [Content_Types].xml: one Override, narrow-patched before </Types>.
	ctOut, err := insertBeforeRootClose(ctXML, fmt.Sprintf(
		`<Override PartName="/%s" ContentType="%s"/>`, footerPart, footerContentType))
	if err != nil {
		return "", "", fmt.Errorf("docx: patch %s: %w", contentTypesPart, err)
	}
	if err := d.SetPart(contentTypesPart, ctOut); err != nil {
		return "", "", err
	}

	// 3. word/_rels/document.xml.rels: one Relationship, narrow-patched
	// before </Relationships>. Target is relative to word/ (the rels
	// part's own source, word/document.xml), matching write.go's own
	// buildDocRelsXML convention ("footer1.xml", not "word/footer1.xml").
	relsOut, err := insertBeforeRootClose(relsXML, fmt.Sprintf(
		`<Relationship Id="%s" Type="%s" Target="%s"/>`,
		footerRelID, footerRelType, strings.TrimPrefix(footerPart, "word/")))
	if err != nil {
		return "", "", fmt.Errorf("docx: patch %s: %w", relsName, err)
	}
	if err := d.SetPart(relsName, relsOut); err != nil {
		return "", "", err
	}

	// 4. word/document.xml: insert <w:footerReference> into every section
	// (there is ordinarily exactly one <w:sectPr>, but a multi-section
	// document can have more -- every one of them lacked a footerReference,
	// per the bail-out check above, so every one gets the SAME footerRelID).
	var patches []Patch
	for i, sect := range sects {
		patches = append(patches, planFooterReferenceInsertPatch(doc, sect, childrenList[i], footerRelID))
	}
	docOut, err := Apply(doc, patches)
	if err != nil {
		return "", "", fmt.Errorf("docx: apply footer reference: %w", err)
	}
	if err := d.SetPart(DocumentPart, docOut); err != nil {
		return "", "", err
	}

	return fmt.Sprintf("page numbers -> added %s", footerPart), "", nil
}
