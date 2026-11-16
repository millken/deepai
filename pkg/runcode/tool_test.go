package runcode

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

// echoHost is a scripted Host for engine-level tests: visibility, parallel
// safety and call behavior are declared per tool name. Calls execute on
// parallel worker goroutines (executeBatch), so mutable state is guarded.
type echoHost struct {
	mu         sync.Mutex
	visible    map[string]bool
	parallel   map[string]bool
	calls      []string // executed call names, in batch order
	fn         func(name string, args map[string]any) HostResult
	batchTimes map[string][2]time.Time // [start,end] per name, for overlap assertions
	delay      time.Duration
}

func newEchoHost() *echoHost {
	return &echoHost{
		visible:    map[string]bool{},
		parallel:   map[string]bool{},
		batchTimes: map[string][2]time.Time{},
	}
}

func (h *echoHost) declare(name string, visible, parallel bool) {
	h.visible[name] = visible
	h.parallel[name] = parallel
}

func (h *echoHost) ToolInfo(name string) (bool, bool) {
	return h.visible[name], h.parallel[name]
}

func (h *echoHost) Call(ctx context.Context, name string, args map[string]any) HostResult {
	t0 := time.Now()
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	h.mu.Lock()
	h.calls = append(h.calls, name)
	h.batchTimes[name] = [2]time.Time{t0, time.Now()}
	h.mu.Unlock()
	if h.fn != nil {
		return h.fn(name, args)
	}
	return HostResult{Status: "completed", Text: "echo:" + name, Data: map[string]any{"tool": name}}
}

func runScript(t *testing.T, host Host, lim Limits, script string) RunOutput {
	t.Helper()
	if lim.Timeout == 0 {
		lim.Timeout = 10 * time.Second
	}
	if lim.MaxToolCalls == 0 {
		lim.MaxToolCalls = 50
	}
	return NewEngine().Run(context.Background(), script, host, lim)
}

// Smoke: end-to-end engine round trip — a script awaits one tool and returns.
func TestEngineSmoke(t *testing.T) {
	h := newEchoHost()
	h.declare("grep", true, true)
	out := runScript(t, h, Limits{}, `
		const r = await tools.grep({pattern: "x"});
		return r.text;
	`)
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	if out.Value != "echo:grep" {
		t.Fatalf("value=%v", out.Value)
	}
	if len(out.Calls) != 1 || out.Calls[0].Tool != "grep" {
		t.Fatalf("calls=%+v", out.Calls)
	}
}

// Criterion 6: invisible tool rejects with the exact message and never runs.
func TestEngineRejectsInvisibleTool(t *testing.T) {
	h := newEchoHost()
	h.declare("grep", true, true)
	out := runScript(t, h, Limits{}, `
		try {
			await tools.edit_file({path: "x"});
			return "resolved?!";
		} catch (e) {
			return e.message;
		}
	`)
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	if msg, _ := out.Value.(string); msg != "tool not available in run_code: edit_file" {
		t.Fatalf("reject message = %q", msg)
	}
	if len(h.calls) != 0 {
		t.Fatalf("invisible tool must not execute, got %v", h.calls)
	}
}

// Criterion 7: the exclusion list matches the charter set exactly, and every
// entry rejects through a registry that even contains the tool (visibility is
// cut by exclusion, not by absence).
func TestExcludedToolsReject(t *testing.T) {
	want := map[string]bool{
		"run_code": true, "task": true, "invoke_acp_agent": true,
		"todo_write": true, "skill": true, "enter_plan_mode": true,
		"exit_plan_mode": true, "write_plan": true,
		"ask_clarification": true, "present_file": true,
	}
	if len(ExcludedTools) != len(want) {
		t.Fatalf("ExcludedTools = %v", ExcludedTools)
	}
	for _, n := range ExcludedTools {
		if !want[n] {
			t.Fatalf("unexpected entry %q in ExcludedTools", n)
		}
	}

	reg := tools.NewRegistry()
	for name := range want {
		_ = reg.Register(models.Tool{Name: name, ParallelSafe: true,
			Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
				return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted}, nil
			}})
	}
	host := NewRegistryHost(&tools.Dispatch{Registry: reg, MaxConcurrency: 4}, "rc_ex")
	for name := range want {
		res := host.Call(context.Background(), name, nil)
		if res.Err == "" {
			t.Errorf("%s resolved, want reject", name)
		} else if !strings.Contains(res.Err, "tool not available in run_code: "+name) {
			t.Errorf("%s reject message = %q", name, res.Err)
		}
	}
}

