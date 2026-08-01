package agent

import (
	"encoding/json"
	"log/slog"

	"github.com/millken/deepai/pkg/models"
)

// Phase 0 measurement framework (docs/spec/token-efficiency.md §6 Phase 0).
//
// The goal is to decide, from real sessions, which token source drives context
// growth — before enabling any compression strategy. It records two things per
// turn plus one per tool result:
//
//   - PRIMARY metric: the provider's own InputTokens/OutputTokens (the model's
//     real tokenizer; authoritative, decision-grade).
//   - AUXILIARY: a per-role byte breakdown of the outgoing prompt, used to
//     EXPLAIN movements in the primary metric (never to decide on its own),
//     and per-tool result sizes to prioritize T2 tool shaping.
//
// Everything is opt-in: AgentConfig.Metrics == nil means zero overhead.

// ContextBytes is the per-role byte breakdown of one outgoing prompt. Byte
// counts are a coarse, explanatory signal — the ratio against the provider's
// real InputTokens also calibrates the byte heuristic's error rate.
type ContextBytes struct {
	SystemBytes    int `json:"system_bytes"`
	SchemaBytes    int `json:"schema_bytes"`
	HumanBytes     int `json:"human_bytes"`
	AIContentBytes int `json:"ai_content_bytes"` // RoleAI text
	AIArgsBytes    int `json:"ai_args_bytes"`    // sum of ToolCalls.Arguments JSON
	ToolBytes      int `json:"tool_bytes"`       // RoleTool content
	TotalBytes     int `json:"total_bytes"`
}

// ToolFraction is tool_bytes / total_bytes — the share of the prompt taken by
// historical tool results, the quantity T1/T2 target.
func (c ContextBytes) ToolFraction() float64 {
	if c.TotalBytes == 0 {
		return 0
	}
	return float64(c.ToolBytes) / float64(c.TotalBytes)
}

// TurnMetrics is the per-turn record: provider truth (primary) plus the byte
// breakdown of the prompt that produced it (auxiliary).
type TurnMetrics struct {
	Turn         int          `json:"turn"`
	InputTokens  int          `json:"input_tokens"`  // provider truth; 0 if unreported
	OutputTokens int          `json:"output_tokens"` // provider truth; 0 if unreported
	Context      ContextBytes `json:"context"`
}

// ToolResultMetric records one tool result's raw size, to identify which tools
// produce large outputs (guides T2 shaping priority).
type ToolResultMetric struct {
	Turn        int    `json:"turn"`
	ToolName    string `json:"tool_name"`
	ResultBytes int    `json:"result_bytes"`
	// M1.2: Enhanced metrics for compression evaluation
	ArgsHash   string `json:"args_hash,omitempty"`   // tool arguments hash for deduplication detection
	Path       string `json:"path,omitempty"`        // file path for file-based tools
	Offloaded  bool   `json:"offloaded,omitempty"`   // whether result was offloaded to disk
	DurationMs int64  `json:"duration_ms,omitempty"` // tool execution duration in milliseconds
}

// MetricsSink receives Phase 0 measurements. Implementations must be safe for
// use across concurrent Runs (a single agent runs turns sequentially, but one
// sink may be shared by several agents).
type MetricsSink interface {
	RecordTurn(TurnMetrics)
	RecordToolResult(ToolResultMetric)
}

// computeContextBytes buckets the outgoing prompt (message view + system prompt
// + schema) into per-role byte counts. Pure and dependency-free for testing.
func computeContextBytes(messages []models.Message, systemPrompt string, schemaBytes int) ContextBytes {
	c := ContextBytes{SystemBytes: len(systemPrompt), SchemaBytes: schemaBytes}
	for _, m := range messages {
		switch m.Role {
		case models.RoleHuman:
			c.HumanBytes += len(m.Content)
		case models.RoleAI:
			c.AIContentBytes += len(m.Content)
			for _, tc := range m.ToolCalls {
				if b, err := json.Marshal(tc.Arguments); err == nil {
					c.AIArgsBytes += len(b)
				}
			}
		case models.RoleTool:
			c.ToolBytes += len(m.Content)
		case models.RoleSystem:
			// Rare inline system messages count toward system bytes.
			c.SystemBytes += len(m.Content)
		}
	}
	c.TotalBytes = c.SystemBytes + c.SchemaBytes + c.HumanBytes +
		c.AIContentBytes + c.AIArgsBytes + c.ToolBytes
	return c
}

// estimateInputTokens estimates input tokens from byte counts when provider
// doesn't return usage data. Uses a calibrated 3.3 bytes/token heuristic based
// on content analysis: 58.7% code (3.5 bytes/token) + 28.9% JSON (3.0) + 12.4% text (3.0).
// This provides ~9% better accuracy than the previous 3.0 bytes/token estimate.
func estimateInputTokens(ctx ContextBytes) int {
	if ctx.TotalBytes == 0 {
		return 0
	}
	// Calibrated estimate: 3.3 bytes per token (based on content-weighted analysis)
	// This is more accurate than the conservative 3.0 bytes/token for code-heavy workloads.
	estimated := int(float64(ctx.TotalBytes) / 3.3)
	return estimated
}

// LoggingMetricsSink emits each record as a structured log line, so real
// sessions produce a grep-able report without any storage wiring.
type LoggingMetricsSink struct {
	Logger *slog.Logger
}

func (s LoggingMetricsSink) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s LoggingMetricsSink) RecordTurn(m TurnMetrics) {
	s.logger().Info("token_metrics_turn",
		"turn", m.Turn,
		"input_tokens", m.InputTokens,
		"output_tokens", m.OutputTokens,
		"system_bytes", m.Context.SystemBytes,
		"schema_bytes", m.Context.SchemaBytes,
		"human_bytes", m.Context.HumanBytes,
		"ai_content_bytes", m.Context.AIContentBytes,
		"ai_args_bytes", m.Context.AIArgsBytes,
		"tool_bytes", m.Context.ToolBytes,
		"total_bytes", m.Context.TotalBytes,
		"tool_fraction", m.Context.ToolFraction(),
	)
}

func (s LoggingMetricsSink) RecordToolResult(m ToolResultMetric) {
	s.logger().Info("token_metrics_tool",
		"turn", m.Turn,
		"tool", m.ToolName,
		"result_bytes", m.ResultBytes,
		// M1.2: Enhanced metrics logging
		"args_hash", m.ArgsHash,
		"path", m.Path,
		"offloaded", m.Offloaded,
		"duration_ms", m.DurationMs,
	)
}
