package builtin

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
