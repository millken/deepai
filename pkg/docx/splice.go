package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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
	// SelfClosing must be set (via PatchRun, from Run.SelfClosing) when the
	// target run is a self-closing <w:t/>. Its Content is a zero-length
	// span sitting outside any <w:t> content model, so splicing text there
	// would insert character data directly into <w:r> — invalid OOXML that
	// Word reports as unreadable content. Apply rejects such a patch
	// outright rather than attempting to rewrite <w:t/> into
	// <w:t>...</w:t>, which is out of scope for this fix.
	SelfClosing bool
	// Old is the raw bytes Content was scanned from, i.e. a snapshot of
	// documentXML[Content.Start:Content.End] at scan time. Apply verifies
	// this snapshot still matches before splicing, which is what turns a
	// stale, foreign, or otherwise misaligned span into a loud error instead
	// of a silent corruption: a span scanned from one document (or an
	// earlier version of the same one) applied to another returns a nil
	// error today, splicing at whatever bytes happen to sit at that offset.
	// nil skips the check, so hand-constructed patches in tests keep
	// working without needing to fabricate a matching Old. PatchRun always
	// populates it.
	Old []byte
	// Raw is the ONLY escaping bypass in this package: when true, Apply
	// splices NewText into Content verbatim instead of XML-escaping it, and
	// skips the xml:space="preserve" tag rewrite entirely (TagSpan is
	// meaningless for a raw patch — see PatchRawSpan). It exists solely so
	// §4.2's paragraph-level insert_before/insert_after/delete operations can
	// splice a whole <w:p> (or <w:r>) subtree, which is not text content and
	// must not be escaped. Because Raw can produce malformed XML if the
	// caller hands it a broken subtree, Apply validates well-formedness of
	// the whole result whenever any patch in the batch sets it. Raw is not
	// intended for arbitrary caller-supplied text.
	Raw bool
}

// PatchRun builds a Patch that replaces r's text with newText. documentXML
// must be the same byte slice (or an unmodified copy of it) that r was
// scanned from: PatchRun snapshots r.Content's raw bytes into Patch.Old so
// Apply can detect a stale or foreign span before it splices.
func PatchRun(documentXML []byte, r Run, newText string) Patch {
	old := make([]byte, r.Content.End-r.Content.Start)
	copy(old, documentXML[r.Content.Start:r.Content.End])
	return Patch{
		Content:     r.Content,
		TagSpan:     r.Start,
		NewText:     newText,
		HasPreserve: r.HasPreserve,
		SelfClosing: r.SelfClosing,
		Old:         old,
	}
}

