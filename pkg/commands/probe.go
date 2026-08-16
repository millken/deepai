package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dnsoa/go/env"
	"github.com/millken/deepai/pkg/netutil"
	"github.com/millken/deepai/pkg/secret"
	"github.com/spf13/cobra"
)

// The stream-idle diagnostic. Sessions show parallel subagent fan-outs where
// most requests die with "stream idle timeout: no data received after 2m0s"
// while one completes normally. Two very different causes produce that:
//
//   - the server accepts every request, returns headers, then feeds only some
//     of them (an account concurrency limit that queues rather than 429s), or
//   - something on our side (SDK, provider, agent) drops or fails to observe
//     data that did arrive.
//
// This probe deliberately bypasses BOTH the vendor SDK and pkg/llm, talking to
// the endpoint over plain net/http, so a stall it reproduces is the server's
// and a clean run points the finger back at our code. It also counts SSE ping
// keepalives, which the Anthropic SDK's ssestream discards before provider code
// can see them (see pkg/llm/anthropic.go) — if the "silent" requests were
// actually receiving pings the whole time, the idle watchdog is killing
// demonstrably live connections and the fix is to observe the wire, not to
// raise the timeout.
//
// It costs real API quota, so it only ever runs when invoked explicitly.

// observation is what one request's response body did on the wire.
type observation struct {
	FirstByte  time.Duration // start -> first byte of body (any byte, including a ping)
	FirstEvent time.Duration // start -> first non-ping SSE event
	MaxGap     time.Duration // longest silence, counting the wait before the first byte
	Bytes      int
	Events     int
	Pings      int
}

// sseObserver timestamps raw reads and classifies SSE lines as they stream past.
// Classification tolerates reads that split lines, since TCP does not respect
// line boundaries.
type sseObserver struct {
	start    time.Time
	lastRead time.Time
	obs      observation
	partial  []byte
}

func newSSEObserver(start time.Time) *sseObserver {
	return &sseObserver{start: start, lastRead: start}
}

// observe records one non-empty read of the body at time at.
func (o *sseObserver) observe(at time.Time, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if gap := at.Sub(o.lastRead); gap > o.obs.MaxGap {
		o.obs.MaxGap = gap
	}
	o.lastRead = at
	if o.obs.Bytes == 0 {
		o.obs.FirstByte = at.Sub(o.start)
	}
	o.obs.Bytes += len(chunk)

	data := append(o.partial, chunk...)
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		o.classify(at, strings.TrimSpace(string(data[:idx])))
		data = data[idx+1:]
	}
	o.partial = append(o.partial[:0], data...)
}

func (o *sseObserver) classify(at time.Time, line string) {
	name, ok := strings.CutPrefix(line, "event:")
	if !ok {
		return
	}
	if strings.TrimSpace(name) == "ping" {
		o.obs.Pings++
		return
	}
	o.obs.Events++
	if o.obs.FirstEvent == 0 {
		o.obs.FirstEvent = at.Sub(o.start)
	}
}

// finish folds in the silence between the last read and the end of the
// request. Without it the stall a STALL row exists to describe — minutes of
// nothing after the final byte — is never measured at all.
func (o *sseObserver) finish(at time.Time) {
	if gap := at.Sub(o.lastRead); gap > o.obs.MaxGap {
		o.obs.MaxGap = gap
	}
}

func (o *sseObserver) result() observation { return o.obs }

// probeResult is one request's outcome.
type probeResult struct {
	Index     int
	HeadersAt time.Duration // start -> response headers (what NewStreaming blocks on)
	Status    int
	Total     time.Duration
	Obs       observation
	Stalled   bool
	Err       error
}

// verdict is the probe's read of a whole run.
type verdict struct {
	Requests       int
	Stalled        int
	ServerWithheld bool // 200, headers returned fast, then no events at all
	KeepalivesSeen bool // a stalled request was still receiving pings
	SlowHeaders    bool // a stalled request never even got headers promptly
	Rejected       bool // the server answered with a non-200 (429, 401, ...)
}

// slowHeaderThreshold separates "the server answered and then went quiet"
// (which the agent's idle watchdog sees, since it only starts once headers are
// in) from "the request never got off the ground" (which it cannot see).
const slowHeaderThreshold = 30 * time.Second

