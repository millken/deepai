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

	// M5 todo tool: this guidance is STATIC (when to build/update a plan
	// never changes turn to turn), unlike the todo LIST itself (which
	// changes every todo_write call and therefore lives in buildTurnInjection
	// instead — see formatTodoNote's doc comment for why mixing the two up
	// would break prefix caching). Gated on tool presence like
	// hasAnyFileTool/hasSearchTools above, so an agent type that never gets
	// todo_write registered doesn't carry dead instructions.
	if a.hasTodoTool() {
		sections = append(sections, todoUsagePrompt)
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

// hasTodoTool reports whether the todo_write tool is registered.
func (a *Agent) hasTodoTool() bool {
	if a == nil || a.tools == nil {
		return false
	}
	return a.tools.Get("todo_write") != nil
}

// todoUsagePrompt is deliberately NOT a mandate ("you must call todo_write
// before any tool use") — the M5 todo-tool design explicitly rejects
// enforcing that (no gate blocking other tools until a plan exists). It only
// tells the model WHEN a plan is worth writing and that every call replaces
// the whole list, leaving the judgment call itself to the model.
//
// Review hardening (weak-model, e.g. GLM-class): the original wording left
// three things for the model to guess at, and a weak model reliably guessed
// wrong —
//   - the FIRST call's status: the in_progress rule only appeared in the
//     "after finishing each step" sentence, so a weak model tended to write
//     everything "pending" and never mark anything in_progress until the
//     first step was already done;
//   - granularity: no guidance invited either a single mega-item or a
//     30-item list that then gets resent whole on every single write;
//   - completion: nothing said what to do with a finished plan, so a model
//     would sometimes pass an empty list on its last step to mean "done" —
//     indistinguishable from "never planned at all" once rendered (an empty
//     list and a cleared list both format to ""), losing the completion
//     record for no reason.
const todoUsagePrompt = "Task planning: before starting a multi-step task (anything that will take several tool calls " +
	"to finish), call todo_write with your full plan — mark the FIRST item in_progress and every other item pending. " +
	"Aim for roughly 3-7 items: too few loses the benefit of writing a plan down at all, too many is expensive to " +
	"resend in full on every single update. After finishing each step, call it again with the FULL list — never just " +
	"the changed item — moving that step to done and, if there's a next one, marking exactly ONE item in_progress. " +
	"When every item is finished, call it once more with every item marked done — do not clear the list to signal " +
	"completion; a cleared list looks identical to a task that was never planned. Skip it for a quick single-step " +
	"request."

// dateNoteFormat is shared by buildTurnInjection and its tests: the
// system-note-style date line appended to every turn injection, mirroring
// the "[System note: ...]" framing pkg/memory/prompt.go already uses for its
// own memory-context wrapper (see buildInjectionWithIDs).
const dateNoteFormat = "[System note: Today's date is %s.]"

// todoNoteHeader introduces the rendered task list inside the turn
// injection. Framed as a "[System note: ...]" the same way dateNoteFormat
// and pkg/memory/prompt.go's memory wrapper are, so the model recognizes it
// as the same family of injected, non-conversational context.
//
// Review hardening: this header re-asserts the plan on EVERY single request
// (unlike todoUsagePrompt, which the model only reads once, in the system
// prompt) — it is the single strongest anti-drift lever this feature has,
// but originally only said HOW to change the list, never WHEN. The added
// reconciliation sentence turns it from a passive status display into an
// actual drift-catching mechanism: if the model's current action doesn't
// match the item marked in_progress, this is the prompt that should make it
// notice and fix the list before doing anything else.
const todoNoteHeader = "[System note: Current task list (call todo_write with the FULL list to change it). " +
	"If what you are doing right now doesn't match the item marked [~] in_progress, update the list with " +
	"todo_write before continuing:]"

// formatTodoNote renders the agent's current todo list (see
// pkg/tools/builtin/todo.go's TodoItem/RenderTodoList) for the turn
// injection. This is the load-bearing design decision of the M5 todo tool:
// the list is a full-table replace on every todo_write call, so its bytes
// change on every single write — baking it into BuildSystemPrompt (position
// 0 of every request) would invalidate the OpenAI-compat automatic
// prefix-cache (DeepSeek/Qwen/GLM: any byte change at position N invalidates
// the cache from N on) on every plan update, undoing exactly what M4-2
// fought to stabilize. Riding the trailing, once-per-Run-rebuilt turn
// injection instead (like date/memory) keeps the prefix — system prompt +
// tool schemas + full message history — byte-stable, while still surfacing
// the CURRENT list on every subsequent request, including after compaction
// has dropped the original todo_write tool_result out of the visible
// history window. Returns "" when there is no list yet (nothing to show —
// same as the pre-todo-tool baseline, and also what a cleared plan, i.e. an
// explicit empty todo_write call, looks like), and ALSO when hasTodoTool is
// false: plan mode (plan.go's enterPlanMode) swaps a.tools for a fixed
// read-only allowlist that never includes todo_write, but does NOT clear
// a.todos — a plan written before plan mode was entered (or carried across a
// Run boundary via SessionCarry, primed at New()) would otherwise keep
// asserting a list the model has no legal way to update or clear (the tool
// isn't callable, so the "call todo_write with the FULL list to change it"
// instruction in todoNoteHeader can't be followed) — directly contradicting
// TestPlanMode_ExcludesTodoWrite's intent that plan mode excludes this
// feature entirely, not just its static usage guidance in the system
// prompt.
func formatTodoNote(hasTodoTool bool, todos []builtin.TodoItem) string {
	if !hasTodoTool || len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(todoNoteHeader)
	b.WriteString("\n")
	b.WriteString(builtin.RenderTodoList(todos))
	return b.String()
}

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

	if note := formatTodoNote(a.hasTodoTool(), a.todos); note != "" {
		b.WriteString("\n\n")
		b.WriteString(note)
	}

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
- Scope each delegation to specific files/line ranges/symbols. Tell the sub-agent to explore via code_map symbol outlines and read_file with start_line/end_line instead of reading files end to end.
- If a delegation comes back too shallow, re-delegate a NARROWER and more SPECIFIC task (fewer files, explicit line ranges or symbols) — do NOT just raise max_tool_calls: an exhaustive uncapped read-through costs hours and hundreds of thousands of tokens for little extra signal.
- After a sub-agent completes, review its output before proceeding. If wrong, re-invoke with corrections.

## Parallel delegation

- When delegating two or more INDEPENDENT sub-tasks, emit ALL of the task calls in the SAME assistant message — one message, several task calls. Issuing them one per message runs them serially and wastes wall-clock time.
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