// PatchRawSpan builds a raw Patch that splices rawXML verbatim into span s,
// with no XML escaping. documentXML must be the same byte slice (or an
// unmodified copy of it) s was taken from: like PatchRun, PatchRawSpan
// snapshots the target bytes into Patch.Old so Apply can detect a stale or
// foreign span before it splices. Unlike PatchRun, no TagSpan is set —
// PatchRawSpan targets whole structural subtrees (e.g. a Para.Span for a
// paragraph-level insert or delete), which have no <w:t> start tag to
// rewrite. Passing an empty s (Start == End) inserts rawXML at that point
// without removing anything; passing "" for rawXML deletes the span's
// content, which is how §4.2's paragraph-level delete is expressed with
// s == Para.Span.
func PatchRawSpan(documentXML []byte, s Span, rawXML string) Patch {
	old := make([]byte, s.End-s.Start)
	copy(old, documentXML[s.Start:s.End])
	return Patch{
		Content: s,
		NewText: rawXML,
		Raw:     true,
		Old:     old,
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

	// hasRaw gates the well-formedness check at the end of Apply: it is only
	// ever computed here (a field read on each patch already being visited
	// for validation), never by re-scanning ordered a second time, so
	// tracking it costs nothing extra on the ordinary (non-raw) path.
	hasRaw := false
	for i := range ordered {
		p := ordered[i]
		if p.Raw {
			hasRaw = true
		}
		if p.Content.Start < 0 || p.Content.End > len(documentXML) || p.Content.Start > p.Content.End {
			return nil, fmt.Errorf("patch span [%d,%d) is out of range for a %d byte document",
				p.Content.Start, p.Content.End, len(documentXML))
		}
		// TagSpan is sliced unconditionally below whenever the replacement
		// needs xml:space="preserve" added, so it needs the same bounds
		// check Content already gets, plus the structural rule that a start
		// tag always sits before its own content: TagSpan must end at or
		// before Content.Start. Without this, a hand-built, stale, or
		// foreign TagSpan panics Apply instead of returning an error.
		if p.TagSpan.Start < 0 || p.TagSpan.End > len(documentXML) || p.TagSpan.Start > p.TagSpan.End {
			return nil, fmt.Errorf("patch tag span [%d,%d) is out of range for a %d byte document",
				p.TagSpan.Start, p.TagSpan.End, len(documentXML))
		}
		if p.TagSpan.End > p.Content.Start {
			return nil, fmt.Errorf("patch tag span [%d,%d) overlaps its own content span [%d,%d)",
				p.TagSpan.Start, p.TagSpan.End, p.Content.Start, p.Content.End)
		}
		if p.SelfClosing {
			return nil, fmt.Errorf("patch targets a self-closing <w:t/> at byte %d, which has no content model to splice into; rewriting <w:t/> into <w:t>...</w:t> is not supported", p.Content.Start)
		}
		// Old is a diff-style context check: it catches a span that was
		// scanned from a different document, or from an earlier version of
		// this one, before Apply ever splices at its (now meaningless)
		// offsets. nil skips the check for hand-constructed patches, but
		// PatchRun always sets it.
		if p.Old != nil && !bytes.Equal(documentXML[p.Content.Start:p.Content.End], p.Old) {
			return nil, fmt.Errorf("patch content span [%d,%d) no longer matches the bytes it was scanned from; the document may have changed or this patch belongs to a different document",
				p.Content.Start, p.Content.End)
		}
		if i > 0 {
			// Two patches targeting the same empty span (Content.Start ==
			// Content.End, as P1b's delete op produces) both have
			// Content.Start == Content.End == that shared offset, so the
			// range-overlap check below (previous.Start < this.End) is
			// false for them — 15 < 15 is false — and would let both apply
			// at the same offset with an order that depends on sort.Slice's
			// unspecified tie-break. Equal starts are never valid regardless
			// of span length, so they are rejected before the range check
			// even runs.
			if ordered[i-1].Content.Start == p.Content.Start {
				return nil, fmt.Errorf("patches overlap: two patches target the same span at byte %d", p.Content.Start)
			}
			// ordered is descending, so the previous patch starts later.
			// Its start must not fall inside this patch's span.
			if ordered[i-1].Content.Start < p.Content.End {
				return nil, fmt.Errorf("patches overlap at byte %d", p.Content.End)
			}
		}
	}

	// Built in a single forward pass instead of re-splicing the whole
	// document once per patch (and again per preserve-attribute rewrite):
	// ordered is already sorted and validated non-overlapping, so walking it
	// once and copying each untouched gap plus each replacement is
	// O(len(documentXML) + total patch text) rather than
	// O(len(documentXML) * len(patches)). ordered is sorted descending for
	// the validation above, so this walks it back-to-front to recover
	// ascending order without re-sorting.
	out := make([]byte, 0, len(documentXML)+32*len(ordered))
	cursor := 0
	for i := len(ordered) - 1; i >= 0; i-- {
		p := ordered[i]

		if p.Raw {
			// TagSpan is the zero value for a raw patch — there is no <w:t>
			// start tag to rewrite for a structural subtree splice — so the
			// preserve-attribute machinery below (which slices TagSpan) must
			// not run at all; rewriting a zero-value span would splice at
			// offset 0 instead of at Content. Untouched bytes go straight
			// through to Content.Start, then rawXML lands verbatim, unescaped.
			out = append(out, documentXML[cursor:p.Content.Start]...)
			out = append(out, p.NewText...)
			cursor = p.Content.End
			continue
		}

		escaped, err := escapeXMLText(p.NewText)
		if err != nil {
			return nil, err
		}

		// Untouched bytes since the end of the previous patch, up to this
		// patch's start tag.
		out = append(out, documentXML[cursor:p.TagSpan.Start]...)

		tag := documentXML[p.TagSpan.Start:p.TagSpan.End]
		if needsPreserve(p.NewText) && !p.HasPreserve {
			newTag, err := withPreserveAttr(tag)
			if err != nil {
				return nil, err
			}
			out = append(out, newTag...)
		} else {
			out = append(out, tag...)
		}

		// The (normally empty) gap between the start tag and the content
		// span, then the replacement content itself.
		out = append(out, documentXML[p.TagSpan.End:p.Content.Start]...)
		out = append(out, escaped...)
		cursor = p.Content.End
	}
	out = append(out, documentXML[cursor:]...)

	// The well-formedness check is a full token-scan of the output, so it is
	// gated behind hasRaw: an ordinary batch (the hot path Apply's own
	// benchmark guards) never pays for it. Raw is the only bypass of
	// escapeXMLText, so it is also the only way a batch can produce
	// malformed XML — a raw patch splicing an unbalanced or otherwise broken
	// subtree must be caught here, before the result ever reaches SetPart or
	// disk.
	if hasRaw {
		if err := checkWellFormed(out); err != nil {
			return nil, fmt.Errorf("patched document is not well-formed XML: %w", err)
		}
	}
	return out, nil
}

// checkWellFormed walks every token of data with encoding/xml, which fails
// on any malformed XML (unbalanced tags, bad escapes, truncated content)
// without caring about the schema. It exists so Apply can validate a raw
// patch's result without pulling in a testing-only helper.
func checkWellFormed(data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// ApplyToPart applies patches to the named part's current content and
// writes the result back with SetPart. This is the RECOMMENDED way to patch
// a part: Part returns a slice that aliases the package's internal storage,
// but only until the next SetPart call replaces it — a caller who holds on
// to an earlier Part() result (say, to build a second batch of patches from
// offsets scanned before the first SetPart) is operating on a stale,
// disconnected copy, and writing it back would silently discard whatever
// the first batch already wrote. ApplyToPart avoids that hazard by always
// reading the part fresh immediately before applying. Callers who need
// Apply's output before deciding whether to keep it should call Part, Apply
// and SetPart themselves; everyone else should call this instead.
func (p *Package) ApplyToPart(name string, patches []Patch) error {
	data, ok := p.Part(name)
	if !ok {
		return fmt.Errorf("docx has no entry named %q", name)
	}
	out, err := Apply(data, patches)
	if err != nil {
		return err
	}
	return p.SetPart(name, out)
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
