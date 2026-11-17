package pool

// release returns one slot to the pool. Callers that ignore the returned
// error leak the slot silently.
func release() error { return nil }
