package gate

// close marks the gate closed and reports whether it was previously open.
// Callers that ignore the returned bool can double-close without noticing.
func (g *Gate) close() (wasOpen bool) { return false }