// Criterion 20: three parallel-safe calls in one Promise.all batch overlap.
func TestEngineParallelBatchOverlaps(t *testing.T) {
	// Warm the shared wazero compilation cache first: the first in-process
	// qjs.New pays a one-time ~300ms compile that has nothing to do with
	// batch concurrency — without this warmup the elapsed assertion below
	// depends on test ordering (a full-suite run passes only because earlier
	// tests already warmed the cache).
	warm := newEchoHost()
	warm.declare("a", true, true)
	if w := runScript(t, warm, Limits{}, `return 1;`); w.Outcome != OutcomeOK {
		t.Fatalf("warmup run: %s %s", w.Outcome, w.Err)
	}

	h := newEchoHost()
	for _, n := range []string{"a", "b", "c"} {
		h.declare(n, true, true)
	}
	h.delay = 150 * time.Millisecond
	out := runScript(t, h, Limits{}, `
		const r = await Promise.all([tools.a({}), tools.b({}), tools.c({})]);
		return r.length;
	`)
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	var maxStart, minEnd time.Time
	for i, n := range []string{"a", "b", "c"} {
		ts := h.batchTimes[n]
		if i == 0 || ts[0].After(maxStart) {
			maxStart = ts[0]
		}
		if i == 0 || ts[1].Before(minEnd) {
			minEnd = ts[1]
		}
	}
	if !maxStart.Before(minEnd) {
		t.Fatalf("no overlap: last start %v >= earliest end %v", maxStart, minEnd)
	}
	if out.Elapsed >= 3*h.delay {
		t.Fatalf("serial execution suspected: elapsed %v >= 3×delay", out.Elapsed)
	}
}

// Criterion 21: two serial writes run in order.
func TestEngineSerialWritesOrdered(t *testing.T) {
	h := newEchoHost()
	h.declare("write_file", true, false)
	var order []string
	h.fn = func(name string, args map[string]any) HostResult {
		order = append(order, args["step"].(string))
		return HostResult{Status: "completed"}
	}
	out := runScript(t, h, Limits{}, `
		await tools.write_file({step: "one"});
		await tools.write_file({step: "two"});
		return "ok";
	`)
	if out.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	if len(order) != 2 || order[0] != "one" || order[1] != "two" {
		t.Fatalf("order=%v", order)
	}
}

// Criterion 22: call-limit trips reject every further call and classify the
// run call_limit.
func TestEngineCallLimit(t *testing.T) {
	h := newEchoHost()
	h.declare("grep", true, true)
	out := runScript(t, h, Limits{MaxToolCalls: 2}, `
		let rejections = 0;
		for (let i = 0; i < 5; i++) {
			try { await tools.grep({i}); } catch (e) { rejections++; }
		}
		return rejections;
	`)
	if out.Outcome != OutcomeCallLimit {
		t.Fatalf("outcome=%s err=%s", out.Outcome, out.Err)
	}
	if n, _ := out.Value.(float64); n != 3 {
		t.Fatalf("expected 3 rejections after 2 allowed calls, got %v", out.Value)
	}
}

