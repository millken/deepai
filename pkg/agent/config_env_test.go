package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/tools"
)

func newTestAgent(t *testing.T, cfg AgentConfig) *Agent {
	t.Helper()
	if cfg.LLMProvider == nil {
		cfg.LLMProvider = &captureProvider{}
	}
	if cfg.Tools == nil {
		cfg.Tools = tools.NewRegistry()
	}
	if cfg.Model == "" {
		cfg.Model = "m"
	}
	return New(cfg)
}

func TestEnvEnabled(t *testing.T) {
	cases := map[string]bool{"1": true, "true": true, "YES": true, "on": true, "0": false, "": false, "off": false}
	for val, want := range cases {
		t.Setenv("DEEPAI_TEST_FLAG", val)
		if got := envEnabled("DEEPAI_TEST_FLAG"); got != want {
			t.Errorf("envEnabled(%q) = %v, want %v", val, got, want)
		}
	}
}

// TestMain strips ambient DEEPAI_TOKEN_* exports before any test in this
// package runs: applyTokenEfficiencyDefaults executes inside every New(), so a
// developer's shell exports (set per the feature docs) would otherwise attach
// sinks/aging to every constructed agent and make unrelated assertions flaky.
// Tests that exercise the env path opt back in explicitly via t.Setenv.
func TestMain(m *testing.M) {
	os.Unsetenv(envTokenMetrics)
	os.Unsetenv(envTokenAging)
	os.Unsetenv(EnvMaxOutputTokens)
	os.Unsetenv(EnvStreamIdleTimeout)
	os.Exit(m.Run())
}

// clearTokenEnv isolates a test from ambient DEEPAI_TOKEN_* exports (a developer
// following the feature docs may have them in their shell; t.Setenv registers
// restoration so the unset never leaks past the test).
func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envTokenMetrics, envTokenAging} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestTokenEfficiency_DisabledByDefault(t *testing.T) {
	clearTokenEnv(t)
	a := newTestAgent(t, AgentConfig{})
	if a.metrics != nil {
		t.Error("metrics should be nil when env unset")
	}
	if a.aging != nil {
		t.Error("aging should be nil when env unset")
	}
}

func TestTokenEfficiency_FalsyMetricsValuesDisable(t *testing.T) {
	// "0"/"false"/"off" mean OFF, mirroring DEEPAI_TOKEN_AGING — they must never
	// be interpreted as an output file path (a stray file named "0").
	for _, v := range []string{"0", "false", "no", "off", "OFF"} {
		t.Setenv(envTokenMetrics, v)
		a := newTestAgent(t, AgentConfig{})
		if a.metrics != nil {
			t.Errorf("DEEPAI_TOKEN_METRICS=%q should disable metrics, got %T", v, a.metrics)
		}
	}
}

func TestTokenEfficiency_MetricsEnvEnablesDefaultPath(t *testing.T) {
	t.Setenv(envTokenMetrics, "1")
	a := newTestAgent(t, AgentConfig{})
	sink, ok := a.metrics.(*FileMetricsSink)
	if !ok {
		t.Fatalf("DEEPAI_TOKEN_METRICS=1 should attach *FileMetricsSink, got %T", a.metrics)
	}
	if !strings.HasSuffix(sink.path, defaultTokenMetricsFile) {
		t.Errorf("default path = %q, want suffix %q", sink.path, defaultTokenMetricsFile)
	}
}

func TestTokenEfficiency_MetricsEnvExplicitPath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-metrics.jsonl")
	t.Setenv(envTokenMetrics, want)
	a := newTestAgent(t, AgentConfig{})
	sink, ok := a.metrics.(*FileMetricsSink)
	if !ok {
		t.Fatalf("explicit path should attach *FileMetricsSink, got %T", a.metrics)
	}
	if sink.path != want {
		t.Errorf("path = %q, want %q", sink.path, want)
	}
}

func TestTokenEfficiency_AgingEnvEnables(t *testing.T) {
	t.Setenv(envTokenAging, "true")
	a := newTestAgent(t, AgentConfig{})
	if a.aging == nil || !a.aging.Enabled {
		t.Fatal("DEEPAI_TOKEN_AGING=true should enable aging")
	}
	if a.aging.MinContextPressure != defaultMinContextPressure {
		t.Errorf("MinContextPressure = %v, want %v", a.aging.MinContextPressure, defaultMinContextPressure)
	}
	// T1-only: T4 must stay off (empty, non-nil ConversationBudgets).
	if a.aging.ConversationBudgets == nil || len(a.aging.ConversationBudgets) != 0 {
		t.Errorf("ConversationBudgets should be empty (T4 off), got %v", a.aging.ConversationBudgets)
	}
	// T1 tool-result budgets: unknown tools fall back to §5.4 "default" row.
	if budget := a.aging.toolResultBudget(1, "unknown_tool"); budget != 4096 {
		t.Errorf("age-1 unknown tool budget = %d, want 4096 (§5.4 default)", budget)
	}
}

// TestResolveMaxOutputTokens_Unset pins the fallback: with no override
// configured, both the main agent and every subagent land on
// DefaultMaxOutputTokens (see mainAgentMaxTokens / subagentMaxTokens, which
// both call this function rather than reading the constant directly).
func TestResolveMaxOutputTokens_Unset(t *testing.T) {
	t.Setenv(EnvMaxOutputTokens, "")
	os.Unsetenv(EnvMaxOutputTokens)
	if got := ResolveMaxOutputTokens(); got != DefaultMaxOutputTokens {
		t.Errorf("ResolveMaxOutputTokens() = %d, want DefaultMaxOutputTokens (%d)", got, DefaultMaxOutputTokens)
	}
}

