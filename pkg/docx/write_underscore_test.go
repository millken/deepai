package docx

import (
	"strings"
	"testing"
)

// paraVisibleText concatenates a paragraph's run text -- what a person
// reading the reopened document would actually see, markers stripped --
// the same pattern writeAndReopen's callers already use elsewhere in this
// package's test suite.
func paraVisibleText(p Para) string {
	var text strings.Builder
	for _, r := range p.Runs {
		text.WriteString(r.Text)
	}
	return text.String()
}

// TestWrite_IntrawordUnderscoreSurvivesLiterally is the bug report itself:
// an underscore immediately preceded and followed by an alphanumeric (or
// CJK) character cannot open or close emphasis per CommonMark, so
// snake_case-shaped identifiers must survive parseInlineCtx with every
// underscore intact and no <w:i/>/<w:b/> anywhere in the document.
func TestWrite_IntrawordUnderscoreSurvivesLiterally(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{
			name: "two enum identifiers separated by a slash",
			md:   "值为 PROXY_ORDER / BALANCE_ADJUST 之一\n",
			want: "值为 PROXY_ORDER / BALANCE_ADJUST 之一",
		},
		{
			name: "single identifier, pinning today's accidental survival",
			md:   "refresh_token\n",
			want: "refresh_token",
		},
		{
			name: "identifier wrapped in parentheses",
			md:   "(PROXY_ORDER)\n",
			want: "(PROXY_ORDER)",
		},
		{
			name: "identifier followed by a full stop",
			md:   "PROXY_ORDER.\n",
			want: "PROXY_ORDER.",
		},
		{
			name: "mixed CJK prose and an identifier",
			md:   "类型 PROXY_ORDER 说明\n",
			want: "类型 PROXY_ORDER 说明",
		},
		{
			name: "three underscore-separated parts stay whole",
			md:   "snake_case_with_three_parts\n",
			want: "snake_case_with_three_parts",
		},
		{
			name: "CJK on both sides of underscored CJK: no spaces to save it",
			md:   "中文_斜体_中文\n",
			want: "中文_斜体_中文",
		},
		{
			name: "double underscore flanked by word characters on both sides",
			md:   "foo__bar__baz\n",
			want: "foo__bar__baz",
		},
		{
			// Regression: the opening side must be checked too, not just
			// the closing side. Without checking that the FIRST "_" here
			// is itself intraword (preceded by 'Y', followed by 'O'), a
			// naive "search forward for any valid, non-flanked closer"
			// would accept the trailing "_" in "end_" as a legitimate
			// close (it is followed by nothing) and wrongly italicise
			// "ORDER end".
			name: "intraword opener must not pair with a valid closer far away",
			md:   "PROXY_ORDER end_\n",
			want: "PROXY_ORDER end_",
		},
		{
			// Same regression, for the bold ("__") marker: the first
			// "__" is intraword (preceded by 'o', followed by 'b') and
			// must not be allowed to open just because a later "__"
			// (followed by a space, so not intraword) would otherwise
			// close it.
			name: "intraword double-underscore opener must not pair with a valid closer far away",
			md:   "foo__bar__ end\n",
			want: "foo__bar__ end",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := writeAndReopen(t, tc.md)
			paras := d.Paras()
			if len(paras) != 1 {
				t.Fatalf("got %d paragraphs, want 1", len(paras))
			}
			if got := paraVisibleText(paras[0]); got != tc.want {
				t.Errorf("visible text = %q, want %q (an underscore vanished or the text was mangled)", got, tc.want)
			}
			doc, _ := d.Part(DocumentPart)
			if strings.Contains(string(doc), "<w:i/>") {
				t.Errorf("no run should be italicised in %q; got:\n%s", tc.md, doc)
			}
			if strings.Contains(string(doc), "<w:b/>") {
				t.Errorf("no run should be bold in %q; got:\n%s", tc.md, doc)
			}
		})
	}
}

