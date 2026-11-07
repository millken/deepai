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
// P1 supports once and interval only. cron is parsed far enough to reject it
// with a helpful, stable error (docs/BOT_SCHEDULER_DESIGN.md §3.3).
type Trigger struct {
	Type    string `json:"type"`
	At      string `json:"at,omitempty"`       // once: absolute time or local datetime
	In      string `json:"in,omitempty"`       // once: relative duration, resolved at creation
	Every   string `json:"every,omitempty"`    // interval: period
	StartAt string `json:"start_at,omitempty"` // interval: first slot
	Expr    string `json:"expr,omitempty"`     // cron (P2)
}

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
		return NewError(CodeTriggerNotSupported,
			"cron triggers are not available yet; use once or interval instead (cron 将在后续版本提供，请改用 interval 或 once)")
	default:
		return NewError(CodeInvalidTrigger, "unknown trigger type %q (want once or interval)", t.Type)
	}
	return nil
}

// nextAfter returns the first planned slot strictly after `after`.
func (t Trigger) nextAfter(after time.Time) (time.Time, bool, error) {
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
	}
	return time.Time{}, false, nil
}

// NextRuns returns the next n slots after `after`, in UTC.
func (t Trigger) NextRuns(after time.Time, n int) []time.Time {
	if n <= 0 {
		n = 3
	}
	out := make([]time.Time, 0, n)
	cur := after
	for i := 0; i < n; i++ {
		slot, ok, err := t.nextAfter(cur)
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
		return fmt.Sprintf("cron %s（%s）", t.Expr, tz)
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