// TestResolveMaxOutputTokens_ValidValueWins pins that an explicit, valid
// setting is used verbatim instead of DefaultMaxOutputTokens.
func TestResolveMaxOutputTokens_ValidValueWins(t *testing.T) {
	t.Setenv(EnvMaxOutputTokens, "32000")
	const want = 32000
	if got := ResolveMaxOutputTokens(); got != want {
		t.Errorf("ResolveMaxOutputTokens() = %d, want explicit value %d", got, want)
	}
	if want == DefaultMaxOutputTokens {
		t.Fatal("test setup bug: chosen value must differ from the default to prove it was actually read")
	}
}

// TestResolveMaxOutputTokens_InvalidFallsBackToDefault is the rule that
// matters most: MaxTokens must never reach the provider as 0, because a
// provider that receives 0 applies its OWN default (8192 for Anthropic),
// silently reintroducing the truncation bug DefaultMaxOutputTokens was added
// to fix. Every rejected form must land on DefaultMaxOutputTokens exactly —
// not merely "some non-input value" — which is why each case asserts equality
// with the constant AND explicitly asserts the result is not 0.
func TestResolveMaxOutputTokens_InvalidFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"non-numeric":    "not-a-number",
		"empty-string":   "",
		"zero":           "0",
		"negative":       "-100",
		"absurdly-large": "100000000000",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvMaxOutputTokens, raw)
			got := ResolveMaxOutputTokens()
			if got != DefaultMaxOutputTokens {
				t.Errorf("ResolveMaxOutputTokens() with %s (%q) = %d, want DefaultMaxOutputTokens (%d)", name, raw, got, DefaultMaxOutputTokens)
			}
			if got == 0 {
				t.Errorf("ResolveMaxOutputTokens() with %s (%q) = 0; must never coerce to 0 (provider would apply its own default)", name, raw)
			}
		})
	}
}

func TestTokenEfficiency_ExplicitConfigWinsOverEnv(t *testing.T) {
	t.Setenv(envTokenMetrics, "1")
	t.Setenv(envTokenAging, "1")
	sink := &sliceMetricsSink{}
	custom := &AgingConfig{Enabled: true, MinContextPressure: 0.9}
	a := newTestAgent(t, AgentConfig{Metrics: sink, Aging: custom})

	if a.metrics != sink {
		t.Error("explicit Metrics must not be overridden by env")
	}
	if a.aging != custom || a.aging.MinContextPressure != 0.9 {
		t.Error("explicit Aging must not be overridden by env")
	}
}

// TestResolveStreamIdleTimeout_Unset pins the fallback: with no override,
// the effective idle window is defaultStreamIdleTimeout (2 minutes) — the
// value every caller got before DEEPAI_STREAM_IDLE_TIMEOUT existed.
func TestResolveStreamIdleTimeout_Unset(t *testing.T) {
	t.Setenv(EnvStreamIdleTimeout, "")
	os.Unsetenv(EnvStreamIdleTimeout)
	if got := ResolveStreamIdleTimeout(); got != defaultStreamIdleTimeout {
		t.Errorf("ResolveStreamIdleTimeout() = %v, want defaultStreamIdleTimeout (%v)", got, defaultStreamIdleTimeout)
	}
}

// TestResolveStreamIdleTimeout_ValidValueWins pins that a user stuck with
// "stream idle timeout: no data received after 2m0s" can raise the window
// themselves without a rebuild — the whole point of exposing this.
func TestResolveStreamIdleTimeout_ValidValueWins(t *testing.T) {
	t.Setenv(EnvStreamIdleTimeout, "5m")
	const want = 5 * time.Minute
	if got := ResolveStreamIdleTimeout(); got != want {
		t.Errorf("ResolveStreamIdleTimeout() = %v, want %v", got, want)
	}
	if want == defaultStreamIdleTimeout {
		t.Fatal("test setup bug: chosen value must differ from the default to prove it was actually read")
	}
}

// TestResolveStreamIdleTimeout_InvalidFallsBackToDefault: an unparseable or
// non-positive duration must never reach the watchdog as-is. A negative or
// unparseable value obviously has no sane meaning; 0 specifically must not
// be allowed to pass through even though consumeStream (pkg/agent/streaming.go)
// happens to also guard <= 0 itself — this resolver must not rely on that
// second guard, since ResolveStreamIdleTimeout is the single source of truth
// New() uses to populate Agent.streamIdleTimeout, and a silently-accepted 0
// stored on the struct would be one guard away from every future caller of
// that field.
func TestResolveStreamIdleTimeout_InvalidFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"non-duration": "not-a-duration",
		"empty-string": "",
		"zero":         "0s",
		"negative":     "-5m",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvStreamIdleTimeout, raw)
			got := ResolveStreamIdleTimeout()
			if got != defaultStreamIdleTimeout {
				t.Errorf("ResolveStreamIdleTimeout() with %s (%q) = %v, want defaultStreamIdleTimeout (%v)", name, raw, got, defaultStreamIdleTimeout)
			}
			if got <= 0 {
				t.Errorf("ResolveStreamIdleTimeout() with %s (%q) = %v; must never be <= 0", name, raw, got)
			}
		})
	}
}

// TestNew_StreamIdleTimeoutFromEnv proves the env var actually reaches
// Agent.streamIdleTimeout through New() — ResolveStreamIdleTimeout alone
// isn't enough evidence since the field could still be wired to the bare
// constant.
func TestNew_StreamIdleTimeoutFromEnv(t *testing.T) {
	t.Setenv(EnvStreamIdleTimeout, "7m")
	a := newTestAgent(t, AgentConfig{})
	if a.streamIdleTimeout != 7*time.Minute {
		t.Errorf("a.streamIdleTimeout = %v, want 7m", a.streamIdleTimeout)
	}
}
