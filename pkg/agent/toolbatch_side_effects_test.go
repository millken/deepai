package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/runcode"
)

// Criterion 31: every tool whose side effect lands in toolBatchState rather
// than its own handler must be either EXCLUDED from run_code scripts or have
// an explicit forwarding path (§4.5). This table is the contract — a new
// toolBatchState branch that is not recorded here fails the source-scan
// below, so it cannot silently bypass the nested-execution path.
var toolBatchSideEffects = map[string]string{
	// tool → side effect applied in toolBatchState (not in the handler)
	"skill":            "applySkillResult — system-prompt/skill state applied by the agent loop",
	"todo_write":       "applyTodoResult — todo state applied by the agent loop",
	"task":             "addSubagentUsage — subagent token usage accounted at the top level only",
	"invoke_acp_agent": "addSubagentUsage — subagent token usage accounted at the top level only",
	"edit_file":        "recordEditedFile — review-gate attribution",
	"write_file":       "recordEditedFile — review-gate attribution",
}

// Each listed tool is either excluded from scripts outright, or its effect is
// forwarded through the run_code result (edited_paths → recordEditedFile's
// run_code branch, which runs BEFORE the completed check so failed scripts
// still attribute their landed writes).
func TestToolBatchSideEffectsDispositions(t *testing.T) {
	excluded := map[string]bool{}
	for _, n := range runcode.ExcludedTools {
		excluded[n] = true
	}
	for tool := range toolBatchSideEffects {
		switch tool {
		case "edit_file", "write_file":
			// Forwarded, not excluded: the run_code branch in
			// recordEditedFile consumes Data["edited_paths"].
			src := readPkgSource(t, "session_carry.go")
			if !strings.Contains(src, `"edited_paths"`) || !strings.Contains(src, `"run_code"`) {
				t.Errorf("%s: run_code forwarding branch (edited_paths) missing from session_carry.go", tool)
			}
		default:
			if !excluded[tool] {
				t.Errorf("%s (%s): neither in runcode.ExcludedTools nor forwarded — a nested call would silently drop its side effect",
					tool, toolBatchSideEffects[tool])
			}
		}
	}
}

// Source scan: the handleResult switch dispatching agent-loop side effects
// must not grow a case that is absent from the table above. This is what
// makes "新增 toolBatchState 分支不更新表即测试失败" mechanical.
func TestToolBatchSideEffectsTableTracksHandleResultCases(t *testing.T) {
	src := readPkgSource(t, "toolexec.go")
	body := extractFuncBody(t, src, "handleResult")
	if body == "" {
		t.Fatal("could not locate handleResult body in toolexec.go")
	}
	caseRe := regexp.MustCompile(`case "([a-z_]+)":`)
	for _, m := range caseRe.FindAllStringSubmatch(body, -1) {
		name := m[1]
		if _, tracked := toolBatchSideEffects[name]; !tracked {
			t.Errorf(`handleResult grew case %q: add it to toolBatchSideEffects in this file and either exclude it from run_code or forward its effect (docs/RUN_CODE_DESIGN.md §4.5)`, name)
		}
	}
}

// Source scan: the run_code exclusion list in pkg/runcode must stay in sync
// with the dispositions asserted above — a tool REMOVED from the exclusion
// list without a forwarding path must fail here too.
func TestToolBatchSideEffectsExclusionListCoversUsageTools(t *testing.T) {
	excluded := map[string]bool{}
	for _, n := range runcode.ExcludedTools {
		excluded[n] = true
	}
	for _, tool := range []string{"task", "invoke_acp_agent", "skill", "todo_write"} {
		if !excluded[tool] {
			t.Errorf("%s carries toolBatchState-side effects but is callable inside run_code scripts", tool)
		}
	}
}

func readPkgSource(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// extractFuncBody pulls `func (x *T) name(` … through balanced closing brace.
func extractFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	idx := strings.Index(src, ") "+name+"(")
	if idx < 0 {
		idx = strings.Index(src, ") "+name+" (")
	}
	if idx < 0 {
		return ""
	}
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := idx + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[idx+open : i+1]
			}
		}
	}
	return ""
}
