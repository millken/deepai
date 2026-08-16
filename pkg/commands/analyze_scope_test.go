package commands

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Analysis used to pool every message in a time window with no session
// attribution, so problems fixed several sessions ago kept showing up in the
// aggregate. Scoping to the most recent N sessions — and reporting per session
// — is what makes "did my fix take?" answerable.

func newAnalyzeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, seq INTEGER NOT NULL,
		role TEXT NOT NULL, content TEXT DEFAULT '', tool_calls TEXT DEFAULT '[]',
		tool_result TEXT DEFAULT '', created_at REAL NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, user_id TEXT DEFAULT '', title TEXT DEFAULT '',
		model TEXT DEFAULT '', cwd TEXT DEFAULT '', source TEXT DEFAULT '',
		state TEXT DEFAULT '', created_at REAL NOT NULL, updated_at REAL NOT NULL,
		metadata TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertSession(t *testing.T, db *sql.DB, id, title string, created, updated time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?,?,?,?)`,
		id, title, float64(created.Unix()), float64(updated.Unix())); err != nil {
		t.Fatal(err)
	}
}

func addMessage(t *testing.T, db *sql.DB, msgID, sessionID, role, calls, result string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages (id, session_id, seq, role, content, tool_calls, tool_result, created_at) VALUES (?,?,1,?,'',?,?,?)`,
		msgID, sessionID, role, calls, result, float64(at.Unix())); err != nil {
		t.Fatal(err)
	}
}

// serialTaskCalls emits n single-task turns WITH their results. The results
// must carry subagent_stats: the serial-delegation rule only fires when at
// least 3 delegations have stats, so calls alone produce no finding.
func serialTaskCalls(t *testing.T, db *sql.DB, sessionID string, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		callID := fmt.Sprintf("%s-c%d", sessionID, i)
		at := base.Add(time.Duration(i) * time.Minute)
		calls := fmt.Sprintf(
			`[{"id":%q,"name":"task","arguments":{"agent_type":"code-reviewer","description":"审查","max_tool_calls":60}}]`,
			callID)
		addMessage(t, db, fmt.Sprintf("%s-m%d", sessionID, i), sessionID, "ai", calls, "", at)

		// tool_calls/llm_turns = 45/47 ≈ 0.96, above the 0.8 serial threshold,
		// while staying under max_tool_calls so budget exhaustion stays quiet.
		result := fmt.Sprintf(
			`{"call_id":%q,"tool_name":"task","status":"completed","data":{"subagent_stats":{"agent_type":"code-reviewer","tool_calls":45,"llm_turns":47,"schema_retries":0,"max_tool_calls":60,"budget_exhausted":false,"duration_ms":60000}}}`,
			callID)
		addMessage(t, db, fmt.Sprintf("%s-r%d", sessionID, i), sessionID, "tool", "[]", result, at.Add(time.Second))
	}
}

func TestRecentSessions_OrderedByLastActivity(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	// "old" was created first but is the most recently active: a long-running
	// session picked back up today must count as recent.
	insertSession(t, db, "old", "长会话", now.Add(-72*time.Hour), now.Add(-1*time.Minute))
	insertSession(t, db, "mid", "中间", now.Add(-48*time.Hour), now.Add(-2*time.Hour))
	insertSession(t, db, "new", "刚建的", now.Add(-1*time.Hour), now.Add(-3*time.Hour))

	refs, err := recentSessions(db, 2)
	if err != nil {
		t.Fatalf("recentSessions() error = %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d sessions, want 2", len(refs))
	}
	if refs[0].id != "old" || refs[1].id != "mid" {
		t.Fatalf("order = %q,%q; want old,mid (by updated_at desc)", refs[0].id, refs[1].id)
	}
	if refs[0].title != "长会话" {
		t.Fatalf("title = %q, want 长会话", refs[0].title)
	}
}

func TestRecentSessions_FewerThanRequested(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	insertSession(t, db, "only", "唯一", now, now)

	refs, err := recentSessions(db, 10)
	if err != nil {
		t.Fatalf("recentSessions() error = %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d, want 1 — N larger than the table must not error", len(refs))
	}
}

