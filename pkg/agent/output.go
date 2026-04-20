package agent

import (
	"context"
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

// AgentRunner creates new Agent instances for RunWithSchema retry.
type AgentRunner interface {
	Create() *Agent
}

// RunWithSchema wraps Agent.Run() with structured output parsing and optional retry.
// Agent.Run() remains unaware of OutputSchema.
func RunWithSchema[T any](ctx context.Context, factory AgentRunner, schema *OutputSchema,
	sessionID string, messages []models.Message) (*T, *RunResult, error) {

	agent := factory.Create()
	result, err := agent.Run(ctx, sessionID, messages)
	if err != nil {
		return nil, result, err
	}

	parsed, parseErr := ParseOutput[T](schema, result.FinalOutput)
	if parseErr == nil || !schema.Strict {
		return parsed, result, parseErr
	}

	for retry := 0; retry < schema.MaxRetries; retry++ {
		retryMsgs := appendParseError(result.Messages, result.FinalOutput, parseErr)
		retryAgent := factory.Create()
		result, err = retryAgent.Run(ctx, sessionID, retryMsgs)
		if err != nil {
			return nil, result, err
		}
		parsed, parseErr = ParseOutput[T](schema, result.FinalOutput)
		if parseErr == nil {
			return parsed, result, nil
		}
	}
	return parsed, result, parseErr
}

// appendParseError adds a user message describing the parse failure for retry.
func appendParseError(msgs []models.Message, output string, parseErr error) []models.Message {
	return append(msgs[:len(msgs):len(msgs)],
		models.Message{Role: models.RoleHuman, Content: fmt.Sprintf(
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
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}
