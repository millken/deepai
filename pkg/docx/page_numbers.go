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

// relationshipsNS is the XML namespace a document.xml root element must
// bind SOME prefix to before any element in its body can legally carry an
// r:id attribute -- copied from write.go's docXMLNamespaceDecl
// (`xmlns:r="..."`, the same literal). planFooterReferenceInsertPatch
// always hard-codes the LITERAL prefix "r:" (not just "whatever prefix
// happens to be bound to this namespace"), so requireRelationshipsPrefix
// below checks for that exact prefix, not merely that the namespace is
// bound to something.
const relationshipsNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

// footerPartNameRe matches an existing word/footerN.xml entry name (no
// leading slash -- a Package.Names() entry), letting nextFooterPartName
// find the smallest N not already in use as a zip entry.
var footerPartNameRe = regexp.MustCompile(`^word/footer([0-9]+)\.xml$`)

// footerOverridePartNameRe is footerPartNameRe's twin for a
// [Content_Types].xml Override's PartName attribute, which is the same
// path with a leading slash ("/word/footer1.xml") -- OPC part names are
// always absolute.
var footerOverridePartNameRe = regexp.MustCompile(`^/word/footer([0-9]+)\.xml$`)

// relationshipIDRe matches a numeric "rIdN"-shaped relationship id, letting
// nextRelationshipID pick a fresh one above the highest already in use. Not
// every producer necessarily names ids this way, which is why
// nextRelationshipID does not stop at "highest matching N plus one" alone
// -- see its own doc comment for the collision-scan fallback that makes
// this regex a good default rather than a hard requirement.
var relationshipIDRe = regexp.MustCompile(`^rId([0-9]+)$`)

// scanContentTypesOverridePartNames returns every Override element's own
// PartName attribute value in ctXML, in document order -- namespace-aware
// XML decoding (encoding/xml), mirroring scanRelationshipIDs, rather than a
// regex over the raw bytes, since an Override's attributes can appear in
// any order.
func scanContentTypesOverridePartNames(ctXML []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(ctXML))
	var names []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Override" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "PartName" {
				names = append(names, a.Value)
			}
		}
	}
	return names, nil
}

