package docx

import "fmt"

// Document owns the open -> scan -> edit -> save lifecycle for a single
// .docx file. It is the layer the read (§4.1) and edit (§4.2) tools build
// on: they consult Paras()/Notes()/HasRevisions() instead of touching the
// underlying Package or re-running Scan themselves.
type Document struct {
	pkg  *Package
	path string
	// doc is the current word/document.xml content. It aliases pkg's
	// internal storage (see Package.Part), which is fine here because
	// Document never mutates it in place — only rescan replaces it wholesale
	// after an edit lands via SetPart/ApplyToPart.
	doc   []byte
	paras []Para
	notes []string
	// modified reports whether Edit has applied any change since the
	// document was opened or since the last successful Save/SaveAs. See
	// Modified.
	modified bool
}

// OpenDocument opens path as a .docx package, scans its main document part
// once, and caches the result. Every later Paras()/Notes()/HasRevisions()
// call reads from that cache; edits (added in a later task) go through
// rescan to refresh it.
func OpenDocument(path string) (*Document, error) {
	pkg, err := Open(path)
	if err != nil {
		return nil, err
	}
	d := &Document{pkg: pkg, path: path}
	if err := d.rescan(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return d, nil
}

// rescan re-reads word/document.xml from the package and re-runs Scan,
// refreshing doc, paras, and notes. OpenDocument calls it once; Edit calls
// it again after ApplyToPart lands a change, since Part's returned slice is
// only valid up to the next SetPart (see ApplyToPart's doc comment).
//
// notes is recomputed here, not just at open: it depends on d.paras (via
// the SkippedTextBox scan below), and an edit can change which paragraphs
// carry that flag — e.g. deleting the one paragraph that held a skipped
// text box. Computing it once in OpenDocument and never again would leave
// "text boxes were skipped" in Notes() forever, even after the paragraph
// that justified it is gone.
func (d *Document) rescan() error {
	data, ok := d.pkg.Part(DocumentPart)
	if !ok {
		return fmt.Errorf("package has no %s part", DocumentPart)
	}
	paras, err := Scan(data)
	if err != nil {
		return err
	}
	d.doc = data
	d.paras = paras
	d.notes = computeNotes(d.pkg.Names(), paras)
	return nil
}

// Paras returns a read-only snapshot of the document's paragraphs. It is a
// deep copy: callers may freely mutate the returned slice, including each
// Para's nested Runs and Breaks slices, without affecting the document's
// internal state on this or any later call.
func (d *Document) Paras() []Para {
	out := make([]Para, len(d.paras))
	for i, p := range d.paras {
		out[i] = p
		if p.Runs != nil {
			out[i].Runs = make([]Run, len(p.Runs))
			copy(out[i].Runs, p.Runs)
		}
		if p.Breaks != nil {
			out[i].Breaks = make([]int, len(p.Breaks))
			copy(out[i].Breaks, p.Breaks)
		}
		if p.Cell != nil {
			cell := *p.Cell
			out[i].Cell = &cell
		}
	}
	return out
}

// TotalParas returns the number of paragraphs in the cached scan, i.e.
// len(d.Paras()) without paying for the deep copy.
func (d *Document) TotalParas() int {
	return len(d.paras)
}

// Notes returns human-readable declarations of content this Document does
// not expose through Paras(), so a caller (or a downstream reader tool)
// never presents a partial document as if it were complete. An empty slice
// means nothing was omitted.
func (d *Document) Notes() []string {
	out := make([]string, len(d.notes))
	copy(out, d.notes)
	return out
}

// computeNotes inspects a package's part names and its scanned paragraphs
// for content Document does not surface: headers, footers, footnotes,
// endnotes, comments, and any skipped text box. It takes names as a plain
// []string (rather than a *Package) so it can be unit-tested with synthetic
// inputs that don't require building a real .docx fixture.
func computeNotes(names []string, paras []Para) []string {
	var (
		hasHeader    bool
		hasFooter    bool
		hasFootnotes bool
		hasEndnotes  bool
		hasComments  bool
	)
	for _, name := range names {
		switch {
		case matchesPart(name, "word/header", ".xml"):
			hasHeader = true
		case matchesPart(name, "word/footer", ".xml"):
			hasFooter = true
		case name == "word/footnotes.xml":
			hasFootnotes = true
		case name == "word/endnotes.xml":
			hasEndnotes = true
		case name == "word/comments.xml":
			hasComments = true
		}
	}

	var notes []string
	if hasHeader {
		notes = append(notes, "headers present but not included")
	}
	if hasFooter {
		notes = append(notes, "footers present but not included")
	}
	if hasFootnotes {
		notes = append(notes, "footnotes present but not included")
	}
	if hasEndnotes {
		notes = append(notes, "endnotes present but not included")
	}
	if hasComments {
		notes = append(notes, "comments present but not included")
	}
	for _, p := range paras {
		if p.SkippedTextBox {
			notes = append(notes, "one or more text boxes were skipped")
			break
		}
	}
	return notes
}

// matchesPart reports whether name is one of the numbered header*.xml /
// footer*.xml parts (word/header1.xml, word/header2.xml, ...), i.e. it
// starts with prefix and ends with suffix.
func matchesPart(name, prefix, suffix string) bool {
	return len(name) > len(prefix)+len(suffix) &&
		name[:len(prefix)] == prefix &&
		name[len(name)-len(suffix):] == suffix
}

// HasRevisions reports whether any paragraph in the document contains a
// <w:ins> or <w:del> element.
func (d *Document) HasRevisions() bool {
	for _, p := range d.paras {
		if p.HasRevisions {
			return true
		}
	}
	return false
}

// Modified reports whether Edit has applied any change to this Document
// since it was opened, or since the last successful Save/SaveAs, whichever
// is more recent. The tool layer (P1c) uses this to know whether there is
// anything to write back without tracking "has this document been edited"
// out of band itself, and to decide when to back up the on-disk file once
// before the first overwrite (design §8) rather than on every save.
func (d *Document) Modified() bool {
	return d.modified
}

// Save writes the document back to the path it was opened from.
func (d *Document) Save() error {
	return d.SaveAs(d.path)
}

// SaveAs writes the document to path. When no edit has been made,
// Package.WriteTo copies every entry's original compressed bytes verbatim,
// so an untouched SaveAs reproduces the source file's entries byte for
// byte. A successful SaveAs clears Modified(), since the document's current
// state has now been persisted.
func (d *Document) SaveAs(path string) error {
	if err := d.pkg.WriteTo(path); err != nil {
		return err
	}
	d.modified = false
	return nil
}
