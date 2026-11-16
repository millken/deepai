package runcode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fastschema/qjs"
)

// runInWedgeChild isolates tests that intentionally wedge a VM goroutine in a
// pure-JS infinite loop. Such a goroutine is NOT preemptible (wazero
// compiler-mode code has no scheduler checkpoints), so once leaked it blocks
// Go's stop-the-world on the next GC — poisoning the WHOLE test process (the
// -count=3 suite hang and the missing timeout panic dump are exactly this).
// The child runs with GOGC=off so no GC ever stops the world: it completes,
// reports, and exits cleanly; the parent asserts a zero exit code.
func runInWedgeChild(t *testing.T) {
	t.Helper()
	if os.Getenv("RC_WEDGE_CHILD") != "" {
		return // we are the child: run the real test below
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "RC_WEDGE_CHILD=1", "GOGC=off")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("wedge child %s timed out (wedge escaped isolation?):\n%s", t.Name(), out)
	}
	if err != nil {
		t.Fatalf("wedge child %s failed: %v\n%s", t.Name(), err, out)
	}
	// The child already ran the real body. The parent must NOT fall through
	// to it — a wedged VM goroutine here would poison this process's next GC
	// stop-the-world (verified: exactly this made the suite hang).
	t.SkipNow()
}

// Spike S1 (§3.3): with the shared wazero compilation cache, creating a
// runtime+context must be far below one model round trip. p50 < 30ms.
func TestSpikeS1_ColdRuntimeCreation(t *testing.T) {
	// First creation compiles/warms the shared module cache.
	warm, err := qjs.New(qjs.Option{MemoryLimit: jsMemoryLimit})
	if err != nil {
		t.Fatalf("warmup runtime: %v", err)
	}
	warm.Close()

	var durs []time.Duration
	for i := 0; i < 100; i++ {
		t0 := time.Now()
		rt, err := qjs.New(qjs.Option{MemoryLimit: jsMemoryLimit})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		_ = rt.Context()
		durs = append(durs, time.Since(t0))
		rt.Close()
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p50 := durs[50]
	t.Logf("S1: runtime+ctx x100 p50=%v p90=%v max=%v", p50, durs[90], durs[99])
	if p50 >= 30*time.Millisecond {
		t.Fatalf("S1 FAIL: p50 %v >= 30ms", p50)
	}
}

// Spike S2 (§3.3): host functions only REGISTER a request and hand control
// back to the VM; tools execute only after the driver's eval has returned,
// and a Promise.all batch overlaps in wall time.
func TestSpikeS2_DrainThenBatch(t *testing.T) {
	h := newEchoHost()
	for _, n := range []string{"a", "b", "c"} {
		h.declare(n, true, true)
	}
	h.delay = 150 * time.Millisecond

	// Track ordering at the engine seam: registration happens inside the VM
	// entry (__rcHost); execution happens in executeBatch after eval returns.
	var registeredAt []time.Time
	rt, err := qjs.New(qjs.Option{MemoryLimit: jsMemoryLimit})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	defer rt.Close()
	c := rt.Context()

	var reqs []vmRequest
	nextID := 0
	c.SetFunc("__rcHost", func(this *qjs.This) (*qjs.Value, error) {
		name := this.Args()[0].String()
		argsJSON := "{}"
		if len(this.Args()) > 1 {
			if s := this.Args()[1].String(); json.Valid([]byte(s)) {
				argsJSON = s
			}
		}
		id := nextID
		nextID++
		visible, parallel := h.ToolInfo(name)
		reqs = append(reqs, vmRequest{id: id, name: name, argsJSON: argsJSON, parallel: parallel && visible, started: visible})
		registeredAt = append(registeredAt, time.Now())
		return this.Context().NewInt32(int32(id)), nil
	})

	if _, err := c.Eval("s2_prelude.js", qjs.Code(enginePrelude)); err != nil {
		t.Fatalf("prelude: %v", err)
	}
	invoke := strings.Replace(engineInvoke, scriptMark, `
		const r = await Promise.all([tools.a({}), tools.b({}), tools.c({})]);
		return r.length;
	`, 1)
	if _, err := c.Eval("run_code.js", qjs.Code(invoke)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	evalReturned := time.Now()

	jrn := newJournal()
	settle := c.Global().GetPropertyStr("__rcSettle")
	items := executeBatch(context.Background(), h, jrn, reqs, 10)
	for _, it := range items {
		if err := settleOne(c, settle, it); err != nil {
			t.Fatalf("settle: %v", err)
		}
	}
	if err := pump(c); err != nil {
		t.Fatalf("pump: %v", err)
	}

	ok, doneJSON := readDone(c)
	if !ok {
		t.Fatal("script never finished")
	}
	var env doneEnvelope
	if err := json.Unmarshal([]byte(doneJSON), &env); err != nil || !env.OK {
		t.Fatalf("done envelope: %v %s", err, doneJSON)
	}

	// Registration strictly before eval returned; execution strictly after.
	for _, ts := range registeredAt {
		if !ts.Before(evalReturned) {
			t.Fatal("host function ran a tool or blocked past eval return")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 3 {
		t.Fatalf("executed %v, want 3", h.calls)
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
		if ts[0].Before(evalReturned) {
			t.Fatal("tool executed before the VM handed control back")
		}
	}
	if !maxStart.Before(minEnd) {
		t.Fatalf("no overlap: last start %v >= earliest end %v", maxStart, minEnd)
	}
}

// Spike S3 (§3.3): a pure-JS infinite loop cannot be preempted inside one
// wasm call, so Run serves the caller via the watchdog — it must return
// promptly (well under one extra second) after the budget expires.
func TestSpikeS3_InfiniteLoopInterruptedByDeadline(t *testing.T) {
	runInWedgeChild(t)
	h := newEchoHost()
	h.declare("grep", true, true)
	out := NewEngine().Run(context.Background(), `while (true) {}`, h,
		Limits{Timeout: 1 * time.Second, MaxToolCalls: 10})
	if out.Outcome != OutcomeTimeout {
		t.Fatalf("outcome=%s, want timeout", out.Outcome)
	}
	if out.Elapsed > 2*time.Second {
		t.Fatalf("watchdog returned %v after start, want ≈1s (budget) — S3 requires prompt return", out.Elapsed)
	}
}

// Spike S4 (§3.3): a 1GiB string allocation under a 256MiB memory cap is a
// catchable script error — never a host panic or OOM.
func TestSpikeS4_MemoryLimitIsACatchableError(t *testing.T) {
	rt, err := qjs.New(qjs.Option{MemoryLimit: jsMemoryLimit})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	defer rt.Close()
	_, err = rt.Context().Eval("s4.js", qjs.Code(`"x".repeat(1024*1024*1024).length`))
	if err == nil {
		t.Fatal("1GiB alloc under a 256MiB cap must fail")
	}
	if !isMemoryError(err.Error()) {
		t.Fatalf("error not classified as memory: %v", err)
	}
}

// Full-path variant: through Run, the memory outcome is reported as
// OutcomeMemory with err == nil from the handler's perspective.
func TestSpikeS4_MemoryOutcomeThroughRun(t *testing.T) {
	h := newEchoHost()
	out := NewEngine().Run(context.Background(), `"x".repeat(1024*1024*1024).length`, h,
		Limits{Timeout: 30 * time.Second, MaxToolCalls: 10})
	if out.Outcome != OutcomeMemory {
		t.Fatalf("outcome=%s, want memory", out.Outcome)
	}
}
