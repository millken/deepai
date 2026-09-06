package skill

import (
	"context"
	"testing"
)

// TestCallerAgentTypeContext is the RED test for M5-1 §2.2: a context.Context
// carries the caller's declared agent type (empty string = main agent) via an
// unexported key, round-tripping through WithCallerAgentType/
// CallerAgentTypeFromContext.
func TestCallerAgentTypeContext(t *testing.T) {
	if got := CallerAgentTypeFromContext(context.Background()); got != "" {
		t.Fatalf("unset context: CallerAgentTypeFromContext() = %q, want \"\"", got)
	}

	ctx := WithCallerAgentType(context.Background(), "document-editor")
	if got := CallerAgentTypeFromContext(ctx); got != "document-editor" {
		t.Fatalf("CallerAgentTypeFromContext() = %q, want document-editor", got)
	}

	// The main agent explicitly stores "" (declares no type) — must be
	// distinguishable in behavior from "never set" only insofar as both
	// return "", which is exactly the semantics ApplyAgentType already gives
	// an undeclared type elsewhere in the codebase.
	ctxMain := WithCallerAgentType(context.Background(), "")
	if got := CallerAgentTypeFromContext(ctxMain); got != "" {
		t.Fatalf("main-agent context: CallerAgentTypeFromContext() = %q, want \"\"", got)
	}
}
