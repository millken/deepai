package builtin

import (
	"strings"
)

// SearchRecommendation represents a tool recommendation for search operations
type SearchRecommendation struct {
	PrimaryTool   string   `json:"primary_tool"`
	Reasoning      string   `json:"reasoning"`
	Alternative    string   `json:"alternative,omitempty"`
	Tips          []string `json:"tips"`
}

// RecommendSearchTool analyzes the search intent and recommends the most appropriate tool
func RecommendSearchTool(query string, args map[string]any) SearchRecommendation {
	// Extract key information from query and arguments
	pattern := ""
	if p, ok := args["pattern"].(string); ok {
		pattern = strings.ToLower(p)
	}
	queryLower := strings.ToLower(query)
	
	// Analyze search intent
	switch {
	case isFileNameSearch(queryLower, args):
		return SearchRecommendation{
			PrimaryTool: "bash",
			Reasoning:   "File name searches are more efficient with 'find' command",
			Alternative: "list_dir",
			Tips: []string{
				"Use: find . -name '*.go' -type f",
				"Add filters: -name '*.txt' -type f",
				"Limit depth: -maxdepth 2",
			},
		}
	
	case isComplexOperation(queryLower):
		return SearchRecommendation{
			PrimaryTool: "bash",
			Reasoning:   "Complex operations with pipes or multiple commands work best in bash",
			Tips: []string{
				"Chain commands: find . -name '*.go' | xargs grep 'pattern'",
				"Combine tools: git log --oneline | head -10",
				"Use shell features: loops, conditions, variables",
			},
		}
	
	case isFileContentSearch(pattern, args):
		return SearchRecommendation{
			PrimaryTool: "grep",
			Reasoning:   "Content search within files is grep's specialty",
			Alternative: "bash (for complex content searches)",
			Tips: []string{
				"Use type filter: type:go for Go files only",
				"Add context: context:3 for surrounding lines",
				"Case insensitive: case_insensitive:true",
				"Limit results: max_results:100",
			},
		}
	
	case isGitRelatedSearch(queryLower):
		return SearchRecommendation{
			PrimaryTool: "bash",
			Reasoning:   "Git history and blame operations are best with git commands",
			Tips: []string{
				"Search commits: git log --all --grep='pattern'",
				"Find author: git log --author='name'",
				"Search content: git log -S'pattern' --all",
				"File history: git log --follow -- file.txt",
			},
		}
	
	case isSystemInfoQuery(queryLower):
		return SearchRecommendation{
			PrimaryTool: "bash",
			Reasoning:   "System information queries require standard Unix tools",
			Tips: []string{
				"Process info: ps aux | grep 'process'",
				"Disk usage: df -h",
				"Memory: free -h",
				"Network: netstat -tuln",
			},
		}
	
	default:
		return SearchRecommendation{
			PrimaryTool: "grep",
			Reasoning:   "For general content searches, grep is the default choice",
			Alternative: "bash (for complex queries)",
			Tips: []string{
				"Start with grep for simple pattern matching",
				"Use bash when you need pipes, complex logic, or system commands",
				"Use find for file name searches",
			},
		}
	}
}

// Helper functions for intent analysis

func isFileNameSearch(query string, args map[string]any) bool {
	// File name indicators
	fileNameIndicators := []string{
		"file name", "filename", "named", "files called",
		"find file", "list files", "search file",
	}
	
	for _, indicator := range fileNameIndicators {
		if strings.Contains(query, indicator) {
			return true
		}
	}
	
	// Check if pattern looks like a file extension
	if pattern, ok := args["pattern"].(string); ok {
		if strings.HasPrefix(pattern, "*.") || strings.HasPrefix(pattern, ".") {
			return true
		}
	}
	
	return false
}

func isComplexOperation(query string) bool {
	// Complex operation indicators
	complexIndicators := []string{
		"pipe", "and then", "after that", "combine",
		"count", "sort", "unique", "filter",
	}
	
	for _, indicator := range complexIndicators {
		if strings.Contains(query, indicator) {
			return true
		}
	}
	
	return false
}

func isFileContentSearch(pattern string, args map[string]any) bool {
	// Content search indicators
	if pattern == "" {
		return false
	}
	
	// If there's a pattern but no file-specific indicators, assume content search
	return true
}

func isGitRelatedSearch(query string) bool {
	gitIndicators := []string{
		"git", "commit", "branch", "author", "history",
		"blame", "log", "diff", "merge", "rebase", "wrote",
	}
	
	for _, indicator := range gitIndicators {
		if strings.Contains(query, indicator) {
			return true
		}
	}
	
	return false
}

func isSystemInfoQuery(query string) bool {
	systemIndicators := []string{
		"process", "memory", "disk", "cpu", "network",
		"running", "system", "environment", "variable",
	}
	
	for _, indicator := range systemIndicators {
		if strings.Contains(query, indicator) {
			return true
		}
	}
	
	return false
}

// GetToolRecommendations returns formatted recommendations for system prompt integration
func GetToolRecommendations() string {
	return `
## Tool Selection Guidelines for Search and Git Operations

### When to use grep (Content Search)
- **Best for**: Searching content within files
- **Use cases**: Find function definitions, variable usage, string patterns
- **Advantages**: Fast, context-aware, type filtering
- **Example**: "Find all functions named 'parse' in Go files"

### When to use bash (File/System/Git Operations)
- **Best for**: File name searches, complex operations, git commands, system queries
- **Use cases**: 
  - Git operations: git status, git diff, git log, git commit, etc.
  - File name searches: find . -name '*.go'
  - Complex operations: Combine multiple tools with pipes
  - System information: ps, df, free, etc.
- **Advantages**: Full Unix toolset, pipes, complex logic, complete git access
- **Example**: "Find all Python files modified in the last 24 hours"

### Git Operations with Bash
Modern models should use bash for git operations rather than dedicated git tools:
- Status: git status
- Diff: git diff (or git diff --stat for summary)
- Log: git log --oneline -10
- Commit: git commit -m "message" (files must be staged first with git add)
- Branch: git branch, git checkout
- History: git log --grep="pattern" --all

### Decision Tree
1. **Content search** → grep
2. **File name search** → bash find
3. **Git operations** → bash git commands
4. **Complex operations** → bash
5. **System information** → bash

### Performance Tips
- Use grep's type filter for faster searches: type:go
- Use git diff --stat for summary instead of full diff
- Limit bash output with head/tail for large results
- Combine tools efficiently: find | xargs grep
`
}