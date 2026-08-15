package commands

import (
	"database/sql"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/subagent"
	_ "modernc.org/sqlite"
)

func TestParseSince(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"48h", 48 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"", 0, true},
		{"xd", 0, true},
		{"-3d", 0, true},
	}
	for _, c := range cases {
		got, err := parseSince(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSince(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSince(%q) error = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSince(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsManualOutline(t *testing.T) {
	manual := []string{
		`grep -n "^pub fn \|^fn " reactive/element.zig | head -100`,
		`grep -n "def \\|class " app.py`,
		`cat main.go | grep -n "func "`,
	}
	for _, cmd := range manual {
		if !isManualOutline(cmd) {
			t.Errorf("isManualOutline(%q) = false, want true", cmd)
		}
	}
	notManual := []string{
		`go test ./...`,
		`grep -rn "TODO" .`,                    // keyword grep, not an outline
		`git log --oneline -15 | head -20`,     // history, no symbol keywords
		`find src -name '*.zig' | xargs wc -l`, // sizing, no grep symbols
	}
	for _, cmd := range notManual {
		if isManualOutline(cmd) {
			t.Errorf("isManualOutline(%q) = true, want false", cmd)
		}
	}
}

func TestAnalyzeDelegations_NoRecords(t *testing.T) {
	if f := analyzeDelegations(nil); f != nil {
		t.Fatalf("analyzeDelegations(nil) = %+v, want nil", f)
	}
}

// TestAnalyzeDelegations_DetectsSerialSingleCall: stats with
// tool_calls ≈ llm_turns across most tasks must surface the serial-calling
// finding — the GLM one-call-per-turn shape that turns N calls into N
// sequential round-trips.
func TestAnalyzeDelegations_DetectsSerialSingleCall(t *testing.T) {
	ds := make([]taskDelegation, 4)
	for i := range ds {
		ds[i] = taskDelegation{
			status: "completed",
			stats:  &subagent.RunStats{ToolCalls: 45, LLMTurns: 47},
		}
	}
	findings := analyzeDelegations(ds)
	found := false
	for _, f := range findings {
		if f.Severity == "high" && f.Title == "串行单调用：工具调用数 ≈ LLM 回合数" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want the serial-single-call finding", findings)
	}
}

// TestAnalyzeDelegations_BatchedCallerNotFlagged: a model that batches 3+
// calls per turn must NOT trip the serial finding.
func TestAnalyzeDelegations_BatchedCallerNotFlagged(t *testing.T) {
	ds := make([]taskDelegation, 4)
	for i := range ds {
		ds[i] = taskDelegation{
			status: "completed",
			stats:  &subagent.RunStats{ToolCalls: 45, LLMTurns: 12},
		}
	}
	for _, f := range analyzeDelegations(ds) {
		if f.Title == "串行单调用：工具调用数 ≈ LLM 回合数" {
			t.Fatalf("batched caller flagged as serial: %+v", f)
		}
	}
}

// TestAnalyzeDelegations_DetectsExhaustionAndLongRuns: budget_exhausted and
// the 30min/150k-token long-tail rule each surface their finding with the
// delegation's evidence.
func TestAnalyzeDelegations_DetectsExhaustionAndLongRuns(t *testing.T) {
	ds := []taskDelegation{
		{
			agentType:   "security-reviewer",
			description: "审查 element.zig 前半",
			status:      "completed",
			stats:       &subagent.RunStats{ToolCalls: 45, LLMTurns: 47, MaxToolCalls: 45, BudgetExhausted: true, DurationMS: 39 * 60 * 1000},
			usage:       &subagent.TokenUsage{CompletionTokens: 334_316},
		},
	}
	findings := analyzeDelegations(ds)
	var exhausted, longRun bool
	for _, f := range findings {
		switch f.Title {
		case "预算截断：task 跑满 max_tool_calls 被迫收尾":
			exhausted = true
		case "长尾运行：单 task 超 30 分钟或 15 万 completion tokens":
			longRun = true
		}
	}
	if !exhausted {
		t.Errorf("findings missing budget-exhaustion: %+v", findings)
	}
	if !longRun {
		t.Errorf("findings missing long-run: %+v", findings)
	}
}

// TestAnalyzeDelegations_FailureRateBuckets: ≥30% failures surface the
// failure finding with the classified bucket in its evidence.
func TestAnalyzeDelegations_FailureRateBuckets(t *testing.T) {
	ds := []taskDelegation{
		{status: "completed"},
		{status: "failed", errText: "subagent task timed out: stream idle timeout: no data received after 2m0s"},
		{status: "failed", errText: "subagent task timed out: stream idle timeout: no data received after 2m0s"},
	}
	findings := analyzeDelegations(ds)
	found := false
	for _, f := range findings {
		if f.Title == "委派失败率高" && f.Severity == "high" {
			found = true
			if f.Evidence == "" {
				t.Errorf("failure finding has empty evidence: %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want the failure-rate finding", findings)
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct{ in, want string }{
		{"subagent task timed out: stream idle timeout: no data received after 2m0s", "流空闲超时"},
		{"context canceled", "被取消"},
		{"agent exceeded token budget (10/5)", "token 预算耗尽"},
		{"some novel failure", "some novel failure"},
		{"", "未知"},
	}
	for _, c := range cases {
		if got := classifyFailure(c.in); got != c.want {
			t.Errorf("classifyFailure(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnalyzeMainAgent_ManualOutline(t *testing.T) {
	usage := mainAgentUsage{
		byTool:        map[string]int{"bash": 10, "code_map": 1},
		manualOutline: []string{`grep -n "^pub fn " element.zig | head -100`, `grep -n "func " main.go`, `cat x.go | grep -n "type "`},
	}
	findings := analyzeMainAgent(usage)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the manual-outline finding", findings)
	}
	if findings[0].Severity != "low" {
		t.Errorf("severity = %q, want low", findings[0].Severity)
	}

	// Below the threshold: not worth a finding.
	if f := analyzeMainAgent(mainAgentUsage{manualOutline: []string{"grep -n func x.go"}}); len(f) != 0 {
		t.Fatalf("single occurrence flagged: %+v", f)
	}
}

// TestLoadAnalysisRecords_JoinsCallToResult pins the two-pass join: a task
// call arrives in the row stream BEFORE its result message (later timestamp),
// so a naive single pass pairs nothing. Writes both rows into an in-memory
// messages table and asserts the pairing plus the stats/usage decode.
func TestLoadAnalysisRecords_JoinsCallToResult(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, seq INTEGER NOT NULL,
		role TEXT NOT NULL, content TEXT DEFAULT '', tool_calls TEXT DEFAULT '[]',
		tool_result TEXT DEFAULT '', created_at REAL NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	callJSON := `[{"id":"c1","name":"task","arguments":{"agent_type":"security-reviewer","description":"审查 core","max_tool_calls":45}}]`
	resultJSON := `{"call_id":"c1","tool_name":"task","status":"failed","error":"agent exceeded tool call budget (45)","data":{"subagent_stats":{"agent_type":"security-reviewer","tool_calls":45,"llm_turns":47,"schema_retries":0,"max_tool_calls":45,"budget_exhausted":true,"duration_ms":2340000},"subagent_usage":{"prompt_tokens":0,"completion_tokens":516000,"total_tokens":0}}}`
	bashJSON := `[{"id":"c2","name":"bash","arguments":{"command":"grep -n \"^pub fn \" element.zig | head -100"}}]`
	for _, ins := range []struct {
		id, role, calls, result string
		ts                      float64
	}{
		{"m1", "human", "[]", "", float64(now - 60)},
		{"m2", "ai", callJSON, "", float64(now - 50)},
		{"m3", "tool", "[]", resultJSON, float64(now - 10)},
		{"m4", "ai", bashJSON, "", float64(now - 5)},
	} {
		if _, err := db.Exec(`INSERT INTO messages (id, session_id, seq, role, content, tool_calls, tool_result, created_at) VALUES (?,?,1,?,'',?,?,?)`,
			ins.id, "s1", ins.role, ins.calls, ins.result, ins.ts); err != nil {
			t.Fatal(err)
		}
	}

	delegations, usage, err := loadAnalysisRecords(db, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("loadAnalysisRecords() error = %v", err)
	}
	if len(delegations) != 1 {
		t.Fatalf("delegations = %+v, want the single task call joined to its result", delegations)
	}
	d := delegations[0]
	if d.callID != "c1" || d.status != "failed" || d.agentType != "security-reviewer" || d.maxToolCalls != 45 {
		t.Fatalf("delegation = %+v, want call c1 joined with status/agentType/maxToolCalls", d)
	}
	if d.stats == nil || d.stats.ToolCalls != 45 || d.stats.LLMTurns != 47 || !d.stats.BudgetExhausted || d.stats.DurationMS != 2340000 {
		t.Fatalf("stats = %+v, want decoded RunStats", d.stats)
	}
	if d.usage == nil || d.usage.CompletionTokens != 516000 {
		t.Fatalf("usage = %+v, want decoded TokenUsage", d.usage)
	}
	if usage.totalCalls != 2 || len(usage.manualOutline) != 1 {
		t.Fatalf("usage = %+v, want 2 total calls and 1 manual outline", usage)
	}
}
