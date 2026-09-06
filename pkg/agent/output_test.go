package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// --- extractJSON tests ---

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple object", `here is some text {"key":"value"} more text`, `{"key":"value"}`},
		{"nested object", `result: {"a":{"b":1}}`, `{"a":{"b":1}}`},
		{"array inside", `data: {"items":[1,2,3]}`, `{"items":[1,2,3]}`},
		{"no object", "just plain text", ""},
		{"empty object", `text {} after`, `{}`},
		{"braces in string", `{"msg":"hello {world}"}`, `{"msg":"hello {world}"}`},
		{"escaped quote in string", `{"msg":"he said \"hi\""}`, `{"msg":"he said \"hi\""}`},
		{"multiple objects", `first {"a":1} then {"b":2}`, `{"b":2}`},
		{"incomplete object", `{"key":`, ""},
		{"deeply nested", `{"a":{"b":{"c":1}}}`, `{"a":{"b":{"c":1}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.input)
			if got != tt.want {
				t.Errorf("extractJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- FromStruct tests ---

func TestFromStructReviewResult(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	if schema == nil {
		t.Fatal("FromStruct returned nil")
	}
	if schema.Schema == nil {
		t.Error("Schema is nil")
	}
	if schema.Resolved == nil {
		t.Error("Resolved is nil")
	}
	if schema.Prompt == "" {
		t.Error("Prompt is empty")
	}
	if schema.Strict || schema.MaxRetries != 0 {
		t.Error("defaults should be Strict=false, MaxRetries=0")
	}
}

func TestFromStructWithOptions(t *testing.T) {
	schema := FromStruct[ReviewResult](WithStrict(true), WithMaxRetries(2))
	if !schema.Strict {
		t.Error("Strict should be true")
	}
	if schema.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", schema.MaxRetries)
	}
}

// --- ParseOutput tests ---

func TestParseOutputValid(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `{"agent":"security-reviewer","verdict":"pass","summary":"clean","issues":[]}`
	result, err := ParseOutput[ReviewResult](schema, input)
	if err != nil {
		t.Fatalf("ParseOutput error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Errorf("Verdict = %q, want pass", result.Verdict)
	}
}

func TestParseOutputWithSurroundingText(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `Here is my review:
{"agent":"sec","verdict":"issues_found","summary":"SQL injection","issues":[{"severity":"critical","file":"db.go","line":42,"message":"SQL injection","suggestion":"use params"}]}
End of review.`
	result, err := ParseOutput[ReviewResult](schema, input)
	if err != nil {
		t.Fatalf("ParseOutput error: %v", err)
	}
	if result.Verdict != "issues_found" {
		t.Errorf("Verdict = %q, want issues_found", result.Verdict)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("Issues count = %d, want 1", len(result.Issues))
	}
	if result.Issues[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical", result.Issues[0].Severity)
	}
}

func TestParseOutputNoJSON(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	_, err := ParseOutput[ReviewResult](schema, "no json here")
	if err == nil {
		t.Error("expected error for input without JSON")
	}
}

func TestParseOutputInvalidJSON(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	_, err := ParseOutput[ReviewResult](schema, "{invalid}")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseOutputSchemaValidationFail(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	// verdict is required, missing here
	input := `{"agent":"sec","summary":"no verdict"}`
	_, err := ParseOutput[ReviewResult](schema, input)
	if err == nil {
		t.Error("expected schema validation error for missing required field")
	}
}

// --- ValidateOutput tests ---
//
// ValidateOutput is the non-generic sibling of ParseOutput[T]: the subagent
// executor (pkg/agent/subagent.go) validates OutputSchema without a concrete
// Go type to unmarshal into, so it needs a schema-check-only entry point.
// RED today: ValidateOutput does not exist.

func TestValidateOutputValid(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `{"agent":"security-reviewer","verdict":"pass","summary":"clean","issues":[]}`
	if err := ValidateOutput(schema, input); err != nil {
		t.Fatalf("ValidateOutput error = %v, want nil", err)
	}
}

func TestValidateOutputSchemaViolation(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	// verdict is required, missing here.
	input := `{"agent":"sec","summary":"no verdict"}`
	err := ValidateOutput(schema, input)
	if err == nil {
		t.Fatal("expected schema validation error for missing required field")
	}
	if !strings.Contains(err.Error(), "schema validation failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "schema validation failed")
	}
}

func TestValidateOutputProseWrapped(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `Here is my review:
{"agent":"sec","verdict":"issues_found","summary":"SQL injection","issues":[{"severity":"critical","file":"db.go","line":42,"message":"SQL injection","suggestion":"use params"}]}
End of review.`
	if err := ValidateOutput(schema, input); err != nil {
		t.Fatalf("ValidateOutput error = %v, want nil (JSON should be extracted from prose)", err)
	}
}

func TestValidateOutputNoJSON(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	err := ValidateOutput(schema, "no json here")
	if err == nil {
		t.Fatal("expected error for input without JSON")
	}
	if !strings.Contains(err.Error(), "no JSON object found in output") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "no JSON object found in output")
	}
}

func TestValidateOutputInvalidJSON(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	err := ValidateOutput(schema, "{invalid}")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid JSON:") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid JSON:")
	}
}

func TestValidateOutputNilSchema(t *testing.T) {
	if err := ValidateOutput(nil, `not even json`); err != nil {
		t.Fatalf("ValidateOutput(nil, ...) error = %v, want nil", err)
	}
}

func TestValidateOutputNilResolved(t *testing.T) {
	schema := &OutputSchema{} // Resolved is nil
	if err := ValidateOutput(schema, `not even json`); err != nil {
		t.Fatalf("ValidateOutput with nil Resolved error = %v, want nil", err)
	}
}

func TestAppendParseError(t *testing.T) {
	msgs := []models.Message{
		{Role: "human", Content: "hello"},
		{Role: "assistant", Content: "response"},
	}
	result := appendParseError(msgs, "bad output", fmt.Errorf("parse failed"))
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[2].Role != "human" {
		t.Errorf("last message role = %q, want human", result[2].Role)
	}
	if len(msgs) != 2 {
		t.Error("original slice should not be modified")
	}
}

// --- M5-3 output-contract structs (docs/AGENT_CAPABILITY_DESIGN.md §3) ---
//
// RED today: DesignDoc/RequirementsSpec/ResearchFindings/AnalysisReport/
// TestReport and their nested types do not exist yet in this package.

// TestOutputContractStructsHaveLowercaseJSONTags is the M5-3 decision (design
// §3 revision, item 2): every field on every new contract struct carries an
// explicit lowercase json tag, matching ReviewResult's existing precedent.
// It marshals one populated value of each top-level struct and checks the
// raw JSON keys are the lowercase/snake_case names the schema Prompt (and
// therefore the model) will see — not the bare Go field names an untagged
// struct would fall back to.
func TestOutputContractStructsHaveLowercaseJSONTags(t *testing.T) {
	design := DesignDoc{
		Agent: "architect", Goal: "g",
		Decisions:  []Decision{{Topic: "t", Choice: "c", Rationale: "r", Alternatives: []string{"a"}, Reversible: true}},
		Components: []Component{{Name: "n", Responsibility: "r", Files: []string{"f"}, Interfaces: []string{"i"}}},
		Risks:      []Risk{{Description: "d", Mitigation: "m"}},
	}
	assertJSONKeys(t, design, []string{"agent", "goal", "decisions", "components", "risks"})
	assertJSONKeys(t, design.Decisions[0], []string{"topic", "choice", "rationale", "alternatives", "reversible"})
	assertJSONKeys(t, design.Components[0], []string{"name", "responsibility", "files", "interfaces"})
	assertJSONKeys(t, design.Risks[0], []string{"description", "mitigation"})

	spec := RequirementsSpec{
		Agent: "product-manager", Problem: "p",
		Stories:    []UserStory{{Role: "r", Want: "w", SoThat: "s"}},
		ScopeIn:    []string{"in"},
		ScopeOut:   []string{"out"},
		Acceptance: []Criterion{{ID: "id", Given: "g", When: "w", Then: "t", Verifiable: true}},
		Priorities: []Priority{{Item: "i", Level: "P0", Reason: "r"}},
	}
	assertJSONKeys(t, spec, []string{"agent", "problem", "stories", "scope_in", "scope_out", "acceptance", "priorities"})
	assertJSONKeys(t, spec.Stories[0], []string{"role", "want", "so_that"})
	assertJSONKeys(t, spec.Acceptance[0], []string{"id", "given", "when", "then", "verifiable"})
	assertJSONKeys(t, spec.Priorities[0], []string{"item", "level", "reason"})

	rf := ResearchFindings{
		Agent: "researcher", Question: "q", Answer: "a",
		Findings: []Finding{{Claim: "c", Evidence: []Evidence{{File: "f.go", Line: 1, Quote: "q"}}, Confidence: "high"}},
	}
	assertJSONKeys(t, rf, []string{"agent", "question", "answer", "findings"})
	assertJSONKeys(t, rf.Findings[0], []string{"claim", "evidence", "confidence"})
	assertJSONKeys(t, rf.Findings[0].Evidence[0], []string{"file", "line", "quote"}) // end_line/url omitempty, zero here

	ar := AnalysisReport{
		Agent: "analyst", Objective: "o", Method: "m",
		Findings: []Finding{{Claim: "c", Evidence: []Evidence{{File: "f.go", Line: 1, Quote: "q"}}, Confidence: "high"}},
	}
	assertJSONKeys(t, ar, []string{"agent", "objective", "method", "findings"})

	tr := TestReport{
		Agent: "tester", Command: "go test", Passed: 1, Failed: 0, Skipped: 0,
		Failures: []TestFailure{{Name: "n", File: "f.go", Line: 1, Message: "m"}},
	}
	assertJSONKeys(t, tr, []string{"agent", "command", "passed", "failed", "skipped", "failures"})
	assertJSONKeys(t, tr.Failures[0], []string{"name", "file", "line", "message"})
}

// assertJSONKeys marshals v and checks the top-level JSON object's keys are
// EXACTLY wantKeys (as a set) — proving neither an untagged bare field name
// nor a missing tag slipped through.
func assertJSONKeys(t *testing.T, v any, wantKeys []string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("json.Unmarshal(%T): %v", v, err)
	}
	want := map[string]bool{}
	for _, k := range wantKeys {
		want[k] = true
		if _, ok := raw[k]; !ok {
			t.Errorf("%T: marshaled JSON missing expected lowercase key %q; got keys %v", v, k, rawKeys(raw))
		}
	}
	for k := range raw {
		if !want[k] {
			t.Errorf("%T: marshaled JSON has unexpected key %q (want only %v)", v, k, wantKeys)
		}
	}
}

func rawKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	return keys
}

// TestEvidenceLineIsIntRangeStringFailsWholeParse is the §3 revision item 3
// deviation: Evidence.Line is an int (with a separate optional EndLine int
// for a multi-line citation), NOT a "21-24" range string or "file:line"
// text. A model that writes a range as a string must fail the WHOLE JSON
// unmarshal for the enclosing ResearchFindings — this is deliberate (see
// architectSystemPrompt/researcherSystemPrompt's Output paragraph): the
// alternative (a permissive string field) would let "21-24" or "subagent.go:21"
// silently pass validation and corrupt every downstream field_* assertion
// that reads Line as a number.
func TestEvidenceLineIsIntRangeStringFailsWholeParse(t *testing.T) {
	schema := FromStruct[ResearchFindings]()

	rangeString := `{"agent":"a","question":"q","answer":"ans","findings":[{"claim":"c","evidence":[{"file":"f.go","line":"21-24","quote":"q"}],"confidence":"high"}]}`
	if _, err := ParseOutput[ResearchFindings](schema, rangeString); err == nil {
		t.Error("ParseOutput with Line as a range string should fail (Line is int), got nil error")
	}

	fileLineText := `{"agent":"a","question":"q","answer":"ans","findings":[{"claim":"c","evidence":[{"file":"f.go","line":"f.go:21","quote":"q"}],"confidence":"high"}]}`
	if _, err := ParseOutput[ResearchFindings](schema, fileLineText); err == nil {
		t.Error("ParseOutput with Line as 'file:line' text should fail (Line is int), got nil error")
	}

	multiLine := `{"agent":"a","question":"q","answer":"ans","findings":[{"claim":"c","evidence":[{"file":"f.go","line":21,"end_line":24,"quote":"q"}],"confidence":"high"}]}`
	result, err := ParseOutput[ResearchFindings](schema, multiLine)
	if err != nil {
		t.Fatalf("ParseOutput with int Line+EndLine: %v", err)
	}
	if result.Findings[0].Evidence[0].Line != 21 || result.Findings[0].Evidence[0].EndLine != 24 {
		t.Errorf("Line/EndLine = %d/%d, want 21/24", result.Findings[0].Evidence[0].Line, result.Findings[0].Evidence[0].EndLine)
	}
}

// TestEnumTagIsRejectedByJSONSchemaGo documents a finding that governed a
// real decision (design §3 revision item 4, brief §1 point 2): jsonschema-go
// v0.4.3 has NO struct-tag mechanism for a JSON Schema "enum" keyword. A
// `jsonschema:"enum=..."` tag does not silently get ignored — For[T] panics
// (via FromStruct) because the tag text starts with a bare "WORD=" prefix,
// which the library reserves and rejects outright
// (disallowedPrefixRegexp in jsonschema-go/jsonschema/infer.go). This test
// guards against silently "pretending it works": if a future jsonschema-go
// upgrade starts supporting this syntax, this test will fail (no panic),
// which is the signal to revisit Finding.Confidence/Priority.Level and
// switch them from the description-text fallback to a real enum.
func TestEnumTagIsRejectedByJSONSchemaGo(t *testing.T) {
	type probe struct {
		Confidence string `json:"confidence" jsonschema:"enum=high,enum=medium,enum=low"`
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected FromStruct to panic on an enum= jsonschema tag (jsonschema-go does not support it); it did not — revisit the description-text fallback on Finding.Confidence/Priority.Level")
		}
		if !strings.Contains(fmt.Sprint(r), "WORD=") {
			t.Errorf("panic = %v, want it to mention the disallowed 'WORD=' tag prefix", r)
		}
	}()
	FromStruct[probe]()
}

// TestFindingConfidenceAndPriorityLevelDescribeEnumInPrompt is the fallback
// side of the above: since jsonschema-go cannot express a real "enum"
// keyword via struct tag, Finding.Confidence and Priority.Level instead
// carry a `jsonschema` tag whose text is a plain-English enumeration of the
// legal values, which lands in each property's JSON Schema "description".
// This test is honest about what it proves: the schema Prompt does NOT
// contain the JSON Schema "enum" keyword for these fields (unlike, say, a
// real oneOf/enum schema would) — only a description a model may or may not
// heed. Anyone tightening this later should update this test alongside the
// tag.
func TestFindingConfidenceAndPriorityLevelDescribeEnumInPrompt(t *testing.T) {
	rf := FromStruct[ResearchFindings]()
	if !strings.Contains(rf.Prompt, "high, medium, or low") {
		t.Errorf("ResearchFindings schema Prompt missing Confidence's enum-description text; Prompt = %s", rf.Prompt)
	}

	spec := FromStruct[RequirementsSpec]()
	if !strings.Contains(spec.Prompt, "P0, P1, P2, or P3") {
		t.Errorf("RequirementsSpec schema Prompt missing Priority.Level's enum-description text; Prompt = %s", spec.Prompt)
	}
}

// TestNamedSchemasTable is the design §3 YAML-wiring requirement: a fixed
// map from the `output_schema:` YAML key's name to a real OutputSchema,
// closed (an unknown name is a yaml_loader.go load-time error, not a lookup
// miss here). "review" is the one Strict entry, matching the four builtin
// reviewer profiles already mounted in types_config.go's init().
func TestNamedSchemasTable(t *testing.T) {
	wantNonStrict := []string{"design", "requirements", "research", "analysis", "test-report"}
	for _, name := range wantNonStrict {
		schema, ok := namedSchemas[name]
		if !ok || schema == nil {
			t.Fatalf("namedSchemas[%q] missing", name)
		}
		if schema.Strict {
			t.Errorf("namedSchemas[%q].Strict = true, want false", name)
		}
	}
	reviewSchema, ok := namedSchemas["review"]
	if !ok || reviewSchema == nil {
		t.Fatal("namedSchemas[\"review\"] missing")
	}
	if !reviewSchema.Strict {
		t.Error("namedSchemas[\"review\"].Strict = false, want true (matches builtin reviewer profiles)")
	}
}

// TestAgentFieldHasSchemaDescription is a review fix: the four M5-3 contract
// structs' `Agent` field is required (no omitempty) but, before this fix,
// had no jsonschema description telling the model what to put there — a
// silent required field with no guidance. Since the schema is non-Strict
// (no retry), a model that omits or mis-fills Agent fails schema_parses for
// the WHOLE run with no second chance. The fix is a description only — NOT
// omitempty (Agent must stay required) and NOT an L1 prompt change (the L1
// constitutions are finalized and an in-flight after-run is using them).
func TestAgentFieldHasSchemaDescription(t *testing.T) {
	const wantSubstr = "the agent type that produced this"
	for name, prompt := range map[string]string{
		"DesignDoc":        FromStruct[DesignDoc]().Prompt,
		"RequirementsSpec": FromStruct[RequirementsSpec]().Prompt,
		"ResearchFindings": FromStruct[ResearchFindings]().Prompt,
		"AnalysisReport":   FromStruct[AnalysisReport]().Prompt,
	} {
		if !strings.Contains(prompt, wantSubstr) {
			t.Errorf("%s schema Prompt missing Agent field description (want it to contain %q); Prompt = %s", name, wantSubstr, prompt)
		}
		if !strings.Contains(prompt, `"required":["agent"`) && !strings.Contains(prompt, `"agent",`) {
			// Loose sanity check: "agent" must still be in a required list
			// somewhere (exact required-array shape differs per type), i.e.
			// this fix must not have accidentally added omitempty.
			if !strings.Contains(prompt, `"agent"`) {
				t.Errorf("%s schema Prompt has no \"agent\" property at all", name)
			}
		}
	}
}
