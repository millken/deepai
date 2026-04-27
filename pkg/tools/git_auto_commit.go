package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

const (
	maxDiffChars              = 12000
	commitMessageSystemPrompt = `You are a git commit message generator. Analyze the code changes and generate a conventional commit message.

Rules:
1. Format: type(scope): description
2. type must be one of: feat, fix, refactor, docs, test, chore, perf, style, build, ci
3. scope is optional, describes the affected module
4. description uses imperative mood ("add feature" not "added feature"), max 72 chars
5. Output ONLY the commit message, nothing else
6. Language: use the same language as the code comments and existing commit messages`
)

// GitAutoCommitTool returns a workflow tool that generates a conventional commit
// message via LLM, commits, and optionally pushes.
//
// This is a higher-order workflow (not a pure git adapter). It explicitly depends
// on llm.LLMProvider — the caller decides which provider to inject.
//
// Behavior:
//   - If "files" is specified: verifies no extraneous pre-staged content exists,
//     then stages those files and commits.
//   - If "files" is omitted: commits whatever is already staged (no implicit git add).
//   - Push failure is returned as a tool error, not silently embedded in output.
func GitAutoCommitTool(provider llm.LLMProvider) models.Tool {
	return models.Tool{
		Name:        "git_auto_commit",
		Description: "Generate a conventional commit message via AI, commit staged or specified files, and optionally push. Does NOT auto-stage all changes — pass explicit files or pre-stage with git_add. Supports author_name/author_email and set_upstream for environment-safe git automation.",
		Groups:      []string{"workflow", "git"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Specific files to stage before committing. If empty, only commits what is already staged.",
				},
				"description": map[string]any{"type": "string", "description": "Brief context of what was done (e.g., 'Fix login bug')"},
				"author_name":  map[string]any{"type": "string", "description": "Optional git author/committer name override for environments without configured identity"},
				"author_email": map[string]any{"type": "string", "description": "Optional git author/committer email override for environments without configured identity"},
				"auto_push":    map[string]any{"type": "boolean", "description": "Push to remote after commit (default: false)"},
				"set_upstream": map[string]any{"type": "boolean", "description": "Use --set-upstream/-u when pushing so the branch tracks the remote on first push"},
				"working_dir": map[string]any{
					"type":        "string",
					"description": "Absolute path to the git repository. Defaults to the current working directory.",
				},
			},
		},
		Handler: gitAutoCommitHandler(provider),
	}
}

func gitAutoCommitHandler(provider llm.LLMProvider) models.ToolHandler {
	return func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		args := call.Arguments
		dir, _ := args["working_dir"].(string)
		description, _ := args["description"].(string)
		autoPush, _ := args["auto_push"].(bool)
		setUpstream, _ := args["set_upstream"].(bool)
		rawFiles, _ := args["files"].([]interface{})
		baseDir, err := gitWorkingDir(dir)
		if err != nil {
			return toolResult(call, ""), err
		}

		// 1. Stage specific files if provided
		if len(rawFiles) > 0 {
			repoRoot, err := gitRepoRoot(ctx, dir)
			if err != nil {
				return toolResult(call, ""), fmt.Errorf("resolve git repo root: %w", err)
			}
			var files []string
			for _, f := range rawFiles {
				if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
					normalized, err := normalizeGitPath(repoRoot, baseDir, s)
					if err != nil {
						return toolResult(call, ""), err
					}
					files = append(files, normalized)
				}
			}
			if len(files) == 0 {
				return toolResult(call, ""), fmt.Errorf("files parameter provided but contains no valid paths")
			}

			// Check for pre-existing staged files BEFORE staging.
			// This avoids modifying the index if we're going to reject the operation.
			preStagedOutput, _ := runGit(ctx, dir, "diff", "--cached", "--name-only")
			preStaged := toSet(splitLines(preStagedOutput))
			requested := toSet(files)
			var extras []string
			for f := range preStaged {
				if !requested[f] {
					extras = append(extras, f)
				}
			}
			if len(extras) > 0 {
				return toolResult(call, ""), fmt.Errorf(
					"repository has pre-staged files not in the requested list (%s). Use git_reset to unstage them first, or omit files to commit all staged changes",
					strings.Join(extras, ", "),
				)
			}

			// Safe to stage now — no pre-existing staged files to conflict.
			stageArgs := append([]string{"add", "--"}, files...)
			if output, err := runGit(ctx, dir, stageArgs...); err != nil {
				return toolResult(call, ""), fmt.Errorf("git add failed: %s: %w", output, err)
			}
		}

		// 2. Check staged changes
		stagedOutput, err := runGit(ctx, dir, "diff", "--cached", "--name-only")
		if err != nil {
			return toolResult(call, ""), fmt.Errorf("failed to check staged changes: %w", err)
		}
		if strings.TrimSpace(stagedOutput) == "" {
			return toolResult(call, ""), fmt.Errorf("no staged changes to commit — use git_add first or pass files parameter")
		}

		// 3. Gather context for message generation — only use staged info
		diffOutput, _ := runGit(ctx, dir, "diff", "--cached")
		statOutput, _ := runGit(ctx, dir, "diff", "--cached", "--stat")
		stagedFiles := splitLines(stagedOutput)
		logOutput, _ := runGit(ctx, dir, "log", "--oneline", "-5")

		// 4. Generate commit message via LLM
		commitMessage := generateLLMCommitMessage(ctx, provider, description, stagedFiles, diffOutput, statOutput, logOutput)
		if commitMessage == "" {
			commitMessage = generateFallbackMessage(description, stagedFiles)
		}

		commitEnv, err := gitCommitEnv(args)
		if err != nil {
			return toolResult(call, ""), err
		}
		if len(commitEnv) == 0 {
			if _, err := runGit(ctx, dir, "var", "GIT_AUTHOR_IDENT"); err != nil {
				return toolResult(call, ""), fmt.Errorf("git commit failed: git user identity is not configured; provide author_name and author_email or configure git user.name/user.email")
			}
		}

		// 5. Commit
		commitOutput, err := runGitWithEnv(ctx, dir, commitEnv, "commit", "-m", commitMessage)
		if err != nil {
			return toolResult(call, ""), fmt.Errorf("git commit failed: %s: %w", commitOutput, err)
		}

		result := map[string]interface{}{
			"message": commitMessage,
			"output":  commitOutput,
			"files":   stagedFiles,
			"stats":   strings.TrimSpace(statOutput),
		}

		// 6. Auto push — failure is returned as error, not silently embedded
		if autoPush {
			branchOutput, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
			if err != nil {
				data, _ := json.Marshal(result)
				return toolResult(call, string(data)), fmt.Errorf("commit succeeded but push failed: could not determine branch: %w", err)
			}
			branch := strings.TrimSpace(branchOutput)
			pushArgs := []string{"push"}
			if setUpstream {
				pushArgs = append(pushArgs, "--set-upstream")
			}
			pushArgs = append(pushArgs, "origin", branch)
			pushOutput, err := runGit(ctx, dir, pushArgs...)
			if err != nil {
				data, _ := json.Marshal(result)
				return toolResult(call, string(data)), fmt.Errorf("commit succeeded but push failed: %s: %w", pushOutput, err)
			}
			result["pushed"] = true
			result["set_upstream"] = setUpstream
		}

		data, _ := json.Marshal(result)
		return toolResult(call, string(data)), nil
	}
}

