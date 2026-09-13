package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// The design gate is a PROGRAM consumer of this schema (pkg/chat's mission
// loop reads scope_files/acceptance to lock the charter), which is the only
// thing that earns a schema its place — see namedSchemas' doc comment.
func TestDesignReviewerProfile(t *testing.T) {
	cfg := GetAgentTypeConfig(AgentTypeDesignReviewer)
	if cfg.Type != AgentTypeDesignReviewer {
		t.Fatalf("Type = %q — design-reviewer is not registered", cfg.Type)
	}
	if cfg.OutputSchema == nil {
		t.Fatal("design-reviewer must be bound to the DesignReviewResult schema")
	}
	if !cfg.OutputSchema.Strict || cfg.OutputSchema.MaxRetries != 1 {
		t.Fatalf("schema binding = strict:%v retries:%d, want strict:true retries:1",
			cfg.OutputSchema.Strict, cfg.OutputSchema.MaxRetries)
	}
	for _, tool := range cfg.DefaultTools {
		// No bash: a plan has no compilable hard signal, and bash would only
		// invite the reviewer to "verify while it is here" and touch the tree
		// (design §5.2). No task: a reviewer never delegates.
		if tool == "bash" || tool == "task" || tool == "write_file" || tool == "edit_file" {
			t.Fatalf("design-reviewer must not carry %q", tool)
		}
	}
	if cfg.MaxToolCalls != defaultReviewerMaxToolCalls {
		t.Fatalf("MaxToolCalls = %d, want %d (same as every other reviewer profile)",
			cfg.MaxToolCalls, defaultReviewerMaxToolCalls)
	}
	if cfg.Temperature != 0.2 {
		t.Fatalf("Temperature = %v, want 0.2 (same as correctness-reviewer)", cfg.Temperature)
	}
}

// The gate's pass test reads scope_files and acceptance, so an output that
// omits them must not validate — a "pass" with an empty charter would lock a
// scope that lets everything (or nothing) through (design §5.2 C4).
func TestDesignReviewSchema_RequiresCharterFields(t *testing.T) {
	schema, ok := NamedSchema("design_review")
	if !ok || schema == nil {
		t.Fatal(`namedSchemas["design_review"] is missing`)
	}
	var s struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(schema.Prompt), &s); err != nil {
		t.Fatalf("unmarshal schema prompt: %v", err)
	}
	for _, want := range []string{"agent", "verdict", "summary", "issues", "scope_files", "acceptance"} {
		found := false
		for _, r := range s.Required {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q must be required in DesignReviewResult; required = %v", want, s.Required)
		}
	}
}

func TestDesignReviewResult_RoundTrip(t *testing.T) {
	schema, _ := NamedSchema("design_review")
	input := `{"agent":"design-reviewer","verdict":"pass","summary":"sound",
		"issues":[],"scope_files":["pkg/chat/mission.go","pkg/chat/mission_test.go"],
		"acceptance":["Given a locked charter, when a turn edits an out-of-scope file, then the gate reverts it"]}`
	got, err := ParseOutput[DesignReviewResult](schema, input)
	if err != nil {
		t.Fatalf("ParseOutput error: %v", err)
	}
	if len(got.ScopeFiles) != 2 || len(got.Acceptance) != 1 {
		t.Fatalf("scope_files=%v acceptance=%v", got.ScopeFiles, got.Acceptance)
	}
}

// area/fault_layer are additive omitempty fields on the SHARED Issue type:
// the four existing reviewers validate their output under WithStrict(true)
// and never emit either, so neither may enter Required (design §5.2 C10).
func TestIssueSchema_AreaAndFaultLayerNotRequired(t *testing.T) {
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
	for _, f := range []string{"area", "fault_layer"} {
		if _, ok := items.Properties[f]; !ok {
			t.Errorf("%q missing from Issue schema properties", f)
		}
		for _, r := range items.Required {
			if r == f {
				t.Errorf("%q must not be required; required = %v", f, items.Required)
			}
		}
	}
	// The legacy shape must still validate against every reviewer's bound schema.
	legacy := `{"agent":"arch-reviewer","verdict":"issues_found","summary":"s",
		"issues":[{"severity":"high","file":"a.go","line":1,"message":"m"}]}`
	for _, at := range []AgentType{AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer, AgentTypeCorrectnessReviewer} {
		cfg := GetAgentTypeConfig(at)
		if err := ValidateOutput(cfg.OutputSchema, legacy); err != nil {
			t.Errorf("%s: legacy output failed strict validation after adding area/fault_layer: %v", at, err)
		}
	}
}

func TestIssue_FaultLayerRoundTrips(t *testing.T) {
	schema := FromStruct[ReviewResult]()
	input := `{"agent":"correctness-reviewer","verdict":"fail","summary":"plan is wrong",
		"issues":[{"severity":"high","file":"a.go","line":10,"message":"interface cannot express the third exit",
		"scenario":"escalation has nowhere to return","fault_layer":"design","area":"feasibility"}]}`
	got, err := ParseOutput[ReviewResult](schema, input)
	if err != nil {
		t.Fatalf("ParseOutput error: %v", err)
	}
	if got.Issues[0].FaultLayer != "design" || got.Issues[0].Area != "feasibility" {
		t.Fatalf("fault_layer=%q area=%q", got.Issues[0].FaultLayer, got.Issues[0].Area)
	}
}

// R1: the schema field is dead weight unless the prompt opens a door for it.
// Rule 3 actively FORBIDS reporting a root cause that lives outside the diff,
// which is exactly what "the plan chose the wrong interface" is. 3a is the
// conditional exception — conditional so a plain /review (no charter in the
// message) behaves exactly as it did before.
func TestCorrectnessReviewerPrompt_OpensFaultLayerOnlyUnderACharter(t *testing.T) {
	p := correctnessReviewerSystemPrompt
	if !strings.Contains(p, "fault_layer") {
		t.Fatal("correctness-reviewer prompt never mentions fault_layer — the schema field cannot be filled")
	}
	if !strings.Contains(p, "If and only if the user message includes a locked charter") {
		t.Error("rule 3a must be conditional on a locked charter being present in the message")
	}
	// Rule 3 itself stays: without a charter it is still what keeps
	// pre-existing bugs out of the implementer's fix rounds.
	if !strings.Contains(p, "A pre-existing problem in code the diff does not touch is out of scope no matter how real it is — do not report it.") {
		t.Error("rule 3 must survive verbatim — it is the no-charter behavior")
	}
	if !strings.Contains(p, `set "implementation"`) {
		t.Error("3a must say what to do when the fault is NOT in the plan")
	}
}

func TestDesignReviewerPrompt_CarriesItsLoadBearingRules(t *testing.T) {
	p := designReviewerSystemPrompt
	for _, want := range []string{
		"brief",            // the fixed anchor, not the latest chatter
		"failure scenario", // rule 2: no scenario, no issue
		"Given/When/Then",  // acceptance must be falsifiable
		"scope_files",      // rule 5: a pass must fill the charter
		"MUST NOT edit",    // read-only
	} {
		if !strings.Contains(p, want) {
			t.Errorf("design-reviewer prompt is missing %q", want)
		}
	}
}
