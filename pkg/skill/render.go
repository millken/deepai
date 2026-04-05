package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sessionIDKey is the context key for session ID.
type sessionIDKey struct{}

// SessionIDFromContext extracts the session ID from context.
// Returns empty string if not set.
func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// ContextWithSessionID returns a new context carrying the session ID.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

const (
	defaultShell   = "bash"
	injectTimeout  = 30 * time.Second
	maxOutputBytes = 64 * 1024 // 64KB cap per command output
)

// dynamicInjectionRegex matches !`command` blocks.
var dynamicInjectionRegex = regexp.MustCompile("!`([^`]*)`")

// argAllRegex matches $ARGUMENTS when not followed by [ or a digit.
var (
	argAllRegex   = regexp.MustCompile(`\$ARGUMENTS`)
	argIndexRegex = regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]`)
	argShortRegex = regexp.MustCompile(`\$(\d+)`)
	envVarRegex   = regexp.MustCompile(`\$\{(\w+)\}`)
)

// Render performs string replacement and dynamic context injection.
// Order: 1) replace variables, 2) execute !`command` blocks.
func Render(ctx context.Context, body string, args string, skill *Skill) (string, error) {
	sessionID := SessionIDFromContext(ctx)

	// 1. Replace variables
	rendered := replaceVariables(body, args, skill, sessionID)

	// 2. Execute !`command` blocks
	injected, err := injectDynamicContext(ctx, rendered, args, skill)
	if err != nil {
		return rendered, err // return partially rendered content
	}

	return injected, nil
}

// replaceVariables handles $ARGUMENTS, $N, $ARGUMENTS[N], ${SESSION_ID}, ${SKILL_DIR}.
// Inside !`command` blocks, args are replaced with quoted env var references for security.
// Outside !`command` blocks, args are replaced with actual values for the LLM to read.
func replaceVariables(body string, args string, skill *Skill, sessionID string) string {
	argParts := splitArgs(args)
	injections := ParseDynamicInjections(body)

	var buf strings.Builder
	lastEnd := 0
	hasArgRef := false

	for _, inj := range injections {
		// Non-command text before this injection
		if inj.Start > lastEnd {
			text := body[lastEnd:inj.Start]
			replaced, found := replaceTextArgs(text, args, argParts)
			buf.WriteString(replaced)
			if found {
				hasArgRef = true
			}
		}

		// Command block — replace args with quoted env var references
		cmd := replaceCommandArgs(inj.Command)
		buf.WriteString("!`")
		buf.WriteString(cmd)
		buf.WriteString("`")

		lastEnd = inj.End
	}

	// Remaining non-command text after last injection
	if lastEnd < len(body) {
		text := body[lastEnd:]
		replaced, found := replaceTextArgs(text, args, argParts)
		buf.WriteString(replaced)
		if found {
			hasArgRef = true
		}
	}

	result := buf.String()

	// If no arg reference found in non-command text, append args
	if !hasArgRef && args != "" {
		result = strings.TrimSpace(result) + "\n\nARGUMENTS: " + args
	}

	// ${SESSION_ID}, ${SKILL_DIR}
	result = replaceEnvVars(result, skill, sessionID)

	return result
}

// replaceTextArgs replaces $ARGUMENTS, $ARGUMENTS[N], $N with actual values
// in non-command text. Returns (replaced text, whether any arg reference was found).
func replaceTextArgs(text string, args string, argParts []string) (string, bool) {
	found := false

	// Step 1: $ARGUMENTS[N] → actual value (most specific first)
	indices := argIndexRegex.FindAllStringSubmatchIndex(text, -1)
	if len(indices) > 0 {
		found = true
		var buf strings.Builder
		lastEnd := 0
		for _, m := range indices {
			buf.WriteString(text[lastEnd:m[0]])
			idx, _ := strconv.Atoi(text[m[2]:m[3]])
			if idx < len(argParts) {
				buf.WriteString(argParts[idx])
			} else {
				buf.WriteString(text[m[0]:m[1]])
			}
			lastEnd = m[1]
		}
		buf.WriteString(text[lastEnd:])
		text = buf.String()
	}

	// Step 2: Standalone $ARGUMENTS (not followed by [ or digit) → all args
	allIndices := argAllRegex.FindAllStringSubmatchIndex(text, -1)
	if len(allIndices) > 0 {
		hasStandalone := false
		for _, m := range allIndices {
			if m[1] >= len(text) || (text[m[1]] != '[' && (text[m[1]] < '0' || text[m[1]] > '9')) {
				hasStandalone = true
				break
			}
		}
		if hasStandalone {
			found = true
			var buf strings.Builder
			lastEnd := 0
			for _, m := range allIndices {
				isIndexed := m[1] < len(text) &&
					(text[m[1]] == '[' || (text[m[1]] >= '0' && text[m[1]] <= '9'))
				buf.WriteString(text[lastEnd:m[0]])
				if isIndexed {
					buf.WriteString(text[m[0]:m[1]])
				} else {
					buf.WriteString(args)
				}
				lastEnd = m[1]
			}
			buf.WriteString(text[lastEnd:])
			text = buf.String()
		}
	}

	// Step 3: $N → actual value
	shortIndices := argShortRegex.FindAllStringSubmatchIndex(text, -1)
	if len(shortIndices) > 0 {
		found = true
		var buf strings.Builder
		lastEnd := 0
		for _, m := range shortIndices {
			buf.WriteString(text[lastEnd:m[0]])
			idx, _ := strconv.Atoi(text[m[2]:m[3]])
			if idx < len(argParts) {
				buf.WriteString(argParts[idx])
			} else {
				buf.WriteString(text[m[0]:m[1]])
			}
			lastEnd = m[1]
		}
		buf.WriteString(text[lastEnd:])
		text = buf.String()
	}

	return text, found
}