// nextFooterPartName picks word/footerN.xml, N the smallest positive
// integer not already used by any word/footerN.xml entry in NEITHER names
// (the package's actual zip entries) NOR any existing [Content_Types].xml
// Override for that same path. A document that already has a footer
// (word/footer1.xml, almost always) has already been ruled out from
// reaching this function at all -- see addPageNumberFooter's "already has
// a footerReference somewhere" bail-out -- but a document CAN have an
// unreferenced word/footer1.xml zip entry with nothing pointing at it (an
// orphaned part Word itself left behind, or one this same call is retried
// against after a prior partial write some other tool made), or a
// [Content_Types].xml Override declared for a part that is not (yet, or
// any longer) an actual zip entry, so this always scans BOTH rather than
// assuming either one alone is authoritative. Skipping past a name only
// declared in Content_Types (not a real entry) avoids ever emitting a
// SECOND Override for the same PartName -- review round-3, item 2: a
// document whose Content_Types already lists an Override for the N this
// function would otherwise have picked must not get a duplicate.
func nextFooterPartName(names []string, ctXML []byte) (string, error) {
	used := map[int]bool{}
	for _, n := range names {
		m := footerPartNameRe.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		if num, err := strconv.Atoi(m[1]); err == nil {
			used[num] = true
		}
	}
	overridePartNames, err := scanContentTypesOverridePartNames(ctXML)
	if err != nil {
		return "", fmt.Errorf("scan content types override part names: %w", err)
	}
	for _, n := range overridePartNames {
		m := footerOverridePartNameRe.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		if num, err := strconv.Atoi(m[1]); err == nil {
			used[num] = true
		}
	}
	for i := 1; ; i++ {
		if !used[i] {
			return fmt.Sprintf("word/footer%d.xml", i), nil
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

// scanRootElement scans data (a whole XML part, e.g. [Content_Types].xml or
// word/_rels/document.xml.rels) for its single element at depth 0,
// returning it as an elemInfo the same shape scanSectPrs already returns
// for <w:sectPr>: tagSpan/selfClosing/attrs describe the root's own start
// tag, and closeStart is where insertBeforeRootClose should splice for a
// non-self-closing root. Both parts this file patches have exactly one
// element at depth 0 by construction (OPC parts are single-rooted), so a
// plain depth counter (rather than name matching) is enough to find it.
func scanRootElement(data []byte) (elemInfo, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var prevOffset, depth int
	var root elemInfo
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return elemInfo{}, err
		}
		offset := int(dec.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				span := Span{prevOffset, offset}
				sc := isSelfClosingSpan(data, span)
				root = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if sc {
					root.closeStart = span.End
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 && root.found && !root.selfClosing {
				root.closeStart = prevOffset
			}
		}
		prevOffset = offset
	}
	if !root.found {
		return elemInfo{}, errors.New("no root element found")
	}
	return root, nil
}

// expandSelfClosingRoot rewrites data's self-closing root element (root,
// from scanRootElement) into an ordinary open/close pair with insertXML as
// its sole child, preserving the start tag's own attributes byte-for-byte.
// This is [Content_Types].xml/word/_rels/document.xml.rels's counterpart to
// planMarginPatches' self-closing <w:sectPr/> handling (format.go) -- the
// same "nothing to anchor an insertion against, so expand in place" shape,
// just for a package-infrastructure part instead of a WordprocessingML one,
// which is why it cannot reuse buildTag (buildTag hardcodes a "w:" prefix
// no infrastructure part's root element carries -- Types/Relationships
// roots are unprefixed).
//
// A self-closing root (e.g. a bare `<Relationships xmlns="..."/>` with zero
// relationships, or `<Types .../>` with zero overrides) is a real shape a
// producer can legally emit and this package must not corrupt: review
// round-3 item 3 caught that the pre-fix insertBeforeRootClose returned an
// offset immediately AFTER the whole self-closing tag (encoding/xml
// synthesizes a same-offset EndElement for one), which is OUTSIDE the root
// element entirely -- splicing there would produce a second top-level
// element sitting next to the closed root, not a child of it, which the
// OPC/XML spec (and every real parser) rejects as malformed.
func expandSelfClosingRoot(data []byte, root elemInfo, insertXML string) string {
	openTag := string(data[root.tagSpan.Start:root.tagSpan.End])
	withoutSlash := strings.TrimSuffix(openTag, "/>") + ">"
	// The raw tag name, exactly as written -- <Name ...>/<Name/> -- read
	// from the same bytes withoutSlash was derived from (not from parsed
	// xml.Name, which drops the sort of literal prefix an OPC infrastructure
	// part is never expected to carry but which this stays correct for
	// regardless).
	nameEnd := strings.IndexAny(openTag[1:], " \t\r\n/>")
	name := openTag[1 : 1+nameEnd]
	return withoutSlash + insertXML + "</" + name + ">"
}

// insertBeforeRootClose is the narrow-patch primitive both
// [Content_Types].xml and word/_rels/document.xml.rels use: insertXML lands
// immediately before the part's root closing tag, so it becomes the LAST
// child of whatever root element the part already has (Types or
// Relationships) without touching a single byte of the part's existing
// content -- the "窄补丁" (narrow patch) task 12 brief item 2 requires for
// both files. A self-closing root (see expandSelfClosingRoot) is expanded
// in place instead of ever splicing outside it.
func insertBeforeRootClose(data []byte, insertXML string) ([]byte, error) {
	root, err := scanRootElement(data)
	if err != nil {
		return nil, err
	}
	if root.selfClosing {
		return Apply(data, []Patch{PatchRawSpan(data, root.tagSpan, expandSelfClosingRoot(data, root, insertXML))})
	}
	return Apply(data, []Patch{PatchRawSpan(data, Span{root.closeStart, root.closeStart}, insertXML)})
}

// requireRelationshipsPrefix returns a descriptive error if documentXML's
// root element does not bind the LITERAL prefix "r" to relationshipsNS,
// rather than letting addPageNumberFooter proceed to write an
// r:id="..." attribute under a prefix Word has never heard declared for
// this document -- review round-3 item 1 (critical): a document whose root
// element never declares xmlns:r at all (or binds relationshipsNS to some
// OTHER prefix, or binds "r" to some OTHER namespace) would otherwise get
// reported as a successful page_numbers call while producing a .docx Word
// refuses to open, because r:id lives in a namespace this specific
// document's root element never bound that prefix to. This mirrors
// requireWordNamespacePrefix's own "identify-able-or-not, refuse
// definitively rather than ever write a namespace-broken part" contract
// (task 9 brief, item 5), just for the "r" prefix instead of "w".
//
// This check is deliberately run only on the AFFIRMATIVE add-footer path
// (after addPageNumberFooter's own "already has a footer, no-op" bail-out),
// not up front for every page_numbers:true call: a document that already
// has a footer is left completely untouched regardless of whatever its
// namespace declarations look like, so gating that no-op behind an
// unrelated namespace check would turn a harmless no-op into a spurious
// error.
func requireRelationshipsPrefix(documentXML []byte) error {
	root, err := scanRootElement(documentXML)
	if err != nil {
		return fmt.Errorf("docx: %s could not be read as XML", DocumentPart)
	}
	for _, a := range root.attrs {
		if a.Name.Space == "xmlns" && a.Name.Local == "r" && a.Value == relationshipsNS {
			return nil
		}
	}
	return fmt.Errorf(
		`docx: %s's root element does not bind the "r" namespace prefix to %s; cannot safely add a <w:footerReference r:id="..."> to it`,
		DocumentPart, relationshipsNS)
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

// pageNumberCaveatNotes reports behavior a caller should know about even
// though addPageNumberFooter is about to add the footer exactly as
// requested (review round-3 item 4): the newly-added footerReference is
// always w:type="default", so it does NOT cover
//
//   - a section using <w:titlePg/> (a distinct first page, which needs its
//     own w:type="first" footerReference to show page numbers at all), or
//   - a document with word/settings.xml's <w:evenAndOddHeaders/> set
//     (distinct odd/even pages, which needs its own w:type="even"
//     footerReference for even pages).
//
// Neither condition changes what this call actually does -- only a
// w:type="default" footerReference is ever added, matching this field's
// documented contract -- so these are notes, not errors: sects/
// childrenList is scanSectPrs' own output (already computed by the caller),
// and settingsXML is word/settings.xml's content, or nil if the package has
// none (some documents omit it entirely; evenAndOddHeaders is then
// necessarily unset).
func pageNumberCaveatNotes(childrenList []map[string]elemInfo, settingsXML []byte) []string {
	var notes []string

	hasTitlePg := false
	for _, children := range childrenList {
		if _, ok := children["titlePg"]; ok {
			hasTitlePg = true
			break
		}
	}
	if hasTitlePg {
		notes = append(notes, "this document has a section with <w:titlePg/> (a distinct first-page layout); "+
			"the added footer is w:type=\"default\" only, so the first page will not show this page number "+
			"unless a w:type=\"first\" footerReference is also added")
	}

	if settingsXML != nil && bytes.Contains(settingsXML, []byte("<w:evenAndOddHeaders")) {
		notes = append(notes, "this document's settings.xml sets <w:evenAndOddHeaders/> (distinct odd/even "+
			"page layout); the added footer is w:type=\"default\" only, so even pages will not show this page "+
			"number unless a w:type=\"even\" footerReference is also added")
	}

	return notes
}

// addPageNumberFooter implements FormatOptions.PageNumbers: see that
// field's doc comment (format.go) for the full contract. It either
// leaves the package completely untouched (every existing <w:sectPr>
// already has its own footerReference -- applied is "" and notes explain
// why) or touches exactly four parts: a brand-new word/footerN.xml
// (Package.AddPart), one narrow Override in [Content_Types].xml, one
// narrow Relationship in word/_rels/document.xml.rels, and one narrow
// <w:footerReference> insertion per <w:sectPr> that lacked one.
func (d *Document) addPageNumberFooter() (applied string, notes []string, err error) {
	doc, ok := d.Part(DocumentPart)
	if !ok {
		return "", nil, fmt.Errorf("docx: package has no %s part", DocumentPart)
	}
	sects, childrenList, err := scanSectPrs(doc)
	if err != nil {
		return "", nil, fmt.Errorf("docx: scan document.xml for sectPr: %w", err)
	}
	if len(sects) == 0 {
		return "", nil, errors.New("docx: document.xml has no <w:sectPr> element; cannot add page numbers")
	}
	for _, children := range childrenList {
		if _, ok := children["footerReference"]; ok {
			// At least one section already has its own footer: leave
			// everything alone rather than guess whether a second footer
			// part (or a second footerReference on top of an existing one)
			// is what the caller actually wanted (task 12 brief, item 2).
			return "", []string{"document already has a footer; not modified"}, nil
		}
	}

	// Every section lacks a footerReference, so this call is committed to
	// actually writing an r:id attribute below: verify the document's root
	// element can legally carry one BEFORE anything is mutated (review
	// round-3, item 1 -- critical).
	if err := requireRelationshipsPrefix(doc); err != nil {
		return "", nil, err
	}

	const relsName = "word/_rels/document.xml.rels"
	relsXML, ok := d.Part(relsName)
	if !ok {
		return "", nil, fmt.Errorf("docx: package has no %s part; cannot add a footer relationship", relsName)
	}
	ctXML, ok := d.Part(contentTypesPart)
	if !ok {
		return "", nil, fmt.Errorf("docx: package has no %s part", contentTypesPart)
	}

	footerPart, err := nextFooterPartName(d.pkg.Names(), ctXML)
	if err != nil {
		return "", nil, fmt.Errorf("docx: pick a footer part name: %w", err)
	}
	footerRelID, err := nextRelationshipID(relsXML)
	if err != nil {
		return "", nil, fmt.Errorf("docx: allocate a relationship id in %s: %w", relsName, err)
	}

	// 1. The new footer part itself -- word/footerN.xml, reusing the exact
	// centered-PAGE-field XML docx_write's own footer1.xml uses (footer.go),
	// so a page number added here renders identically to one docx_write
	// produces from scratch.
	if err := d.AddPart(footerPart, []byte(footer1XML)); err != nil {
		return "", nil, fmt.Errorf("docx: add %s: %w", footerPart, err)
	}

	// 2. [Content_Types].xml: one Override, narrow-patched before </Types>
	// (or into a self-closing <Types/>, see expandSelfClosingRoot).
	ctOut, err := insertBeforeRootClose(ctXML, fmt.Sprintf(
		`<Override PartName="/%s" ContentType="%s"/>`, footerPart, footerContentType))
	if err != nil {
		return "", nil, fmt.Errorf("docx: patch %s: %w", contentTypesPart, err)
	}
	if err := d.SetPart(contentTypesPart, ctOut); err != nil {
		return "", nil, err
	}

	// 3. word/_rels/document.xml.rels: one Relationship, narrow-patched
	// before </Relationships> (or into a self-closing <Relationships/>).
	// Target is relative to word/ (the rels part's own source,
	// word/document.xml), matching write.go's own buildDocRelsXML
	// convention ("footer1.xml", not "word/footer1.xml").
	relsOut, err := insertBeforeRootClose(relsXML, fmt.Sprintf(
		`<Relationship Id="%s" Type="%s" Target="%s"/>`,
		footerRelID, footerRelType, strings.TrimPrefix(footerPart, "word/")))
	if err != nil {
		return "", nil, fmt.Errorf("docx: patch %s: %w", relsName, err)
	}
	if err := d.SetPart(relsName, relsOut); err != nil {
		return "", nil, err
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
		return "", nil, fmt.Errorf("docx: apply footer reference: %w", err)
	}
	if err := d.SetPart(DocumentPart, docOut); err != nil {
		return "", nil, err
	}

	settingsXML, _ := d.Part("word/settings.xml")
	notes = pageNumberCaveatNotes(childrenList, settingsXML)

	return fmt.Sprintf("page numbers -> added %s", footerPart), notes, nil
}
