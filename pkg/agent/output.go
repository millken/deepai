package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/millken/deepai/pkg/models"
)

// OutputSchema constrains agent output to a structured JSON format.
type OutputSchema struct {
	Schema     *jsonschema.Schema
	Resolved   *jsonschema.Resolved
	Prompt     string // JSON Schema as JSON string, injected into system prompt
	Strict     bool   // if true, parse failure triggers retry
	MaxRetries int    // max retries on parse failure (only when Strict)
}

// SchemaOption configures an OutputSchema.
type SchemaOption func(*OutputSchema)

// WithStrict enables retry on parse failure.
func WithStrict(strict bool) SchemaOption {
	return func(os *OutputSchema) { os.Strict = strict }
}

// WithMaxRetries sets the maximum number of retries on parse failure.
func WithMaxRetries(n int) SchemaOption {
	return func(os *OutputSchema) { os.MaxRetries = n }
}

// FromStruct infers a JSON Schema from a Go struct type T and constructs an OutputSchema.
func FromStruct[T any](opts ...SchemaOption) *OutputSchema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("OutputSchema.FromStruct: %v", err))
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("OutputSchema.Resolve: %v", err))
	}
	promptBytes, _ := schema.MarshalJSON()
	os := &OutputSchema{
		Schema:   schema,
		Resolved: resolved,
		Prompt:   string(promptBytes),
	}
	for _, opt := range opts {
		opt(os)
	}
	return os
}

// ParseOutput extracts a typed value from raw text using the OutputSchema.
// It finds the first JSON object in the text and validates it against the schema.
func ParseOutput[T any](schema *OutputSchema, text string) (*T, error) {
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON object found in output")
	}

	// Validate against schema if available.
	if schema.Resolved != nil {
		var raw any
		if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		if err := schema.Resolved.Validate(raw); err != nil {
			return nil, fmt.Errorf("schema validation failed: %w", err)
		}
	}

	var result T
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("JSON unmarshal to %T: %w", result, err)
	}
	return &result, nil
}

// ValidateOutput checks raw text output against schema without unmarshaling
// into a concrete Go type. It exists for call sites (e.g. SubagentExecutor.Execute)
// that hold an *OutputSchema but have no type parameter T to hand ParseOutput —
// the executor works generically across agent-type profiles, so it cannot be
// generic itself. Error wording mirrors ParseOutput so retry prompts built via
// appendParseError stay identical regardless of which entry point produced the
// error. Nil-safe: a nil schema, or a schema with no resolved validator (e.g.
// zero-value &OutputSchema{}), means "nothing to validate" and returns nil.
func ValidateOutput(schema *OutputSchema, text string) error {
	if schema == nil || schema.Resolved == nil {
		return nil
	}
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return fmt.Errorf("no JSON object found in output")
	}
	var raw any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := schema.Resolved.Validate(raw); err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}
	return nil
}

// extractJSON finds the last balanced JSON object in text.
// Returns the last match to handle cases where the model includes
// JSON examples in preamble text before the actual structured output.
func extractJSON(text string) string {
	lastEnd := -1
	lastStart := -1
	offset := 0
	for offset < len(text) {
		start := strings.IndexByte(text[offset:], '{')
		if start < 0 {
			break
		}
		start += offset
		depth := 0
		inStr := false
		escape := false
		end := -1
	loop:
		for i := start; i < len(text); i++ {
			ch := text[i]
			if escape {
				escape = false
				continue
			}
			if ch == '\\' && inStr {
				escape = true
				continue
			}
			if ch == '"' {
				inStr = !inStr
				continue
			}
			if inStr {
				continue
			}
			switch ch {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					end = i + 1
					break loop
				}
			}
		}
		if end < 0 {
			break
		}
		lastStart = start
		lastEnd = end
		offset = end
	}
	if lastStart >= 0 && lastEnd > lastStart {
		return text[lastStart:lastEnd]
	}
	return ""
}

