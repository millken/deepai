package runcode

import (
	"context"
	"encoding/json"
	"time"
)

// settleItem is one promise settlement the driver pushes into the VM after a
// batch ran: ok=true resolves {status,text,data}; ok=false rejects with
// {message,tool,text,data} (§4.4).
type settleItem struct {
	id        int
	valueJSON string
	ok        bool
}

// segment is a run of consecutive parallel-safe requests, or one lone serial
// request — the same partition the top-level loop's partitionToolCalls uses
// (toolexec.go:705), re-expressed over pending script calls (§5).
type segment struct {
	start, end int
	parallel   bool
}

func partition(reqs []vmRequest) []segment {
	var segs []segment
	for i, r := range reqs {
		if r.poison != "" {
			segs = append(segs, segment{start: i, end: i + 1, parallel: false})
			continue
		}
		if r.parallel && len(segs) > 0 && segs[len(segs)-1].parallel && segs[len(segs)-1].end == i {
			segs[len(segs)-1].end = i + 1
			continue
		}
		segs = append(segs, segment{start: i, end: i + 1, parallel: r.parallel})
	}
	return segs
}

// rejectSettle builds the rejection for a request that will never execute
// (invisible/excluded tool, call-limit trip). Poisoned requests get no journal
// entry — nothing started, nothing may have landed.
func rejectSettle(r vmRequest) settleItem {
	return settleItem{
		id: r.id,
		valueJSON: mustJSON(map[string]any{
			"message": r.poison,
			"tool":    "",
			"text":    "",
			"data":    nil,
		}),
		ok: false,
	}
}

// executeBatch runs one drained batch: segments execute in order, parallel
// segments fan out capped at maxConcurrency, serial requests run alone. Every
// executed call is journaled running BEFORE the call and finalized after
// (§6.2). Settlements come back in the batch's original request order.
// The script budget gates STARTS at three points (segment head, serial
// request head, after a parallel worker acquires its slot): once expired,
// remaining requests settle as rejections WITHOUT executing or journaling —
// an unstarted write must never land after the watchdog has already returned
// (it would be invisible to edited_paths and the review gate).
func executeBatch(scriptCtx context.Context, host Host, jrn *journal, batch []vmRequest, maxConcurrency int) []settleItem {
	items := make([]settleItem, len(batch))
	for i, r := range batch {
		if r.poison != "" {
			items[i] = rejectSettle(r)
		}
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}
	budgetGated := func(i int) bool {
		if scriptCtx.Err() == nil {
			return false
		}
		items[i] = settleItem{
			id:        batch[i].id,
			valueJSON: mustJSON(map[string]any{"message": "script budget expired before this call started", "tool": batch[i].name}),
			ok:        false,
		}
		return true
	}
	for _, seg := range partition(batch) {
		if !seg.parallel || seg.end-seg.start == 1 {
			for i := seg.start; i < seg.end; i++ {
				if batch[i].poison != "" || budgetGated(i) {
					continue
				}
				items[i] = runOne(scriptCtx, host, jrn, batch[i])
			}
			continue
		}
		sem := make(chan struct{}, maxConcurrency)
		done := make(chan struct{}, seg.end-seg.start)
		for i := seg.start; i < seg.end; i++ {
			if batch[i].poison != "" {
				done <- struct{}{}
				continue
			}
			go func(i int) {
				sem <- struct{}{}
				// Re-check AFTER acquiring the slot: a sibling worker that
				// finished past the deadline must not be followed by a fresh
				// write start that only existed because a slot freed up.
				if scriptCtx.Err() != nil {
					items[i] = settleItem{
						id:        batch[i].id,
						valueJSON: mustJSON(map[string]any{"message": "script budget expired before this call started", "tool": batch[i].name}),
						ok:        false,
					}
					<-sem
					done <- struct{}{}
					return
				}
				items[i] = runOne(scriptCtx, host, jrn, batch[i])
				<-sem
				done <- struct{}{}
			}(i)
		}
		for i := seg.start; i < seg.end; i++ {
			<-done
		}
	}
	return items
}

// runOne executes a single visible request and finalizes its journal entry.
// "Command failed" (bash exit 1) stays a resolution; only tool-layer failures
// reject (F2b) — registryHost.Call encodes that distinction in HostResult.Err.
func runOne(scriptCtx context.Context, host Host, jrn *journal, r vmRequest) settleItem {
	var args map[string]any
	if r.argsJSON != "" {
		_ = json.Unmarshal([]byte(r.argsJSON), &args)
	}
	entry := jrn.begin(r.name, args, !r.parallel)
	t0 := time.Now()
	res := host.Call(scriptCtx, r.name, args)
	ms := time.Since(t0).Milliseconds()

	if res.Err != "" {
		jrn.finish(entry, JournalFailed, ms, len(res.Text), res.Err)
		return settleItem{
			id: r.id,
			valueJSON: mustJSON(map[string]any{
				"message": res.Err,
				"tool":    res.Tool,
				"text":    res.Text,
				"data":    res.Data,
			}),
			ok: false,
		}
	}
	value := map[string]any{
		"status": res.Status,
		"text":   res.Text,
		"data":   res.Data,
	}
	envJSON := mustJSON(value)
	jrn.finish(entry, JournalCompleted, ms, len(envJSON), "")
	return settleItem{id: r.id, valueJSON: envJSON, ok: true}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
