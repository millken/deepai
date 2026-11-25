package schedule

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

func signGitHub(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignatureFilterDedupRateLimitAndRender(t *testing.T) {
	ctx := context.Background()
	db := openTestSQLite(t)
	store := NewSQLiteStoreForDir(db, "/repo/a")
	clk := newFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	cfg := DefaultConfig()
	cfg.WorkDir = "/repo/a"
	cfg.Timezone = "UTC"

	svc, err := NewService(store, cfg, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	// No dispatcher attached so runs stay pending and can be inspected deterministically.

	// 1. Create webhook with auto-generated secret; verify secret is returned once and masked afterward.
	sch, err := svc.Create(ctx, CreateInput{
		Name:   "gh-pr",
		Prompt: "Review PR {{.Payload.pull_request.html_url}} on event {{.Event}}",
		Trigger: Trigger{
			Type: TriggerWebhook,
			EventFilter: map[string]string{
				"header.X-GitHub-Event": "pull_request",
				"payload.action":        "opened",
			},
			RateLimitPerMin: 2,
		},
	})
	if err != nil {
		t.Fatalf("Create webhook: %v", err)
	}
	if sch.WebhookSecret == "" || !sch.Trigger.HasSecret || sch.Trigger.Secret != "" {
		t.Fatalf("unexpected secret handling on Create: secret=%q has=%v trSecret=%q", sch.WebhookSecret, sch.Trigger.HasSecret, sch.Trigger.Secret)
	}
	got, err := svc.Get(ctx, sch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WebhookSecret != "" || got.Trigger.Secret != "" || !got.Trigger.HasSecret {
		t.Fatalf("secret leaked on Get: %+v", got)
	}

	body := []byte(`{"action":"opened","pull_request":{"html_url":"https://github.com/org/repo/pull/42"}}`)

	// 2. Missing / wrong signature -> CodeUnauthorized.
	if _, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Body: body}); ErrorCode(err) != CodeUnauthorized {
		t.Fatalf("missing sig err = %v, want unauthorized", err)
	}
	badHdr := http.Header{"X-Hub-Signature-256": {"sha256=0000"}}
	if _, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: badHdr, Body: body}); ErrorCode(err) != CodeUnauthorized {
		t.Fatalf("bad sig err = %v, want unauthorized", err)
	}

	// 3. Filter mismatch -> Ignored: true, no Run created.
	closedBody := []byte(`{"action":"closed"}`)
	hdrClosed := http.Header{
		"X-Hub-Signature-256": {signGitHub(sch.WebhookSecret, closedBody)},
		"X-GitHub-Event":      {"pull_request"},
		"X-GitHub-Delivery":   {"deliv-ignored"},
	}
	resIgn, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdrClosed, Body: closedBody})
	if err != nil || !resIgn.Ignored || resIgn.Run != nil {
		t.Fatalf("ignored webhook = (%+v, %v)", resIgn, err)
	}

	// 4. Valid signed delivery triggers Run with rendered prompt + untrusted fence.
	hdr1 := http.Header{
		"X-Hub-Signature-256": {signGitHub(sch.WebhookSecret, body)},
		"X-GitHub-Event":      {"pull_request"},
		"X-GitHub-Delivery":   {"deliv-1"},
	}
	res1, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdr1, Body: body})
	if err != nil || res1.Run == nil || res1.Deduplicated {
		t.Fatalf("first webhook = (%+v, %v)", res1, err)
	}
	if res1.Run.Trigger != FireWebhook || res1.Run.RequestID != "wh:deliv-1" {
		t.Fatalf("run metadata = %+v", res1.Run)
	}
	if !strings.Contains(res1.Run.PromptOverride, "https://github.com/org/repo/pull/42") ||
		!strings.Contains(res1.Run.PromptOverride, "on event pull_request") ||
		!strings.Contains(res1.Run.PromptOverride, "<untrusted_webhook_payload") {
		t.Fatalf("PromptOverride missing expected rendering/fence:\n%s", res1.Run.PromptOverride)
	}
	// Reload from SQLite to confirm PromptOverride persisted cleanly.
	reloaded, err := svc.GetRun(ctx, res1.Run.ID)
	if err != nil || reloaded.PromptOverride != res1.Run.PromptOverride {
		t.Fatalf("reloaded PromptOverride mismatch: %q vs %q (err=%v)", reloaded.PromptOverride, res1.Run.PromptOverride, err)
	}

	// 5. Duplicate delivery ID is idempotent and does not consume rate limit.
	resDup, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdr1, Body: body})
	if err != nil || !resDup.Deduplicated || resDup.Run == nil || resDup.Run.ID != res1.Run.ID {
		t.Fatalf("dedup webhook = (%+v, %v)", resDup, err)
	}

	// 6. Rate limit (2/min): 2nd unique delivery succeeds, 3rd returns CodeLimitExceeded,
	// advancing clock by 61s allows 4th delivery.
	hdr2 := hdr1.Clone()
	hdr2.Set("X-GitHub-Delivery", "deliv-2")
	if _, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdr2, Body: body}); err != nil {
		t.Fatalf("2nd delivery: %v", err)
	}
	hdr3 := hdr1.Clone()
	hdr3.Set("X-GitHub-Delivery", "deliv-3")
	if _, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdr3, Body: body}); ErrorCode(err) != CodeLimitExceeded {
		t.Fatalf("3rd delivery err = %v, want limit_exceeded", err)
	}
	clk.Advance(61 * time.Second)
	if res4, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{Headers: hdr3, Body: body}); err != nil || res4.Run == nil {
		t.Fatalf("delivery after rate window = (%+v, %v)", res4, err)
	}

	// 7. SecretRef env:<VAR> + bearer scheme verification.
	t.Setenv("TEST_DEEPAI_WH_TOKEN", "my-bearer-secret")
	schBearer, err := svc.Create(ctx, CreateInput{
		Name:   "alert-hook",
		Prompt: "Handle alert",
		Trigger: Trigger{
			Type:            TriggerWebhook,
			SecretRef:       "env:TEST_DEEPAI_WH_TOKEN",
			SignatureScheme: WebhookSigBearer,
		},
	})
	if err != nil {
		t.Fatalf("Create bearer webhook: %v", err)
	}
	if schBearer.WebhookSecret != "" {
		t.Fatalf("env secret_ref should not auto-generate WebhookSecret")
	}
	bHdr := http.Header{"Authorization": {"Bearer my-bearer-secret"}}
	if resB, err := svc.TriggerWebhook(ctx, schBearer.ID, WebhookRequest{Headers: bHdr, Body: []byte(`{"alert":"high"}`)}); err != nil || resB.Run == nil {
		t.Fatalf("bearer webhook = (%+v, %v)", resB, err)
	}
}

