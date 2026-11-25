package schedule

import (
	"fmt"
	"strings"
	"time"
)

// Trigger describes when a task fires. It is stored as a JSON object on the
// Schedule. Time strings without an offset are interpreted in the Schedule's
// timezone; Normalize rewrites them to canonical UTC RFC3339 before storage.
//
// Supports once, interval and a well-defined 5-field cron subset
// (docs/BOT_SCHEDULER_DESIGN.md §3.3, §5).
type Trigger struct {
	Type    string `json:"type"`
	At      string `json:"at,omitempty"`       // once: absolute time or local datetime
	In      string `json:"in,omitempty"`       // once: relative duration, resolved at creation
	Every   string `json:"every,omitempty"`    // interval: period
	StartAt string `json:"start_at,omitempty"` // interval: first slot
	Expr    string `json:"expr,omitempty"`     // cron: 5-field expression or macro

	// Webhook trigger fields (docs/BOT_NEXT_DESIGN.md §4.2).
	SecretRef       string            `json:"secret_ref,omitempty"`
	Secret          string            `json:"secret,omitempty"` // persisted auto-generated secret; masked by Decorate/API
	HasSecret       bool              `json:"has_secret,omitempty"`
	SignatureHeader string            `json:"signature_header,omitempty"`
	SignatureScheme string            `json:"signature_scheme,omitempty"`
	DedupHeader     string            `json:"dedup_header,omitempty"`
	EventFilter     map[string]string `json:"event_filter,omitempty"`
	RateLimitPerMin int               `json:"rate_limit_per_min,omitempty"`

	// Chain trigger fields (docs/BOT_NEXT_DESIGN.md §4.3).
	AfterScheduleID string `json:"after_schedule_id,omitempty"`
	OnStatus        string `json:"on_status,omitempty"` // completed (default) | failed | any
}

// IsEventDriven reports whether the trigger is fired by external events or
// upstream runs rather than a wall-clock slot.
func (t Trigger) IsEventDriven() bool { return t.Type == TriggerWebhook || t.Type == TriggerChain }

// IsPeriodic reports whether the trigger fires more than once.
func (t Trigger) IsPeriodic() bool { return t.Type == TriggerInterval || t.Type == TriggerCron }