// toSet converts a string slice to a set for membership checks.
func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		if item != "" {
			set[item] = true
		}
	}
	return set
}

// splitLines splits a string by newlines, trimming whitespace and filtering empties.
func splitLines(s string) []string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	var result []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// generateLLMCommitMessage calls the LLM to produce a conventional commit message.
// stagedFiles is the list of files about to be committed (from cached diff).
// Returns empty string on any failure (caller should fall back).
func generateLLMCommitMessage(ctx context.Context, provider llm.LLMProvider, description string, stagedFiles []string, diff, stat, log string) string {
	if provider == nil || strings.TrimSpace(diff) == "" {
		return ""
	}

	truncated := diff
	if len(truncated) > maxDiffChars {
		truncated = truncated[:maxDiffChars] + fmt.Sprintf("\n... [truncated: showing %d of %d bytes]", maxDiffChars, len(diff))
	}

	userPrompt := "Recent commits:\n" + log + "\n\nStaged files:\n" + strings.Join(stagedFiles, "\n") + "\n\nStats:\n" + stat + "\n\nDiff:\n" + truncated
	if description != "" {
		userPrompt = "Context: " + description + "\n\n" + userPrompt
	}

	temp := 0.1
	maxTokens := 100
	model := resolveGitModel()

	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:        model,
		Messages:     []models.Message{{Role: models.RoleHuman, Content: userPrompt}},
		SystemPrompt: commitMessageSystemPrompt,
		Temperature:  &temp,
		MaxTokens:    &maxTokens,
	})
	if err != nil || resp.Message.Content == "" {
		return ""
	}
	return cleanCommitMessage(resp.Message.Content)
}

// generateFallbackMessage produces a basic commit message when LLM is unavailable.
func generateFallbackMessage(description string, stagedFiles []string) string {
	if description == "" {
		description = "update"
	}
	if len(stagedFiles) == 0 {
		return fmt.Sprintf("%s: update files", description)
	}
	return fmt.Sprintf("%s: %d file(s)", description, len(stagedFiles))
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

func gitWorkingDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return os.Getwd()
	}
	return filepath.Abs(dir)
}

func gitRepoRoot(ctx context.Context, dir string) (string, error) {
	output, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Abs(strings.TrimSpace(output))
}

func normalizeGitPath(repoRoot, baseDir, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("file path is required")
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve file path %q: %w", raw, err)
	}
	relPath, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("normalize file path %q: %w", raw, err)
	}
	relPath = filepath.Clean(relPath)
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q is outside repository root %q", raw, repoRoot)
	}
	return filepath.ToSlash(relPath), nil
}

// cleanCommitMessage sanitizes LLM output into a valid one-line commit message.
func cleanCommitMessage(s string) string {
	msg := strings.TrimSpace(s)
	if strings.HasPrefix(msg, "```") {
		if idx := strings.Index(msg, "\n"); idx >= 0 {
			msg = msg[idx+1:]
		} else {
			msg = strings.TrimPrefix(msg, "```")
		}
	}
	msg = strings.TrimSuffix(msg, "```")
	msg = strings.TrimSpace(msg)
	msg = strings.Trim(msg, "\"'`")
	msg = strings.TrimSpace(msg)
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg
}

func resolveGitModel() string {
	if m := strings.TrimSpace(os.Getenv("DEEPAI_GIT_MODEL")); m != "" {
		return m
	}
	return strings.TrimSpace(os.Getenv("DEEPAI_MODEL"))
}

// runGit executes a git command with optional working directory.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitWithEnv(ctx, dir, nil, args...)
}

func runGitWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func toolResult(call models.ToolCall, content string) models.ToolResult {
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: content}
}
