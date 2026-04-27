package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// workingDirFromArgs extracts the optional "working_dir" argument from a tool call.
// Returns "" if not specified (caller should use process CWD).
func workingDirFromArgs(args map[string]any) string {
	dir, _ := args["working_dir"].(string)
	return strings.TrimSpace(dir)
}

// gitCmd builds an exec.Cmd for git with optional working directory.
func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd
}

func gitCommitEnv(args map[string]any) ([]string, error) {
	authorName, _ := args["author_name"].(string)
	authorEmail, _ := args["author_email"].(string)
	authorName = strings.TrimSpace(authorName)
	authorEmail = strings.TrimSpace(authorEmail)
	if authorName == "" && authorEmail == "" {
		return nil, nil
	}
	if authorName == "" || authorEmail == "" {
		return nil, fmt.Errorf("author_name and author_email must be provided together")
	}
	return []string{
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName,
		"GIT_COMMITTER_EMAIL=" + authorEmail,
	}, nil
}

func isMissingGitIdentity(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "author identity unknown") ||
		strings.Contains(output, "unable to auto-detect email address")
}

type gitStatusEntry struct {
	Path           string `json:"path"`
	IndexStatus    string `json:"index_status,omitempty"`
	WorktreeStatus string `json:"worktree_status,omitempty"`
}