func probeVerdict(results []probeResult) verdict {
	v := verdict{Requests: len(results)}
	for _, r := range results {
		if !r.Stalled {
			continue
		}
		v.Stalled++
		switch {
		case r.Status != 0 && r.Status != http.StatusOK:
			// An outright rejection (429, 401) is the opposite of withholding:
			// the server answered, promptly and explicitly.
			v.Rejected = true
		case r.HeadersAt >= slowHeaderThreshold:
			v.SlowHeaders = true
		case r.Obs.Events == 0:
			v.ServerWithheld = true
		}
		if r.Obs.Pings > 0 {
			v.KeepalivesSeen = true
		}
	}
	return v
}

func addProbe(topLevel *cobra.Command) {
	var (
		parallel int
		model    string
		baseURL  string
		keyEnv   string
		prompt   string
		idle     time.Duration
		maxTok   int
	)

	cmd := &cobra.Command{
		Use:   "probe-stream",
		Short: "Fire N parallel streaming requests and report where each one stalls",
		Long: `Diagnose "stream idle timeout" failures.

Sends N identical streaming requests concurrently, straight over net/http —
no vendor SDK, no pkg/llm — and reports, per request: time to response
headers, time to first byte, time to first real SSE event, ping keepalives
received, and the longest silence.

That distinguishes a server that accepts requests and then withholds data
(a concurrency limit that queues instead of returning 429) from a stall
inside our own code, and shows whether "silent" connections were in fact
receiving pings the whole time.

This spends real API quota. Start with --parallel 1 as a control, then
raise it to the fan-out that fails.`,
		Example: "  deepai probe-stream --parallel 1\n  deepai probe-stream --parallel 5",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := LoadConfig(ConfigFile())
			if model == "" {
				model = cfg.Model
			}
			if model == "" {
				return fmt.Errorf("no model: set one in %s or pass --model", ConfigFile())
			}
			if baseURL == "" {
				baseURL = strings.TrimSpace(env.Get("ANTHROPIC_BASE_URL", ""))
			}
			if baseURL == "" {
				return fmt.Errorf("no base URL: set ANTHROPIC_BASE_URL or pass --base-url")
			}
			apiKey, err := secret.Reveal(strings.TrimSpace(env.Get(keyEnv, "")))
			if err != nil {
				return fmt.Errorf("read %s: %w", keyEnv, err)
			}
			if apiKey == "" {
				return fmt.Errorf("no API key in %s", keyEnv)
			}

			results := runProbe(cmd.Context(), probeConfig{
				parallel:  parallel,
				model:     model,
				baseURL:   strings.TrimSuffix(baseURL, "/"),
				apiKey:    apiKey,
				prompt:    prompt,
				idle:      idle,
				maxTokens: maxTok,
			})
			printProbeReport(cmd.OutOrStdout(), results, idle)
			return nil
		},
	}

	cmd.Flags().IntVar(&parallel, "parallel", 5, "number of concurrent requests")
	cmd.Flags().StringVar(&model, "model", "", "model id (default: config.yaml model)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "API base URL (default: $ANTHROPIC_BASE_URL)")
	cmd.Flags().StringVar(&keyEnv, "api-key-env", "ANTHROPIC_API_KEY", "env var holding the API key")
	cmd.Flags().StringVar(&prompt, "prompt", "Count slowly from 1 to 30, one number per line.", "prompt to send")
	cmd.Flags().DurationVar(&idle, "idle-timeout", 2*time.Minute, "silence after which a request is declared stalled (matches the agent's watchdog)")
	cmd.Flags().IntVar(&maxTok, "max-tokens", 1024, "max_tokens for each request")

	topLevel.AddCommand(cmd)
}

type probeConfig struct {
	parallel  int
	model     string
	baseURL   string
	apiKey    string
	prompt    string
	idle      time.Duration
	maxTokens int
}

func runProbe(ctx context.Context, cfg probeConfig) []probeResult {
	// One shared client, mirroring how the agent runs concurrent subagents
	// through a single pooled transport (pkg/llm/http.go).
	client := &http.Client{
		Timeout:   0,
		Transport: &http.Transport{Proxy: netutil.EnvProxyFunc, ForceAttemptHTTP2: true},
	}

	results := make([]probeResult, cfg.parallel)
	var wg sync.WaitGroup
	for i := 0; i < cfg.parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = probeOnce(ctx, client, cfg, i+1)
		}(i)
	}
	wg.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Index < results[b].Index })
	return results
}

