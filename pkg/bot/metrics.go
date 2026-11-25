package bot

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Metrics tracks Prometheus-compatible counters and gauges for deepai-bot
// without introducing external dependencies (docs/BOT_NEXT_DESIGN.md §6.3).
type Metrics struct {
	mu sync.Mutex

	turnsTotal        map[[2]string]int64   // {bot_id, status}
	turnDurSum        map[[2]string]float64 // {bot_id, status}
	turnDurCount      map[[2]string]int64   // {bot_id, status}
	tokensTotal       map[[3]string]int64   // {bot_id, model, kind}
	toolCallsTotal    map[[3]string]int64   // {bot_id, tool, status}
	scheduleRunsTotal map[[3]string]int64   // {bot_id, trigger, status}
}

func NewMetrics() *Metrics {
	return &Metrics{
		turnsTotal:        map[[2]string]int64{},
		turnDurSum:        map[[2]string]float64{},
		turnDurCount:      map[[2]string]int64{},
		tokensTotal:       map[[3]string]int64{},
		toolCallsTotal:    map[[3]string]int64{},
		scheduleRunsTotal: map[[3]string]int64{},
	}
}

func (m *Metrics) ObserveTurnEnd(botID string, sum *TurnSummary) {
	if m == nil || sum == nil {
		return
	}
	botID = effectiveBotID(botID)
	status := string(sum.Status)
	dur := sum.EndedAt.Sub(sum.StartedAt).Seconds()
	if dur < 0 {
		dur = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{botID, status}
	m.turnsTotal[k]++
	m.turnDurSum[k] += dur
	m.turnDurCount[k]++
	if sum.Usage != nil {
		model := sum.Model
		if sum.Usage.InputTokens > 0 {
			m.tokensTotal[[3]string{botID, model, "input"}] += int64(sum.Usage.InputTokens)
		}
		if sum.Usage.OutputTokens > 0 {
			m.tokensTotal[[3]string{botID, model, "output"}] += int64(sum.Usage.OutputTokens)
		}
	}
}

func (m *Metrics) ObserveToolCall(botID, tool, status string) {
	if m == nil || tool == "" {
		return
	}
	m.mu.Lock()
	m.toolCallsTotal[[3]string{effectiveBotID(botID), tool, status}]++
	m.mu.Unlock()
}

func (m *Metrics) ObserveScheduleRun(botID, trigger, status string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.scheduleRunsTotal[[3]string{effectiveBotID(botID), trigger, status}]++
	m.mu.Unlock()
}

// ServeHTTP writes standard Prometheus text exposition format.
func (m *Metrics) WritePrometheus(w http.ResponseWriter, mgr *Manager) {
	activeByBot := map[string]int{}
	queueByBot := map[string]int{}
	if mgr != nil {
		mgr.mu.Lock()
		for _, s := range mgr.sessions {
			s.mu.Lock()
			bid := effectiveBotID(s.botID)
			if s.turn != nil {
				activeByBot[bid]++
			}
			if n := len(s.queue); n > 0 {
				queueByBot[bid] += n
			}
			s.mu.Unlock()
		}
		mgr.mu.Unlock()
	}

	var b strings.Builder
	b.WriteString("# HELP deepai_bot_active_turns Currently running turns per bot.\n")
	b.WriteString("# TYPE deepai_bot_active_turns gauge\n")
	for _, bid := range sortedKeys(activeByBot) {
		fmt.Fprintf(&b, "deepai_bot_active_turns{bot_id=%q} %d\n", bid, activeByBot[bid])
	}

	b.WriteString("# HELP deepai_bot_queue_depth Queued follow-up messages per bot.\n")
	b.WriteString("# TYPE deepai_bot_queue_depth gauge\n")
	for _, bid := range sortedKeys(queueByBot) {
		fmt.Fprintf(&b, "deepai_bot_queue_depth{bot_id=%q} %d\n", bid, queueByBot[bid])
	}

	m.mu.Lock()
	b.WriteString("# HELP deepai_bot_turns_total Completed turns by bot and status.\n")
	b.WriteString("# TYPE deepai_bot_turns_total counter\n")
	for _, k := range sortedKeys2(m.turnsTotal) {
		fmt.Fprintf(&b, "deepai_bot_turns_total{bot_id=%q,status=%q} %d\n", k[0], k[1], m.turnsTotal[k])
	}

	b.WriteString("# HELP deepai_bot_turn_duration_seconds Turn duration summary by bot and status.\n")
	b.WriteString("# TYPE deepai_bot_turn_duration_seconds summary\n")
	for _, k := range sortedKeys2(m.turnDurCount) {
		fmt.Fprintf(&b, "deepai_bot_turn_duration_seconds_sum{bot_id=%q,status=%q} %.6f\n", k[0], k[1], m.turnDurSum[k])
		fmt.Fprintf(&b, "deepai_bot_turn_duration_seconds_count{bot_id=%q,status=%q} %d\n", k[0], k[1], m.turnDurCount[k])
	}

	b.WriteString("# HELP deepai_bot_tokens_total Token usage by bot, model and kind.\n")
	b.WriteString("# TYPE deepai_bot_tokens_total counter\n")
	for _, k := range sortedKeys3(m.tokensTotal) {
		fmt.Fprintf(&b, "deepai_bot_tokens_total{bot_id=%q,model=%q,kind=%q} %d\n", k[0], k[1], k[2], m.tokensTotal[k])
	}

	b.WriteString("# HELP deepai_bot_tool_calls_total Tool calls by bot, tool and status.\n")
	b.WriteString("# TYPE deepai_bot_tool_calls_total counter\n")
	for _, k := range sortedKeys3(m.toolCallsTotal) {
		fmt.Fprintf(&b, "deepai_bot_tool_calls_total{bot_id=%q,tool=%q,status=%q} %d\n", k[0], k[1], k[2], m.toolCallsTotal[k])
	}

	b.WriteString("# HELP deepai_bot_schedule_runs_total Finished schedule runs by bot, trigger and status.\n")
	b.WriteString("# TYPE deepai_bot_schedule_runs_total counter\n")
	for _, k := range sortedKeys3(m.scheduleRunsTotal) {
		fmt.Fprintf(&b, "deepai_bot_schedule_runs_total{bot_id=%q,trigger=%q,status=%q} %d\n", k[0], k[1], k[2], m.scheduleRunsTotal[k])
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sortedKeys2[V any](m map[[2]string]V) [][2]string {
	keys := make([][2]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	return keys
}

func sortedKeys3[V any](m map[[3]string]V) [][3]string {
	keys := make([][3]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [3]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		if c := strings.Compare(a[1], b[1]); c != 0 {
			return c
		}
		return strings.Compare(a[2], b[2])
	})
	return keys
}
