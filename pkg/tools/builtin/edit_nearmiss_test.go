package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// The miss this covers is the one that actually happens. Session history for
// this project holds 49 edit_file misses; every analyzable one anchored
// correctly, matched exactly for several lines, and then differed on a single
// line the model had retyped from memory. The old error talked only about
// line-number prefixes and escaped newlines — neither of which was ever the
// cause — so the model found nothing to correct and resent the same string.
func TestEditFile_NearMissNamesTheDivergingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.go")
	const src = "package admin\n" +
		"\n" +
		"func mount() {\n" +
		"\t// Unconditional, for exactly the reasons on mountFileManager above.\n" +
		"\t// A route registered twice is a startup panic, so a duplicate mount\n" +
		"\t// would panic there.\n" +
		"\tmountPanel()\n" +
		"}\n"
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	// Same block, one comment line paraphrased.
	old := "\t// Unconditional, for exactly the reasons on mountFileManager above.\n" +
		"\t// A route registered twice is a startup panic, so a duplicate mount\n" +
		"\t// would panic there rather than in production.\n"

	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:   "c1",
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": old,
			"new_string": "\t// replaced\n",
		},
	})
	if err == nil {
		t.Fatal("expected the edit to fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"line 4",                       // where the near-miss starts
		"matches your first 2 line(s)", // how far it got
		"differs at line 6",            // the diverging file line
		"would panic there rather than in production", // what was sent
		`"\t// would panic there."`,                   // what the file actually has
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "line number prefix") {
		t.Errorf("the generic quoting advice misdirects on a near-miss:\n%s", msg)
	}

	// The file must be untouched: a near-miss is explained, never applied.
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != src {
		t.Fatalf("the file was modified by a failed edit:\n%s", after)
	}
}

// An old_string with no recognizable counterpart must not point anywhere:
// naming an unrelated line would send the next attempt to the wrong place.
func TestEditFile_UnrelatedStringKeepsTheGenericAdvice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID:   "c1",
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "SELECT * FROM orders WHERE status = 'RESERVING'",
			"new_string": "x",
		},
	})
	if err == nil {
		t.Fatal("expected the edit to fail")
	}
	if !strings.Contains(err.Error(), "no line in the file resembles") {
		t.Errorf("expected the generic quoting advice, got:\n%s", err)
	}
}

// Whitespace-only differences are handled by applyEdit's tolerant pass, so
// they must never be reported as the divergence.
func TestNearestMissHint_IgnoresWhitespaceOnlyDifferences(t *testing.T) {
	content := "func f() {\n\tif x {\n\t\treturn 1\n\t}\n}\n"
	// Indentation differs throughout; the real difference is the returned value.
	old := "    if x {\n        return 2\n    }\n"
	hint := nearestMissHint(content, old)
	if !strings.Contains(hint, "differs at line 3") {
		t.Fatalf("expected the value line to be named, got: %s", hint)
	}
}
