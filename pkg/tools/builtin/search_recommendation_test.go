package builtin

import (
	"testing"
)

func TestRecommendSearchTool(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		args     map[string]any
		expected string
		reason   string
	}{
		{
			name:     "file name search",
			query:    "find files named main.go",
			args:     map[string]any{},
			expected: "bash",
			reason:   "File name searches",
		},
		{
			name:     "content search",
			query:    "search for function parse",
			args:     map[string]any{"pattern": "parse"},
			expected: "grep",
			reason:   "Content search within files",
		},
		{
			name:     "complex operation",
			query:    "find all go files and count lines",
			args:     map[string]any{},
			expected: "bash",
			reason:   "Complex operations with pipes",
		},
		{
			name:     "git related search",
			query:    "find commits by author john",
			args:     map[string]any{},
			expected: "bash",
			reason:   "Git history and blame operations",
		},
		{
			name:     "system info query",
			query:    "show running processes",
			args:     map[string]any{},
			expected: "bash",
			reason:   "System information queries",
		},
		{
			name:     "default content search",
			query:    "search for error handling",
			args:     map[string]any{"pattern": "error"},
			expected: "grep",
			reason:   "Content search within files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := RecommendSearchTool(tt.query, tt.args)
			if rec.PrimaryTool != tt.expected {
				t.Errorf("Expected tool %q, got %q", tt.expected, rec.PrimaryTool)
			}
			if !contains(rec.Reasoning, tt.reason) {
				t.Errorf("Expected reasoning to contain %q, got %q", tt.reason, rec.Reasoning)
			}
			if len(rec.Tips) == 0 {
				t.Error("Expected tips to be provided")
			}
		})
	}
}

func TestIsFileNameSearch(t *testing.T) {
	tests := []struct {
		name string
		query string
		args map[string]any
		want bool
	}{
		{"file name indicator", "find files named test", map[string]any{}, true},
		{"filename keyword", "search for filename main.go", map[string]any{}, true},
		{"file extension pattern", "find files", map[string]any{"pattern": "*.go"}, true},
		{"content search", "search for function parse", map[string]any{"pattern": "parse"}, false},
		{"generic search", "find test", map[string]any{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFileNameSearch(tt.query, tt.args)
			if got != tt.want {
				t.Errorf("isFileNameSearch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsComplexOperation(t *testing.T) {
	tests := []struct {
		name string
		query string
		want bool
	}{
		{"pipe operation", "find files and count them", true},
		{"combine keyword", "combine grep and sort", true},
		{"complex logic", "after finding files, filter them", true},
		{"simple search", "find files", false},
		{"content search", "search for pattern", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isComplexOperation(tt.query)
			if got != tt.want {
				t.Errorf("isComplexOperation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsGitRelatedSearch(t *testing.T) {
	tests := []struct {
		name string
		query string
		want bool
	}{
		{"git commit search", "find commits by author", true},
		{"git history", "show git history for file", true},
		{"git blame", "who wrote this line", true},
		{"generic search", "find files", false},
		{"content search", "search for function", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isGitRelatedSearch(tt.query)
			if got != tt.want {
				t.Errorf("isGitRelatedSearch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetToolRecommendations(t *testing.T) {
	rec := GetToolRecommendations()
	if rec == "" {
		t.Error("Expected non-empty recommendations")
	}
	
	// Check that it contains key sections
	keySections := []string{
		"When to use grep",
		"When to use bash",
		"Decision Tree",
		"Performance Tips",
	}
	
	for _, section := range keySections {
		if !contains(rec, section) {
			t.Errorf("Expected recommendations to contain %q", section)
		}
	}
}

// Helper function - use existing contains from grep_test.go
func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}