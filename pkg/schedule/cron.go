package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec is a parsed 5-field cron expression (minute hour dom month dow).
// Only the documented subset is accepted (docs/BOT_SCHEDULER_DESIGN.md §5):
//   - 5 space-separated fields, or macros @hourly/@daily/@weekly/@monthly/@yearly
//   - *, n, a-b, a,b,c, */s, a-b/s
//   - DOM supports L (last day of month)
//   - Month supports JAN–DEC; DOW supports 0–7 (0 and 7 = Sunday) and SUN–SAT
//   - Rejects seconds/year fields, ?, W, #, @reboot, @every
type CronSpec struct {
	expr    string
	minutes uint64 // bits 0..59
	hours   uint32 // bits 0..23
	dom     uint32 // bits 1..31
	domLast bool   // L in DOM
	domStar bool   // DOM field was literally "*"
	months  uint16 // bits 1..12
	dow     uint8  // bits 0..6 (0 = Sunday)
	dowStar bool   // DOW field was literally "*"
}

var cronMacros = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 1", // Monday 00:00 per design §5
	"@monthly": "0 0 1 * *",
	"@yearly":  "0 0 1 1 *",
}

var cronMonthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var cronDOWNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// ParseCron parses a 5-field cron expression or supported macro.
func ParseCron(expr string) (*CronSpec, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, NewError(CodeInvalidCron, "cron expression is empty")
	}
	if strings.HasPrefix(raw, "@") {
		low := strings.ToLower(raw)
		if low == "@reboot" || strings.HasPrefix(low, "@every") {
			return nil, NewError(CodeInvalidCron, "cron macro %q is not supported; use interval trigger instead", raw)
		}
		expanded, ok := cronMacros[low]
		if !ok {
			return nil, NewError(CodeInvalidCron, "unknown cron macro %q (supported: @hourly, @daily, @weekly, @monthly, @yearly)", raw)
		}
		raw = expanded
	}
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, NewError(CodeInvalidCron, "cron expression must have 5 fields (minute hour day month weekday), got %d; seconds/year fields are not supported", len(fields))
	}
	for i, f := range fields {
		if strings.ContainsAny(f, "?#") {
			return nil, NewError(CodeInvalidCron, "cron token %q uses unsupported syntax (?, W, # are not supported)", f)
		}
		// Reject 'W' modifier in any field except the legitimate weekday name "WED" in DOW.
		if strings.ContainsAny(strings.ToUpper(f), "W") && !(i == 4 && isWeekdayOnlyW(f)) {
			return nil, NewError(CodeInvalidCron, "cron token %q uses unsupported syntax (?, W, # are not supported)", f)
		}
	}

	minBits, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, NewError(CodeInvalidCron, "cron minute %q: %v", fields[0], err)
	}
	hourBits, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, NewError(CodeInvalidCron, "cron hour %q: %v", fields[1], err)
	}
	domBits, domLast, err := parseDOMField(fields[2])
	if err != nil {
		return nil, NewError(CodeInvalidCron, "cron day-of-month %q: %v", fields[2], err)
	}
	monthBits, err := parseCronField(fields[3], 1, 12, cronMonthNames)
	if err != nil {
		return nil, NewError(CodeInvalidCron, "cron month %q: %v", fields[3], err)
	}
	dowBits, err := parseDOWField(fields[4])
	if err != nil {
		return nil, NewError(CodeInvalidCron, "cron weekday %q: %v", fields[4], err)
	}

	return &CronSpec{
		expr:    strings.Join(fields, " "),
		minutes: minBits,
		hours:   uint32(hourBits),
		dom:     uint32(domBits),
		domLast: domLast,
		domStar: fields[2] == "*",
		months:  uint16(monthBits),
		dow:     uint8(dowBits),
		dowStar: fields[4] == "*",
	}, nil
}

// isWeekdayOnlyW reports whether every 'W' in a DOW field belongs to "WED".
func isWeekdayOnlyW(f string) bool {
	cleaned := strings.ReplaceAll(strings.ToUpper(f), "WED", "")
	return !strings.ContainsRune(cleaned, 'W')
}

func parseDOMField(field string) (uint64, bool, error) {
	var bits uint64
	var last bool
	for _, part := range strings.Split(field, ",") {
		if strings.EqualFold(part, "L") {
			last = true
			continue
		}
		b, err := parseCronPart(part, 1, 31, nil)
		if err != nil {
			return 0, false, err
		}
		bits |= b
	}
	if bits == 0 && !last {
		return 0, false, fmt.Errorf("empty day-of-month field")
	}
	return bits, last, nil
}

