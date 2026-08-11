package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

// EnvMaxOutputTokens overrides DefaultMaxOutputTokens (see types.go) for both
// the main REPL agent (pkg/chat/repl.go's mainAgentMaxTokens) and every
// subagent (pkg/commands/chat.go's subagentMaxTokens) — the two call sites
// that set AgentConfig.MaxTokens explicitly rather than leaving it nil, so
// they must both resolve through ResolveMaxOutputTokens rather than reading
// the env var (or the constant) themselves. Exported so those packages' tests
// can set it without duplicating the string literal.
//
//	DEEPAI_MAX_OUTPUT_TOKENS=32000   use 32000 instead of DefaultMaxOutputTokens
const EnvMaxOutputTokens = "DEEPAI_MAX_OUTPUT_TOKENS"

// maxPlausibleOutputTokens rejects settings far outside any real model's
// output limit — the largest advertised by any of this project's providers
// today is on the order of 128k-200k tokens — so a typo like an extra digit
// doesn't get forwarded to the provider as-is.
const maxPlausibleOutputTokens = 1_000_000

// ResolveMaxOutputTokens returns the effective max-output-tokens setting:
// EnvMaxOutputTokens if it parses as a positive integer within a plausible
// range, otherwise DefaultMaxOutputTokens. This is the single resolution
// point; every caller that used to read DefaultMaxOutputTokens directly must
// call this instead so an explicit setting takes effect everywhere at once.
//
// Rejection never falls through to 0: a provider that receives MaxTokens: 0
// applies its OWN default (8192 for Anthropic — see anthropic.go's
// buildMessageParams), which is the exact truncation bug DefaultMaxOutputTokens
// was introduced to fix. So every invalid form (non-numeric, empty, zero,
// negative, or absurdly large) is logged at debug level and mapped to
// DefaultMaxOutputTokens, never to the parsed (or zero) value.
func ResolveMaxOutputTokens() int {
	raw := strings.TrimSpace(os.Getenv(EnvMaxOutputTokens))
	if raw == "" {
		return DefaultMaxOutputTokens
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		slog.Debug("ignoring invalid max output tokens setting: not an integer",
			"env", EnvMaxOutputTokens, "value", raw, "using", DefaultMaxOutputTokens)
		return DefaultMaxOutputTokens
	}
	if n <= 0 {
		slog.Debug("ignoring invalid max output tokens setting: not positive",
			"env", EnvMaxOutputTokens, "value", raw, "using", DefaultMaxOutputTokens)
		return DefaultMaxOutputTokens
	}
	if n > maxPlausibleOutputTokens {
		slog.Debug("ignoring invalid max output tokens setting: exceeds plausible range",
			"env", EnvMaxOutputTokens, "value", raw, "using", DefaultMaxOutputTokens)
		return DefaultMaxOutputTokens
	}
	return n
}

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