func TestTaskChainTriggerContextCycleDetectionAndMaxDepth(t *testing.T) {
	ctx := context.Background()
	db := openTestSQLite(t)
	store := NewSQLiteStoreForDir(db, "/repo/a")
	clk := newFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	cfg := DefaultConfig()
	cfg.WorkDir = "/repo/a"
	cfg.Timezone = "UTC"

	svc, err := NewService(store, cfg, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	// Use a manual-finish dispatcher that records fired runs.
	disp := &errDispatcher{}
	svc.SetDispatcher(disp)

	// Create A -> B (on completed) -> C (on failed).
	schA, err := svc.Create(ctx, CreateInput{
		Name:    "step-a",
		Prompt:  "collect metrics",
		Trigger: Trigger{Type: TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	schB, err := svc.Create(ctx, CreateInput{
		Name:    "step-b",
		Prompt:  "Review ({{prev.status}} / {{prev.run_id}}): {{prev.output}}",
		Trigger: Trigger{Type: TriggerChain, AfterScheduleID: schA.ID, OnStatus: ChainOnCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	schC, err := svc.Create(ctx, CreateInput{
		Name:    "step-c",
		Prompt:  "Escalate failure: {{prev.error}}",
		Trigger: Trigger{Type: TriggerChain, AfterScheduleID: schB.ID, OnStatus: ChainOnFailed},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cycle detection: A -> C creates A -> B -> C -> A cycle -> CodeCycleDetected.
	trCycle := Trigger{Type: TriggerChain, AfterScheduleID: schC.ID}
	if _, err := svc.Update(ctx, schA.ID, Patch{Trigger: &trCycle}); ErrorCode(err) != CodeCycleDetected {
		t.Fatalf("cycle update err = %v, want cycle_detected", err)
	}
	trSelf := Trigger{Type: TriggerChain, AfterScheduleID: schB.ID}
	if _, err := svc.Update(ctx, schB.ID, Patch{Trigger: &trSelf}); ErrorCode(err) != CodeCycleDetected {
		t.Fatalf("self-cycle update err = %v, want cycle_detected", err)
	}

	// Fire A and finish as completed -> B should trigger with rendered {{prev.*}} and ChainDepth=1.
	runA, err := svc.RunNow(ctx, schA.ID, "req-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinishRun(ctx, runA.ID, Outcome{Status: RunCompleted, Output: "cpu=42%"}); err != nil {
		t.Fatal(err)
	}

	runsB, err := svc.Runs(ctx, schB.ID, 5)
	if err != nil || len(runsB) != 1 {
		t.Fatalf("runsB = %+v, err=%v; want 1 chained run", runsB, err)
	}
	runB := runsB[0]
	if runB.Trigger != FireChain || runB.ChainDepth != 1 || runB.RequestID != "chain:"+runA.ID {
		t.Fatalf("unexpected runB: %+v", runB)
	}
	wantPromptB := "Review (completed / " + runA.ID + "): cpu=42%"
	if runB.PromptOverride != wantPromptB {
		t.Fatalf("runB.PromptOverride = %q, want %q", runB.PromptOverride, wantPromptB)
	}

	// Finish B as failed -> C triggers with ChainDepth=2.
	if _, err := svc.FinishRun(ctx, runB.ID, Outcome{Status: RunFailed, Error: "audit failed"}); err != nil {
		t.Fatal(err)
	}
	runsC, err := svc.Runs(ctx, schC.ID, 5)
	if err != nil || len(runsC) != 1 || runsC[0].ChainDepth != 2 {
		t.Fatalf("runsC = %+v, err=%v; want 1 run with depth=2", runsC, err)
	}
	if runsC[0].PromptOverride != "Escalate failure: audit failed" {
		t.Fatalf("runC.PromptOverride = %q", runsC[0].PromptOverride)
	}

	// MaxChainDepth cap: a run already at depth 5 must NOT cascade further.
	deepRun := Run{
		ID: newID("run_"), ScheduleID: schA.ID, Seq: 99,
		Trigger: FireChain, ScheduledFor: clk.Now(), Status: RunRunning, ChainDepth: MaxChainDepth,
	}
	if err := store.InsertRun(ctx, deepRun); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinishRun(ctx, deepRun.ID, Outcome{Status: RunCompleted, Output: "too deep"}); err != nil {
		t.Fatal(err)
	}
	runsBAfter, _ := svc.Runs(ctx, schB.ID, 10)
	if len(runsBAfter) != 1 {
		t.Fatalf("MaxChainDepth did not stop cascade: runsB count = %d, want 1", len(runsBAfter))
	}
}

func TestNotifyIfPolicies(t *testing.T) {
	cases := []struct {
		name     string
		notify   string
		notifyIf *NotifyIf
		status   RunStatus
		output   string
		want     bool
	}{
		{"marker suppresses all-clear", NotifyAlways, &NotifyIf{Mode: NotifyIfMarker}, RunCompleted, "[NO_NOTIFY] nothing to report", false},
		{"marker notifies alert", NotifyAlways, &NotifyIf{Mode: NotifyIfMarker}, RunCompleted, "ERROR: 500 spike detected", true},
		{"marker still notifies failure", NotifyAlways, &NotifyIf{Mode: NotifyIfMarker}, RunFailed, "[NO_NOTIFY] ignored on fail", true},
		{"contains match", NotifyAlways, &NotifyIf{Mode: NotifyIfContains, Pattern: "CRITICAL"}, RunCompleted, "found CRITICAL error", true},
		{"contains miss", NotifyAlways, &NotifyIf{Mode: NotifyIfContains, Pattern: "CRITICAL"}, RunCompleted, "all healthy", false},
		{"not_contains match", NotifyAlways, &NotifyIf{Mode: NotifyIfNotContains, Pattern: "OK"}, RunCompleted, "FAILED", true},
		{"not_contains miss", NotifyAlways, &NotifyIf{Mode: NotifyIfNotContains, Pattern: "OK"}, RunCompleted, "status: OK", false},
		{"regex match", NotifyAlways, &NotifyIf{Mode: NotifyIfRegex, Pattern: `error_count=[1-9]\d*`}, RunCompleted, "summary error_count=12", true},
		{"regex miss", NotifyAlways, &NotifyIf{Mode: NotifyIfRegex, Pattern: `error_count=[1-9]\d*`}, RunCompleted, "summary error_count=0", false},
		{"notify never overrides", NotifyNever, &NotifyIf{Mode: NotifyIfContains, Pattern: "ALERT"}, RunCompleted, "ALERT", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldNotify(tc.notify, tc.notifyIf, tc.status, tc.output); got != tc.want {
				t.Fatalf("ShouldNotify = %v, want %v", got, tc.want)
			}
		})
	}

	// Validation rejects bad regex and missing pattern.
	if _, err := normalizeNotifyIf(&NotifyIf{Mode: NotifyIfRegex, Pattern: "["}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if _, err := normalizeNotifyIf(&NotifyIf{Mode: NotifyIfContains}); err == nil {
		t.Fatal("expected error for empty contains pattern")
	}
}

func TestWebhookPromptTruncationTemplateValidationAndAuthOrder(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(NewMemoryStore(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// 60-L1: invalid template rejected on Create.
	if _, err := svc.Create(ctx, CreateInput{
		Name:    "bad-tmpl",
		Prompt:  "Review {{.Payload.body",
		Trigger: Trigger{Type: TriggerWebhook, SignatureScheme: WebhookSigBearer, SignatureHeader: "Authorization"},
	}); err == nil || ErrorCode(err) != CodeBadRequest {
		t.Fatalf("Create invalid template err = %v, want CodeBadRequest", err)
	}

	// 60-L2: bare "*" tool_allowlist rejected in service normalizeOptions.
	if _, err := svc.Create(ctx, CreateInput{
		Name:    "bad-allowlist",
		Prompt:  "hello",
		Trigger: Trigger{Type: TriggerInterval, Every: "1h"},
		Options: Options{ToolAllowlist: []string{"*"}},
	}); err == nil || ErrorCode(err) != CodeBadRequest {
		t.Fatalf("Create bare wildcard allowlist err = %v, want CodeBadRequest", err)
	}

	// 60-M1: large 300 KiB payload expanded via {{.Payload.body}} stays strictly bounded.
	sch, err := svc.Create(ctx, CreateInput{
		Name:    "wh-cap",
		Prompt:  "Inspect: {{.Payload.body}}",
		Trigger: Trigger{Type: TriggerWebhook, SignatureScheme: WebhookSigBearer, SignatureHeader: "Authorization"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hugeBody := []byte(`{"body":"` + strings.Repeat("A", 300<<10) + `"}`)
	res, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{
		Headers: http.Header{"Authorization": {"Bearer " + sch.WebhookSecret}},
		Body:    hugeBody,
	})
	if err != nil || res.Run == nil {
		t.Fatalf("TriggerWebhook = (%+v, %v)", res, err)
	}
	if len(res.Run.PromptOverride) > 2*MaxWebhookPayloadBytes {
		t.Fatalf("PromptOverride len = %d, exceeds 2*MaxWebhookPayloadBytes (%d)", len(res.Run.PromptOverride), 2*MaxWebhookPayloadBytes)
	}

	// 60-L1: invalid template rejected on Update.
	badPrompt := "Broken {{.Event"
	if _, err := svc.Update(ctx, sch.ID, Patch{Prompt: &badPrompt}); err == nil || ErrorCode(err) != CodeBadRequest {
		t.Fatalf("Update invalid template err = %v, want CodeBadRequest", err)
	}

	// 60-L2: paused webhook must return CodeUnauthorized (not CodeInvalidState) for bad auth.
	if _, err := svc.Pause(ctx, sch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TriggerWebhook(ctx, sch.ID, WebhookRequest{
		Headers: http.Header{"Authorization": {"Bearer wrong"}},
		Body:    []byte(`{}`),
	}); err == nil || ErrorCode(err) != CodeUnauthorized {
		t.Fatalf("unauthenticated paused webhook err = %v, want CodeUnauthorized", err)
	}
}

func TestChainRejectsCrossBotUpstreamFromBotContext(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(NewMemoryStore(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	secretSch, err := svc.Create(ctx, CreateInput{
		BotID:   "secret",
		Name:    "secret-job",
		Prompt:  "do secret stuff",
		Trigger: Trigger{Type: TriggerInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	revCtx := WithBotID(ctx, "reviewer")
	if _, err := svc.Create(revCtx, CreateInput{
		BotID:   "reviewer",
		Name:    "steal-output",
		Prompt:  "leak {{prev.output}}",
		Trigger: Trigger{Type: TriggerChain, AfterScheduleID: secretSch.ID},
	}); err == nil || ErrorCode(err) != CodeNotFound {
		t.Fatalf("cross-bot chain err = %v, want CodeNotFound", err)
	}
}
