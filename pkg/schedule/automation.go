package schedule

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	// MaxWebhookPayloadBytes bounds untrusted webhook payload injected into prompts.
	MaxWebhookPayloadBytes = 16 << 10
	// MaxChainPrevOutputBytes bounds upstream output injected into {{prev.output}}.
	MaxChainPrevOutputBytes = 8 << 10
)

// WebhookRequest carries an inbound HTTP webhook delivery for verification and
// execution (docs/BOT_NEXT_DESIGN.md §4.2).
type WebhookRequest struct {
	Headers http.Header
	Body    []byte
}

// WebhookResult describes the outcome of TriggerWebhook.
type WebhookResult struct {
	Run          *Run   `json:"run,omitempty"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
	Ignored      bool   `json:"ignored,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// ─────────────────────── NotifyIf (conditional notify) ───────────────────────

func normalizeNotifyIf(in *NotifyIf) (*NotifyIf, error) {
	if in == nil {
		return nil, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	pat := in.Pattern
	if mode == "" {
		mode = NotifyIfAll
	}
	switch mode {
	case NotifyIfAll:
		return &NotifyIf{Mode: NotifyIfAll}, nil
	case NotifyIfContains, NotifyIfNotContains:
		if strings.TrimSpace(pat) == "" {
			return nil, NewError(CodeBadRequest, "notify_if.%s requires a non-empty pattern", mode)
		}
		return &NotifyIf{Mode: mode, Pattern: pat}, nil
	case NotifyIfRegex:
		if strings.TrimSpace(pat) == "" {
			return nil, NewError(CodeBadRequest, "notify_if.regex requires a non-empty pattern")
		}
		if _, err := regexp.Compile(pat); err != nil {
			return nil, NewError(CodeBadRequest, "notify_if.regex invalid pattern: %v", err)
		}
		return &NotifyIf{Mode: mode, Pattern: pat}, nil
	case NotifyIfMarker:
		if strings.TrimSpace(pat) == "" {
			pat = DefaultNotifyMarker
		}
		return &NotifyIf{Mode: NotifyIfMarker, Pattern: strings.TrimSpace(pat)}, nil
	default:
		return nil, NewError(CodeBadRequest, "notify_if.mode %q invalid (want all, contains, not_contains, regex, or marker)", in.Mode)
	}
}

// ShouldNotify evaluates the base notify policy together with an optional
// NotifyIf output filter (docs/BOT_NEXT_DESIGN.md §4.4).
//
// Failures, timeouts, and missed runs still follow base notify policy (never
// silenced by NotifyIf, which only filters noisy "all-clear" completed outputs).
func ShouldNotify(notify string, notifyIf *NotifyIf, status RunStatus, output string) bool {
	if status == RunSkipped || notify == NotifyNever {
		return false
	}
	switch notify {
	case NotifyOnFailure:
		return status == RunFailed || status == RunTimedOut || status == RunMissed
	default: // NotifyAlways or empty
		if status != RunCompleted {
			return status == RunFailed || status == RunTimedOut || status == RunMissed || status == RunCancelled
		}
	}
	if notifyIf == nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(notifyIf.Mode)) {
	case "", NotifyIfAll:
		return true
	case NotifyIfContains:
		return strings.Contains(output, notifyIf.Pattern)
	case NotifyIfNotContains:
		return !strings.Contains(output, notifyIf.Pattern)
	case NotifyIfRegex:
		re, err := regexp.Compile(notifyIf.Pattern)
		if err != nil {
			return true
		}
		return re.MatchString(output)
	case NotifyIfMarker:
		marker := strings.TrimSpace(notifyIf.Pattern)
		if marker == "" {
			marker = DefaultNotifyMarker
		}
		return !strings.HasPrefix(strings.TrimSpace(output), marker)
	default:
		return true
	}
}

// ─────────────────────── Inbound Webhook Trigger ───────────────────────

// TriggerWebhook authenticates, filters, deduplicates, rate-limits, renders,
// and dispatches an inbound webhook delivery for scheduleID.
func (s *Service) TriggerWebhook(ctx context.Context, scheduleID string, req WebhookRequest) (WebhookResult, error) {
	// Pre-check and verify signature BEFORE acquiring s.mu or inspecting
	// schedule state, so unauthenticated requests neither contend on the
	// scheduler mutex nor learn whether a schedule is paused/pending.
	pre, err := s.store.GetSchedule(ctx, scheduleID)
	if err != nil {
		return WebhookResult{}, notFound(err, "schedule "+scheduleID)
	}
	if pre.State == StateDeleted {
		return WebhookResult{}, NewError(CodeNotFound, "schedule %s not found", scheduleID)
	}
	if pre.Trigger.Type != TriggerWebhook {
		return WebhookResult{}, NewError(CodeNotFound, "schedule %s is not a webhook trigger", scheduleID)
	}
	if err := verifyWebhookSignature(pre.Trigger, req.Headers, req.Body); err != nil {
		return WebhookResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	v, err := s.store.GetSchedule(ctx, scheduleID)
	if err != nil {
		return WebhookResult{}, notFound(err, "schedule "+scheduleID)
	}
	if v.State == StateDeleted {
		return WebhookResult{}, NewError(CodeNotFound, "schedule %s not found", scheduleID)
	}
	if v.Trigger.Type != TriggerWebhook {
		return WebhookResult{}, NewError(CodeNotFound, "schedule %s is not a webhook trigger", scheduleID)
	}
	if v.Trigger.Secret != pre.Trigger.Secret || v.Trigger.SecretRef != pre.Trigger.SecretRef ||
		v.Trigger.SignatureScheme != pre.Trigger.SignatureScheme || v.Trigger.SignatureHeader != pre.Trigger.SignatureHeader {
		if err := verifyWebhookSignature(v.Trigger, req.Headers, req.Body); err != nil {
			return WebhookResult{}, err
		}
	}
	if v.State != StateActive {
		return WebhookResult{}, NewError(CodeInvalidState, "schedule %s is %s", scheduleID, v.State)
	}

	var payloadMap map[string]any
	if len(bytes.TrimSpace(req.Body)) > 0 {
		_ = json.Unmarshal(req.Body, &payloadMap)
	}

	if !matchWebhookFilter(v.Trigger.EventFilter, req.Headers, payloadMap) {
		return WebhookResult{Ignored: true, Reason: "event_filter"}, nil
	}

	// Dedup BEFORE rate-limiting so retried identical deliveries stay idempotent.
	var requestID string
	if v.Trigger.DedupHeader != "" && req.Headers != nil {
		if deliv := strings.TrimSpace(headerGet(req.Headers, v.Trigger.DedupHeader)); deliv != "" {
			requestID = "wh:" + deliv
			if prev, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: scheduleID, RequestID: requestID, Limit: 1}); len(prev) > 0 {
				cp := prev[0]
				return WebhookResult{Run: &cp, Deduplicated: true}, nil
			}
		}
	}

	now := s.now()
	if !s.allowWebhookRateLocked(scheduleID, v.Trigger.RateLimitPerMin, now) {
		return WebhookResult{}, NewError(CodeLimitExceeded, "webhook rate limit (%d/min) exceeded for schedule %s", v.Trigger.RateLimitPerMin, scheduleID)
	}
	if err := s.checkDailyLimitLocked(ctx, scheduleID); err != nil {
		return WebhookResult{}, err
	}

	renderedPrompt := renderWebhookPrompt(v.Prompt, v.Trigger, req.Headers, req.Body, payloadMap)
	r := Run{
		ID:             newID("run_"),
		ScheduleID:     scheduleID,
		Seq:            s.nextSeqLocked(ctx, scheduleID),
		Trigger:        FireWebhook,
		ScheduledFor:   now,
		RequestID:      requestID,
		Status:         RunPending,
		PromptOverride: renderedPrompt,
	}

	if active := s.activeRunsLocked(ctx, scheduleID); len(active) > 0 {
		switch v.Options.Overlap {
		case OverlapQueue, OverlapReplace:
			r.QueuedReason = "overlap"
			r.QueueDeadline = timePtr(now.Add(s.timeoutOf(v)))
			if v.Options.Overlap == OverlapReplace && s.dispatcher != nil {
				_ = s.dispatcher.Cancel(active[0], CancelReplaced)
			}
		default:
			return WebhookResult{}, NewError(CodeRunInProgress, "schedule %s already has a run in progress", scheduleID)
		}
	}

	if err := s.insertRunLocked(ctx, r); err != nil {
		if errors.Is(err, ErrConflict) && requestID != "" {
			if prev, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: scheduleID, RequestID: requestID, Limit: 1}); len(prev) > 0 {
				cp := prev[0]
				return WebhookResult{Run: &cp, Deduplicated: true}, nil
			}
		}
		return WebhookResult{}, err
	}
	s.tryStartLocked(ctx, r, v)
	s.kickLocked()
	got, err := s.store.GetRun(ctx, r.ID)
	if err != nil {
		return WebhookResult{Run: &r}, nil
	}
	return WebhookResult{Run: &got}, nil
}

