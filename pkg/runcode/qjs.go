package runcode

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fastschema/qjs"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// jsMemoryLimit is the WASM linear-memory cap (§6.1): 256 MiB.
const jsMemoryLimit = 256 << 20

// maxReturnBytes is the 1 MiB hard cap on the script's return value (§6.1).
const maxReturnBytes = 1 << 20

// enginePrelude sets up the request/settle machinery WITHOUT any user code:
// evaluating it is bounded machine setup, so the script budget clock can start
// only after it completes — otherwise first-run wazero compilation (~360ms)
// would eat into a short script's budget and a pending write would never even
// begin (review #2).
const enginePrelude = `
globalThis.__rcWaiters = {};
globalThis.__rcSettle = (id, value, ok) => {
  const w = __rcWaiters[id]; if (!w) return; delete __rcWaiters[id];
  if (ok) { w.resolve(value); return; }
  const e = new Error(value && value.message || String(value));
  if (value) { e.tool = value.tool; e.text = value.text; e.data = value.data; }
  w.reject(e);
};
globalThis.tools = new Proxy({}, { get: (t, name) => (args) => new Promise((resolve, reject) => {
  const id = __rcHost(String(name), JSON.stringify(args || {}));
  __rcWaiters[id] = {resolve, reject};
})});
globalThis.__rcDone = null;
0
`

// engineInvoke wraps the user script (§4.2) so top-level await/return work.
// The user code runs inside this eval; a pure-JS infinite loop here hangs the
// eval, which the driver-side deadline + Run-side watchdog timer cover.
// __rcDone is JSON.stringify'd INSIDE the VM: reading a plain string crosses
// the wasm boundary via a buffer the shim does NOT free, unlike
// Value.JSONStringify (whose backing buffer QJS_JSONStringify frees before
// returning — a structural use-after-free; verified: the same corrupt byte on
// every retry, review #1).
const engineInvoke = `
globalThis.__rcRun = async (fn) => {
  let v;
  try {
    v = { ok: true, value: await fn() };
  } catch (e) {
    // QuickJS-ng's e.stack carries frames only — fold the message in or
    // memory-cap errors ("RangeError: invalid string length") lose the text
    // the host classifies OutcomeMemory on.
    v = { ok: false, error: e && e.name ? e.name + ": " + (e.message || "") + "\n" + (e.stack || "") : String(e) };
  }
  globalThis.__rcDone = JSON.stringify(v);
};
__rcRun(async () => {
__RC_SCRIPT__
});
0
`

const scriptMark = "__RC_SCRIPT__"

// scriptLineOffset: lines preceding the user's first line inside engineInvoke,
// so VM stack lines can be reported as user-script lines (§4.2).
var scriptLineOffset = strings.Count(strings.SplitN(engineInvoke, scriptMark, 2)[0], "\n")

var vmLineRe = regexp.MustCompile(`run_code\.js:(\d+)`)

func adjustStackLines(msg string) string {
	return vmLineRe.ReplaceAllStringFunc(msg, func(m string) string {
		n, err := strconv.Atoi(m[strings.LastIndex(m, ":")+1:])
		if err != nil || n <= scriptLineOffset {
			return m
		}
		return "run_code.js:" + strconv.Itoa(n-scriptLineOffset)
	})
}

// qjsEngine is the single engine (§3.2): QuickJS-ng compiled to WASM once per
// process, a fresh runtime/context per Run.
type qjsEngine struct{}

// NewEngine returns the run_code script engine.
func NewEngine() Engine { return qjsEngine{} }

// cappedWriter collects console output with a hard cap (§6.1, 8 KiB).
type cappedWriter struct {
	mu    sync.Mutex
	buf   strings.Builder
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.limit <= 0 {
		return len(p), nil
	}
	if w.buf.Len() >= w.limit {
		return len(p), nil
	}
	if w.buf.Len()+len(p) > w.limit {
		w.buf.WriteString(models.TruncateBytes(string(p), w.limit-w.buf.Len()))
		w.limit = 0
		w.buf.WriteString("\n[log truncated]")
		return len(p), nil
	}
	w.buf.WriteString(string(p))
	return len(p), nil
}

func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type vmRequest struct {
	id       int
	name     string
	argsJSON string
	parallel bool
	started  bool // journaled + executing (vs poison: never runs)
	poison   string
}

type vmResult struct {
	outcome Outcome
	value   any
	errText string
}

