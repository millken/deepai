package tools

import "context"

// A scheduled turn carries its schedule/run identity through the tool
// context, so tools (notably the schedule tool) can tell an unattended run
// from an interactive one and enforce their own policy. These are plain
// strings on purpose: pkg/tools must not depend on pkg/schedule.
// contextKey is shared with registry.go; the constants below are new keys
// on that same private type.
const (
	scheduleIDContextKey contextKey = "tool_schedule_id"
	runIDContextKey      contextKey = "tool_schedule_run_id"
	turnIDContextKey     contextKey = "tool_turn_id"
)

// WithScheduleRun marks ctx as belonging to a scheduled run. Empty values
// are a no-op, so callers can pass the pair unconditionally.
func WithScheduleRun(ctx context.Context, scheduleID, runID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if scheduleID == "" && runID == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, scheduleIDContextKey, scheduleID)
	return context.WithValue(ctx, runIDContextKey, runID)
}

// ScheduleRunFromContext returns the schedule/run ids of the current turn,
// or two empty strings for an ordinary interactive turn.
func ScheduleRunFromContext(ctx context.Context) (scheduleID, runID string) {
	if ctx == nil {
		return "", ""
	}
	scheduleID, _ = ctx.Value(scheduleIDContextKey).(string)
	runID, _ = ctx.Value(runIDContextKey).(string)
	return scheduleID, runID
}

// WithTurnID attaches the current turn id to a tool context.
func WithTurnID(ctx context.Context, turnID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if turnID == "" {
		return ctx
	}
	return context.WithValue(ctx, turnIDContextKey, turnID)
}

// TurnIDFromContext returns the turn id attached with WithTurnID.
func TurnIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(turnIDContextKey).(string)
	return id
}

// IsScheduledRun reports whether ctx belongs to a scheduled turn.
func IsScheduledRun(ctx context.Context) bool {
	id, _ := ScheduleRunFromContext(ctx)
	return id != ""
}
