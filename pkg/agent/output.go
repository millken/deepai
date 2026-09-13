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
	// Area classifies a DESIGN review finding: scope | feasibility |
	// completeness | acceptance | risk. Empty for every code reviewer.
	Area string `json:"area,omitempty"`
	// FaultLayer is the mission loop's escalation signal (design
	// docs/LONG_TASK_LOOP_DESIGN.md §5.4.3, D4): "implementation" (or empty)
	// means the code is wrong, "design" means the code faithfully implements
	// a plan that cannot satisfy the brief, and the loop must re-enter the
	// design phase instead of spending another fix round. Only the
	// correctness reviewer fills it, and only when its message carried a
	// locked charter (see rule 3a in correctnessReviewerSystemPrompt) —
	// without one, rule 3 still forbids reporting anything outside the diff.
	//
	// Both fields are omitempty for the same reason Scenario is: the four
	// existing reviewers share this type under a Strict schema and emit
	// neither, and google/jsonschema-go infers Required from the absence of
	// omitempty.
	FaultLayer string `json:"fault_layer,omitempty"`
}

// DesignReviewResult is the structured output of the design reviewer — a
// SEPARATE type from ReviewResult, not a superset of it (design §八 C10):
// merging the two would make scope_files/acceptance required of the four
// code reviewers, whose Strict validation would then start failing on
// output they have always produced.
//
// ScopeFiles and Acceptance are not decoration: on a passing verdict they
// BECOME the mission charter (§5.3), so the gate treats an empty either as
// a failure rather than a pass (isDesignPass) — a charter with no scope
// would make the implementation phase's hard scope check admit everything.
type DesignReviewResult struct {
	Agent      string   `json:"agent"`
	Verdict    string   `json:"verdict"`
	Summary    string   `json:"summary"`
	Issues     []Issue  `json:"issues"`
	ScopeFiles []string `json:"scope_files"`
	Acceptance []string `json:"acceptance"`
}
