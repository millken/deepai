package agent

import (
	"strings"
	"testing"
)

// TestBuiltinOutputSchemaMounting is the design §8 revision four (M5-4)
// rollback requirement: the M5-3 non-Strict OutputSchema mounted on
// architect/product-manager/researcher/analyst is gone — none of the five
// contract types had a real program consumer, only the eval harness reading
// its own invention, so all four now carry OutputSchema == nil, same as
// every other non-reviewer builtin profile. The four reviewers keep their
// existing Strict ReviewResult schema untouched — that one has a real
// consumer (pkg/chat/review.go's review gate).
func TestBuiltinOutputSchemaMounting(t *testing.T) {
	noSchemaWant := []AgentType{
		AgentTypeGeneral, AgentTypeCoder, AgentTypeBash, AgentTypeFrontend,
		AgentTypeUIDesigner, AgentTypeNews, AgentTypeDocEditor,
		AgentTypeArchitect, AgentTypeProductManager, AgentTypeResearch, AgentTypeAnalyst,
	}
	for _, at := range noSchemaWant {
		cfg := GetAgentTypeConfig(at)
		if cfg.OutputSchema != nil {
			t.Errorf("%s: OutputSchema = %+v, want nil (M5-4 rollback)", at, cfg.OutputSchema)
		}
	}

	strictWant := []AgentType{
		AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer, AgentTypeCorrectnessReviewer,
	}
	for _, at := range strictWant {
		cfg := GetAgentTypeConfig(at)
		if cfg.OutputSchema == nil || !cfg.OutputSchema.Strict {
			t.Errorf("%s: OutputSchema must remain Strict (untouched by the M5-4 rollback)", at)
		}
	}
}

// TestBuiltinDescriptionsFollowSpec covers ONLY the seven roles M5-3 actually
// rewrote (design §4 revision item 7 / brief §6): the four reviewers'
// Focus-and-Rules text was left alone by an earlier period and every other
// builtin (coder, frontend, correctness-reviewer, etc.) still carries its
// pre-M5-3 Description, which does not follow this template and would fail
// these assertions immediately — that's expected, and out of scope until a
// later milestone rewrites them too. This whitelist is the same reason:
// widen it only when a role's Description is actually rewritten to match.
func TestBuiltinDescriptionsFollowSpec(t *testing.T) {
	roles := []AgentType{
		AgentTypeArchitect,
		AgentTypeProductManager,
		AgentTypeResearch,
		AgentTypeAnalyst,
		AgentTypeSecurityReviewer,
		AgentTypeArchReviewer,
		AgentTypePerfReviewer,
	}
	for _, at := range roles {
		cfg := GetAgentTypeConfig(at)
		desc := cfg.Description
		if n := len([]rune(desc)); n > 100 {
			t.Errorf("%s: Description is %d runes, want <=100: %q", at, n, desc)
		}
		if !strings.HasPrefix(desc, "Use when ") {
			t.Errorf("%s: Description does not start with %q: %q", at, "Use when ", desc)
		}
		if !strings.Contains(desc, "Not for ") {
			t.Errorf("%s: Description does not contain %q: %q", at, "Not for ", desc)
		}
	}
}

// TestFourRolesL1DropsJSONContract is the design §8 revision four (M5-4)
// rollback requirement: architect/product-manager/researcher/analyst each
// had an "Output: your entire final message is ONE JSON object..." paragraph
// appended to their L1 constitution in M5-3; that paragraph (and only that
// paragraph — Evidence/Executability-or-Verifiability/Budget stay) is gone.
// The rest of each constitution is untouched content, so it must not
// mention "JSON" anywhere else either — the assertion is a whole-prompt
// substring check, not just "the last paragraph is gone".
func TestFourRolesL1DropsJSONContract(t *testing.T) {
	roles := []AgentType{
		AgentTypeArchitect, AgentTypeProductManager, AgentTypeResearch, AgentTypeAnalyst,
	}
	for _, at := range roles {
		prompt := GetAgentTypeConfig(at).SystemPrompt
		if strings.Contains(prompt, "JSON") {
			t.Errorf("%s: SystemPrompt still mentions JSON (want the Output contract paragraph gone): %q", at, prompt)
		}
	}
}
