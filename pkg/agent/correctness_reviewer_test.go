package agent

import (
	"encoding/json"
	"testing"
)

// The scenario field must stay OUT of the schema's required set: the three
// legacy reviewers (security/arch/perf) don't emit it, and their output is
// validated under WithStrict(true). This is the regression guarantee the
// design leans on (docs/ADVERSARIAL_REVIEW_DESIGN.md §4.3): omitempty keeps
// a field out of Required under google/jsonschema-go inference.
func TestReviewSchemaScenarioOptionalForLegacyReviewers(t *testing.T) {
	cfg := GetAgentTypeConfig(AgentTypeSecurityReviewer)
	if cfg.OutputSchema == nil {
		t.Fatal("security-reviewer lost its OutputSchema binding")
	}
	legacy := `{"agent":"security-reviewer","verdict":"issues_found","summary":"s",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"m","suggestion":"s"}]}`
	if err := ValidateOutput(cfg.OutputSchema, legacy); err != nil {
		t.Fatalf("legacy reviewer output without scenario failed strict validation: %v", err)
	}
}

func TestReviewSchemaScenarioNotRequired(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	var s struct {
		Properties struct {
			Issues struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                   `json:"required"`
				} `json:"items"`
			} `json:"issues"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema.Prompt), &s); err != nil {
		t.Fatalf("unmarshal schema prompt: %v", err)
	}
	items := s.Properties.Issues.Items
	if _, ok := items.Properties["scenario"]; !ok {
		t.Fatal("scenario missing from Issue schema properties")
	}
	for _, r := range items.Required {
		if r == "scenario" || r == "suggestion" {
			t.Fatalf("%q must not be required (legacy reviewers omit it); required = %v", r, items.Required)
		}
	}
	// message must still be required — omitempty was added selectively.
	found := false
	for _, r := range items.Required {
		if r == "message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("message should remain required; required = %v", items.Required)
	}
}

func TestParseOutputScenarioRoundTrip(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `{"agent":"correctness-reviewer","verdict":"fail","summary":"broken",
		"issues":[{"severity":"high","file":"a.go","line":10,"message":"nil deref",
		"scenario":"call F(nil) -> panic at a.go:10","suggestion":"guard nil"}]}`
	result, err := ParseOutput[ReviewResult](schema, input)
	if err != nil {
		t.Fatalf("ParseOutput error: %v", err)
	}
	if got := result.Issues[0].Scenario; got != "call F(nil) -> panic at a.go:10" {
		t.Errorf("Scenario = %q, want the round-tripped value", got)
	}
}

func TestCorrectnessReviewerProfile(t *testing.T) {
	cfg := GetAgentTypeConfig(AgentTypeCorrectnessReviewer)
	// GetAgentTypeConfig falls back to general-purpose for unknown types, so
	// the Type assertion proves registration.
	if cfg.Type != AgentTypeCorrectnessReviewer {
		t.Fatalf("Type = %q — correctness-reviewer is not registered", cfg.Type)
	}
	if cfg.OutputSchema == nil {
		t.Fatal("correctness-reviewer must be bound to the ReviewResult schema")
	}
	if !cfg.OutputSchema.Strict || cfg.OutputSchema.MaxRetries != 1 {
		t.Fatalf("schema binding = strict:%v retries:%d, want strict:true retries:1",
			cfg.OutputSchema.Strict, cfg.OutputSchema.MaxRetries)
	}
	hasBash := false
	for _, tool := range cfg.DefaultTools {
		if tool == "task" {
			t.Fatal("reviewer must never get the task tool")
		}
		if tool == "bash" {
			hasBash = true
		}
	}
	if !hasBash {
		t.Fatal("correctness-reviewer needs bash to substantiate charges by running tests")
	}
	if cfg.SystemPrompt == "" || cfg.MaxToolCalls != 0 {
		t.Fatalf("unexpected profile: prompt empty=%v maxToolCalls=%d", cfg.SystemPrompt == "", cfg.MaxToolCalls)
	}
}