// Normalize validates the rule and rewrites it into a canonical, persisted
// form. allowPast is false only at creation time: a stored rule is allowed
// to be historical (it will be recovered as missed on restart).
func (t *Trigger) Normalize(tz string, now time.Time, minInterval time.Duration, allowPast bool) error {
	loc, err := Location(tz)
	if err != nil {
		return err
	}
	typ := strings.ToLower(strings.TrimSpace(t.Type))
	switch typ {
	case TriggerOnce:
		if t.In != "" && t.At != "" {
			return NewError(CodeInvalidTrigger, "once: pass either 'at' or 'in', not both")
		}
		var at time.Time
		if t.In != "" {
			d, err := parsePositiveDuration(t.In)
			if err != nil {
				return NewError(CodeInvalidTrigger, "once.in %q: %v", t.In, err)
			}
			at = now.Add(d)
		} else if strings.TrimSpace(t.At) != "" {
			at, err = parseTimeSpec(t.At, loc)
			if err != nil {
				return NewError(CodeInvalidTrigger, "once.at %q: %v", t.At, err)
			}
		} else {
			return NewError(CodeInvalidTrigger, "once requires 'at' or 'in'")
		}
		if !allowPast && at.Before(now.Add(-time.Minute)) {
			return NewError(CodeTimeInPast, "once.at is in the past (%s); choose a future time or use 'in'", at.Format(time.RFC3339))
		}
		t.Type = TriggerOnce
		t.At = at.UTC().Format(time.RFC3339)
		t.In, t.Every, t.StartAt, t.Expr = "", "", "", ""

	case TriggerInterval:
		every, err := parsePositiveDuration(t.Every)
		if err != nil {
			return NewError(CodeInvalidTrigger, "interval.every %q: %v", t.Every, err)
		}
		if minInterval <= 0 {
			minInterval = DefaultConfig().MinInterval
		}
		if every < minInterval {
			return NewError(CodeIntervalTooShort, "interval.every %s is below the %s minimum", every, minInterval)
		}
		start := now.Add(every)
		if strings.TrimSpace(t.StartAt) != "" {
			start, err = parseTimeSpec(t.StartAt, loc)
			if err != nil {
				return NewError(CodeInvalidTrigger, "interval.start_at %q: %v", t.StartAt, err)
			}
		}
		t.Type = TriggerInterval
		t.Every = every.String()
		t.StartAt = start.UTC().Format(time.RFC3339)
		t.At, t.In, t.Expr = "", "", ""

	case TriggerCron:
		spec, err := ParseCron(t.Expr)
		if err != nil {
			return err
		}
		if minInterval <= 0 {
			minInterval = DefaultConfig().MinInterval
		}
		// Ensure the expression actually matches within 5 years and respects
		// min_interval across the next 10 occurrences (design §3.3, §5).
		first, ok := spec.Next(now, loc)
		if !ok {
			return NewError(CodeInvalidCron, "cron %q has no matching time within the next 5 years", t.Expr)
		}
		prev := first
		for i := 1; i < 10; i++ {
			next, ok := spec.Next(prev, loc)
			if !ok {
				break
			}
			if gap := next.Sub(prev); gap < minInterval {
				return NewError(CodeIntervalTooShort, "cron %q fires %s apart, below the %s minimum", t.Expr, gap, minInterval)
			}
			prev = next
		}
		t.Type = TriggerCron
		t.Expr = spec.expr
		t.At, t.In, t.Every, t.StartAt = "", "", "", ""
	case TriggerWebhook:
		t.Type = TriggerWebhook
		t.At, t.In, t.Every, t.StartAt, t.Expr = "", "", "", "", ""
		t.AfterScheduleID, t.OnStatus = "", ""
		t.SecretRef = strings.TrimSpace(t.SecretRef)
		if t.SecretRef != "" && !strings.HasPrefix(t.SecretRef, "env:") {
			return NewError(CodeInvalidTrigger, "webhook.secret_ref must be empty (auto-generate) or use 'env:<VAR>'")
		}
		if t.SecretRef != "" && strings.TrimSpace(strings.TrimPrefix(t.SecretRef, "env:")) == "" {
			return NewError(CodeInvalidTrigger, "webhook.secret_ref env var name is empty")
		}
		scheme := strings.ToLower(strings.TrimSpace(t.SignatureScheme))
		if scheme == "" {
			scheme = WebhookSigGitHubSHA256
		}
		switch scheme {
		case WebhookSigGitHubSHA256, WebhookSigHMACSHA256, WebhookSigBearer:
			t.SignatureScheme = scheme
		default:
			return NewError(CodeInvalidTrigger, "webhook.signature_scheme %q invalid (want github_sha256, hmac_sha256, or bearer)", t.SignatureScheme)
		}
		t.SignatureHeader = strings.TrimSpace(t.SignatureHeader)
		if t.SignatureHeader == "" {
			switch scheme {
			case WebhookSigGitHubSHA256:
				t.SignatureHeader = "X-Hub-Signature-256"
			case WebhookSigHMACSHA256:
				t.SignatureHeader = "X-Signature-256"
			case WebhookSigBearer:
				t.SignatureHeader = "Authorization"
			}
		}
		t.DedupHeader = strings.TrimSpace(t.DedupHeader)
		if t.DedupHeader == "" && scheme == WebhookSigGitHubSHA256 {
			t.DedupHeader = "X-GitHub-Delivery"
		}
		if t.RateLimitPerMin <= 0 {
			t.RateLimitPerMin = 5
		}
		if t.RateLimitPerMin > 60 {
			return NewError(CodeInvalidTrigger, "webhook.rate_limit_per_min %d exceeds maximum 60", t.RateLimitPerMin)
		}
		for k := range t.EventFilter {
			if !strings.HasPrefix(k, "header.") && !strings.HasPrefix(k, "payload.") {
				return NewError(CodeInvalidTrigger, "webhook.event_filter key %q must start with 'header.' or 'payload.'", k)
			}
		}
	case TriggerChain:
		t.Type = TriggerChain
		t.At, t.In, t.Every, t.StartAt, t.Expr = "", "", "", "", ""
		t.SecretRef, t.Secret, t.SignatureHeader, t.SignatureScheme, t.DedupHeader = "", "", "", "", ""
		t.EventFilter, t.RateLimitPerMin = nil, 0
		t.AfterScheduleID = strings.TrimSpace(t.AfterScheduleID)
		if t.AfterScheduleID == "" {
			return NewError(CodeInvalidTrigger, "chain requires after_schedule_id")
		}
		on := strings.ToLower(strings.TrimSpace(t.OnStatus))
		if on == "" {
			on = ChainOnCompleted
		}
		switch on {
		case ChainOnCompleted, ChainOnFailed, ChainOnAny:
			t.OnStatus = on
		default:
			return NewError(CodeInvalidTrigger, "chain.on_status %q invalid (want completed, failed, or any)", t.OnStatus)
		}
	default:
		return NewError(CodeInvalidTrigger, "unknown trigger type %q (want once, interval, cron, webhook, or chain)", t.Type)
	}
	return nil
}

