package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/spf13/cobra"
)

// analyze scans persisted sessions for delegation-efficiency problems. It is
// deliberately READ-ONLY: findings are printed (or emitted as --json) with
// concrete suggestions, and applying them stays a human decision — the
// analysis rules encode what past runs measured, not what the next run
// should blindly do.
//
// Data sources (all from the session DB, so nothing needs to be enabled or
// configured ahead of time):
//   - task tool calls + their results, whose Data carries subagent_stats /
//     subagent_usage (populated since the RunStats work; older rows simply
//     lack stats and are skipped by the rules that need them)
//   - the main agent's own tool calls, to detect bash-side patterns (e.g.
//     hand-rolled symbol outlines instead of code_map)

// analyzeScope is the resolved answer to "which sessions am I looking at".
type analyzeScope struct {
	last     int           // most recent N sessions (default)
	window   time.Duration // used only when useSince
	useSince bool
}

// defaultAnalyzeLast is how many recent sessions a bare `deepai analyze`
// covers. Scoping by session count rather than by calendar window is the
// default because the usual question is "how are things NOW" — a time window
// keeps mixing in the sessions from before whatever you just fixed.
const defaultAnalyzeLast = 5

// addAnalyze registers `deepai analyze`.
func addAnalyze(topLevel *cobra.Command) {
	var since, format string
	var last int
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze recent sessions for delegation-efficiency problems",
		Long: `Analyze persisted sessions and report workload problems:
serial single-call delegation, budget exhaustion, long-running tasks,
failure patterns, and bash usage that reimplements code_map.

Findings are grouped per session, newest first, so a problem that stopped
happening after a fix is visible as such. By default the most recent 5
sessions are analyzed; --since switches to a calendar window instead.

Findings are read-only diagnostics with suggestions — nothing is modified.`,
		Example: "  deepai analyze\n  deepai analyze --last 3\n  deepai analyze --since 7d",
		RunE: func(cmd *cobra.Command, args []string) error {
			lastSet := cmd.Flags().Changed("last")
			sinceSet := cmd.Flags().Changed("since")
			if err := validateScopeFlags(last, since, lastSet, sinceSet); err != nil {
				return err
			}
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be 'text' or 'json', got %q", format)
			}
			scope := analyzeScope{last: last}
			if sinceSet {
				window, err := parseSince(since)
				if err != nil {
					return err
				}
				scope = analyzeScope{window: window, useSince: true}
			}
			return runAnalyze(cmd.Context(), scope, format)
		},
	}
	cmd.Flags().IntVar(&last, "last", defaultAnalyzeLast, "Analyze the most recently active N sessions")
	cmd.Flags().StringVar(&since, "since", "7d", "Analyze a calendar window instead: e.g. 48h, 7d, 30d (mutually exclusive with --last)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text (default) or json")
	topLevel.AddCommand(cmd)
}

