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

	patched, err := Apply(doc, []Patch{PatchRun(doc, target, "Howdy ")})
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
