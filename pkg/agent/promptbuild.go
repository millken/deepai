package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// BuildSystemPrompt assembles the request's system prompt from the
// session-stable pieces only: the base prompt, the file-op rule, tool
// recommendations, the delegation prompt + catalog, and plan-mode text.
//
// M4-2: this used to ALSO layer in per-request memory injections (user-scope
// + session-scope) and a "Today's date is X" line, making the returned
// string vary every single turn with whatever the memory relevance
// heuristic picked and with the calendar date — both sat at position 0 of
// every request and defeated automatic prefix caching on every OpenAI-
// compat provider (DeepSeek/Qwen/GLM: any byte change at position N
// invalidates the cache from N on). That volatile content now lives in a
// per-Run, once-computed TRAILING injection message instead — see
// buildTurnInjection and appendTurnInjection. This is why the method takes
// no parameters: nothing it assembles depends on the request's session,
// message history, or ctx anymore — buildTurnInjection is the new home for
// everything that did.
func (a *Agent) BuildSystemPrompt() string {
	sections := []string{strings.TrimSpace(a.systemPrompt)}

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

// dateNoteFormat is shared by buildTurnInjection and its tests: the
// system-note-style date line appended to every turn injection, mirroring
// the "[System note: ...]" framing pkg/memory/prompt.go already uses for its
// own memory-context wrapper (see buildInjectionWithIDs).
const dateNoteFormat = "[System note: Today's date is %s.]"

// buildTurnInjection assembles the per-Run volatile-content injection: the
// current date plus (when a memory service is configured) the user- and
// session-scope memory injections, each already self-wrapped by
// pkg/memory/prompt.go (buildInjectionWithIDs wraps them in
// "<memory-context>...[System note: recalled memory, not new user
// input.]...</memory-context>"). Returns a single RoleHuman message with a
// STABLE SHAPE across every call: the date line is always present, so the
// message always exists — only its total byte length varies with whether
// memory is configured and what it contains.
//
// M4-2 design (see task-22-brief.md): computed ONCE per Run, at Run start,
// from the runMessages present at that moment (see Run's call site in
// react.go) — memory extraction runs on an async 5-turn cadence elsewhere,
// so recomputing this per REQUEST inside the turn loop would buy nothing
// while costing prefix stability (every request before this trailing message
// would otherwise still be re-scored against a shifting relevance context).
// Subagents (no MemoryService) fall through the nil check below and get a
// date-only injection — same mechanism, no special-casing.
//
// The memory fence (activeSource = "skill:"+name, for cross-skill fact
// penalization) is evaluated here from a.ActiveSkill() at the moment this is
// called. At Run start that is "" for a session-less Run (nothing carried),
// but M4-3 (task-23-brief.md) can seed it non-empty from a carried
// SessionCarry — a skill loaded in a PREVIOUS Run sharing that session is
// already active when THIS Run's very first buildTurnInjection call happens,
// so the fence applies from request one, not just from a mid-Run load
// onward. Either way, "once per Run" is really "once per activeSource
// segment", not a hard one-shot: react.go's skill-result handling calls this
// AGAIN
// immediately after a.activeSkill.Store(name) whenever a "skill" tool call
// changes it mid-Run, so the injection IS re-fenced starting with the very
// next request after a skill loads — see the call site there for why that
// recompute is prefix-cache-free (AppendSystemPrompt already invalidated the
// prefix that same turn). Only genuinely per-request recomputation (once per
// REQUEST rather than once per activeSource change) is what this function
// avoids.
func (a *Agent) buildTurnInjection(ctx context.Context, sessionID string, runMessages []models.Message) models.Message {
	var b strings.Builder
	fmt.Fprintf(&b, dateNoteFormat, time.Now().Format("2006-01-02"))

	if a.memoryService != nil {
		activeSource := ""
		if skillName := a.ActiveSkill(); skillName != "" {
			activeSource = "skill:" + skillName
		}
		relevanceContext := recentConversationContext(runMessages)
		if uid := strings.TrimSpace(a.memoryUserID); uid != "" {
			if userMem := a.memoryService.InjectScopeWithContext(ctx, memory.UserScope(uid), relevanceContext, activeSource); userMem != "" {
				b.WriteString("\n\n")
				b.WriteString(userMem)
			}
		}
		if strings.TrimSpace(sessionID) != "" {
			if sessionMem := a.memoryService.InjectWithContext(ctx, sessionID, relevanceContext, activeSource); sessionMem != "" {
				b.WriteString("\n\n")
				b.WriteString(sessionMem)
			}
		}
	}

	return models.Message{Role: models.RoleHuman, Content: b.String()}
}

// appendTurnInjection returns a NEW slice — view is never mutated — with
// injection (see buildTurnInjection) appended as the final message. Every
// site in this package that (re)builds the message view actually sent to
// the provider calls this exactly once, so the M3 metering invariant holds:
// the estimate that decides whether to compact is computed over the same
// bytes the request actually sends, and the request sends what was measured.
//
// Position rationale (M4-2 design): appending at the END, rather than
// prepending or folding into the system prompt, keeps everything BEFORE it —
// the stable system prompt, tool schemas, and the entire canonical message
// history — a stable, monotonically growing prefix. That maximizes
// automatic prefix-cache reuse on providers that cache by byte-identical
// prefix (DeepSeek/Qwen/GLM). On a tool-call turn, the view ends [..,
// tool_result, injection]; the Anthropic mapper's appendOrMergeUser (see
// pkg/llm/anthropic.go) merges this trailing RoleHuman text into the same
// open user turn as the preceding tool_result blocks (tool_result blocks
// first, text after — contract-valid), exactly like the pre-existing
// RoleHuman "hint" messages it already handles (M1-7).
func appendTurnInjection(view []models.Message, injection models.Message) []models.Message {
	out := make([]models.Message, len(view)+1)
	copy(out, view)
	out[len(view)] = injection
	return out
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
