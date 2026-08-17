package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// threatPatterns matches potentially dangerous content that should not be stored as memory.
var threatPatterns = []struct {
	regex  *regexp.Regexp
	threat string
}{
	{regexp.MustCompile(`(?i)ignore\s+(previous|all|above|prior)\s+instructions`), "prompt_injection"},
	{regexp.MustCompile(`(?i)you\s+are\s+now\s+(a|an|the|acting)\s+`), "role_hijack"},
	{regexp.MustCompile(`(?i)system\s*:\s*you\s+are`), "role_hijack"},
	{regexp.MustCompile(`(?i)new\s+instructions?\s*:`), "prompt_injection"},
	{regexp.MustCompile(`(?i)<\|imagine\|>.*?(<\/\|imagine\|>)`), "injection_tag"},
	{regexp.MustCompile(`(?i)curl.*\$(\{?[A-Z_]+[A-Z_0-9]*\}?)`), "credential_exfil"},
	{regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[=:]\s*\S`), "credential_leak"},
	{regexp.MustCompile(`(?i)(https?://[^\s"<>]+\.(env|pem|key|secret))`), "secret_url"},
}

// invisibleUnicodePatterns matches invisible Unicode characters that could be used for obfuscation.
var invisibleUnicodePatterns = []*regexp.Regexp{
	regexp.MustCompile("[\u200B\u200C\u200D\u200E\u200F\u2060\uFEFF]"),
	regexp.MustCompile("[\u202A-\u202E]"),
	regexp.MustCompile("[\u2066-\u2069]"),
}

// ScanError reports a security threat found in memory content.
type ScanError struct {
	Content string
	Threat  string
}

func (e ScanError) Error() string {
	return fmt.Sprintf("memory content blocked (threat: %s): %.100s", e.Threat, e.Content)
}

// ScanContent checks content for security threats. Returns nil if content is safe.
func ScanContent(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	// Check for invisible Unicode characters.
	for _, pat := range invisibleUnicodePatterns {
		if pat.MatchString(content) {
			return ScanError{Content: content, Threat: "invisible_unicode"}
		}
	}

	// Check for invisible control characters (except newline, tab).
	for _, r := range content {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return ScanError{Content: content, Threat: "control_char"}
		}
	}

	// Check for threat patterns.
	for _, tp := range threatPatterns {
		if tp.regex.MatchString(content) {
			return ScanError{Content: content, Threat: tp.threat}
		}
	}

	return nil
}