type gitLogEntry struct {
	Hash        string `json:"hash"`
	ShortHash   string `json:"short_hash"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Subject     string `json:"subject"`
	Timestamp   string `json:"timestamp"`
}

func parseGitStatus(output string) (entries []gitStatusEntry, staged []string, unstaged []string, untracked []string) {
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) == "" || len(line) < 3 {
			continue
		}
		entry := gitStatusEntry{Path: strings.TrimSpace(line[3:])}
		index := string(line[0])
		worktree := string(line[1])
		if index == "?" && worktree == "?" {
			untracked = append(untracked, entry.Path)
			entries = append(entries, gitStatusEntry{Path: entry.Path, IndexStatus: "?", WorktreeStatus: "?"})
			continue
		}
		if index != " " {
			entry.IndexStatus = index
			staged = append(staged, entry.Path)
		}
		if worktree != " " {
			entry.WorktreeStatus = worktree
			unstaged = append(unstaged, entry.Path)
		}
		entries = append(entries, entry)
	}
	sort.Strings(staged)
	sort.Strings(unstaged)
	sort.Strings(untracked)
	return entries, staged, unstaged, untracked
}

func parseGitLogEntries(output string) []gitLogEntry {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	var entries []gitLogEntry
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 6 {
			continue
		}
		entries = append(entries, gitLogEntry{
			Hash:        parts[0],
			ShortHash:   parts[1],
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
			Timestamp:   parts[4],
			Subject:     parts[5],
		})
	}
	return entries
}

func splitLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// GitStatusHandler shows the current git status
func GitStatusHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	dir := workingDirFromArgs(call.Arguments)
	cmd := gitCmd(ctx, dir, "status", "--porcelain")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git status failed: %w", err)
	}
	entries, staged, unstaged, untracked := parseGitStatus(string(output))

	result := map[string]interface{}{
		"status":    string(output),
		"clean":     strings.TrimSpace(string(output)) == "",
		"entries":   entries,
		"staged":    staged,
		"unstaged":  unstaged,
		"untracked": untracked,
	}
	data, _ := json.Marshal(result)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data), Data: result}, nil
}

// GitDiffHandler shows the diff of changes
func GitDiffHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	staged, _ := args["staged"].(bool)

	gitArgs := []string{"diff"}
	if staged {
		gitArgs = []string{"diff", "--cached"}
	}

	cmd := gitCmd(ctx, dir, gitArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git diff failed: %w", err)
	}
	nameArgs := append(append([]string(nil), gitArgs...), "--name-only")
	nameOutput, _ := gitCmd(ctx, dir, nameArgs...).CombinedOutput()
	statArgs := append(append([]string(nil), gitArgs...), "--stat")
	statOutput, _ := gitCmd(ctx, dir, statArgs...).CombinedOutput()
	files := splitLines(string(nameOutput))
	result := map[string]any{
		"staged": staged,
		"files":  files,
		"stats":  strings.TrimSpace(string(statOutput)),
		"diff":   string(output),
	}
	data, _ := json.Marshal(result)

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data), Data: result}, nil
}

// GitLogHandler shows recent commit history
func GitLogHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	count := 10
	if n, ok := args["count"].(float64); ok && n > 0 && n <= 50 {
		count = int(n)
	}
	oneline, _ := args["oneline"].(bool)

	gitArgs := []string{"log", fmt.Sprintf("-%d", count)}
	if oneline {
		gitArgs = append(gitArgs, "--oneline")
	}

	cmd := gitCmd(ctx, dir, gitArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git log failed: %w", err)
	}
	entryArgs := []string{"log", fmt.Sprintf("-%d", count), "--format=%H%x1f%h%x1f%an%x1f%ae%x1f%aI%x1f%s"}
	entryOutput, _ := gitCmd(ctx, dir, entryArgs...).CombinedOutput()
	entries := parseGitLogEntries(string(entryOutput))
	result := map[string]any{
		"text":    string(output),
		"entries": entries,
		"oneline": oneline,
		"count":   count,
	}
	data, _ := json.Marshal(result)

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data), Data: result}, nil
}

// GitAddHandler stages files for commit
func GitAddHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	files, ok := args["files"].([]interface{})
	if !ok || len(files) == 0 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("files array is required")
	}

	fileArgs := make([]string, 0, len(files))
	for _, f := range files {
		if file, ok := f.(string); ok && strings.TrimSpace(file) != "" {
			fileArgs = append(fileArgs, file)
		}
	}

	if len(fileArgs) == 0 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("no valid files provided")
	}

	// Use -- to prevent filenames starting with - from being interpreted as options
	cmdArgs := append([]string{"add", "--"}, fileArgs...)
	cmd := gitCmd(ctx, dir, cmdArgs...)
	if err := cmd.Run(); err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git add failed: %w", err)
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: fmt.Sprintf("Staged files: %s", strings.Join(fileArgs, ", "))}, nil
}

// GitCommitHandler creates a commit with the given message
func GitCommitHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	message, ok := args["message"].(string)
	if !ok || strings.TrimSpace(message) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("commit message is required")
	}

	// Check if there are staged changes
	statusCmd := gitCmd(ctx, dir, "diff", "--cached", "--name-only")
	statusOutput, err := statusCmd.Output()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("failed to check staged changes: %w", err)
	}

	if strings.TrimSpace(string(statusOutput)) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("no staged changes to commit")
	}

	commitEnv, err := gitCommitEnv(args)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}
	if len(commitEnv) == 0 {
		identCmd := gitCmd(ctx, dir, "var", "GIT_AUTHOR_IDENT")
		if output, err := identCmd.CombinedOutput(); err != nil {
			_ = output
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git commit failed: git user identity is not configured; provide author_name and author_email or configure git user.name/user.email")
		}
	}

	// Create commit
	cmd := gitCmd(ctx, dir, "commit", "-m", message)
	if len(commitEnv) > 0 {
		cmd.Env = append(os.Environ(), commitEnv...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if isMissingGitIdentity(details) {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git commit failed: git user identity is not configured; provide author_name and author_email or configure git user.name/user.email")
		}
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git commit failed: %w", err)
	}

	result := map[string]interface{}{
		"message": message,
		"output":  string(output),
		"staged":  strings.Split(strings.TrimSpace(string(statusOutput)), "\n"),
	}
	data, _ := json.Marshal(result)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

// GitPushHandler pushes commits to remote repository
func GitPushHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	remote, _ := args["remote"].(string)
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	setUpstream, _ := args["set_upstream"].(bool)
	branch, _ := args["branch"].(string)
	if strings.TrimSpace(branch) == "" {
		branchCmd := gitCmd(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
		branchOutput, err := branchCmd.Output()
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("failed to get current branch: %w", err)
		}
		branch = strings.TrimSpace(string(branchOutput))
	}

	pushArgs := []string{"push"}
	if setUpstream {
		pushArgs = append(pushArgs, "--set-upstream")
	}
	pushArgs = append(pushArgs, remote, branch)
	cmd := gitCmd(ctx, dir, pushArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git push failed: %w", err)
	}

	result := map[string]interface{}{
		"remote":       remote,
		"branch":       branch,
		"set_upstream": setUpstream,
		"output":       string(output),
	}
	data, _ := json.Marshal(result)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

// workingDirSchema is the shared working_dir property definition for git tool schemas.
var workingDirSchema = map[string]any{
	"type":        "string",
	"description": "Absolute path to the git repository. Defaults to the current working directory if not specified.",
}

// GitStatusTool returns the git status tool
func GitStatusTool() models.Tool {
	return models.Tool{
		Name:        "git_status",
		Description: "Show the current git repository status.",
		Groups:      []string{"builtin", "git"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"working_dir": workingDirSchema,
			},
		},
		Handler: GitStatusHandler,
	}
}

// GitDiffTool returns the git diff tool
func GitDiffTool() models.Tool {
	return models.Tool{
		Name:        "git_diff",
		Description: "Show the diff of changes in the repository.",
		Groups:      []string{"builtin", "git"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"staged":      map[string]any{"type": "boolean", "description": "Show staged changes instead of working directory changes"},
				"working_dir": workingDirSchema,
			},
		},
		Handler: GitDiffHandler,
	}
}

// GitLogTool returns the git log tool
func GitLogTool() models.Tool {
	return models.Tool{
		Name:        "git_log",
		Description: "Show recent git commit history.",
		Groups:      []string{"builtin", "git"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count":       map[string]any{"type": "number", "description": "Number of commits to show (default 10, max 50)"},
				"oneline":     map[string]any{"type": "boolean", "description": "Use oneline format (default false)"},
				"working_dir": workingDirSchema,
			},
		},
		Handler: GitLogHandler,
	}
}

// GitAddTool returns the git add tool
func GitAddTool() models.Tool {
	return models.Tool{
		Name:        "git_add",
		Description: "Stage files for commit.",
		Groups:      []string{"builtin", "git"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "List of files to stage",
				},
				"working_dir": workingDirSchema,
			},
			"required": []any{"files"},
		},
		Handler: GitAddHandler,
	}
}

// GitCommitTool returns the git commit tool
func GitCommitTool() models.Tool {
	return models.Tool{
		Name:        "git_commit",
		Description: "Create a commit with the given message. Requires staged changes. Optional author_name/author_email let the tool work in environments without configured git identity.",
		Groups:      []string{"builtin", "git"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message":      map[string]any{"type": "string", "description": "Commit message"},
				"author_name":  map[string]any{"type": "string", "description": "Optional git author/committer name override for environments without configured identity"},
				"author_email": map[string]any{"type": "string", "description": "Optional git author/committer email override for environments without configured identity"},
				"working_dir":  workingDirSchema,
			},
			"required": []any{"message"},
		},
		Handler: GitCommitHandler,
	}
}

// GitPushTool returns the git push tool
func GitPushTool() models.Tool {
	return models.Tool{
		Name:        "git_push",
		Description: "Push commits to remote repository. Optional set_upstream configures the branch tracking relationship on first push.",
		Groups:      []string{"builtin", "git"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"remote":       map[string]any{"type": "string", "description": "Remote repository name (default: origin)"},
				"branch":       map[string]any{"type": "string", "description": "Branch name to push (default: current branch)"},
				"set_upstream": map[string]any{"type": "boolean", "description": "Use --set-upstream/-u so future pushes can omit remote and branch"},
				"working_dir":  workingDirSchema,
			},
		},
		Handler: GitPushHandler,
	}
}

// GitResetHandler unstages files from the index
func GitResetHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	dir := workingDirFromArgs(args)
	rawFiles, _ := args["files"].([]interface{})

	cmdArgs := []string{"reset", "HEAD"}
	if len(rawFiles) > 0 {
		var files []string
		for _, f := range rawFiles {
			if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
				files = append(files, s)
			}
		}
		if len(files) == 0 {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("files parameter provided but contains no valid paths")
		}
		cmdArgs = append(cmdArgs, "--")
		cmdArgs = append(cmdArgs, files...)
	}

	cmd := gitCmd(ctx, dir, cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git reset failed: %w: %s", err, string(output))
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(output)}, nil
}

// GitResetTool returns the git reset tool
func GitResetTool() models.Tool {
	return models.Tool{
		Name:        "git_reset",
		Description: "Unstage files from the index (git reset HEAD). If files are specified, only unstage those; otherwise unstage all.",
		Groups:      []string{"builtin", "git"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Specific files to unstage. If empty, unstages everything.",
				},
				"working_dir": workingDirSchema,
			},
		},
		Handler: GitResetHandler,
	}
}

// GitTools returns all deterministic git operation tools.
func GitTools() []models.Tool {
	return []models.Tool{
		GitStatusTool(),
		GitDiffTool(),
		GitLogTool(),
		GitAddTool(),
		GitCommitTool(),
		GitResetTool(),
		GitPushTool(),
	}
}
