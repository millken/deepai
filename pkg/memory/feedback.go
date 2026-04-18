package memory

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MinPositiveLength is the minimum message length (in runes) to be considered
// a positive signal. Shorter messages are treated as neutral (no helpful increment).
const MinPositiveLength = 10

// negativePatterns are regex patterns that indicate user dissatisfaction.
// When a user message matches any of these, the fact feedback is classified
// as negative and HelpfulCount is NOT incremented.
// Note: \b is not used for Chinese tokens because Go RE2 does not support
// Unicode word boundaries. Chinese patterns are anchored with common
// context characters instead.
var negativePatterns = []*regexp.Regexp{
	// Direct corrections — Chinese without \b, English with \b
	regexp.MustCompile(`(?i)(不对|错了|搞错了|不是这样|不是我要的)|(?:^|[\s,;.!?])wrong(?:$|[\s,;.!?])|(?:^|[\s,;.!?])incorrect(?:$|[\s,;.!?])|(?:^|[\s,;.!?])that'?s not what i meant(?:$|[\s,;.!?])|(?:^|[\s,;.!?])you'?re wrong(?:$|[\s,;.!?])`),
	// Retry / rephrase
	regexp.MustCompile(`(?i)(我的意思是|换个说法|我说的是|我想表达的是)|(?:^|[\s,;.!?])let me rephrase(?:$|[\s,;.!?])|(?:^|[\s,;.!?])what i meant was(?:$|[\s,;.!?])`),
	// Dissatisfaction
	regexp.MustCompile(`(?i)(没用|看不懂|不太对|不太行|答非所问|偏题)|(?:^|[\s,;.!?])doesn'?t help(?:$|[\s,;.!?])|(?:^|[\s,;.!?])too long(?:$|[\s,;.!?])`),
	// Request redo
	regexp.MustCompile(`(?i)(重新做|重来|重做|撤回|重新生成)|(?:^|[\s,;.!?])redo(?:$|[\s,;.!?])|(?:^|[\s,;.!?])try again(?:$|[\s,;.!?])|(?:^|[\s,;.!?])start over(?:$|[\s,;.!?])`),
}

// IsNegativeFeedback returns true if the message matches any negative feedback pattern.
func IsNegativeFeedback(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	for _, re := range negativePatterns {
		if re.MatchString(msg) {
			return true
		}
	}
	return false
}

// ClassifyUserResponse classifies a user message as positive, negative, or neutral
// in the context of fact helpfulness feedback.
//
// Returns true (positive) when:
//   - message is non-empty
//   - length >= MinPositiveLength
//   - does not match any negative feedback pattern
//   - cosine similarity with previous message <= 0.7 (not a simple retry)
func ClassifyUserResponse(msg string, prevMsg string, similarity float64) FeedbackResult {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return FeedbackResult{Classification: FeedbackNeutral}
	}

	// Check negative patterns first.
	if IsNegativeFeedback(msg) {
		return FeedbackResult{Classification: FeedbackNegative}
	}

	// Short messages are neutral — not enough signal.
	if utf8.RuneCountInString(msg) < MinPositiveLength {
		return FeedbackResult{Classification: FeedbackNeutral}
	}

	// If previous message is provided, check similarity to detect retries.
	if prevMsg != "" && similarity > 0.7 {
		return FeedbackResult{Classification: FeedbackNeutral}
	}

	return FeedbackResult{Classification: FeedbackPositive}
}

// FeedbackResult represents the classification of a user response for feedback purposes.
type FeedbackResult struct {
	Classification FeedbackClassification
}

// FeedbackClassification represents the type of user feedback.
type FeedbackClassification int

const (
	FeedbackNeutral  FeedbackClassification = iota // No signal
	FeedbackPositive                               // Likely helpful
	FeedbackNegative                               // Likely unhelpful
)

// String returns a human-readable label for the classification.
func (c FeedbackClassification) String() string {
	switch c {
	case FeedbackPositive:
		return "positive"
	case FeedbackNegative:
		return "negative"
	default:
		return "neutral"
	}
}

// TextCosineSimilarity computes cosine similarity between two strings using token overlap.
// Returns a value in [0, 1]. Empty strings return 0.
func TextCosineSimilarity(a, b string) float64 {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return 0
	}

	tokensA := tokenize(a)
	tokensB := tokenize(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}

	// Build frequency maps.
	freqA := make(map[string]int, len(tokensA))
	for _, t := range tokensA {
		freqA[t]++
	}
	freqB := make(map[string]int, len(tokensB))
	for _, t := range tokensB {
		freqB[t]++
	}

	// Dot product.
	var dot float64
	for t, fa := range freqA {
		if fb, ok := freqB[t]; ok {
			dot += float64(fa * fb)
		}
	}

	// Magnitudes.
	var magA, magB float64
	for _, fa := range freqA {
		magA += float64(fa * fa)
	}
	for _, fb := range freqB {
		magB += float64(fb * fb)
	}

	if magA == 0 || magB == 0 {
		return 0
	}

	return dot / (sqrt(magA) * sqrt(magB))
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	// Split on whitespace and common punctuation.
	var tokens []string
	var buf strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			buf.WriteRune(r)
		} else if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
		// CJK characters are tokens by themselves.
		if r >= 0x4E00 && r <= 0x9FFF {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			tokens = append(tokens, string(r))
		}
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}
	return tokens
}

func sqrt(x float64) float64 {
	// Newton's method.
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = 0.5 * (z + x/z)
	}
	return z
}

// lastRetrieval holds the fact IDs retrieved in the previous turn for a session,
// along with a timestamp for stale cleanup.
type lastRetrieval struct {
	ids []string
	ts  int64 // unix nano
}
