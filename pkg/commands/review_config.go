package commands

import "time"

// defaultReviewTokenBudget bounds one adversarial-review subagent when
// config.yaml doesn't say otherwise (design §4.5's sample value).
const defaultReviewTokenBudget = 30_000

// defaultReviewTimeout matches pkg/chat's own fallback; resolving it here
// too keeps config.yaml's contract ("0 = default") in one place.
const defaultReviewTimeout = 5 * time.Minute

// resolveReviewTokenBudget maps the config value to an effective budget:
// 0/absent → the 30k default, negative → unlimited (0 downstream).
func resolveReviewTokenBudget(configured int) int {
	switch {
	case configured == 0:
		return defaultReviewTokenBudget
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// resolveReviewTimeout maps config.yaml's minutes int (0 = default) to a
// duration.
func resolveReviewTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		return defaultReviewTimeout
	}
	return time.Duration(minutes) * time.Minute
}