// parseSince accepts "48h"/"7d"/"30d" style windows (Go's time.ParseDuration
// rejects the "d" unit) and a bare number meaning days.
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("--since is empty")
	}
	if !strings.HasSuffix(s, "d") {
		return time.ParseDuration(s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--since %q: bad day count", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// taskDelegation pairs one task tool call with its persisted result.
type taskDelegation struct {
	when         time.Time
	callID       string
	agentType    string
	description  string
	maxToolCalls int
	tokenBudget  int
	status       string // result status: completed/failed/...; "" = no result persisted
	errText      string
	stats        *subagent.RunStats
	usage        *subagent.TokenUsage
}

// bashUsage counts the main agent's bash commands matching a pattern class.
type mainAgentUsage struct {
	totalCalls    int
	byTool        map[string]int
	manualOutline []string // bash commands that hand-roll a symbol outline
}

// sessionRef identifies one analyzed session.
type sessionRef struct {
	id        string
	title     string
	updatedAt time.Time
}

// sessionRecords is one session's slice of the analysis inputs. Grouping by
// session is what lets the report answer "is this still happening?" rather
// than "did this ever happen in the window?" — a distinction that matters as
// soon as a problem has been fixed, since a pooled window keeps reporting the
// sessions from before the fix.
type sessionRecords struct {
	ref         sessionRef
	delegations []taskDelegation
	usage       mainAgentUsage
}

// recentSessions returns the n most recently ACTIVE sessions, newest first.
// Ordering is by updated_at, not created_at: a long-running session started
// days ago but picked back up today is recent in every sense that matters
// here. Asking for more sessions than exist is not an error.
func recentSessions(db *sql.DB, n int) ([]sessionRef, error) {
	rows, err := db.Query(
		`SELECT id, title, updated_at FROM sessions ORDER BY updated_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []sessionRef
	for rows.Next() {
		var id, title string
		var updated float64
		if err := rows.Scan(&id, &title, &updated); err != nil {
			return nil, err
		}
		refs = append(refs, sessionRef{id: id, title: title, updatedAt: time.Unix(int64(updated), 0)})
	}
	return refs, rows.Err()
}

// sessionsSince returns every session active at or after the given time,
// newest first. This backs --since, which stays available for the occasions
// where a calendar window is what you actually mean.
func sessionsSince(db *sql.DB, since time.Time) ([]sessionRef, error) {
	rows, err := db.Query(
		`SELECT id, title, updated_at FROM sessions WHERE updated_at >= ? ORDER BY updated_at DESC`,
		float64(since.Unix()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []sessionRef
	for rows.Next() {
		var id, title string
		var updated float64
		if err := rows.Scan(&id, &title, &updated); err != nil {
			return nil, err
		}
		refs = append(refs, sessionRef{id: id, title: title, updatedAt: time.Unix(int64(updated), 0)})
	}
	return refs, rows.Err()
}

// loadSessionRecords reads the given sessions' messages and returns one record
// per ref, in the order the refs were given. Sessions with no messages still
// get a record: "this session produced no findings" is the signal a fix took,
// so it must not be silently dropped.
func loadSessionRecords(db *sql.DB, refs []sessionRef) ([]sessionRecords, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(refs))
	args := make([]any, len(refs))
	for i, ref := range refs {
		placeholders[i] = "?"
		args[i] = ref.id
	}
	rows, err := db.Query(
		`SELECT session_id, created_at, role, tool_calls, tool_result FROM messages
		 WHERE session_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY created_at`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySession, err := groupMessages(rows)
	if err != nil {
		return nil, err
	}

	out := make([]sessionRecords, 0, len(refs))
	for _, ref := range refs {
		rec := sessionRecords{ref: ref, usage: mainAgentUsage{byTool: map[string]int{}}}
		if got, ok := bySession[ref.id]; ok {
			rec.delegations = got.delegations
			rec.usage = got.usage
		}
		out = append(out, rec)
	}
	return out, nil
}

// loadAnalysisRecords reads the messages table into the two shapes the
// analysis rules consume, flattened across every session in the window. Kept
// for the time-window path and its existing callers; session-scoped analysis
// goes through loadSessionRecords instead.
func loadAnalysisRecords(db *sql.DB, since time.Time) ([]taskDelegation, mainAgentUsage, error) {
	rows, err := db.Query(
		`SELECT session_id, created_at, role, tool_calls, tool_result FROM messages WHERE created_at >= ? ORDER BY created_at`,
		since.Unix())
	if err != nil {
		return nil, mainAgentUsage{}, err
	}
	defer rows.Close()

	bySession, err := groupMessages(rows)
	if err != nil {
		return nil, mainAgentUsage{}, err
	}

	var delegations []taskDelegation
	usage := mainAgentUsage{byTool: map[string]int{}}
	for _, rec := range bySession {
		delegations = append(delegations, rec.delegations...)
		usage.totalCalls += rec.usage.totalCalls
		usage.manualOutline = append(usage.manualOutline, rec.usage.manualOutline...)
		for tool, n := range rec.usage.byTool {
			usage.byTool[tool] += n
		}
	}
	sort.Slice(delegations, func(i, j int) bool { return delegations[i].when.Before(delegations[j].when) })
	return delegations, usage, nil
}

// groupMessages parses a message result set (session_id, created_at, role,
// tool_calls, tool_result) into per-session records. Tool-result Data arrives
// as decoded JSON maps, so subagent_stats/subagent_usage go through a second
// marshal/unmarshal to reach their concrete types.
func groupMessages(rows *sql.Rows) (map[string]*sessionRecords, error) {
	// task results land as separate role=tool messages; index them by call_id
	// in a FIRST pass so the second pass can join call -> result — a single
	// streaming pass would see every call before its (later-timestamped)
	// result and join nothing.
	type rawRow struct {
		sessionID      string
		when           time.Time
		role           string
		toolCallsJSON  string
		toolResultJSON string
	}
	var all []rawRow
	for rows.Next() {
		var ts float64
		var sessionID, role, toolCallsJSON, toolResultJSON string
		if err := rows.Scan(&sessionID, &ts, &role, &toolCallsJSON, &toolResultJSON); err != nil {
			return nil, err
		}
		all = append(all, rawRow{
			sessionID: sessionID, when: time.Unix(int64(ts), 0), role: role,
			toolCallsJSON: toolCallsJSON, toolResultJSON: toolResultJSON,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type rawResult struct {
		status string
		err    string
		data   map[string]any
	}
	results := map[string]rawResult{}
	for _, row := range all {
		if row.role != string(models.RoleTool) || row.toolResultJSON == "" {
			continue
		}
		var tr struct {
			CallID string         `json:"call_id"`
			Status string         `json:"status"`
			Error  string         `json:"error"`
			Data   map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(row.toolResultJSON), &tr) == nil && tr.CallID != "" {
			results[tr.CallID] = rawResult{status: tr.Status, err: tr.Error, data: tr.Data}
		}
	}

	bySession := map[string]*sessionRecords{}
	recordFor := func(id string) *sessionRecords {
		if rec, ok := bySession[id]; ok {
			return rec
		}
		rec := &sessionRecords{ref: sessionRef{id: id}, usage: mainAgentUsage{byTool: map[string]int{}}}
		bySession[id] = rec
		return rec
	}

	for _, row := range all {
		var calls []struct {
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal([]byte(row.toolCallsJSON), &calls) != nil {
			continue
		}
		for _, c := range calls {
			rec := recordFor(row.sessionID)
			rec.usage.totalCalls++
			rec.usage.byTool[c.Name]++
			if c.Name != "task" {
				if c.Name == "bash" || c.Name == "run_command" {
					if cmd, _ := c.Arguments["command"].(string); isManualOutline(cmd) {
						rec.usage.manualOutline = append(rec.usage.manualOutline, cmd)
					}
				}
				continue
			}
			d := taskDelegation{
				when:        row.when,
				callID:      c.ID,
				agentType:   argString(c.Arguments["agent_type"]),
				status:      "no-result",
				errText:     "",
				tokenBudget: argInt(c.Arguments["token_budget"]),
			}
			if d.agentType == "" {
				d.agentType = argString(c.Arguments["subagent_type"])
			}
			if raw, ok := c.Arguments["max_tool_calls"]; ok && raw != nil {
				d.maxToolCalls = argInt(raw)
			} else if raw, ok := c.Arguments["max_turns"]; ok && raw != nil {
				d.maxToolCalls = argInt(raw)
			}
			d.description = argString(c.Arguments["description"])
			if r, ok := results[c.ID]; ok {
				d.status = r.status
				d.errText = r.err
				d.stats = decodeRunStats(r.data)
				d.usage = decodeTokenUsage(r.data)
			}
			rec.delegations = append(rec.delegations, d)
		}
	}
	return bySession, nil
}

// decodeRunStats pulls Data["subagent_stats"] through a second JSON round-trip
// into the concrete type. Returns nil when absent (older sessions).
func decodeRunStats(data map[string]any) *subagent.RunStats {
	raw, ok := data["subagent_stats"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var s subagent.RunStats
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func decodeTokenUsage(data map[string]any) *subagent.TokenUsage {
	raw, ok := data["subagent_usage"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var u subagent.TokenUsage
	if json.Unmarshal(b, &u) != nil {
		return nil
	}
	return &u
}

// manualOutlinePattern matches a bash command that hand-rolls what code_map
// depth=symbols already provides: grep piped at symbol keywords (func/def/
// pub fn/class/struct/...), with or without head/sed pagination. That exact
// shape was observed live costing 3+ calls per file.
var manualOutlinePattern = regexp.MustCompile(`grep[^|;]*\b(func|fn|def|class|struct|interface|enum)\b`)

// argString/argInt coerce decoded-JSON argument values (string / float64) —
// the same job pkg/tools' unexported helpers do for live tool calls.
func argString(v any) string {
	s, _ := v.(string)
	return s
}

func argInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func isManualOutline(cmd string) bool {
	return cmd != "" && manualOutlinePattern.MatchString(cmd)
}

// Finding is one diagnosed problem: what was measured, the evidence for it,
// and what to change. Suggestions are advisory — applying them is a human
// decision (the command never writes).
type Finding struct {
	Severity   string `json:"severity"` // high / medium / low
	Title      string `json:"title"`
	Evidence   string `json:"evidence"`
	Suggestion string `json:"suggestion"`
}

// analyzeDelegations applies the delegation rules over the paired records.
// All thresholds are calibrated against observed runs (2026-08-15 waves:
// 5.7min-failing → 2h23m exhaustive) and are deliberately coarse — they gate
// a printed suggestion, not an enforcement action.
func analyzeDelegations(ds []taskDelegation) []Finding {
	var findings []Finding
	total := len(ds)
	if total == 0 {
		return nil
	}

	withStats, completed, failed := 0, 0, 0
	serial, exhausted, longRuns, schemaRetried := 0, 0, 0, 0
	var exhaustedExamples, longRunExamples []string
	failures := map[string]int{}
	for _, d := range ds {
		if d.stats != nil {
			withStats++
		}
		switch d.status {
		case "completed":
			completed++
		case "failed":
			failed++
			failures[classifyFailure(d.errText)]++
		}
		s := d.stats
		if s == nil {
			continue
		}
		// Serial single-caller: ratio ≈ 1 (calls ≈ turns). A batched caller
		// (Claude-style 3+ calls/turn) lands well above 1.5, and a small
		// task dominated by thinking turns lands below 0.8 — neither is
		// this problem.
		if r := float64(s.ToolCalls) / float64(s.LLMTurns); s.LLMTurns >= 10 && r >= 0.8 && r <= 1.5 {
			serial++
		}
		if s.BudgetExhausted {
			exhausted++
			if len(exhaustedExamples) < 3 {
				exhaustedExamples = append(exhaustedExamples,
					fmt.Sprintf("%s cap=%d calls=%d %s", d.label(), s.MaxToolCalls, s.ToolCalls, fmtDuration(s.DurationMS)))
			}
		}
		if s.DurationMS >= 30*60*1000 || (d.usage != nil && d.usage.CompletionTokens >= 150_000) {
			longRuns++
			if len(longRunExamples) < 3 {
				tokens := 0
				if d.usage != nil {
					tokens = d.usage.CompletionTokens
				}
				longRunExamples = append(longRunExamples,
					fmt.Sprintf("%s %s / %dk completion tokens", d.label(), fmtDuration(s.DurationMS), tokens/1000))
			}
		}
		if s.SchemaRetries > 0 {
			schemaRetried++
		}
	}

	pct := func(n, d int) int {
		if d == 0 {
			return 0
		}
		return n * 100 / d
	}

	if withStats >= 3 && pct(serial, withStats) >= 50 {
		findings = append(findings, Finding{
			Severity:   "high",
			Title:      "串行单调用：工具调用数 ≈ LLM 回合数",
			Evidence:   fmt.Sprintf("%d/%d 个 task 的 tool_calls/llm_turns ≥ 0.8 —— 模型每回合只发 1 次调用，N 次调用就是 N 个串行回合", serial, withStats),
			Suggestion: "委派 prompt 里把独立子任务合并要求（同一消息发多个调用），并把大任务拆成可并行的窄任务；主 agent 的委派 guidance 已含此约束，仍频发则考虑在 DEEPAI.md 强化",
		})
	}
	if exhausted > 0 {
		findings = append(findings, Finding{
			Severity:   "medium",
			Title:      "预算截断：task 跑满 max_tool_calls 被迫收尾",
			Evidence:   fmt.Sprintf("%d/%d 个 task budget_exhausted（如 %s）", exhausted, withStats, strings.Join(exhaustedExamples, "；")),
			Suggestion: "下一次委派收窄范围（具体文件/行号/符号域），不要调大 max_tool_calls——截断说明任务粒度超过单次委派的合理工作量",
		})
	}
	if longRuns > 0 {
		findings = append(findings, Finding{
			Severity:   "medium",
			Title:      "长尾运行：单 task 超 30 分钟或 15 万 completion tokens",
			Evidence:   strings.Join(longRunExamples, "；"),
			Suggestion: "指示 subagent 用 code_map 大纲 + read_file 行区间选择性阅读，避免整文件无跳读通读；超大文件按符号域拆成多个并行委派",
		})
	}
	if schemaRetried > 0 && pct(schemaRetried, withStats) >= 30 {
		findings = append(findings, Finding{
			Severity:   "low",
			Title:      "Schema 重试比例偏高",
			Evidence:   fmt.Sprintf("%d/%d 个 task 触发了输出格式重试", schemaRetried, withStats),
			Suggestion: "检查该 agent type 的 OutputSchema.Prompt 是否与 wrap-up 指令冲突；重试消耗的是同一预算，比例高说明格式约束需要更醒目",
		})
	}
	if failed > 0 && pct(failed, total) >= 30 {
		var parts []string
		for kind, n := range failures {
			parts = append(parts, fmt.Sprintf("%s×%d", kind, n))
		}
		sort.Strings(parts)
		findings = append(findings, Finding{
			Severity:   "high",
			Title:      "委派失败率高",
			Evidence:   fmt.Sprintf("%d/%d 个 task 失败：%s", failed, total, strings.Join(parts, "、")),
			Suggestion: "按失败类别处理：stream idle 超时多为长思考被 watchdog 杀（考虑换非推理模型或调大 idle 阈值）；context canceled 多为父级提前取消",
		})
	}
	return findings
}

// classifyFailure buckets error text into stable categories for counting.
func classifyFailure(errText string) string {
	switch {
	case errText == "":
		return "未知"
	case strings.Contains(errText, "stream idle"):
		return "流空闲超时"
	case strings.Contains(errText, "context canceled"), strings.Contains(errText, "cancelled"):
		return "被取消"
	case strings.Contains(errText, "timed out"), strings.Contains(errText, "DeadlineExceeded"):
		return "超时"
	case strings.Contains(errText, "token budget"):
		return "token 预算耗尽"
	case strings.Contains(errText, "tool call budget"):
		return "工具预算耗尽且收尾为空"
	default:
		if len(errText) > 40 {
			return errText[:40] + "…"
		}
		return errText
	}
}

// analyzeMainAgent applies the main-agent-side rules.
func analyzeMainAgent(usage mainAgentUsage) []Finding {
	var findings []Finding
	if len(usage.manualOutline) >= 3 {
		ex := usage.manualOutline[0]
		if len(ex) > 80 {
			ex = ex[:80] + "…"
		}
		findings = append(findings, Finding{
			Severity:   "low",
			Title:      "手工模拟 code_map：bash grep 构建符号大纲",
			Evidence:   fmt.Sprintf("%d 次 bash 调用匹配符号大纲模式（如 %q），同期 code_map 调用 %d 次", len(usage.manualOutline), ex, usage.byTool["code_map"]),
			Suggestion: "code_map 已提供行数与符号大纲（含 zig pub var）；仍出现时在 DEEPAI.md 写明探索优先用 code_map，减少逐文件 grep 分页",
		})
	}
	return findings
}

// label renders a delegation for evidence lines.
func (d taskDelegation) label() string {
	name := d.agentType
	if name == "" {
		name = "general-purpose"
	}
	if d.description != "" {
		desc := d.description
		if len([]rune(desc)) > 24 {
			desc = string([]rune(desc)[:24]) + "…"
		}
		return fmt.Sprintf("[%s] %s", name, desc)
	}
	return fmt.Sprintf("[%s]", name)
}

func fmtDuration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

// sessionFindings is one session's slice of the report.
type sessionFindings struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	Tasks     int       `json:"tasks"`
	Findings  []Finding `json:"findings"`
}

// analysisReport is the --json payload. Sessions carries the per-session
// breakdown; the top-level aggregate fields (including the flattened Findings)
// are kept so existing consumers keep working.
type analysisReport struct {
	Scope          string            `json:"scope"`
	Window         string            `json:"window"`
	TotalTasks     int               `json:"total_tasks"`
	Completed      int               `json:"completed"`
	Failed         int               `json:"failed"`
	WithStats      int               `json:"with_stats"`
	MainAgentCalls int               `json:"main_agent_calls"`
	Findings       []Finding         `json:"findings"`
	Sessions       []sessionFindings `json:"sessions"`
}

func scopeLabelLast(n int) string {
	return fmt.Sprintf("最近 %d 个 session", n)
}

func scopeLabelSince(window time.Duration) string {
	return fmt.Sprintf("近 %s", fmtDuration(int64(window/time.Millisecond)))
}

// validateScopeFlags rejects the combinations that would otherwise silently
// pick one scope and drop the other.
func validateScopeFlags(last int, since string, lastSet, sinceSet bool) error {
	if lastSet && sinceSet {
		return fmt.Errorf("--last and --since are mutually exclusive: pass one or the other")
	}
	if lastSet && last < 1 {
		return fmt.Errorf("--last must be at least 1, got %d", last)
	}
	if sinceSet {
		if _, err := parseSince(since); err != nil {
			return err
		}
	}
	return nil
}

// buildReport runs the analysis rules per session and assembles both views.
// Running the rules per session rather than over a pooled window is the point
// of the scoping work: patterns like serial single-call delegation only mean
// something within one conversation, and a fixed problem should stop appearing
// in the sessions that came after the fix.
func buildReport(records []sessionRecords, scope string) analysisReport {
	report := analysisReport{Scope: scope, Window: scope}
	for _, rec := range records {
		findings := append(analyzeDelegations(rec.delegations), analyzeMainAgent(rec.usage)...)
		report.Sessions = append(report.Sessions, sessionFindings{
			ID:        rec.ref.id,
			Title:     rec.ref.title,
			UpdatedAt: rec.ref.updatedAt,
			Tasks:     len(rec.delegations),
			Findings:  findings,
		})
		report.Findings = append(report.Findings, findings...)
		report.TotalTasks += len(rec.delegations)
		report.MainAgentCalls += rec.usage.totalCalls
		for _, d := range rec.delegations {
			switch d.status {
			case "completed":
				report.Completed++
			case "failed":
				report.Failed++
			}
			if d.stats != nil {
				report.WithStats++
			}
		}
	}
	return report
}

// printReport renders the text view: newest session first, each with its own
// findings, and clean sessions explicitly marked rather than omitted.
func printReport(w io.Writer, report analysisReport) {
	fmt.Fprintf(w, "deepai analyze — %s（task %d 个：完成 %d / 失败 %d / 带统计 %d；主 agent 调用 %d 次）\n",
		report.Scope, report.TotalTasks, report.Completed, report.Failed, report.WithStats, report.MainAgentCalls)

	if len(report.Sessions) == 0 {
		fmt.Fprintf(w, "\n没有匹配的 session。\n")
		return
	}
	if report.WithStats == 0 && report.TotalTasks > 0 {
		fmt.Fprintf(w, "\n注意：这些 task 早于 subagent_stats 落库，委派规则无数据可用；升级后新会话才有统计。\n")
	}

	fmt.Fprintln(w)
	for _, s := range report.Sessions {
		title := s.Title
		if strings.TrimSpace(title) == "" {
			title = "(无标题)"
		}
		fmt.Fprintf(w, "%s  %s  task %d", s.ID, title, s.Tasks)
		if len(s.Findings) == 0 {
			fmt.Fprintf(w, "  ✓ 无问题\n")
			continue
		}
		fmt.Fprintln(w)
		for _, f := range s.Findings {
			fmt.Fprintf(w, "  [%s] %s\n", f.Severity, f.Title)
			fmt.Fprintf(w, "    依据: %s\n", f.Evidence)
			fmt.Fprintf(w, "    建议: %s\n", f.Suggestion)
		}
	}

	clean := 0
	for _, s := range report.Sessions {
		if len(s.Findings) == 0 {
			clean++
		}
	}
	fmt.Fprintf(w, "\n共 %d 个问题，分布在 %d/%d 个 session（%d 个无问题）。\n",
		len(report.Findings), len(report.Sessions)-clean, len(report.Sessions), clean)
}

func runAnalyze(ctx context.Context, scope analyzeScope, format string) error {
	dbPath := DBFile()
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	var refs []sessionRef
	var label string
	if scope.useSince {
		refs, err = sessionsSince(db, time.Now().Add(-scope.window))
		label = scopeLabelSince(scope.window)
	} else {
		refs, err = recentSessions(db, scope.last)
		label = scopeLabelLast(scope.last)
	}
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}

	records, err := loadSessionRecords(db, refs)
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}
	report := buildReport(records, label)

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printReport(os.Stdout, report)
	return nil
}
