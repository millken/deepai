package commands

import (
	"time"

	"github.com/millken/deepai/pkg/chat"
)

// defaultReviewTokenBudget bounds one adversarial-review subagent when
// config.yaml doesn't say otherwise.
//
// Raised from the design's 30k sample value because exceeding it is a HARD
// error inside the subagent (react.go returns "agent exceeded token budget"
// with an EMPTY final output — the whole review is lost, not truncated), and
// 30k does not cover what the review gate is allowed to hand the reviewer in
// the first place: pkg/chat's rungs admit up to a 200KiB diff plus a 256KiB
// full-text bundle, i.e. ~110k tokens of input on the FIRST turn alone. The
// budget is a runaway backstop, not a spend target — a small change still
// costs a small review — so it has to sit above the largest input the gate
// itself permits, or the biggest changes are exactly the ones that go
// unreviewed.
//
// Set review_token_budget in config.yaml to override; negative means
// unlimited.
const defaultReviewTokenBudget = 150_000

// defaultReviewTimeout is pkg/chat's own fallback, referenced rather than
// re-declared: resolving config.yaml's "0 = default" contract here as well
// used to mean two constants that had to be edited together.
const defaultReviewTimeout = chat.DefaultReviewTimeout

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