func probeOnce(ctx context.Context, client *http.Client, cfg probeConfig, index int) probeResult {
	res := probeResult{Index: index}
	start := time.Now()

	body, err := json.Marshal(map[string]any{
		"model":      cfg.model,
		"max_tokens": cfg.maxTokens,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": cfg.prompt}},
	})
	if err != nil {
		res.Err = err
		return res
	}

	// The per-request context is cancelled once the body goes quiet for longer
	// than the idle window, reproducing exactly what the agent's watchdog does.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		res.Err = err
		return res
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", cfg.apiKey)

	// Arm the watchdog BEFORE the request, not after: a server that accepts the
	// connection and never writes response headers is precisely the queued-
	// request shape this probe reports on, and client.Do has no timeout of its
	// own. Armed afterwards, the probe would hang forever on its headline case
	// and print nothing at all.
	obs := newSSEObserver(start)
	idleTimer := time.AfterFunc(cfg.idle, cancel)
	defer idleTimer.Stop()

	resp, err := client.Do(req)
	res.HeadersAt = time.Since(start)
	if err != nil {
		res.Err = err
		res.Total = time.Since(start)
		res.Stalled = true
		obs.finish(time.Now())
		res.Obs = obs.result()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	idleTimer.Reset(cfg.idle)

	buf := make([]byte, 8192)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			obs.observe(time.Now(), buf[:n])
			idleTimer.Reset(cfg.idle)
		}
		if readErr != nil {
			if readErr != io.EOF {
				res.Err = readErr
				// The read died because our own idle timer cancelled the
				// context — that is the stall, not a transport fault.
				if reqCtx.Err() != nil {
					res.Stalled = true
					res.Err = fmt.Errorf("idle for %s: %w", cfg.idle, readErr)
				}
			}
			break
		}
	}

	obs.finish(time.Now())
	res.Obs = obs.result()
	res.Total = time.Since(start)
	if res.Obs.Events == 0 {
		res.Stalled = true
	}
	return res
}

func printProbeReport(w io.Writer, results []probeResult, idle time.Duration) {
	fmt.Fprintf(w, "\n%-3s %-7s %-11s %-11s %-12s %-8s %-7s %-6s %-9s\n",
		"#", "status", "headers", "first-byte", "first-event", "max-gap", "events", "pings", "total")
	for _, r := range results {
		status := "ok"
		if r.Stalled {
			status = "STALL"
		}
		fmt.Fprintf(w, "%-3d %-7s %-11s %-11s %-12s %-8s %-7d %-6d %-9s\n",
			r.Index, status,
			dur(r.HeadersAt), dur(r.Obs.FirstByte), dur(r.Obs.FirstEvent), dur(r.Obs.MaxGap),
			r.Obs.Events, r.Obs.Pings, dur(r.Total))
		if r.Err != nil {
			fmt.Fprintf(w, "    err: %v\n", r.Err)
		}
		if r.Status != 0 && r.Status != http.StatusOK {
			fmt.Fprintf(w, "    http status: %d\n", r.Status)
		}
	}

	v := probeVerdict(results)
	fmt.Fprintf(w, "\n%d/%d stalled (idle window %s)\n", v.Stalled, v.Requests, idle)
	switch {
	case v.Stalled == 0:
		fmt.Fprintln(w, "No stall reproduced at this parallelism. Raise --parallel, or the cause is not the endpoint.")
	case v.Rejected:
		fmt.Fprintln(w, "The server rejected requests outright (see the http status lines above) rather than withholding output.")
		fmt.Fprintln(w, "A 429 here means the limit is explicit — check whether the provider retries it silently.")
	case v.ServerWithheld:
		fmt.Fprintln(w, "Server returned headers promptly, then sent no events: it accepted the request and withheld output.")
		fmt.Fprintln(w, "Compare against --parallel 1. If that is clean, the limit is concurrency-related.")
	case v.SlowHeaders:
		fmt.Fprintln(w, "Stalled requests never got response headers. Note the agent's idle watchdog cannot")
		fmt.Fprintln(w, "cause this failure — it only starts once headers are in — so a live run failing here is a different bug.")
	}
	if v.KeepalivesSeen {
		fmt.Fprintln(w, "Stalled requests WERE receiving ping keepalives: those connections were alive.")
		fmt.Fprintln(w, "The SDK discards pings before provider code sees them, so the watchdog reads them as silence.")
	}
}

func dur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(10 * time.Millisecond).String()
}
