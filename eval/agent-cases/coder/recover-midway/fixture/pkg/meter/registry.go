package meter

// registry aggregates both sampling styles.
var defaultWindow = 10 * time.Second

// defaultWindowSize is a DIFFERENT identifier that merely starts with the
// same words — it is not the rename target and must survive untouched.
const defaultWindowSize = 64

func registry() {}
