package commands

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/millken/deepai/pkg/agent"
)

// Mirrors of docs/AGENT_CAPABILITY_DESIGN.md §3's proposed per-role output
// structs. M5-3 is what actually wires these into pkg/agent's
// AgentTypeConfig.OutputSchema for architect/product-manager/researcher/
// analyst/tester; until then no role emits schema-shaped JSON at all, so
// every schema_parses assertion in the M5-2 baseline is expected to fail —
// that is the point (see the "design" case comment in eval/agent-cases).
//
// Defined here (not in pkg/agent) because this period is zero production
// code changes outside the pkg/chat snapshot export (see agent_eval.go's
// package doc): these types exist only as an eval-side yardstick. When M5-3
// lands the real structs in pkg/agent, this file's types should be deleted
// and the schema map below should point at agent.FromStruct[agent.DesignDoc]
// etc. instead.
type evalDecision struct {
	Topic        string
	Choice       string
	Rationale    string
	Alternatives []string
	Reversible   bool
}

type evalComponent struct {
	Name           string
	Responsibility string
	Files          []string
	Interfaces     []string
}

type evalRisk struct {
	Description string
	Mitigation  string
}

type evalDesignDoc struct {
	Agent         string
	Goal          string
	Decisions     []evalDecision
	Components    []evalComponent
	Risks         []evalRisk `json:",omitempty"`
	OpenQuestions []string   `json:",omitempty"`
	Milestones    []string   `json:",omitempty"`
}

type evalUserStory struct {
	Role   string
	Want   string
	SoThat string
}

type evalCriterion struct {
	ID         string
	Given      string
	When       string
	Then       string
	Verifiable bool
}

type evalPriority struct {
	Item   string
	Level  string
	Reason string
}

type evalRequirementsSpec struct {
	Agent         string
	Problem       string
	Stories       []evalUserStory
	ScopeIn       []string
	ScopeOut      []string
	Acceptance    []evalCriterion
	Priorities    []evalPriority
	OpenQuestions []string `json:",omitempty"`
}

type evalEvidence struct {
	File  string
	Line  int
	Quote string
	URL   string
}

type evalFinding struct {
	Claim      string
	Evidence   []evalEvidence
	Confidence string
}

type evalResearchFindings struct {
	Agent    string
	Question string
	Answer   string
	Findings []evalFinding
	Gaps     []string `json:",omitempty"`
}

type evalAnalysisReport struct {
	Agent     string
	Objective string
	Method    string
	Findings  []evalFinding
	Caveats   []string `json:",omitempty"`
	Artifacts []string `json:",omitempty"`
}

type evalTestFailure struct {
	Name    string
	File    string
	Line    int
	Message string
}

type evalTestReport struct {
	Agent    string
	Command  string
	Passed   int
	Failed   int
	Skipped  int
	Failures []evalTestFailure
	Coverage string `json:",omitempty"`
}

// namedSchemaKind enumerates the schema_parses names the manifests may use.
// Kept as a closed set (rather than accepting any string) so a typo in a
// manifest's `schema_parses: designe` fails loudly at case-load time instead
// of silently always-failing like a real mismatch would.
var namedSchemaKinds = map[string]bool{
	"design":       true,
	"requirements": true,
	"research":     true,
	"analysis":     true,
	"test-report":  true,
}

// evalSchemas holds one agent.OutputSchema per named schema, built once via
// the exported agent.FromStruct/agent.ParseOutput so JSON extraction and
// validation are the same code production will use once M5-3 wires a real
// OutputSchema onto these agent types — the eval-only mirror types above are
// the only thing standing in for the not-yet-written production structs.
var evalSchemas = map[string]*agent.OutputSchema{
	"design":       agent.FromStruct[evalDesignDoc](),
	"requirements": agent.FromStruct[evalRequirementsSpec](),
	"research":     agent.FromStruct[evalResearchFindings](),
	"analysis":     agent.FromStruct[evalAnalysisReport](),
	"test-report":  agent.FromStruct[evalTestReport](),
}

// parseNamedSchema attempts to parse text as the named schema's JSON shape,
// returning the decoded value (a pointer to one of the evalXxx structs above)
// on success. It never panics on an unknown name — callers validate the name
// against namedSchemaKinds at manifest-load time, but this stays defensive.
func parseNamedSchema(name, text string) (any, error) {
	schema, ok := evalSchemas[name]
	if !ok {
		return nil, fmt.Errorf("unknown schema name %q", name)
	}
	switch name {
	case "design":
		return agent.ParseOutput[evalDesignDoc](schema, text)
	case "requirements":
		return agent.ParseOutput[evalRequirementsSpec](schema, text)
	case "research":
		return agent.ParseOutput[evalResearchFindings](schema, text)
	case "analysis":
		return agent.ParseOutput[evalAnalysisReport](schema, text)
	case "test-report":
		return agent.ParseOutput[evalTestReport](schema, text)
	default:
		return nil, fmt.Errorf("unknown schema name %q", name)
	}
}

// fieldByPath walks a single path segment (a top-level field name, matched
// case-insensitively so a manifest can write `decisions` against a Go field
// named Decisions) off a parsed schema value. v must be a struct or a
// pointer to one.
func fieldByPath(v reflect.Value, name string) (reflect.Value, bool) {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if strings.EqualFold(t.Field(i).Name, name) {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// fieldMinCount implements the `field_min_count: {path, n}` assertion: path
// names a top-level slice field (e.g. "decisions"); it passes when that
// field is a slice/array of length >= n.
func fieldMinCount(parsed any, path string, n int) (pass bool, count int, err error) {
	field, ok := fieldByPath(reflect.ValueOf(parsed), path)
	if !ok {
		return false, 0, fmt.Errorf("field %q not found on parsed schema", path)
	}
	if field.Kind() != reflect.Slice && field.Kind() != reflect.Array {
		return false, 0, fmt.Errorf("field %q is not a list (kind %s)", path, field.Kind())
	}
	count = field.Len()
	return count >= n, count, nil
}

// fieldNonemptyAll implements `field_nonempty_all: <array>[].<subfield>`: for
// every element of the named top-level array field, the named subfield must
// be non-empty (non-zero for scalars, len()>0 for slices/strings/maps). An
// empty array fails deliberately — "all elements have evidence" is not
// satisfied by producing zero elements to vacuously satisfy it.
func fieldNonemptyAll(parsed any, path string) (pass bool, checked int, err error) {
	arrayName, subName, ok := strings.Cut(path, "[].")
	if !ok {
		return false, 0, fmt.Errorf("field_nonempty_all path %q must have the form array[].subfield", path)
	}
	arrayField, ok := fieldByPath(reflect.ValueOf(parsed), arrayName)
	if !ok {
		return false, 0, fmt.Errorf("field %q not found on parsed schema", arrayName)
	}
	if arrayField.Kind() != reflect.Slice && arrayField.Kind() != reflect.Array {
		return false, 0, fmt.Errorf("field %q is not a list (kind %s)", arrayName, arrayField.Kind())
	}
	if arrayField.Len() == 0 {
		return false, 0, nil
	}
	for i := 0; i < arrayField.Len(); i++ {
		sub, ok := fieldByPath(arrayField.Index(i), subName)
		if !ok {
			return false, i, fmt.Errorf("field %q not found on %s[%d]", subName, arrayName, i)
		}
		if isEmptyValue(sub) {
			return false, i + 1, nil
		}
	}
	return true, arrayField.Len(), nil
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		return v.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	default:
		return v.IsZero()
	}
}
