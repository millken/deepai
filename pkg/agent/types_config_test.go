package agent

import (
	"strings"
	"testing"
)

// TestBuiltinOutputSchemaMounting is the M5-3 wiring requirement (design
// §3/§8 item 2): architect/product-manager/researcher/analyst each get a
// non-Strict OutputSchema; the four reviewers keep their existing Strict
// ReviewResult schema untouched; every other builtin profile (tester is not
// even a builtin — it's project YAML, out of scope this period) has none.
func TestBuiltinOutputSchemaMounting(t *testing.T) {
	nonStrictWant := map[AgentType]bool{
		AgentTypeArchitect:      true,
		AgentTypeProductManager: true,
		AgentTypeResearch:       true,
		AgentTypeAnalyst:        true,
	}
	for at := range nonStrictWant {
		cfg := GetAgentTypeConfig(at)
		if cfg.OutputSchema == nil {
			t.Errorf("%s: OutputSchema = nil, want non-nil", at)
			continue
		}
		if cfg.OutputSchema.Strict {
			t.Errorf("%s: OutputSchema.Strict = true, want false (design §8 item 2: observe one period first)", at)
		}
		if cfg.OutputSchema.Prompt == "" {
			t.Errorf("%s: OutputSchema.Prompt is empty", at)
		}
	}

	strictWant := []AgentType{
		AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer, AgentTypeCorrectnessReviewer,
	}
	for _, at := range strictWant {
		cfg := GetAgentTypeConfig(at)
		if cfg.OutputSchema == nil || !cfg.OutputSchema.Strict {
			t.Errorf("%s: OutputSchema must remain Strict (unchanged this period)", at)
		}
	}

	noSchemaWant := []AgentType{
		AgentTypeGeneral, AgentTypeCoder, AgentTypeBash, AgentTypeFrontend,
		AgentTypeUIDesigner, AgentTypeNews, AgentTypeDocEditor,
	}
	for _, at := range noSchemaWant {
		cfg := GetAgentTypeConfig(at)
		if cfg.OutputSchema != nil {
			t.Errorf("%s: OutputSchema = %+v, want nil", at, cfg.OutputSchema)
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