// Run drives the VM on an inner goroutine and serves the caller via a
// watchdog: a pure-JS infinite loop cannot be preempted inside one wasm call
// (spike S3 finding — the qjs shim's interrupt handler is compiled out), so
// on budget expiry Run returns immediately and abandons the driver.
//
// Two-phase bootstrap: the prelude (no user code) runs under the parent ctx
// only; the budget timer is armed once the prelude is done, so engine setup
// never eats the script's budget. On an abort, journal entries already begun
// snapshot as interrupted — a write that started is always visible (§6.2).
func (e qjsEngine) Run(ctx context.Context, script string, host Host, lim Limits) RunOutput {
	start := time.Now()
	if lim.Timeout <= 0 {
		lim.Timeout = 120 * time.Second
	}
	// Nested events (§7.2 Phase 2) flow through the ctx-injected sink — the
	// same injection pattern as WithDispatch; the Engine interface stays put.
	jrn := newJournal(tools.NestedEventSinkFromContext(ctx))
	logs := &cappedWriter{limit: 8 << 10}
	resCh := make(chan vmResult, 1)
	preludeDone := make(chan error, 1)

	go driveVM(ctx, script, host, lim, jrn, logs, resCh, preludeDone)

	select {
	case err := <-preludeDone:
		if err != nil {
			return e.build(RunOutput{Outcome: errOutcome(err), Err: adjustStackLines(err.Error()), Logs: logs.String(), Calls: jrn.snapshot(false)}, start, ctx)
		}
	case <-ctx.Done():
		return e.aborted(ctx, jrn, logs, start)
	}

	timer := time.NewTimer(lim.Timeout)
	defer timer.Stop()
	select {
	case out := <-resCh:
		res := RunOutput{
			Outcome: out.outcome,
			Value:   out.value,
			Logs:    logs.String(),
			Err:     adjustStackLines(out.errText),
			Calls:   jrn.snapshot(false),
			Elapsed: time.Since(start),
		}
		// Parent cancellation outranks a cooperative timeout report (§6.2).
		if ctx.Err() != nil && res.Outcome == OutcomeTimeout {
			res.Outcome = OutcomeCancelled
		}
		// A memory-cap throw surfaces as a caught script error (bootstrap's
		// catch) — reclassify so S4's outcome reads memory, not error.
		if res.Outcome == OutcomeError && isMemoryError(res.Err) {
			res.Outcome = OutcomeMemory
		}
		return res
	case <-ctx.Done():
		return e.aborted(ctx, jrn, logs, start)
	case <-timer.C:
		return e.aborted(ctx, jrn, logs, start)
	}
}

func (e qjsEngine) aborted(ctx context.Context, jrn *journal, logs *cappedWriter, start time.Time) RunOutput {
	outcome := OutcomeTimeout
	if ctx.Err() != nil {
		outcome = OutcomeCancelled
	}
	return e.build(RunOutput{
		Outcome: outcome,
		Logs:    logs.String(),
		Calls:   jrn.snapshot(true),
		Elapsed: time.Since(start),
	}, start, ctx)
}

func (e qjsEngine) build(res RunOutput, start time.Time, _ context.Context) RunOutput {
	if res.Elapsed == 0 {
		res.Elapsed = time.Since(start)
	}
	if res.Outcome == OutcomeOK && isMemoryError(res.Err) {
		res.Outcome = OutcomeMemory
	}
	return res
}

var memoryErrorMarkers = []string{"out of memory", "invalid string length", "memory allocation of"}

