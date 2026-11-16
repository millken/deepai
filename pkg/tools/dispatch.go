package tools

import "context"

// Dispatch is the nested-execution mirror injected at call time: the registry
// the CALLING agent currently has in a.tools plus the concurrency cap its own
// batch scheduler uses (RUN_CODE_DESIGN §4.3). run_code builds tools.<name>
// from this, so plan mode's restricted set and subagent allowlists apply
// automatically; reading it at call time (not registration) is what keeps the
// mirror current across a.tools swaps.
type Dispatch struct {
	Registry       *Registry
	MaxConcurrency int
}

const dispatchContextKey contextKey = "tool_dispatch"

func WithDispatch(ctx context.Context, d *Dispatch) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, dispatchContextKey, d)
}

func DispatchFromContext(ctx context.Context) *Dispatch {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(dispatchContextKey).(*Dispatch)
	return d
}