// Criterion 19, engine side: a write interrupted by the budget clock lands in
// the journal as interrupted AND in edited_paths + writes:.
func TestEngineTimeoutInterruptsPendingWrite(t *testing.T) {
	h := newEchoHost()
	h.declare("edit_file", true, false)
	block := make(chan struct{})
	h.fn = func(name string, args map[string]any) HostResult {
		<-block // an edit still executing when the budget expires
		return HostResult{Status: "completed"}
	}
	defer close(block)
	out := NewEngine().Run(context.Background(), `
		await tools.edit_file({path: "/tmp/x/a.go"});
		return "never";
	`, h, Limits{Timeout: 300 * time.Millisecond, MaxToolCalls: 10})
	if out.Outcome != OutcomeTimeout {
		t.Fatalf("outcome=%s", out.Outcome)
	}
	if len(out.Calls) != 1 || out.Calls[0].Status != JournalInterrupted {
		t.Fatalf("calls=%+v", out.Calls)
	}
	if got := editedPaths(out.Calls); len(got) != 1 || got[0] != "/tmp/x/a.go" {
		t.Fatalf("editedPaths=%v", got)
	}
	line := writesLine(out.Calls)
	if !strings.Contains(line, "edit_file /tmp/x/a.go [interrupted]") {
		t.Fatalf("writes line = %q", line)
	}
}

// Criterion 24, engine side: parent cancellation outranks the budget clock.
func TestEngineParentCancelBeatsBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newEchoHost()
	h.declare("bash", true, false)
	block := make(chan struct{})
	h.fn = func(name string, args map[string]any) HostResult {
		<-block
		return HostResult{Status: "completed"}
	}
	defer close(block)
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	out := NewEngine().Run(ctx, `await tools.bash({command: "x"}); return 1;`, h,
		Limits{Timeout: 60 * time.Second, MaxToolCalls: 10})
	if out.Outcome != OutcomeCancelled {
		t.Fatalf("outcome=%s, want cancelled", out.Outcome)
	}
}

// Criteria 11–16 + 9 + 12: the return-value contract through the REAL
// builtin handlers over a real registry.
func TestRegistryHostReturnContract(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("matchme\nplain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	for _, tl := range builtin.FileTools() {
		_ = reg.Register(tl)
	}
	_ = reg.Register(builtin.BashTool())
	host := NewRegistryHost(&tools.Dispatch{Registry: reg, MaxConcurrency: 4}, "rc_1")

	t.Run("bash exit 1 resolves with data.exit_code", func(t *testing.T) {
		res := host.Call(context.Background(), "bash", map[string]any{"command": "exit 1"})
		if res.Err != "" {
			t.Fatalf("unexpected reject: %s", res.Err)
		}
		if res.Status != "completed" {
			t.Fatalf("status=%s want completed", res.Status)
		}
		m, ok := res.Data.(map[string]any)
		if !ok || m["exit_code"].(float64) != 1 {
			t.Fatalf("data=%#v", res.Data)
		}
	})
	t.Run("bash timeout rejects with non-empty text", func(t *testing.T) {
		res := host.Call(context.Background(), "bash", map[string]any{"command": "sleep 5", "timeout": float64(1)})
		if res.Err == "" {
			t.Fatal("bash timeout must reject (tool-layer failure)")
		}
		if strings.TrimSpace(res.Text) == "" {
			t.Fatal("reject lost .text — the model must see the tool's own output")
		}
	})
	t.Run("grep matches are plain objects", func(t *testing.T) {
		res := host.Call(context.Background(), "grep", map[string]any{"pattern": "matchme", "path": dir})
		if res.Err != "" {
			t.Fatalf("reject: %s", res.Err)
		}
		m, ok := res.Data.(map[string]any)
		if !ok {
			t.Fatalf("data=%#v", res.Data)
		}
		matches, _ := m["matches"].([]any)
		if len(matches) == 0 {
			t.Fatal("no matches")
		}
		first, _ := matches[0].(map[string]any)
		for _, k := range []string{"file", "line", "content", "hash"} {
			if _, ok := first[k]; !ok {
				t.Fatalf("match missing %q: %v", k, first)
			}
		}
		if _, bad := m["offloaded"]; bad {
			t.Fatal("internal key leaked into data")
		}
	})
	t.Run("glob data is an array", func(t *testing.T) {
		res := host.Call(context.Background(), "glob", map[string]any{"pattern": "*.txt", "root": dir})
		if res.Err != "" {
			t.Fatalf("reject: %s", res.Err)
		}
		arr, ok := res.Data.([]any)
		if !ok || len(arr) == 0 {
			t.Fatalf("data=%#v, want non-empty array", res.Data)
		}
	})
	t.Run("read_file text data is null, hashes in text", func(t *testing.T) {
		res := host.Call(context.Background(), "read_file", map[string]any{"path": filepath.Join(dir, "a.txt")})
		if res.Err != "" {
			t.Fatalf("reject: %s", res.Err)
		}
		if res.Data != nil {
			t.Fatalf("data=%#v, want nil for a text read", res.Data)
		}
		if !hasHashlinePrefix(res.Text) {
			t.Fatalf("text lacks N:hhhhhh prefixes: %q", res.Text)
		}
	})
	t.Run("edit before read rejects with the gate message", func(t *testing.T) {
		// Without a tracker in ctx edit.go skips the gate by design; the
		// live path always injects one (runOneTool), so mirror that here.
		gateCtx := tools.WithReadTracker(context.Background(), tools.NewReadTracker())
		res := host.Call(gateCtx, "edit_file", map[string]any{
			"path":       filepath.Join(dir, "a.txt"),
			"old_string": "matchme", "new_string": "changed",
		})
		if res.Err == "" {
			t.Fatal("edit without read must reject")
		}
		if !strings.Contains(res.Err, "has not been read in this session") {
			t.Fatalf("reject message = %q, want the read-gate text", res.Err)
		}
	})
}

