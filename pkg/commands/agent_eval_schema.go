package commands

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/millken/deepai/pkg/agent"
)

// M5-3 deleted this file's former eval-only mirror types (evalDesignDoc,
// evalFinding, ...) in favor of the real structs pkg/agent now carries
// (agent.DesignDoc, agent.Finding, ...) — see docs/AGENT_CAPABILITY_DESIGN.md
// §3 and pkg/agent/output.go. Those mirrors predated M5-3's OutputSchema
// mounting and had NO json tags, while the real types added lowercase json
// tags (M5-3's decision: a model asked to match a JSON Schema Prompt writes
// lowercase snake_case by instinct). Kept apart, the two would drift: the
// mirror would score a role against a shape (PascalCase keys) the role was
// never told to produce, because its actual OutputSchema Prompt (built from
// the real type) asks for the OTHER shape (snake_case keys). Decoding
// against the same types production mounts means the scored contract and
// the contract the role is told about are physically the same Go value —
// they cannot diverge.
//
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

// evalSchemas holds one agent.OutputSchema per named schema, built via the
// same agent.FromStruct call production uses to mount OutputSchema onto
// AgentTypeConfig (pkg/agent/types_config.go's init() and namedSchemas
// table) — not a second construction, just the same function applied to the
// same type, so eval's decode/validate path and production's are identical
// modulo the Strict/MaxRetries options (which ParseOutput ignores; it only
// consults schema.Resolved for validation).
var evalSchemas = map[string]*agent.OutputSchema{
	"design":       agent.FromStruct[agent.DesignDoc](),
	"requirements": agent.FromStruct[agent.RequirementsSpec](),
	"research":     agent.FromStruct[agent.ResearchFindings](),
	"analysis":     agent.FromStruct[agent.AnalysisReport](),
	"test-report":  agent.FromStruct[agent.TestReport](),
}

// parseNamedSchema attempts to parse text as the named schema's JSON shape,
// returning the decoded value (a pointer to one of the real pkg/agent
// structs above) on success. It never panics on an unknown name — callers
// validate the name against namedSchemaKinds at manifest-load time, but this
// stays defensive.
func parseNamedSchema(name, text string) (any, error) {
	schema, ok := evalSchemas[name]
	if !ok {
		return nil, fmt.Errorf("unknown schema name %q", name)
	}
	switch name {
	case "design":
		return agent.ParseOutput[agent.DesignDoc](schema, text)
	case "requirements":
		return agent.ParseOutput[agent.RequirementsSpec](schema, text)
	case "research":
		return agent.ParseOutput[agent.ResearchFindings](schema, text)
	case "analysis":
		return agent.ParseOutput[agent.AnalysisReport](schema, text)
	case "test-report":
		return agent.ParseOutput[agent.TestReport](schema, text)
	default:
		return nil, fmt.Errorf("unknown schema name %q", name)
	}
}

// fieldByPath walks a single path segment off a parsed schema value. v must
// be a struct or a pointer to one.
//
// It matches the json tag name FIRST (case-insensitively), falling back to
// the bare Go field name (also case-insensitively) only for a field with no
// tag or an empty/"-" one. This is not equivalent to matching the Go field
// name alone: a single-word field's tag happens to equal its EqualFold'd Go
// name ("decisions" == EqualFold "Decisions"), which is why that case looked
// unaffected before, but a multi-word field's tag does NOT — RequirementsSpec's
// ScopeIn field carries `json:"scope_in"`, and EqualFold("ScopeIn",
// "scope_in") is false. A manifest author writes the path exactly as it
// appears in the schema Prompt the MODEL sees (scope_in, so_that,
// open_questions, end_line, ...), so matching only the Go name would fail
// every multi-word path a manifest is actually likely to use.
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
		if tagName := jsonTagName(t.Field(i)); tagName != "" && strings.EqualFold(tagName, name) {
			return v.Field(i), true
		}
	}
	for i := 0; i < t.NumField(); i++ {
		if strings.EqualFold(t.Field(i).Name, name) {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// jsonTagName extracts the name portion of a struct field's json tag (the
// part before a comma, e.g. "scope_in" out of `json:"scope_in,omitempty"`),
// or "" if there is no tag, the tag is "-" (explicitly excluded from JSON),
// or the name portion is empty (a bare `json:",omitempty"` naming nothing).
func jsonTagName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return ""
	}
	return name
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
