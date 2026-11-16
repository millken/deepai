package runcode

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The partition rule mirrors the top-level partitionToolCalls (toolexec.go:705):
// maximal runs of consecutive parallel-safe requests; every unsafe or
// poisoned request stands alone.
func TestPartitionMirrorsTopLevelRules(t *testing.T) {
	// r=parallel-safe, w=unsafe, p=poison
	mk := func(spec string) []vmRequest {
		var reqs []vmRequest
		for i, ch := range spec {
			r := vmRequest{id: i, name: string(ch)}
			switch ch {
			case 'r':
				r.parallel = true
				r.started = true
			case 'w':
				r.started = true
			case 'p':
				r.poison = "x"
			}
			reqs = append(reqs, r)
		}
		return reqs
	}
	tests := []struct {
		spec string
		want []segment
	}{
		{"rrr", []segment{{0, 3, true}}},
		{"rwr", []segment{{0, 1, true}, {1, 2, false}, {2, 3, true}}},
		{"rrwr", []segment{{0, 2, true}, {2, 3, false}, {3, 4, true}}},
		{"w", []segment{{0, 1, false}}},
		{"rpr", []segment{{0, 1, true}, {1, 2, false}, {2, 3, true}}},
		{"", nil},
	}
	for _, tt := range tests {
		got := partition(mk(tt.spec))
		if len(got) != len(tt.want) {
			t.Fatalf("partition(%q) = %+v, want %+v", tt.spec, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("partition(%q)[%d] = %+v, want %+v", tt.spec, i, got[i], tt.want[i])
			}
		}
	}
}

// Concurrency cap: a parallel segment of 6 with maxConcurrency 2 never has
// more than 2 calls in flight.
func TestExecuteBatchRespectsConcurrencyCap(t *testing.T) {
	host := newEchoHost()
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		host.declare(n, true, true)
	}
	var inFlight, maxSeen atomic.Int32
	host.fn = func(name string, args map[string]any) HostResult {
		cur := inFlight.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return HostResult{Status: "completed"}
	}
	var reqs []vmRequest
	for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
		reqs = append(reqs, vmRequest{id: i, name: n, parallel: true, started: true})
	}
	items := executeBatch(context.Background(), host, newJournal(nil), reqs, 2)
	if len(items) != 6 {
		t.Fatalf("items=%d", len(items))
	}
	if m := maxSeen.Load(); m > 2 {
		t.Fatalf("max in-flight = %d, cap was 2", m)
	}
}

// Criterion 21: serial writes run in issue order and the second sees the
// first's effect; poisoned requests reject without journaling.
func TestExecuteBatchSerialOrderAndPoison(t *testing.T) {
	host := newEchoHost()
	host.declare("write_file", true, false)
	var mu sync.Mutex
	state := map[string]int{}
	var order []string
	host.fn = func(name string, args map[string]any) HostResult {
		mu.Lock()
		defer mu.Unlock()
		step, _ := args["step"].(string)
		state[step] = len(order)
		order = append(order, step)
		return HostResult{Status: "completed"}
	}
	jrn := newJournal(nil)
	reqs := []vmRequest{
		{id: 0, name: "write_file", argsJSON: `{"step":"one"}`, started: true},
		{id: 1, poison: "tool not available in run_code: task"},
		{id: 2, name: "write_file", argsJSON: `{"step":"two"}`, started: true},
	}
	items := executeBatch(context.Background(), host, jrn, reqs, 10)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "one" || order[1] != "two" {
		t.Fatalf("order=%v (writes must serialize in issue order)", order)
	}
	if state["two"] <= state["one"] {
		t.Fatal("second write must observe the first's effect (run after it)")
	}
	if items[1].ok {
		t.Fatal("poisoned request must reject")
	}
	if !strings.Contains(items[1].valueJSON, "tool not available in run_code: task") {
		t.Fatalf("poison message = %q", items[1].valueJSON)
	}
	// Poison gets no journal entry: nothing started, nothing may have landed.
	calls := jrn.snapshot(false)
	if len(calls) != 2 {
		t.Fatalf("journal entries = %+v, want only the two real writes", calls)
	}
	for _, c := range calls {
		if c.Status != JournalCompleted {
			t.Fatalf("entry %+v not finalized", c)
		}
	}
}

// Review H2: once the script budget has expired, executeBatch must not START
// any further request — an unstarted edit_file must never land on disk after
// the watchdog already returned. Gated requests settle as rejections with NO
// journal entry (nothing started, nothing may have landed).
func TestExecuteBatchGatesOnExpiredBudget(t *testing.T) {
	host := newEchoHost()
	host.declare("edit_file", true, false)
	host.declare("grep", true, true)

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	jrn := newJournal(nil)
	batch := []vmRequest{
		{id: 0, name: "edit_file", argsJSON: `{"step":"one"}`, started: true},
		{id: 1, name: "grep", argsJSON: `{}`, parallel: true, started: true},
		{id: 2, name: "grep", argsJSON: `{}`, parallel: true, started: true},
		{id: 3, name: "edit_file", argsJSON: `{"step":"two"}`, started: true},
	}
	items := executeBatch(expired, host, jrn, batch, 1)

	host.mu.Lock()
	calls := append([]string(nil), host.calls...)
	host.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("expired budget still executed: %v", calls)
	}
	if entries := jrn.snapshot(false); len(entries) != 0 {
		t.Fatalf("gated requests must leave no journal entries: %+v", entries)
	}
	for i, it := range items {
		if it.ok {
			t.Fatalf("items[%d] resolved, want gated rejection", i)
		}
		if !strings.Contains(it.valueJSON, "budget") {
			t.Fatalf("items[%d] rejection = %q, want budget message", i, it.valueJSON)
		}
	}
}