// Criterion 10: nested call IDs are <runcodeID>.<n>.
func TestRegistryHostCallIDShape(t *testing.T) {
	reg := tools.NewRegistry()
	var ids []string
	_ = reg.Register(models.Tool{Name: "t1", ParallelSafe: true,
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			ids = append(ids, c.ID)
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted}, nil
		}})
	host := NewRegistryHost(&tools.Dispatch{Registry: reg}, "rc_parent_1")
	host.Call(context.Background(), "t1", nil)
	host.Call(context.Background(), "t1", nil)
	if len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
	for i, id := range ids {
		want := "rc_parent_1." + strconv.Itoa(i+1)
		if id != want {
			t.Fatalf("id[%d]=%q want %q", i, id, want)
		}
	}
}

// Criteria 17 + 25, handler level: a script's successful write lands in
// Data["edited_paths"], and the handler never returns an error.
func TestRunCodeHandlerEditedPaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "w.txt")
	reg := tools.NewRegistry()
	for _, tl := range builtin.FileTools() {
		_ = reg.Register(tl)
	}
	_ = reg.Register(builtin.BashTool())
	reg2 := tools.NewRegistry()
	_ = reg2.Register(models.Tool{Name: "run_code", Handler: RunCodeHandler})

	ctx := tools.WithDispatch(context.Background(), &tools.Dispatch{Registry: reg, MaxConcurrency: 4})
	res, err := RunCodeHandler(ctx, models.ToolCall{
		ID: "rc_1", Name: "run_code",
		Arguments: map[string]any{
			"script": "await tools.write_file({path: " + strconv.Quote(target) + ", content: \"hi\\n\"});\nreturn 1;",
		},
	})
	if err != nil {
		t.Fatalf("handler returned err — contract requires nil: %v", err)
	}
	if res.Status != models.CallStatusCompleted {
		t.Fatalf("status=%s err=%q content=%q", res.Status, res.Error, res.Content)
	}
	paths, _ := res.Data["edited_paths"].([]string)
	if len(paths) != 1 || paths[0] != target {
		t.Fatalf("edited_paths=%v", paths)
	}
	if !strings.Contains(res.Content, "write_file "+target+" [completed]") {
		t.Fatalf("content missing writes line: %q", res.Content)
	}
	_ = reg2
}