func TestLoadSessionRecords_GroupsBySession(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	insertSession(t, db, "s1", "会话一", now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	insertSession(t, db, "s2", "会话二", now.Add(-time.Hour), now.Add(-time.Hour))

	serialTaskCalls(t, db, "s1", 3, now.Add(-2*time.Hour))
	addMessage(t, db, "s2-m1", "s2", "ai",
		`[{"id":"s2-c1","name":"bash","arguments":{"command":"ls"}}]`, "", now.Add(-time.Hour))

	refs, err := recentSessions(db, 5)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadSessionRecords(db, refs)
	if err != nil {
		t.Fatalf("loadSessionRecords() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d session records, want 2", len(records))
	}

	byID := map[string]sessionRecords{}
	for _, r := range records {
		byID[r.ref.id] = r
	}
	if got := len(byID["s1"].delegations); got != 3 {
		t.Fatalf("s1 delegations = %d, want 3", got)
	}
	if got := len(byID["s2"].delegations); got != 0 {
		t.Fatalf("s2 delegations = %d, want 0 — bash-only session", got)
	}
	if got := byID["s2"].usage.totalCalls; got != 1 {
		t.Fatalf("s2 main-agent calls = %d, want 1 (usage must not leak across sessions)", got)
	}
	if got := byID["s1"].usage.totalCalls; got != 3 {
		t.Fatalf("s1 main-agent calls = %d, want 3", got)
	}
}

// buildScopedReport is the seam the CLI and these tests share.
func reportFor(t *testing.T, db *sql.DB, last int) analysisReport {
	t.Helper()
	refs, err := recentSessions(db, last)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadSessionRecords(db, refs)
	if err != nil {
		t.Fatal(err)
	}
	return buildReport(records, scopeLabelLast(last))
}

func TestAnalyze_LastNExcludesOlderSessions(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	// The problem lives in the oldest session only — the exact "already fixed"
	// shape that a pooled time window kept resurfacing.
	insertSession(t, db, "broken", "批量迁移", now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	insertSession(t, db, "fixed1", "重构", now.Add(-48*time.Hour), now.Add(-48*time.Hour))
	insertSession(t, db, "fixed2", "审查", now.Add(-24*time.Hour), now.Add(-24*time.Hour))

	serialTaskCalls(t, db, "broken", 3, now.Add(-72*time.Hour))
	addMessage(t, db, "fixed1-m1", "fixed1", "ai", `[{"id":"f1","name":"grep","arguments":{}}]`, "", now.Add(-48*time.Hour))
	addMessage(t, db, "fixed2-m1", "fixed2", "ai", `[{"id":"f2","name":"grep","arguments":{}}]`, "", now.Add(-24*time.Hour))

	recent := reportFor(t, db, 2)
	if len(recent.Findings) != 0 {
		t.Fatalf("--last 2 should not surface the old session's findings, got %+v", recent.Findings)
	}
	if recent.TotalTasks != 0 {
		t.Fatalf("--last 2 TotalTasks = %d, want 0", recent.TotalTasks)
	}

	all := reportFor(t, db, 3)
	if len(all.Findings) == 0 {
		t.Fatal("--last 3 must include the old session's findings")
	}
	if all.TotalTasks != 3 {
		t.Fatalf("--last 3 TotalTasks = %d, want 3", all.TotalTasks)
	}
}

func TestAnalyze_SessionWithoutFindingsStillListed(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	insertSession(t, db, "quiet", "干净的会话", now, now)
	addMessage(t, db, "q-m1", "quiet", "ai", `[{"id":"q1","name":"grep","arguments":{}}]`, "", now)

	report := reportFor(t, db, 5)
	if len(report.Sessions) != 1 {
		t.Fatalf("got %d sessions in report, want 1 — a clean session is the signal, not noise", len(report.Sessions))
	}
	if len(report.Sessions[0].Findings) != 0 {
		t.Fatalf("clean session should have no findings, got %+v", report.Sessions[0].Findings)
	}
	if report.Sessions[0].Title != "干净的会话" {
		t.Fatalf("title = %q", report.Sessions[0].Title)
	}
}

func TestAnalyze_ReportKeepsTopLevelAggregatesForJSONConsumers(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	insertSession(t, db, "a", "会话 A", now.Add(-time.Hour), now.Add(-time.Hour))
	insertSession(t, db, "b", "会话 B", now, now)
	serialTaskCalls(t, db, "a", 3, now.Add(-time.Hour))
	addMessage(t, db, "b-m1", "b", "ai", `[{"id":"b1","name":"grep","arguments":{}}]`, "", now)

	report := reportFor(t, db, 5)

	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"total_tasks", "completed", "failed", "with_stats", "main_agent_calls", "findings", "sessions"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("json payload lost key %q; existing consumers would break", key)
		}
	}
	// The flattened list must still hold every session's findings.
	var perSession int
	for _, s := range report.Sessions {
		perSession += len(s.Findings)
	}
	if perSession != len(report.Findings) {
		t.Fatalf("flattened findings = %d, per-session total = %d; they must agree", len(report.Findings), perSession)
	}
}

func TestAnalyze_TextOutputGroupsBySession(t *testing.T) {
	db := newAnalyzeTestDB(t)
	now := time.Now()
	insertSession(t, db, "broken", "批量迁移", now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	insertSession(t, db, "clean", "审查仓库", now, now)
	serialTaskCalls(t, db, "broken", 3, now.Add(-2*time.Hour))
	addMessage(t, db, "c-m1", "clean", "ai", `[{"id":"c1","name":"grep","arguments":{}}]`, "", now)

	var buf bytes.Buffer
	printReport(&buf, reportFor(t, db, 5))
	out := buf.String()

	for _, want := range []string{"批量迁移", "审查仓库"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing session title %q:\n%s", want, out)
		}
	}
	// The clean session must be visibly marked, not merely absent of findings.
	if !strings.Contains(out, "无问题") {
		t.Fatalf("clean session not marked as problem-free:\n%s", out)
	}
}

func TestAnalyzeScope_LastAndSinceAreMutuallyExclusive(t *testing.T) {
	if err := validateScopeFlags(3, "7d", true, true); err == nil {
		t.Fatal("passing both --last and --since must error rather than silently ignoring one")
	}
	if err := validateScopeFlags(3, "", true, false); err != nil {
		t.Fatalf("--last alone must be valid: %v", err)
	}
	if err := validateScopeFlags(0, "7d", false, true); err != nil {
		t.Fatalf("--since alone must be valid: %v", err)
	}
	if err := validateScopeFlags(0, "", true, false); err == nil {
		t.Fatal("--last 0 must error")
	}
	if err := validateScopeFlags(-1, "", true, false); err == nil {
		t.Fatal("--last -1 must error")
	}
}
