package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	builtin "github.com/millken/deepai/pkg/tools/builtin"
)

func recentConversationContext(messages []models.Message) string {
	const maxMessages = 6
	const maxBytes = 4000
	var parts []string
	for i := len(messages) - 1; i >= 0 && len(parts) < maxMessages; i-- {
		m := messages[i]
		if m.Role != models.RoleHuman && m.Role != models.RoleAI {
			continue
		}
		if c := strings.TrimSpace(m.Content); c != "" {
			parts = append(parts, c)
		}
	}
	joined := strings.Join(parts, "\n")
	if len(joined) > maxBytes {
		joined = joined[len(joined)-maxBytes:]
	}
	return joined
}

func (a *Agent) BuildSystemPrompt(ctx context.Context, sessionID string, runMessages []models.Message) string {
	sections := []string{strings.TrimSpace(a.systemPrompt)}

	if a.memoryService != nil {
		activeSource := ""
		if skillName := a.ActiveSkill(); skillName != "" {
			activeSource = "skill:" + skillName
		}
		relevanceContext := recentConversationContext(runMessages)
		if uid := strings.TrimSpace(a.memoryUserID); uid != "" {
			if userMem := a.memoryService.InjectScopeWithContext(ctx, memory.UserScope(uid), relevanceContext, activeSource); userMem != "" {
				sections = append(sections, userMem)
			}
		}
		if strings.TrimSpace(sessionID) != "" {
			if injection := a.memoryService.InjectWithContext(ctx, sessionID, relevanceContext, activeSource); injection != "" {
				sections = append(sections, injection)
			}
		}
	}

	// T5c: only carry the file-operation routing rule when the agent has ANY of
	// the dedicated file tools it references — an agent with edit_file but not
	// read_file still needs "use edit_file, not sed -i". Only a truly file-tool-
	// less agent (e.g. bash-only) omits the ~400-char rule.
	if a.hasAnyFileTool() {
		sections = append(sections, "File-operation rule: ALWAYS use the dedicated tools, never bash, to read, edit, write, search, or list files \xe2\x80\x94 read_file (not cat/head/tail/sed), edit_file (not sed/awk/perl), write_file (not echo>/cat>/tee), list_dir (not ls), find (not the find command), grep (not grep/rg/ag). If an edit_file call fails to match, re-read the file with read_file and retry edit_file; do NOT fall back to bash sed/perl. For git operations, use bash commands (git status, git diff, git log, etc.) rather than dedicated git tools.")
	}

	// M2.2+: Smart tool selection guidance for search operations
	if a.hasSearchTools() {
		sections = append(sections, builtin.GetToolRecommendations())
	}

	// Team awareness: when the agent can spawn sub-agents (has the task tool),
	// inject delegation guidance so it knows when to delegate vs do itself.
	// Skipped for non-interactive agents (sub-agents) to avoid recursion, and
	// when the catalog is empty (no agents to delegate to).
	// Note: plan mode replaces a.tools (enterPlanMode), removing the task tool,
	// so this block is naturally skipped — that prevents using sub-agents to
	// bypass plan-mode file restrictions.
	if !a.nonInteractive && a.tools.Get("task") != nil && len(a.agentCatalog) > 0 {
		sections = append(sections, renderDelegationPrompt(a.agentCatalog))
	}

	sections = a.appendPlanModePrompt(sections)
	return strings.Join(sections, "\n\n")
}

// hasAnyFileTool reports whether any of the dedicated file tools named by the
// file-operation rule is registered.
func (a *Agent) hasAnyFileTool() bool {
	if a == nil || a.tools == nil {
		return false
	}
	for _, name := range []string{"read_file", "edit_file", "write_file", "list_dir", "find", "grep"} {
		if a.tools.Get(name) != nil {
			return true
		}
	}
	return false
}

// hasSearchTools reports whether search-related tools are registered
func (a *Agent) hasSearchTools() bool {
	if a == nil || a.tools == nil {
		return false
	}
	// Check for grep and bash (bash can be used for file searches and git operations)
	for _, name := range []string{"grep", "bash"} {
		if a.tools.Get(name) != nil {
			return true
		}
	}
	return false
}

func buildSystemPrompt(base string, date string) string {
	var b strings.Builder
	if base != "" {
		b.WriteString(base)
		b.WriteString("\n\n")
	}
	b.WriteString("# Current date\nToday's date is ")
	b.WriteString(date)
	b.WriteByte('.')
	return b.String()
}

// delegationStrategy is the static policy text for team delegation. The agent
// catalog (available types) is rendered separately from the live EnumerateAgents
// result, so this text never lists specific agent names.
const delegationStrategy = `# Team Delegation

You lead a team of specialized sub-agents. Use the task tool to delegate when a sub-agent can do a better job than you.

## When to delegate

- Complex feature (new page, new module, multi-file change) → delegate implementation.
- UI/design work, requirements analysis, technical design, deep code review → delegate to the matching specialist.
- For multi-step projects, run sub-agents in dependency order. A sub-agent's result comes back as the task tool's return value; pass relevant context from prior steps in the next sub-agent's prompt.

## When NOT to delegate

- Simple edits, quick fixes, answering questions → do it yourself.
- You already know the answer from context → just answer.

## How to delegate

- Give the sub-agent a self-contained prompt with all needed context (file paths, requirements, constraints).
- Sub-agents cannot see your conversation history. They start fresh. Always include: what to do, what input/context it needs, and what its final answer should contain.
- Pass the files the sub-agent needs via context_files instead of pasting their contents into the prompt.
- After a sub-agent completes, review its output before proceeding. If wrong, re-invoke with corrections.

## Parallel delegation

- Fan out independent sub-tasks as multiple task calls in ONE response; they run concurrently (bounded by the pool).
- Parallel tasks MUST operate on disjoint file sets and MUST NOT both run git operations (they share the working tree and git index).
- Prefer parallel fan-out for independent read/analysis work; use serial, dependency-ordered calls when a later step needs an earlier one's result.
- Each sub-agent costs tokens — don't fan out for trivial work.`

// renderDelegationPrompt combines the static strategy text with a dynamically
// rendered agent catalog, so the prompt always reflects the actual available
// types (project > plugin > builtin) instead of a hardcoded list.
func renderDelegationPrompt(catalog []AgentInfo) string {
	var b strings.Builder
	b.WriteString(delegationStrategy)
	b.WriteString("\n\n## Available agents\n")
	b.WriteString("Use these agent_type values with the task tool:\n\n")
	for _, a := range catalog {
		desc := strings.TrimSpace(a.Description)
		desc = strings.ReplaceAll(desc, "\n", " ")
		if len([]rune(desc)) > 100 {
			desc = string([]rune(desc)[:99]) + "…"
		}
		fmt.Fprintf(&b, "- **%s** — %s\n", a.Type, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}
