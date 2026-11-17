package notify

// Policy defaults apply to channels without an explicit sender config;
// they are intentionally more conservative than any sender's own setting.
const MaxRetries = 3

func defaultPolicy() string { return "balanced" }