// Criteria 23 + 25 + 28, handler level: a pure-JS infinite loop under a 1s
// budget — Run returns within ~1s, Status failed, outcome timeout, err==nil,
// Error==Content, and the text ends with a writes: line.
func TestRunCodeHandlerInfiniteLoop(t *testing.T) {
	// The wedged VM goroutine is non-preemptible and would block the next
	// GC's stop-the-world in THIS process (see runInWedgeChild) — isolate.
	runInWedgeChild(t)
	reg := tools.NewRegistry()
	ctx := tools.WithDispatch(context.Background(), &tools.Dispatch{Registry: reg})
	start := time.Now()
	res, err := RunCodeHandler(ctx, models.ToolCall{
		ID: "rc_2", Name: "run_code",
		Arguments: map[string]any{"script": "while (true) {}", "timeout_sec": 1.0},
	})
	if err != nil {
		t.Fatalf("handler returned err — contract requires nil: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watchdog too slow: %v", elapsed)
	}
	if res.Status != models.CallStatusFailed {
		t.Fatalf("status=%s", res.Status)
	}
	if got, _ := res.Data["outcome"].(string); got != "timeout" {
		t.Fatalf("outcome=%v", res.Data["outcome"])
	}
	if res.Error != res.Content {
		t.Fatal("Error must carry the same text as Content on failure")
	}
	lines := strings.Split(strings.TrimRight(res.Content, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "writes:") {
		t.Fatalf("last line %q, want writes:…", last)
	}
}

// Criterion 28: no writes renders a trailing "writes: none".
func TestRenderWritesNone(t *testing.T) {
	res := Render(RunOutput{Outcome: OutcomeOK, Value: "x"}, "s")
	lines := strings.Split(strings.TrimRight(res.Content, "\n"), "\n")
	if last := lines[len(lines)-1]; last != "writes: none" {
		t.Fatalf("last line = %q", last)
	}
}

// Criterion 27: an over-budget writes line keeps the head, ends with
// "… +k more (N writes total)", and the total count is always visible.
func TestRenderWritesTruncation(t *testing.T) {
	var calls []JournalEntry
	for i := 0; i < 30; i++ {
		calls = append(calls, JournalEntry{
			Seq: i + 1, Tool: "edit_file", Status: JournalCompleted, Write: true,
			args: map[string]any{"path": "/tmp/x/very-long-directory-name/file-" + strconv.Itoa(i) + ".go"},
		})
	}
	line := writesLine(calls)
	if len(line) > writesLineMaxBytes+80 { // suffix len headroom
		t.Fatalf("line not capped: %d bytes", len(line))
	}
	if !strings.Contains(line, "… +") || !strings.Contains(line, "(30 writes total)") {
		t.Fatalf("line = %q", line)
	}
}

// Review #6: a single over-budget write (huge path) is itself capped so the
// line can never exceed 600B — otherwise one extra log line pushes it out of
// the offload tail preview.
func TestRenderSingleWriteCapped(t *testing.T) {
	long := strings.Repeat("d/", 400) + "file.go"
	line := writesLine([]JournalEntry{{
		Seq: 1, Tool: "edit_file", Status: JournalCompleted, Write: true,
		args: map[string]any{"path": long},
	}})
	if len(line) > writesLineMaxBytes {
		t.Fatalf("single-item line not capped: %d bytes", len(line))
	}
	if !strings.HasPrefix(line, "writes: edit_file ") {
		t.Fatalf("line = %q", line)
	}
	if !strings.HasSuffix(line, "…") {
		t.Fatalf("capped line must end with ellipsis: %q", line)
	}
}

// Criterion 16: internal marker keys never reach the script's data.
func TestToJSONValueDropsInternalKeys(t *testing.T) {
	v := toJSONValue(map[string]any{
		"matches":                []any{map[string]any{"file": "a"}},
		models.ToolDataOffloaded: "x",
		models.ToolDataNoOffload: "y",
	})
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("v=%#v", v)
	}
	if _, bad := m[models.ToolDataOffloaded]; bad {
		t.Fatal("offloaded key leaked")
	}
	if _, bad := m[models.ToolDataNoOffload]; bad {
		t.Fatal("offload_exempt key leaked")
	}
	if _, ok := m["matches"]; !ok {
		t.Fatal("payload key lost")
	}
}

// hasHashlinePrefix checks for an "N:hhhhhh" line prefix without regexp.
func hasHashlinePrefix(s string) bool {
	for _, ln := range strings.Split(s, "\n") {
		i := strings.Index(ln, ":")
		if i <= 0 {
			continue
		}
		rest := ln[i+1:]
		if len(rest) >= 6 && isHex(rest[:6]) {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