func parseDOWField(field string) (uint64, error) {
	raw, err := parseCronField(field, 0, 7, cronDOWNames)
	if err != nil {
		return 0, err
	}
	// Normalize 7 (Sunday) to 0.
	if raw&(1<<7) != 0 {
		raw |= 1 << 0
		raw &^= 1 << 7
	}
	return raw, nil
}

func parseCronField(field string, minVal, maxVal int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		b, err := parseCronPart(part, minVal, maxVal, names)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	if bits == 0 {
		return 0, fmt.Errorf("empty field")
	}
	return bits, nil
}

func parseCronPart(part string, minVal, maxVal int, names map[string]int) (uint64, error) {
	if part == "" {
		return 0, fmt.Errorf("empty segment")
	}
	step := 1
	rangePart := part
	if idx := strings.IndexByte(part, '/'); idx >= 0 {
		stepStr := part[idx+1:]
		if strings.ContainsRune(stepStr, '/') {
			return 0, fmt.Errorf("invalid step in %q", part)
		}
		s, err := strconv.Atoi(stepStr)
		if err != nil || s <= 0 {
			return 0, fmt.Errorf("step must be a positive integer in %q", part)
		}
		step = s
		rangePart = part[:idx]
	}

	var lo, hi int
	if rangePart == "*" {
		lo, hi = minVal, maxVal
	} else if idx := strings.IndexByte(rangePart, '-'); idx >= 0 {
		var err error
		lo, err = parseCronAtom(rangePart[:idx], minVal, maxVal, names)
		if err != nil {
			return 0, err
		}
		hi, err = parseCronAtom(rangePart[idx+1:], minVal, maxVal, names)
		if err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range start %d > end %d in %q", lo, hi, part)
		}
	} else {
		v, err := parseCronAtom(rangePart, minVal, maxVal, names)
		if err != nil {
			return 0, err
		}
		if step > 1 {
			// Disallow ambiguous "5/10" without explicit range or "*".
			return 0, fmt.Errorf("step requires '*' or range (e.g. %d-%d/%d), got %q", v, maxVal, step, part)
		}
		lo, hi = v, v
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

func parseCronAtom(s string, minVal, maxVal int, names map[string]int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	if names != nil {
		if v, ok := names[strings.ToUpper(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("value %d out of range [%d..%d]", v, minVal, maxVal)
	}
	return v, nil
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func (c *CronSpec) matchesDay(year int, month time.Month, day int, wd time.Weekday) bool {
	domMatch := (c.dom&(1<<uint(day)) != 0) || (c.domLast && day == daysInMonth(year, month))
	dowMatch := c.dow&(1<<uint(wd)) != 0
	if !c.domStar && !c.dowStar {
		return domMatch || dowMatch
	}
	if !c.domStar {
		return domMatch
	}
	if !c.dowStar {
		return dowMatch
	}
	return true
}

// Next returns the first firing time strictly after `after`, evaluated in loc
// and returned in UTC. Searches up to 5 years forward.
//
// DST semantics (design §5):
//   - Spring-forward gap (skipped hour): a wall-clock match that does not exist
//     in loc fires once at the first minute after the transition.
//   - Fall-back overlap (repeated hour): a wall-clock match fires only on its
//     first occurrence (earlier UTC instant).
func (c *CronSpec) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	// Candidate wall-clock starts at the next whole minute after `after`.
	// If that 1-minute UTC step crosses a DST spring-forward gap (e.g. 01:59
	// EST -> 03:00 EDT), wall-clock minutes inside the skipped gap (02:00..02:59)
	// would never be visited if we started at `base`'s wall clock. Start from
	// `after`'s wall clock + 1 minute in wall-clock arithmetic so any match
	// inside the gap is still resolved to the first minute after the transition.
	afterLocal := after.In(loc)
	base := afterLocal.Truncate(time.Minute).Add(time.Minute)
	limit := base.AddDate(5, 0, 0)

	year, month, day := afterLocal.Year(), afterLocal.Month(), afterLocal.Day()
	hour, min := afterLocal.Hour(), afterLocal.Minute()+1
	if min >= 60 {
		min = 0
		hour++
		if hour >= 24 {
			hour = 0
			day++
		}
	}

	// Track the last instant we tried in a gap/overlap so we never loop.
	for year <= limit.Year() {
		if c.months&(1<<uint(month)) == 0 {
			month++
			if month > time.December {
				month = time.January
				year++
			}
			day, hour, min = 1, 0, 0
			continue
		}
		dim := daysInMonth(year, month)
		if day > dim {
			month++
			if month > time.December {
				month = time.January
				year++
			}
			day, hour, min = 1, 0, 0
			continue
		}
		// Weekday depends only on the calendar date.
		wd := time.Date(year, month, day, 12, 0, 0, 0, time.UTC).Weekday()
		if !c.matchesDay(year, month, day, wd) {
			day++
			hour, min = 0, 0
			continue
		}
		if c.hours&(1<<uint(hour)) == 0 {
			hour++
			min = 0
			if hour >= 24 {
				day++
				hour = 0
			}
			continue
		}
		if c.minutes&(1<<uint(min)) == 0 {
			min++
			if min >= 60 {
				hour++
				min = 0
				if hour >= 24 {
					day++
					hour = 0
				}
			}
			continue
		}

		// Wall clock (year, month, day, hour, min) matches all 5 fields.
		cand, ok := resolveWallTime(year, month, day, hour, min, loc, after)
		if ok {
			if cand.After(limit) {
				return time.Time{}, false
			}
			return cand.UTC(), true
		}

		// Candidate was not strictly after `after` (e.g. fall-back duplicate
		// already fired on first occurrence): advance to next minute.
		min++
		if min >= 60 {
			hour++
			min = 0
			if hour >= 24 {
				day++
				hour = 0
			}
		}
	}
	return time.Time{}, false
}

// resolveWallTime converts a matching local wall time to a concrete instant,
// handling DST spring-forward gaps and fall-back overlaps per design §5.
func resolveWallTime(year int, month time.Month, day, hour, min int, loc *time.Location, after time.Time) (time.Time, bool) {
	t := time.Date(year, month, day, hour, min, 0, 0, loc)

	// Check for spring-forward gap: Go's time.Date normalizes a non-existent
	// wall time by shifting it, so its local hour/min no longer equals the
	// requested wall clock. Find the exact first minute after the transition
	// by walking forward from 2 hours before t (guaranteed pre-DST).
	if t.Year() != year || t.Month() != month || t.Day() != day || t.Hour() != hour || t.Minute() != min {
		start := t.Add(-2 * time.Hour).Truncate(time.Minute)
		_, preOff := start.In(loc).Zone()
		transition := t
		for i := 1; i <= 240; i++ {
			cur := start.Add(time.Duration(i) * time.Minute).In(loc)
			_, off := cur.Zone()
			if off != preOff {
				transition = cur
				break
			}
		}
		if transition.After(after) {
			return transition, true
		}
		return time.Time{}, false
	}

	// Check for fall-back overlap: Go's time.Date returns the earlier (first)
	// occurrence of an ambiguous wall time. Verify by checking if an hour
	// earlier has the same wall clock (in case of future Go stdlib changes).
	earlier := t.Add(-time.Hour)
	if earlier.Year() == year && earlier.Month() == month && earlier.Day() == day && earlier.Hour() == hour && earlier.Minute() == min {
		t = earlier
	}

	if t.After(after) {
		return t, true
	}
	return time.Time{}, false
}

// HumanSummary produces a concise Chinese summary of common cron patterns.
func (c *CronSpec) HumanSummary(tz string) string {
	fields := strings.Fields(c.expr)
	if len(fields) != 5 {
		return fmt.Sprintf("cron %s（%s）", c.expr, tz)
	}
	minF, hourF, domF, monF, dowF := fields[0], fields[1], fields[2], fields[3], fields[4]

	if h, okH := singleInt(hourF); okH {
		if m, okM := singleInt(minF); okM {
			hm := fmt.Sprintf("%02d:%02d", h, m)
			if domF == "*" && monF == "*" && dowF == "*" {
				return fmt.Sprintf("每天 %s（%s）", hm, tz)
			}
			if domF == "*" && monF == "*" {
				if wd, ok := singleDOW(dowF); ok {
					return fmt.Sprintf("每%s %s（%s）", zhWeekday(wd), hm, tz)
				}
			}
			if monF == "*" && dowF == "*" {
				if strings.EqualFold(domF, "L") {
					return fmt.Sprintf("每月最后一天 %s（%s）", hm, tz)
				}
				if d, ok := singleInt(domF); ok {
					return fmt.Sprintf("每月 %d 日 %s（%s）", d, hm, tz)
				}
			}
		}
	}
	if minF == "0" && hourF == "*" && domF == "*" && monF == "*" && dowF == "*" {
		return fmt.Sprintf("每小时整点（%s）", tz)
	}
	return fmt.Sprintf("cron %s（%s）", c.expr, tz)
}

func singleInt(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

func singleDOW(s string) (int, bool) {
	if v, ok := cronDOWNames[strings.ToUpper(s)]; ok {
		return v, true
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || v > 7 {
		return 0, false
	}
	if v == 7 {
		v = 0
	}
	return v, true
}

func zhWeekday(wd int) string {
	names := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	if wd >= 0 && wd < len(names) {
		return names[wd]
	}
	return fmt.Sprintf("周%d", wd)
}
