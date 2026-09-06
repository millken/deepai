package skill

import "context"

// callerAgentTypeKey is the unexported context key for the caller's declared
// agent type, so no other package can set or read it except through the
// functions below.
type callerAgentTypeKey struct{}

// WithCallerAgentType stores the CALLING agent's declared agent type on ctx.
// "" means the main agent — the REPL declares no agent type (see
// agent.ApplyAgentType's `declared` check) — so a subagent executor that sets
// this to the empty string for its own delegated main-agent-shaped caller is
// using the same convention deliberately, not leaving it unset by omission.
// pkg/agent/subagent.go's Execute sets this once per subagent run, alongside
// tools.WithUserInteraction; pkg/skill/tool.go's skill-tool handler reads it
// to route a context:fork skill to the right place (§2.3).
func WithCallerAgentType(ctx context.Context, agentType string) context.Context {
	return context.WithValue(ctx, callerAgentTypeKey{}, agentType)
}

// CallerAgentTypeFromContext returns the caller's declared agent type, or ""
// if never set (which is indistinguishable from — and intentionally shares
// the meaning of — the main agent's own empty declaration).
func CallerAgentTypeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(callerAgentTypeKey{}).(string)
	return v
}
