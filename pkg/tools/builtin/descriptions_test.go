package builtin

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// allBuiltinTools gathers every tool bundle for description-level assertions.
func allBuiltinTools() []models.Tool {
	var all []models.Tool
	all = append(all, BashTool())
	all = append(all, FileTools()...) // includes read/write/edit/list/glob/grep/find/code_map
	all = append(all, GitTools()...)
	all = append(all, WebTools()...)
	return all
}

// TestT3a_NoRedundantBashRouting guards T3a: per-tool descriptions must not
// re-list the bash→dedicated-tool routing table, which the system prompt's
// always-present file-operation rule already owns. The single allowed mention
// is the bash tool's own generic "Do NOT use bash for file operations" nudge.
func TestT3a_NoRedundantBashRouting(t *testing.T) {
	for _, tool := range allBuiltinTools() {
		d := tool.Description
		if strings.Contains(d, "via bash") {
			t.Errorf("%s description still contains routing dup 'via bash': %q", tool.Name, d)
		}
		// The specific command-routing lists must be gone from file tools.
		if tool.Name != "bash" {
			for _, phrase := range []string{"cat/head/tail", "echo>/cat>/tee", "grep/rg/ag", "sed/awk/perl"} {
				if strings.Contains(d, phrase) {
					t.Errorf("%s description duplicates system-prompt routing (%q): %q", tool.Name, phrase, d)
				}
			}
		}
	}
}

// TestT3a_BashKeepsFileOpNudge guards quality: slimming must not delete the
// behavior-critical "don't use bash for file operations" signal at the point of
// temptation (the bash tool itself).
func TestT3a_BashKeepsFileOpNudge(t *testing.T) {
	d := BashTool().Description
	if !strings.Contains(strings.ToLower(d), "do not use bash for file operations") {
		t.Errorf("bash must keep the file-op nudge, got: %q", d)
	}
	// But it must NOT re-enumerate every dedicated tool (that's the system prompt's job).
	if strings.Contains(d, "read_file") && strings.Contains(d, "list_dir") {
		t.Errorf("bash description should not re-list the full tool routing table: %q", d)
	}
}

// TestT3_DescriptionsStillFunctional guards that slimming kept each tool's
// functional gist (name-relevant keyword), so the model can still pick it.
func TestT3_DescriptionsStillFunctional(t *testing.T) {
	wantKeyword := map[string]string{
		"read_file":  "Read",
		"write_file": "Write",
		"edit_file":  "Replace",
		"list_dir":   "directory",
		"glob":       "glob",
		"grep":       "regex",
		"find":       "find",
		"code_map":   "structure",
	}
	byName := map[string]string{}
	for _, tool := range FileTools() {
		byName[tool.Name] = tool.Description
	}
	for name, kw := range wantKeyword {
		d, ok := byName[name]
		if !ok {
			t.Errorf("tool %s not found in FileTools()", name)
			continue
		}
		if !strings.Contains(d, kw) {
			t.Errorf("%s description lost its functional gist (want %q): %q", name, kw, d)
		}
	}
}