// replaceCommandArgs replaces $ARGUMENTS, $ARGUMENTS[N], $N with quoted env var
// references inside !`command` blocks. This prevents command injection — user input
// is never interpolated into the command string, only passed via environment variables.
func replaceCommandArgs(cmd string) string {
	// Step 1: $ARGUMENTS[N] → "$SKILL_ARG_N"
	indices := argIndexRegex.FindAllStringSubmatchIndex(cmd, -1)
	if len(indices) > 0 {
		var buf strings.Builder
		lastEnd := 0
		for _, m := range indices {
			buf.WriteString(cmd[lastEnd:m[0]])
			buf.WriteString(`"$SKILL_ARG_` + cmd[m[2]:m[3]] + `"`)
			lastEnd = m[1]
		}
		buf.WriteString(cmd[lastEnd:])
		cmd = buf.String()
	}

	// Step 2: Standalone $ARGUMENTS → "$SKILL_ARGS"
	allIndices := argAllRegex.FindAllStringSubmatchIndex(cmd, -1)
	if len(allIndices) > 0 {
		var buf strings.Builder
		lastEnd := 0
		for _, m := range allIndices {
			isIndexed := m[1] < len(cmd) &&
				(cmd[m[1]] == '[' || (cmd[m[1]] >= '0' && cmd[m[1]] <= '9'))
			buf.WriteString(cmd[lastEnd:m[0]])
			if isIndexed {
				buf.WriteString(cmd[m[0]:m[1]])
			} else {
				buf.WriteString(`"$SKILL_ARGS"`)
			}
			lastEnd = m[1]
		}
		buf.WriteString(cmd[lastEnd:])
		cmd = buf.String()
	}

	// Step 3: $N → "$SKILL_ARG_N"
	shortIndices := argShortRegex.FindAllStringSubmatchIndex(cmd, -1)
	if len(shortIndices) > 0 {
		var buf strings.Builder
		lastEnd := 0
		for _, m := range shortIndices {
			buf.WriteString(cmd[lastEnd:m[0]])
			buf.WriteString(`"$SKILL_ARG_` + cmd[m[2]:m[3]] + `"`)
			lastEnd = m[1]
		}
		buf.WriteString(cmd[lastEnd:])
		cmd = buf.String()
	}

	return cmd
}

// replaceEnvVars replaces ${SESSION_ID} and ${SKILL_DIR} with actual values.
func replaceEnvVars(text string, skill *Skill, sessionID string) string {
	return envVarRegex.ReplaceAllStringFunc(text, func(match string) string {
		sub := envVarRegex.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		switch sub[1] {
		case "SESSION_ID":
			return sessionID
		case "SKILL_DIR":
			return skill.Dir
		default:
			return match
		}
	})
}

// injectDynamicContext finds all !`command` blocks, executes them safely,
// and replaces them with their output.
func injectDynamicContext(ctx context.Context, content string, args string, skill *Skill) (string, error) {
	matches := dynamicInjectionRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	// Parse args into positional slices for env vars
	argParts := splitArgs(args)

	// Build from end to start to preserve indices
	result := content
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		fullStart := m[0]
		fullEnd := m[1]
		cmdStr := content[m[2]:m[3]]

		output, execErr := executeCommand(ctx, cmdStr, argParts, skill)
		if execErr != nil {
			output = fmt.Sprintf("[命令执行失败: %s]", execErr)
		}

		result = result[:fullStart] + output + result[fullEnd:]
	}

	return result, nil
}

// executeCommand runs a shell command safely with args passed as environment variables.
func executeCommand(ctx context.Context, cmdStr string, argParts []string, skill *Skill) (string, error) {
	shell := defaultShell
	if skill != nil && skill.Meta.Shell != "" {
		shell = skill.Meta.Shell
	}

	ctx, cancel := context.WithTimeout(ctx, injectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-c", cmdStr)

	// Pass args as environment variables (never interpolate into command string).
	// Inherit parent env so tools like gh, git, docker can find their configs.
	env := os.Environ()
	env = append(env, fmt.Sprintf("SKILL_ARGS=%s", strings.Join(argParts, " ")))
	for i, arg := range argParts {
		env = append(env, fmt.Sprintf("SKILL_ARG_%d=%s", i, arg))
	}
	if skill != nil {
		env = append(env, fmt.Sprintf("SKILL_DIR=%s", skill.Dir))
	}
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}

	// Truncate oversized output
	if len(output) > maxOutputBytes {
		output = output[:maxOutputBytes]
		output = append(output, []byte("\n... [output truncated]")...)
	}

	return strings.TrimSpace(string(output)), nil
}

// splitArgs splits arguments string by whitespace.
func splitArgs(args string) []string {
	if args == "" {
		return nil
	}
	return strings.Fields(args)
}

// ParseDynamicInjections extracts all !`command` blocks for analysis.
func ParseDynamicInjections(content string) []DynamicInjection {
	matches := dynamicInjectionRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil
	}

	injections := make([]DynamicInjection, 0, len(matches))
	for _, m := range matches {
		raw := content[m[0]:m[1]]
		cmd := content[m[2]:m[3]]

		injections = append(injections, DynamicInjection{
			Raw:     raw,
			Command: cmd,
			Start:   m[0],
			End:     m[1],
			HasArgs: strings.Contains(cmd, "$ARGUMENTS") ||
				argIndexRegex.MatchString(cmd) ||
				argShortRegex.MatchString(cmd),
		})
	}

	return injections
}