func isMemoryError(msg string) bool {
	if msg == "" {
		return false
	}
	low := strings.ToLower(msg)
	for _, m := range memoryErrorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func errOutcome(err error) Outcome {
	if isMemoryError(err.Error()) {
		return OutcomeMemory
	}
	return OutcomeError
}

// driveVM owns the VM end to end on ONE goroutine: every qjs call (host fn
// entry, settle Invoke, pump Eval, done poll) happens here, so no VM locking
// is needed; only journal state crosses goroutines (mutex-guarded).
func driveVM(ctx context.Context, script string, host Host, lim Limits, jrn *journal, logs *cappedWriter, resCh chan<- vmResult, preludeDone chan<- error) {
	rt, err := qjs.New(qjs.Option{MemoryLimit: jsMemoryLimit, Stdout: logs, Stderr: logs})
	if err != nil {
		preludeDone <- fmt.Errorf("engine init: %w", err)
		return
	}
	defer rt.Close()
	c := rt.Context()

	var reqs []vmRequest
	nextID := 0
	executed := 0
	limitHit := false

	c.SetFunc("__rcHost", func(this *qjs.This) (*qjs.Value, error) {
		name := this.Args()[0].String()
		// Args arrive pre-stringified inside the VM (prelude): JSONStringify
		// on this side reads a buffer the wasm shim frees before returning.
		argsJSON := "{}"
		if len(this.Args()) > 1 {
			if s := this.Args()[1].String(); json.Valid([]byte(s)) {
				argsJSON = s
			}
		}
		id := nextID
		nextID++
		visible, parallel := host.ToolInfo(name)
		if !visible {
			reqs = append(reqs, vmRequest{id: id, poison: fmt.Sprintf("tool not available in run_code: %s", name)})
			return this.Context().NewInt32(int32(id)), nil
		}
		if lim.MaxToolCalls > 0 && executed >= lim.MaxToolCalls {
			limitHit = true
			reqs = append(reqs, vmRequest{id: id, poison: fmt.Sprintf("tool call limit exceeded (max_tool_calls=%d)", lim.MaxToolCalls)})
			return this.Context().NewInt32(int32(id)), nil
		}
		executed++
		reqs = append(reqs, vmRequest{id: id, name: name, argsJSON: argsJSON, parallel: parallel, started: true})
		return this.Context().NewInt32(int32(id)), nil
	})

	if _, err := c.Eval("run_code_prelude.js", qjs.Code(enginePrelude)); err != nil {
		preludeDone <- err
		return
	}
	preludeDone <- nil

	// Budget clock starts here — engine setup is off the clock (review #2).
	deadline := time.Now().Add(lim.Timeout)
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	invoke := strings.Replace(engineInvoke, scriptMark, script, 1)
	if _, err := c.Eval("run_code.js", qjs.Code(invoke)); err != nil {
		resCh <- vmResult{outcome: errOutcome(err), errText: err.Error()}
		return
	}

	settle := c.Global().GetPropertyStr("__rcSettle")
	for step := 0; ; step++ {
		if ctx.Err() != nil {
			resCh <- vmResult{outcome: OutcomeCancelled}
			return
		}
		if time.Now().After(deadline) {
			resCh <- vmResult{outcome: OutcomeTimeout}
			return
		}
		if ok, s := readDone(c); ok {
			out := parseDone(s)
			if limitHit && out.outcome == OutcomeOK {
				out.outcome = OutcomeCallLimit
				out.errText = fmt.Sprintf("tool call limit exceeded (max_tool_calls=%d)", lim.MaxToolCalls)
			}
			resCh <- out
			return
		}
		if len(reqs) == 0 {
			if err := pump(c); err != nil {
				resCh <- vmResult{outcome: OutcomeError, errText: err.Error()}
				return
			}
			// Bounded idle tick: a script parked on an unresolved promise
			// waits here until the wall clock ends the run.
			if step%64 == 63 {
				time.Sleep(time.Millisecond)
			}
			continue
		}
		batch := reqs
		reqs = nil
		for _, it := range executeBatch(callCtx, host, jrn, batch, lim.MaxConcurrency) {
			if serr := settleOne(c, settle, it); serr != nil {
				resCh <- vmResult{outcome: OutcomeError, errText: serr.Error()}
				return
			}
		}
		if err := pump(c); err != nil {
			resCh <- vmResult{outcome: OutcomeError, errText: err.Error()}
			return
		}
	}
}

// pump drains the VM's pending-job queue: Eval executes quickjs's std loop,
// which runs promise continuations until no runnable job is left — the script
// then sits at its awaits and control is back with the driver (spike S2).
func pump(c *qjs.Context) error {
	res, err := c.Eval("pump.js", qjs.Code("0"))
	if err != nil {
		return err
	}
	res.Free()
	return nil
}

type doneEnvelope struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

func readDone(c *qjs.Context) (bool, string) {
	v := c.Global().GetPropertyStr("__rcDone")
	defer v.Free()
	if v.IsNull() || v.IsUndefined() {
		return false, ""
	}
	s := v.String()
	if !json.Valid([]byte(s)) {
		return true, `{"ok":false,"error":"failed to serialize script result"}`
	}
	return true, s
}

func parseDone(s string) vmResult {
	var env doneEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return vmResult{outcome: OutcomeError, errText: fmt.Sprintf("failed to parse script result: %v", err)}
	}
	if env.OK {
		var value any
		if len(env.Value) > 0 {
			_ = json.Unmarshal(env.Value, &value)
		}
		if b, err := json.Marshal(value); err == nil && len(b) > maxReturnBytes {
			return vmResult{outcome: OutcomeError, errText: fmt.Sprintf("return value exceeds the 1 MiB cap (%d bytes)", len(b))}
		}
		return vmResult{outcome: OutcomeOK, value: value}
	}
	return vmResult{outcome: OutcomeError, errText: env.Error}
}

func settleOne(c *qjs.Context, settle *qjs.Value, it settleItem) error {
	pv := c.ParseJSON(it.valueJSON)
	defer pv.Free()
	idv := c.NewInt32(int32(it.id))
	okv := c.NewBool(it.ok)
	res, err := c.Invoke(settle, c.Global(), idv, pv, okv)
	if err != nil {
		return fmt.Errorf("settle call %d: %w", it.id, err)
	}
	res.Free()
	return nil
}
