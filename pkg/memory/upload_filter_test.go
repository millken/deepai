package memory

import (
	"reflect"
	"strings"
	"testing"
)

// The regression this file exists for: every memory write ran through
// stripUploadSentences, which split on '.' and rejoined without it, so a
// stored path came back as "/Users/x/github com/y/HANDOFF md" and the model
// replayed that broken path into read_file.
func TestStripUploadSentencesKeepsTextWithoutUploadMention(t *testing.T) {
	cases := []string{
		"开发 jp-small 项目，位于 /Users/millken/github.com/millken/jp-small，采用分支-PR 流程。",
		"HANDOFF.md 为交接文档；先读「当前状态」。",
		"Pinned to v1.2.3. Run make test. Then ship!",
		"Read github.com/millken/deepai/pkg/memory/prompt.go first.",
	}
	for _, in := range cases {
		if got := stripUploadSentences(in); got != in {
			t.Errorf("stripUploadSentences(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestStripUploadSentencesDropsOnlyTheUploadSentence(t *testing.T) {
	in := "Project lives at /Users/x/github.com/y. The user uploaded a file with the specs. Tests run via make test."
	want := "Project lives at /Users/x/github.com/y. Tests run via make test."
	if got := stripUploadSentences(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripUploadSentencesPreservesCJKPunctuation(t *testing.T) {
	// uploadMentionRE only recognizes the English phrasing, so the sentence to
	// be dropped is written that way; the point here is that the Chinese
	// sentences around it keep their 。 terminators and their dotted tokens.
	in := "配置在 config.yaml 中。The user uploaded a document 附件。默认模型是 glm-5.3。"
	got := stripUploadSentences(in)
	if strings.Contains(got, "附件") {
		t.Errorf("upload sentence not dropped: %q", got)
	}
	for _, want := range []string{"config.yaml 中。", "glm-5.3。"} {
		if !strings.Contains(got, want) {
			t.Errorf("got %q, want it to contain %q", got, want)
		}
	}
}

func TestSplitIntoSentencesKeepsTerminatorsAndIntraWordDots(t *testing.T) {
	got := splitIntoSentences("Go to github.com/millken now. Then stop! Ok?\nNext 中文。尾巴")
	want := []string{"Go to github.com/millken now.", " Then stop!", " Ok?", "Next 中文。", "尾巴"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestSanitizeUpdateForStorageKeepsPathsIntact(t *testing.T) {
	path := "/Users/millken/github.com/millken/jp-small/HANDOFF.md"
	update := Update{
		User:  UserMemory{WorkContext: "项目位于 " + path},
		Facts: []Fact{{ID: "f1", Content: "交接文档是 " + path}, {ID: "f2", Content: "the user uploaded a file with the schema"}},
	}
	got := sanitizeUpdateForStorage(update)
	if !strings.Contains(got.User.WorkContext, path) {
		t.Errorf("work context lost the path: %q", got.User.WorkContext)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID != "f1" {
		t.Fatalf("expected only the upload fact dropped, got %#v", got.Facts)
	}
	if !strings.Contains(got.Facts[0].Content, path) {
		t.Errorf("fact lost the path: %q", got.Facts[0].Content)
	}
}
