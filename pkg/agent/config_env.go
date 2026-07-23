package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// Token-efficiency features (T1 aging, Phase 0 metrics) ship disabled and are
// opt-in via environment variables, applied at the single New() chokepoint so
// the REPL, gateway, and subagents all behave uniformly. Explicit AgentConfig
// values always win over env defaults.
//
//	DEEPAI_TOKEN_METRICS=1        write Phase 0 records as JSONL to the default
//	                             path ($TMPDIR/deepai-token-metrics.jsonl)
//	DEEPAI_TOKEN_METRICS=/p.jsonl write JSONL to an explicit path
//	DEEPAI_TOKEN_AGING=1          enable T1 tool-result aging (T1-only; T4 stays
//	                             off until calibrated against Phase 0 data)
const (
	envTokenMetrics = "DEEPAI_TOKEN_METRICS"
	envTokenAging   = "DEEPAI_TOKEN_AGING"

	defaultTokenMetricsFile = "deepai-token-metrics.jsonl"
)

// tokenMetricsPath resolves DEEPAI_TOKEN_METRICS to a JSONL output path:
// empty when unset, the default temp-dir file for a truthy flag, or the value
// itself treated as an explicit path.
func tokenMetricsPath() string {
	v := strings.TrimSpace(os.Getenv(envTokenMetrics))
	switch strings.ToLower(v) {
	case "", "0", "false", "no", "off":
		// Falsy values disable, mirroring envEnabled for DEEPAI_TOKEN_AGING —
		// they must never be mistaken for an output file path.
		return ""
	case "1", "true", "yes", "on":
		return filepath.Join(os.TempDir(), defaultTokenMetricsFile)
	default:
		return v
	}
}

// envEnabled reports whether an environment variable is set to a truthy value.
func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// applyTokenEfficiencyDefaults fills Metrics/Aging from environment variables
// when the caller left them unset. A no-op unless the env vars are truthy, so
// default behavior is unchanged.
func applyTokenEfficiencyDefaults(cfg *AgentConfig) {
	if cfg.Metrics == nil {
		if path := tokenMetricsPath(); path != "" {
			cfg.Metrics = NewFileMetricsSink(path)
		}
	}
	if cfg.Aging == nil && envEnabled(envTokenAging) {
		cfg.Aging = &AgingConfig{
			Enabled:            true,
			MinContextPressure: defaultMinContextPressure,
			// T1 only: T4 (conversation-text compression) stays off until its
			// budgets are calibrated against Phase 0 data (see spec Phase 2).
			ConversationBudgets: map[int]int{},
		}
	}
}