// appendParseError adds a user message describing the parse failure for retry.
// Used by SubagentExecutor.Execute's Strict-schema retry loop (subagent.go)
// to seed the retry request with what went wrong.
func appendParseError(msgs []models.Message, output string, parseErr error) []models.Message {
	return append(msgs[:len(msgs):len(msgs)],
		models.Message{Role: models.RoleHuman,
			Metadata: map[string]string{metaAgentInjected: "true"},
			Content: fmt.Sprintf(
				"Your previous output could not be parsed as the required schema:\n\nError: %s\n\nOutput:\n%s\n\nPlease output valid JSON matching the schema.",
				parseErr, output,
			)},
	)
}

// ReviewResult is the structured output for reviewer agents.
type ReviewResult struct {
	Agent   string  `json:"agent"`
	Verdict string  `json:"verdict"`
	Summary string  `json:"summary"`
	Issues  []Issue `json:"issues"`
}

// Issue represents a single finding from a code review.
type Issue struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
	// Scenario is a reproducible failure scenario: specific input or state
	// → specific wrong output or behavior. The correctness reviewer is
	// required by its prompt to fill it (an issue without a scenario does
	// not count); other reviewers may leave it empty — omitempty keeps the
	// field out of Required for them (google/jsonschema-go infers Required
	// from the absence of omitempty/omitzero).
	Scenario   string `json:"scenario,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// ---------------------------------------------------------------------------
// M5-3 output contracts (docs/AGENT_CAPABILITY_DESIGN.md §3).
//
// Every field below carries an explicit lowercase json tag, matching
// ReviewResult/Issue's existing precedent above. This is a deliberate call
// (design §3 revision, item 2), not an oversight: the schema Prompt these
// types produce (FromStruct's MarshalJSON of the inferred JSON Schema) is
// JSON Schema text, and a model asked to match it writes lowercase
// snake_case property names by instinct — PascalCase is the counter-instinct
// choice, and every place a prompt asks for the counter-instinct is a place
// parsing quietly fails. The eval harness (pkg/commands/agent_eval_schema.go)
// decodes against these SAME types, not a mirror, so the scored contract and
// the contract the role is told about cannot drift apart.
// ---------------------------------------------------------------------------

// DesignDoc is the structured output contract for the architect agent type.
// Decisions/Components are required (no omitempty): a DesignDoc with neither
// is not a design. Risks/OpenQuestions/Milestones are optional.
type DesignDoc struct {
	Agent         string      `json:"agent" jsonschema:"the agent type that produced this, e.g. architect"`
	Goal          string      `json:"goal"`
	Decisions     []Decision  `json:"decisions"`
	Components    []Component `json:"components"`
	Risks         []Risk      `json:"risks,omitempty"`
	OpenQuestions []string    `json:"open_questions,omitempty"`
	Milestones    []string    `json:"milestones,omitempty"`
}

// Decision is one design decision: what was chosen, why, what was rejected,
// and whether it can still be undone.
type Decision struct {
	Topic        string   `json:"topic"`
	Choice       string   `json:"choice"`
	Rationale    string   `json:"rationale"`
	Alternatives []string `json:"alternatives"`
	Reversible   bool     `json:"reversible"`
}

// Component is one piece of the design: its responsibility, the files it
// touches, and the interfaces it defines or changes.
type Component struct {
	Name           string   `json:"name"`
	Responsibility string   `json:"responsibility"`
	Files          []string `json:"files"`
	Interfaces     []string `json:"interfaces"`
}

// Risk is a design risk and its mitigation.
type Risk struct {
	Description string `json:"description"`
	Mitigation  string `json:"mitigation"`
}

// RequirementsSpec is the structured output contract for the
// product-manager agent type.
type RequirementsSpec struct {
	Agent         string      `json:"agent" jsonschema:"the agent type that produced this, e.g. product-manager"`
	Problem       string      `json:"problem"`
	Stories       []UserStory `json:"stories"`
	ScopeIn       []string    `json:"scope_in"`
	ScopeOut      []string    `json:"scope_out"`
	Acceptance    []Criterion `json:"acceptance"`
	Priorities    []Priority  `json:"priorities"`
	OpenQuestions []string    `json:"open_questions,omitempty"`
}

// UserStory is one Role/Want/SoThat story.
type UserStory struct {
	Role   string `json:"role"`
	Want   string `json:"want"`
	SoThat string `json:"so_that"`
}

// Criterion is one Given/When/Then acceptance criterion. Verifiable=false is
// the honest exit for a criterion only human judgment can settle — see
// productManagerSystemPrompt's Verifiability paragraph.
type Criterion struct {
	ID         string `json:"id"`
	Given      string `json:"given"`
	When       string `json:"when"`
	Then       string `json:"then"`
	Verifiable bool   `json:"verifiable"`
}

// Priority ranks one item P0..P3. Level has no schema-ENFORCED enum: see the
// package-level note above Finding.Confidence — jsonschema-go v0.4.3 has no
// struct-tag mechanism for a JSON Schema "enum" keyword (confirmed:
// TestEnumTagIsRejectedByJSONSchemaGo in output_test.go), so the jsonschema
// tag below is a description-only fallback, not a validated constraint.
type Priority struct {
	Item   string `json:"item"`
	Level  string `json:"level" jsonschema:"one of exactly: P0, P1, P2, or P3"`
	Reason string `json:"reason"`
}

// ResearchFindings is the structured output contract for the researcher
// agent type.
type ResearchFindings struct {
	Agent    string    `json:"agent" jsonschema:"the agent type that produced this, e.g. researcher"`
	Question string    `json:"question"`
	Answer   string    `json:"answer"`
	Findings []Finding `json:"findings"`
	Gaps     []string  `json:"gaps,omitempty"`
}

// Finding is shared by ResearchFindings and AnalysisReport (design §3): a
// researcher hands over what it read, an analyst hands over what it means —
// the deliverable differs, the evidence shape does not.
type Finding struct {
	Claim    string     `json:"claim"`
	Evidence []Evidence `json:"evidence"`
	// Confidence has NO schema-enforced enum — see Priority.Level's comment
	// above; same jsonschema-go limitation, same description-only fallback.
	Confidence string `json:"confidence" jsonschema:"one of exactly: high, medium, or low"`
}

// Evidence anchors a Claim to source: File + Line (+ optional EndLine for a
// multi-line citation) + a verbatim Quote.
//
// Line/EndLine are int, NOT a "21-24" range string or "file:line" text — a
// deliberate deviation from the §3 draft's single `Line` field (design §3
// revision, item 3). A model citing a multi-line quote is the common case;
// letting Line be a permissive string would let a range or "file:line" text
// pass json.Unmarshal for that one field while silently meaning something
// no int-typed consumer can use. Worse, a STRICT int field rejects such a
// value at the json.Unmarshal stage for the WHOLE enclosing struct — every
// field_* assertion on that output is skipped, not merely wrong, for one
// mis-shaped field. EndLine is omitempty: a single-line citation needs only
// Line.
type Evidence struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	Quote   string `json:"quote"`
	URL     string `json:"url,omitempty"`
}

// AnalysisReport is the structured output contract for the analyst agent
// type. Method is required and comes before Findings in the struct (and in
// the schema's PropertyOrder) because analystSystemPrompt's load-bearing
// rule is "method before results".
type AnalysisReport struct {
	Agent     string    `json:"agent" jsonschema:"the agent type that produced this, e.g. analyst"`
	Objective string    `json:"objective"`
	Method    string    `json:"method"`
	Findings  []Finding `json:"findings"`
	Caveats   []string  `json:"caveats,omitempty"`
	Artifacts []string  `json:"artifacts,omitempty"`
}

// TestReport is the structured output contract for the tester role (a
// project YAML profile, not a builtin AgentType). It is defined here now
// because the eval harness needs a real type to decode and score
// `schema_parses: test-report` against, but it is deliberately NOT mounted
// onto any AgentTypeConfig.OutputSchema this period — tester stays
// unmodified as M5-3's control group (see types_config.go's init()).
type TestReport struct {
	Agent    string        `json:"agent"`
	Command  string        `json:"command"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Failures []TestFailure `json:"failures"`
	Coverage string        `json:"coverage,omitempty"`
}

// TestFailure is one failing test: its name, where it failed, and why.
type TestFailure struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}
