package agent

import (
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

// TestEnumTagIsRejectedByJSONSchemaGo documents a finding that governed a
// real decision (design §3 revision item 4): jsonschema-go v0.4.3 has NO
// struct-tag mechanism for a JSON Schema "enum" keyword. A
// `jsonschema:"enum=..."` tag does not silently get ignored — For[T] panics
// (via FromStruct) because the tag text starts with a bare "WORD=" prefix,
// which the library reserves and rejects outright
// (disallowedPrefixRegexp in jsonschema-go/jsonschema/infer.go). This test
// guards against silently "pretending it works": if a future jsonschema-go
// upgrade starts supporting this syntax, this test will fail (no panic),
// which is the signal that a real oneOf/enum schema became available for any
// future schema-carrying struct.
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

// TestNamedSchemasTable is the design §8 revision four (M5-4) rollback
// requirement: the five non-Strict M5-3 contract schemas (design/
// requirements/research/analysis/test-report) are gone — they had no real
// program consumer, only the eval harness reading its own invention.
//
// The surviving entries are exactly the ones a PROGRAM reads: "review"
// (pkg/chat's post-edit gate parses ReviewResult) and "design_review"
// (LONG_TASK_LOOP_DESIGN §5.2 — the mission loop's design gate parses
// scope_files/acceptance out of it to lock the charter). Both are Strict.
// Adding a third entry means demonstrating the same thing: code reads it,
// not just another model.
func TestNamedSchemasTable(t *testing.T) {
	reviewSchema, ok := namedSchemas["review"]
	if !ok || reviewSchema == nil {
		t.Fatal("namedSchemas[\"review\"] missing")
	}
	if !reviewSchema.Strict {
		t.Error("namedSchemas[\"review\"].Strict = false, want true (matches builtin reviewer profiles)")
	}

	designSchema, ok := namedSchemas["design_review"]
	if !ok || designSchema == nil {
		t.Fatal("namedSchemas[\"design_review\"] missing")
	}
	if !designSchema.Strict {
		t.Error("namedSchemas[\"design_review\"].Strict = false, want true — the mission gate parses it")
	}
	if len(namedSchemas) != 2 {
		t.Errorf("namedSchemas has %d entries, want exactly 2 (\"review\" and \"design_review\"): %v", len(namedSchemas), namedSchemas)
	}
	for _, removed := range []string{"design", "requirements", "research", "analysis", "test-report"} {
		if _, ok := namedSchemas[removed]; ok {
			t.Errorf("namedSchemas[%q] still present, want it removed (M5-4 rollback)", removed)
		}
	}
}