// nextAfter returns the first planned slot strictly after `after`.
// tz is used for cron matching; once and interval are already UTC.
func (t Trigger) nextAfter(after time.Time, tz string) (time.Time, bool, error) {
	switch t.Type {
	case TriggerOnce:
		at, err := time.Parse(time.RFC3339Nano, t.At)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse once.at %q: %w", t.At, err)
		}
		if !at.After(after) {
			return time.Time{}, false, nil
		}
		return at.UTC(), true, nil
	case TriggerInterval:
		every, err := time.ParseDuration(t.Every)
		if err != nil || every <= 0 {
			return time.Time{}, false, fmt.Errorf("parse interval.every %q: %v", t.Every, err)
		}
		start, err := time.Parse(time.RFC3339Nano, t.StartAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse interval.start_at %q: %w", t.StartAt, err)
		}
		if start.After(after) {
			return start.UTC(), true, nil
		}
		steps := after.Sub(start)/every + 1
		if steps > 1<<40 {
			return time.Time{}, false, nil
		}
		slot := start.Add(time.Duration(steps) * every)
		if !slot.After(after) { // guard against duration overflow
			return time.Time{}, false, nil
		}
		return slot.UTC(), true, nil
	case TriggerCron:
		spec, err := ParseCron(t.Expr)
		if err != nil {
			return time.Time{}, false, err
		}
		loc, err := Location(tz)
		if err != nil {
			loc = time.UTC
		}
		slot, ok := spec.Next(after, loc)
		return slot, ok, nil
	}
	return time.Time{}, false, nil
}

// NextRuns returns the next n slots after `after`, in UTC.
// Pass optional timezone for cron rules (defaults to UTC if empty).
func (t Trigger) NextRuns(after time.Time, n int, tz ...string) []time.Time {
	if n <= 0 {
		n = 3
	}
	zone := ""
	if len(tz) > 0 {
		zone = tz[0]
	}
	out := make([]time.Time, 0, n)
	cur := after
	for i := 0; i < n; i++ {
		slot, ok, err := t.nextAfter(cur, zone)
		if err != nil || !ok {
			break
		}
		out = append(out, slot)
		cur = slot
	}
	return out
}

// Summary is the human-readable rule shown in confirmations and tool output.
func (t Trigger) Summary(tz string) string {
	loc, err := Location(tz)
	if err != nil {
		loc = time.UTC
	}
	local := func(s string) string {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return s
		}
		return v.In(loc).Format("2006-01-02 15:04")
	}
	switch t.Type {
	case TriggerOnce:
		return fmt.Sprintf("一次性任务：%s（%s）", local(t.At), tz)
	case TriggerInterval:
		return fmt.Sprintf("每 %s，从 %s 开始（%s）", t.Every, local(t.StartAt), tz)
	case TriggerCron:
		if spec, err := ParseCron(t.Expr); err == nil {
			return spec.HumanSummary(tz)
		}
		return fmt.Sprintf("cron %s（%s）", t.Expr, tz)
	case TriggerWebhook:
		return fmt.Sprintf("Webhook 触发（%s，限流 %d/min）", t.SignatureScheme, t.RateLimitPerMin)
	case TriggerChain:
		return fmt.Sprintf("任务链：在 %s %s 后触发", t.AfterScheduleID, t.OnStatus)
	}
	return "未知触发规则"
}

func parsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}

// parseTimeSpec accepts RFC3339 (with an offset) and a few local datetime
// forms; the latter are interpreted in loc, so a task's timezone is the only
// place that decides what "22:00" means.
func parseTimeSpec(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return v.UTC(), nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if v, err := time.ParseInLocation(layout, s, loc); err == nil {
			return v.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("want RFC3339 or YYYY-MM-DDTHH:MM[:SS]")
}