// TestWrite_IntrawordUnderscoreSurvivesInATableCell pins the exact place the
// user found the bug: a table cell listing enum values.
func TestWrite_IntrawordUnderscoreSurvivesInATableCell(t *testing.T) {
	md := "| Field | Values |\n|---|---|\n| status | PROXY_ORDER / BALANCE_ADJUST |\n"
	d, _, _ := writeAndReopen(t, md)

	var cellParas []Para
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	if len(cellParas) != 4 {
		t.Fatalf("got %d cell paragraphs, want 4", len(cellParas))
	}
	got := paraVisibleText(cellParas[3])
	want := "PROXY_ORDER / BALANCE_ADJUST"
	if got != want {
		t.Errorf("table cell visible text = %q, want %q", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:i/>") {
		t.Errorf("table cell identifier must not be italicised; got:\n%s", doc)
	}
}

// TestWrite_UnderscoreEmphasisStillWorks is the flip side of the bug fix:
// underscore emphasis that is NOT intraword must keep working exactly as
// before, including at a paragraph's very start/end where there is no
// space to lean on -- only the absence of an adjacent word character on
// the outward-facing side.
func TestWrite_UnderscoreEmphasisStillWorks(t *testing.T) {
	t.Run("surrounded by spaces", func(t *testing.T) {
		d, _, _ := writeAndReopen(t, "before _真正的斜体_ after\n")
		paras := d.Paras()
		if len(paras) != 1 {
			t.Fatalf("got %d paragraphs, want 1", len(paras))
		}
		if got, want := paraVisibleText(paras[0]), "before 真正的斜体 after"; got != want {
			t.Errorf("visible text = %q, want %q", got, want)
		}
		doc, _ := d.Part(DocumentPart)
		if !strings.Contains(string(doc), "<w:i/>") {
			t.Errorf("expected an italic run; got:\n%s", doc)
		}
	})

	t.Run("delimiters are the whole paragraph, start and end", func(t *testing.T) {
		d, _, _ := writeAndReopen(t, "_真正的斜体_\n")
		paras := d.Paras()
		if len(paras) != 1 {
			t.Fatalf("got %d paragraphs, want 1", len(paras))
		}
		if got, want := paraVisibleText(paras[0]), "真正的斜体"; got != want {
			t.Errorf("visible text = %q, want %q", got, want)
		}
		doc, _ := d.Part(DocumentPart)
		if !strings.Contains(string(doc), "<w:i/>") {
			t.Errorf("expected an italic run; got:\n%s", doc)
		}
	})

	t.Run("double underscore bold still bolds", func(t *testing.T) {
		d, _, _ := writeAndReopen(t, "__bold__\n")
		paras := d.Paras()
		if len(paras) != 1 {
			t.Fatalf("got %d paragraphs, want 1", len(paras))
		}
		if got, want := paraVisibleText(paras[0]), "bold"; got != want {
			t.Errorf("visible text = %q, want %q", got, want)
		}
		doc, _ := d.Part(DocumentPart)
		if !strings.Contains(string(doc), "<w:b/>") {
			t.Errorf("expected a bold run; got:\n%s", doc)
		}
	})
}

// TestWrite_AsteriskIntrawordEmphasisIsUnchanged pins that this fix is
// underscore-specific: intraword "*" emphasis is correct CommonMark and
// must keep working exactly as before.
func TestWrite_AsteriskIntrawordEmphasisIsUnchanged(t *testing.T) {
	d, _, _ := writeAndReopen(t, "a*b*c\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "abc"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:i/>") {
		t.Errorf("expected 'b' to be italicised; got:\n%s", doc)
	}
}

// TestWrite_LoneTrailingAndLeadingUnderscoreSurviveByAccident pins the
// behaviour the bug report calls out explicitly: an unpaired underscore at
// a word boundary has nothing to close/open with, so it falls through to
// parseInlineCtx's "unclosed marker" branch and survives as literal text --
// not because the intraword rule saved it (there is no word character on
// the outward-facing side), but because it never found a partner.
func TestWrite_LoneTrailingAndLeadingUnderscoreSurviveByAccident(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{name: "leading underscore, rest of word, no partner", md: "_leading\n", want: "_leading"},
		{name: "trailing underscore, no partner", md: "trailing_\n", want: "trailing_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := writeAndReopen(t, tc.md)
			paras := d.Paras()
			if len(paras) != 1 {
				t.Fatalf("got %d paragraphs, want 1", len(paras))
			}
			if got := paraVisibleText(paras[0]); got != tc.want {
				t.Errorf("visible text = %q, want %q", got, tc.want)
			}
			doc, _ := d.Part(DocumentPart)
			if strings.Contains(string(doc), "<w:i/>") {
				t.Errorf("no run should be italicised in %q; got:\n%s", tc.md, doc)
			}
		})
	}
}