func (s *Service) allowWebhookRateLocked(scheduleID string, limit int, now time.Time) bool {
	if limit <= 0 {
		limit = 5
	}
	if s.webhookHits == nil {
		s.webhookHits = map[string][]time.Time{}
	}
	cutoff := now.Add(-time.Minute)
	hits := s.webhookHits[scheduleID]
	kept := hits[:0]
	for _, ts := range hits {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		s.webhookHits[scheduleID] = kept
		return false
	}
	s.webhookHits[scheduleID] = append(kept, now)
	return true
}

func headerGet(h http.Header, name string) string {
	if h == nil || name == "" {
		return ""
	}
	if v := h.Get(name); v != "" {
		return v
	}
	for k, vals := range h {
		if strings.EqualFold(k, name) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func resolveWebhookSecret(tr Trigger) (string, error) {
	if strings.HasPrefix(tr.SecretRef, "env:") {
		envName := strings.TrimSpace(strings.TrimPrefix(tr.SecretRef, "env:"))
		val := os.Getenv(envName)
		if val == "" {
			return "", NewError(CodeUnauthorized, "webhook secret env %q is not set", envName)
		}
		return val, nil
	}
	if tr.Secret != "" {
		return tr.Secret, nil
	}
	return "", NewError(CodeUnauthorized, "webhook secret is not configured")
}

func verifyWebhookSignature(tr Trigger, headers http.Header, body []byte) error {
	secret, err := resolveWebhookSecret(tr)
	if err != nil {
		return err
	}
	hdrVal := ""
	if headers != nil {
		hdrVal = strings.TrimSpace(headerGet(headers, tr.SignatureHeader))
	}
	if hdrVal == "" {
		return NewError(CodeUnauthorized, "missing webhook signature header %s", tr.SignatureHeader)
	}
	switch tr.SignatureScheme {
	case WebhookSigBearer:
		got := hdrVal
		if strings.EqualFold(tr.SignatureHeader, "Authorization") {
			if !strings.HasPrefix(got, "Bearer ") {
				return NewError(CodeUnauthorized, "invalid bearer signature")
			}
			got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			return NewError(CodeUnauthorized, "invalid webhook bearer token")
		}
		return nil

	case WebhookSigGitHubSHA256, WebhookSigHMACSHA256:
		rawSig := hdrVal
		if tr.SignatureScheme == WebhookSigGitHubSHA256 {
			if !strings.HasPrefix(rawSig, "sha256=") {
				return NewError(CodeUnauthorized, "signature must have sha256= prefix")
			}
			rawSig = strings.TrimPrefix(rawSig, "sha256=")
		} else {
			rawSig = strings.TrimPrefix(rawSig, "sha256=")
		}
		gotMAC, err := hex.DecodeString(rawSig)
		if err != nil {
			return NewError(CodeUnauthorized, "malformed hex signature")
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if !hmac.Equal(gotMAC, mac.Sum(nil)) {
			return NewError(CodeUnauthorized, "webhook signature mismatch")
		}
		return nil

	default:
		return NewError(CodeUnauthorized, "unsupported signature scheme %q", tr.SignatureScheme)
	}
}

func matchWebhookFilter(filter map[string]string, headers http.Header, payload map[string]any) bool {
	for k, want := range filter {
		switch {
		case strings.HasPrefix(k, "header."):
			name := strings.TrimPrefix(k, "header.")
			got := ""
			if headers != nil {
				got = strings.TrimSpace(headerGet(headers, name))
			}
			if got != want {
				return false
			}
		case strings.HasPrefix(k, "payload."):
			dotPath := strings.TrimPrefix(k, "payload.")
			got, ok := lookupJSONPath(payload, dotPath)
			if !ok || got != want {
				return false
			}
		}
	}
	return true
}

func lookupJSONPath(obj map[string]any, dotPath string) (string, bool) {
	if obj == nil || dotPath == "" {
		return "", false
	}
	var cur any = obj
	for _, part := range strings.Split(dotPath, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[part]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case nil:
		return "", false
	default:
		return fmt.Sprint(v), true
	}
}

func validateWebhookPrompt(promptTmpl string) error {
	if !strings.Contains(promptTmpl, "{{") {
		return nil
	}
	if _, err := template.New("webhook").Funcs(template.FuncMap{}).Option("missingkey=zero").Parse(promptTmpl); err != nil {
		return NewError(CodeBadRequest, "invalid webhook prompt template: %v", err)
	}
	return nil
}

func renderWebhookPrompt(promptTmpl string, tr Trigger, headers http.Header, rawBody []byte, payload map[string]any) string {
	safeHeaders := map[string]string{}
	sigCanon := http.CanonicalHeaderKey(tr.SignatureHeader)
	for k, vals := range headers {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" || ck == sigCanon || strings.Contains(strings.ToLower(ck), "secret") || strings.Contains(strings.ToLower(ck), "token") {
			continue
		}
		if len(vals) > 0 {
			safeHeaders[ck] = truncateBytes(vals[0], 1024)
		}
	}
	eventName := safeHeaders["X-Github-Event"]
	if eventName == "" {
		eventName = safeHeaders["X-Event-Type"]
	}
	const maxFencePayloadBytes = MaxWebhookPayloadBytes - 256
	payloadStr := truncateBytes(string(bytes.TrimSpace(rawBody)), maxFencePayloadBytes)

	data := struct {
		Event       string
		Headers     map[string]string
		Payload     map[string]any
		PayloadJSON string
	}{
		Event:       eventName,
		Headers:     safeHeaders,
		Payload:     payload,
		PayloadJSON: payloadStr,
	}

	rendered := promptTmpl
	if strings.Contains(promptTmpl, "{{") {
		tmpl, err := template.New("webhook").Funcs(template.FuncMap{}).Option("missingkey=zero").Parse(promptTmpl)
		if err == nil {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err == nil {
				rendered = buf.String()
			}
		}
	}
	// Bound the rendered template output as well as the fenced payload so
	// {{.Payload}} / {{.Headers}} on large bodies cannot bypass MaxWebhookPayloadBytes.
	rendered = strings.TrimSpace(rendered)
	if len(rendered) > MaxWebhookPayloadBytes {
		const suffix = "...[truncated]"
		rendered = truncateBytes(rendered, MaxWebhookPayloadBytes-len(suffix)) + suffix
	}
	// Always wrap untrusted external payload in an explicit data fence.
	var fence bytes.Buffer
	fence.WriteString("\n\n<untrusted_webhook_payload")
	if eventName != "" {
		fence.WriteString(` event="`)
		fence.WriteString(strings.ReplaceAll(eventName, `"`, ""))
		fence.WriteString(`"`)
	}
	fence.WriteString(">\n")
	fence.WriteString(payloadStr)
	fence.WriteString("\n</untrusted_webhook_payload>")
	return rendered + fence.String()
}

// ─────────────────────── Task Chaining (A -> B) ───────────────────────

func (s *Service) validateChainLocked(ctx context.Context, selfID string, tr Trigger) error {
	if tr.Type != TriggerChain {
		return nil
	}
	upstreamID := tr.AfterScheduleID
	if selfID != "" && upstreamID == selfID {
		return NewError(CodeCycleDetected, "schedule %s cannot chain after itself", selfID)
	}
	upstream, err := s.store.GetSchedule(ctx, upstreamID)
	if err != nil || upstream.State == StateDeleted {
		return NewError(CodeNotFound, "upstream schedule %s not found", upstreamID)
	}
	if callerBot := strings.TrimSpace(BotIDFromContext(ctx)); callerBot != "" {
		if effectiveBotID(upstream.BotID) != effectiveBotID(callerBot) {
			return NewError(CodeNotFound, "upstream schedule %s not found", upstreamID)
		}
	}

	// Build adjacency map (child -> parent) across non-deleted schedules and
	// walk ancestors starting from upstreamID to verify selfID is not reached
	// and no cycle exists.
	all, err := s.store.ListSchedules(ctx, ScheduleFilter{WorkDir: s.cfg.WorkDir})
	if err != nil {
		return err
	}
	parentOf := make(map[string]string, len(all)+1)
	for _, item := range all {
		if item.State == StateDeleted {
			continue
		}
		if item.Trigger.Type == TriggerChain && item.Trigger.AfterScheduleID != "" {
			parentOf[item.ID] = item.Trigger.AfterScheduleID
		}
	}
	if selfID != "" {
		parentOf[selfID] = upstreamID
	}

	visited := map[string]bool{}
	if selfID != "" {
		visited[selfID] = true
	}
	cur := upstreamID
	for cur != "" {
		if visited[cur] {
			return NewError(CodeCycleDetected, "chain cycle detected involving schedule %s", cur)
		}
		visited[cur] = true
		cur = parentOf[cur]
	}
	return nil
}

func chainStatusMatches(onStatus string, status RunStatus) bool {
	switch onStatus {
	case ChainOnAny:
		return status == RunCompleted || status == RunFailed || status == RunTimedOut
	case ChainOnFailed:
		return status == RunFailed || status == RunTimedOut
	default: // ChainOnCompleted
		return status == RunCompleted
	}
}

func renderChainPrompt(prompt string, prev Run) string {
	prevOutput := truncateBytes(prev.Output, MaxChainPrevOutputBytes)
	repl := strings.NewReplacer(
		"{{prev.status}}", string(prev.Status),
		"{{prev.output}}", prevOutput,
		"{{prev.run_id}}", prev.ID,
		"{{prev.session_id}}", prev.SessionID,
		"{{prev.error}}", prev.Error,
	)
	return repl.Replace(prompt)
}

func (s *Service) triggerDownstreamChainsLocked(ctx context.Context, upstream Run) {
	if upstream.Status != RunCompleted && upstream.Status != RunFailed && upstream.Status != RunTimedOut {
		return
	}
	if upstream.ChainDepth >= MaxChainDepth {
		s.log.Warn("schedule: max chain depth reached; suppressing downstream trigger",
			"schedule", upstream.ScheduleID, "run", upstream.ID, "depth", upstream.ChainDepth)
		return
	}
	list, err := s.store.ListSchedules(ctx, ScheduleFilter{WorkDir: s.cfg.WorkDir, State: StateActive})
	if err != nil {
		s.log.Warn("schedule: list downstream chain schedules", "err", err)
		return
	}
	now := s.now()
	reqID := "chain:" + upstream.ID
	for _, ds := range list {
		if ds.Trigger.Type != TriggerChain || ds.Trigger.AfterScheduleID != upstream.ScheduleID {
			continue
		}
		if !chainStatusMatches(ds.Trigger.OnStatus, upstream.Status) {
			continue
		}
		if prev, _ := s.store.FindRun(ctx, RunFilter{ScheduleID: ds.ID, RequestID: reqID, Limit: 1}); len(prev) > 0 {
			continue
		}
		if err := s.checkDailyLimitLocked(ctx, ds.ID); err != nil {
			s.log.Warn("schedule: chain daily limit reached", "downstream", ds.ID, "err", err)
			continue
		}
		child := Run{
			ID:             newID("run_"),
			ScheduleID:     ds.ID,
			Seq:            s.nextSeqLocked(ctx, ds.ID),
			Trigger:        FireChain,
			ScheduledFor:   now,
			RequestID:      reqID,
			Status:         RunPending,
			PromptOverride: renderChainPrompt(ds.Prompt, upstream),
			ChainDepth:     upstream.ChainDepth + 1,
		}
		if active := s.activeRunsLocked(ctx, ds.ID); len(active) > 0 {
			switch ds.Options.Overlap {
			case OverlapQueue, OverlapReplace:
				child.QueuedReason = "overlap"
				child.QueueDeadline = timePtr(now.Add(s.timeoutOf(ds)))
				if ds.Options.Overlap == OverlapReplace && s.dispatcher != nil {
					_ = s.dispatcher.Cancel(active[0], CancelReplaced)
				}
			default:
				child.Status = RunSkipped
				child.QueuedReason = "overlap"
				child.EndedAt = &now
			}
		}
		if err := s.insertRunLocked(ctx, child); err != nil {
			s.log.Warn("schedule: insert chain run", "downstream", ds.ID, "err", err)
			continue
		}
		if child.Status == RunPending {
			s.tryStartLocked(ctx, child, ds)
		}
	}
}
